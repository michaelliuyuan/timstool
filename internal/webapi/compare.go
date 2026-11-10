package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/michaelliuyuan/timstool/internal/common"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"github.com/michaelliuyuan/timstool/internal/source"
	"github.com/michaelliuyuan/timstool/internal/validator"
	"go.uber.org/zap"
)

// Standalone data-comparison tasks (独立数据比对): run Validator against
// explicit source/target connections without any migration. Tasks are stored
// under dataDir/compare/{id}/ (task.json + report.json), separate from the
// migration task store; passwords are NEVER persisted in plaintext.

const (
	CompareStatusQueued    = "queued" // A1: waiting for the single run slot (FIFO queue)
	CompareStatusRunning   = "running"
	CompareStatusCompleted = "completed"
	CompareStatusFailed    = "failed"
	CompareStatusCancelled = "cancelled"
)

// CompareTaskRequest is the POST /compare/tasks body.
type CompareTaskRequest struct {
	Name                 string                  `json:"name"`
	Source               config.SourceConfig     `json:"source"`
	Target               config.TargetConfig     `json:"target"`
	SourceRef            string                  `json:"source_ref"` // datasource id (F-02)
	TargetRef            string                  `json:"target_ref"` // datasource id (tidb)
	Mode                 string                  `json:"mode"`       // quick | sample | checksum (= watermark.base_mode when watermark active)
	SampleRatio          float64                 `json:"sample_ratio"`
	ChecksumChunkSize    int64                   `json:"checksum_chunk_size"`
	ChecksumParallel     int                     `json:"checksum_parallel"` // deprecated (#t4): kept for compat, folded into Concurrency
	Parallel             int                     `json:"parallel"`          // deprecated (#t4): kept for compat, folded into Concurrency
	Concurrency          int                     `json:"concurrency"`       // #t4 unified budget 1-8 (0 = derive from legacy knobs)
	QueryTimeout         string                  `json:"query_timeout"`     // #t94 per-query deadline (empty = validator default 30m)
	Tables               []string                `json:"tables"`            // empty = all tables
	Watermark            *config.WatermarkFilter `json:"watermark,omitempty"`
	TableWatermarks      []config.TableWatermark `json:"table_watermarks,omitempty"` // #wm-override per-table filters (P1A)
	IgnoreColumns        []string                `json:"ignore_columns,omitempty"`   // #ignore value-comparison exclusion list
	PossiblyStuckTimeout string                  `json:"possibly_stuck_timeout"`     // #t95 P1 heartbeat threshold (empty = validator default 5m)
	// A2 multi-db (#102): databases holds explicit names and/or wildcard
	// patterns (* ?); exclude_databases subtracts from the resolved set.
	// Both absent/empty = the legacy single-database task (byte-compatible).
	Databases        []string `json:"databases,omitempty"`
	ExcludeDatabases []string `json:"exclude_databases,omitempty"`
}

// CompareTask is the persisted (password-redacted) compare task state.
type CompareTask struct {
	ID                   string                  `json:"id"`
	Name                 string                  `json:"name"`
	Status               string                  `json:"status"`
	Source               config.SourceConfig     `json:"source"`               // password redacted on disk
	Target               config.TargetConfig     `json:"target"`               // password redacted on disk
	SourceRef            string                  `json:"source_ref,omitempty"` // A1 queue: datasource id only (never a secret) — lets a queued task re-resolve credentials after a restart
	TargetRef            string                  `json:"target_ref,omitempty"` // A1 queue: datasource id only (never a secret)
	QueueSeq             uint64                  `json:"queue_seq,omitempty"`  // A1 queue: strict admission order (monotonic per dataDir), 0 = unset/legacy
	Mode                 string                  `json:"mode"`
	SampleRatio          float64                 `json:"sample_ratio"`
	ChecksumChunkSize    int64                   `json:"checksum_chunk_size"`
	ChecksumParallel     int                     `json:"checksum_parallel"` // deprecated (#t4)
	Parallel             int                     `json:"parallel"`          // deprecated (#t4)
	Concurrency          int                     `json:"concurrency"`       // #t4 unified budget (effective value, 0 = auto)
	QueryTimeout         string                  `json:"query_timeout"`     // #t94 per-query deadline (empty = validator default 30m)
	Tables               []string                `json:"tables"`
	Watermark            *config.WatermarkFilter `json:"watermark,omitempty"`
	TableWatermarks      []config.TableWatermark `json:"table_watermarks,omitempty"` // #wm-override per-table filters
	IgnoreColumns        []string                `json:"ignore_columns,omitempty"`
	TablesDone           int                     `json:"tables_done"`
	TablesTotal          int                     `json:"tables_total"`
	CurrentTable         string                  `json:"current_table,omitempty"`
	CurrentChunk         int                     `json:"current_chunk,omitempty"`          // #t95 P1: 1-based chunk ordinal (checksum mode)
	TotalChunks          int                     `json:"total_chunks,omitempty"`           // #t95 P1: chunk count upper bound (checksum mode)
	PossiblyStuck        bool                    `json:"possibly_stuck,omitempty"`         // #t95 P1: no progress within possibly_stuck_timeout
	LastProgressAt       time.Time               `json:"last_progress_at,omitempty"`       // #t95 P1: last per-table/per-chunk progress event
	PossiblyStuckTimeout string                  `json:"possibly_stuck_timeout,omitempty"` // #t95 P1: heartbeat threshold (empty = 5m)
	// A2 multi-db (#102). All omitempty: a legacy single-database task
	// persists byte-identically (no new keys), and old task.json files load
	// untouched. ResolvedDatabases/DatabasesDone/DatabasesTotal/
	// CurrentDatabase/DatabaseResults are run-time state written by the
	// multi-db loop (the resolved list persists before the first database
	// starts, so progress and the ?db= report routing survive a restart).
	Databases         []string                `json:"databases,omitempty"`
	ExcludeDatabases  []string                `json:"exclude_databases,omitempty"`
	ResolvedDatabases []string                `json:"resolved_databases,omitempty"`
	DatabasesDone     int                     `json:"databases_done,omitempty"`
	DatabasesTotal    int                     `json:"databases_total,omitempty"`
	CurrentDatabase   string                  `json:"current_database,omitempty"`
	DatabaseResults   []CompareDatabaseResult `json:"database_results,omitempty"`
	Error             string                  `json:"error,omitempty"`
	CreatedAt         time.Time               `json:"created_at"`
	StartedAt         *time.Time              `json:"started_at,omitempty"`
	FinishedAt        *time.Time              `json:"finished_at,omitempty"`
}

// compareQueuedCreds carries a queued task's passwords in memory ONLY (A1):
// task.json redacts them, so the same-process dispatch re-attaches them from
// here. After a restart the stash is gone — only datasource-ref tasks can
// re-enter the run (passwords are NEVER persisted; that red line stands).
type compareQueuedCreds struct {
	sourcePassword string
	targetPassword string
}

// compareState guards the single-running-compare invariant and serializes
// compare-options saves (read→merge→write must not interleave, or two
// concurrent PUTs can lose each other's update). A1: the FIFO queue lives on
// disk (status=queued task.json files); every queue decision — enqueue,
// dispatch, cancel, delete — happens under mu in the same critical section
// that guards the running slot, so queue-vs-slot races are impossible.
type compareState struct {
	mu          sync.Mutex
	cancel      context.CancelFunc // non-nil while a compare task is running
	runningID   string
	optionsMu   sync.Mutex                    // serializes compare-options load→merge→write
	queuedCreds map[string]compareQueuedCreds // A1: keyed by queued task id; memory only, never persisted
	queueSeq    uint64                        // A1: last admission sequence handed out (0 = not seeded yet)
}

// nextQueueSeq returns the next strict admission sequence (A1). Seeded once
// per process from the on-disk max so the order survives a restart; every
// caller already holds s.compare.mu, which makes the counter race-free.
func (s *Server) nextQueueSeq() uint64 {
	if s.compare.queueSeq == 0 {
		s.compare.queueSeq = s.maxPersistedQueueSeq()
	}
	s.compare.queueSeq++
	return s.compare.queueSeq
}

// maxPersistedQueueSeq scans the compare dir for the highest QueueSeq. Only
// the 0-seed path pays this O(N) scan.
func (s *Server) maxPersistedQueueSeq() uint64 {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "compare"))
	if err != nil {
		return 0
	}
	var max uint64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.loadCompareTask(e.Name())
		if err != nil || t == nil {
			continue
		}
		if t.QueueSeq > max {
			max = t.QueueSeq
		}
	}
	return max
}

func (s *Server) compareDir(id string) string {
	return filepath.Join(s.dataDir, "compare", id)
}

// compareIDRe is the {compareID} allow-list (MC-17 ⑥a): server-generated
// ids are uuid.New().String()[:8] — exactly eight lowercase hex chars.
// compareIDRe gates every /compare/tasks/{compareID} route BEFORE the id
// reaches the filesystem (compareDir/loadCompareTask/os.RemoveAll), closing
// the path-traversal surface (..%2F, %5C, overlong, uppercase all 404).
var compareIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// requireValidCompareID is the front gate middleware for the
// /compare/tasks/{compareID} subrouter: a non-conforming id is a 404 that
// never touches disk — indistinguishable from an unknown-but-well-formed id.
func (s *Server) requireValidCompareID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !compareIDRe.MatchString(chi.URLParam(r, "compareID")) {
			s.writeError(w, http.StatusNotFound, "compare task not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// errCompareCredsLost is the explicit failure for queued tasks whose inline
// credentials can no longer be attached in memory (restart recovery): task.json
// redacts passwords by design, so a task without datasource refs on BOTH ends
// can never run again — it fails visibly instead of waiting forever (A1).
var errCompareCredsLost = errors.New("凭据不随任务持久化，请重新提交")

// recoverStaleCompares reconciles on-disk compare tasks once at server
// startup, before any handler runs. Running tasks are marked failed — the
// process died mid-run, so they can never finish or be cancelled (M3:
// otherwise the orphan sticks in the history list forever). A1: queued tasks
// survive the restart — ref-backed ones are re-dispatched in FIFO order
// (credentials re-resolved from the datasource registry), inline-credential
// ones fail explicitly with errCompareCredsLost (the password red line means
// they can never be recovered silently).
func (s *Server) recoverStaleCompares() {
	// M1: a crash can leave row-hash spool temp files behind (the run that
	// would have cleaned them died). No compare can be running during
	// recovery, so the whole task-level spool tree is stale — purge it.
	if err := os.RemoveAll(filepath.Join(s.dataDir, "spool")); err != nil {
		zap.L().Warn("failed to purge stale compare spool dir", zap.Error(err))
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "compare"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.loadCompareTask(e.Name())
		if err != nil || t == nil {
			// P1-1: a corrupt task.json must not be skipped silently — the
			// compare would stay invisible-but-present forever (no history
			// entry, no dispatch). Repair it into an explicit failed record so
			// the operator sees it (the dir name is the id, so a minimal task
			// can be rebuilt even when the body is unparseable).
			s.failCorruptCompareOnRecovery(e.Name(), err)
			continue
		}
		switch t.Status {
		case CompareStatusRunning:
			t.Status = CompareStatusFailed
			t.Error = "interrupted by service restart"
			fin := time.Now()
			t.FinishedAt = &fin
			if err := s.persistCompareTask(t); err != nil {
				zap.L().Warn("failed to recover stale compare task", zap.String("id", t.ID), zap.Error(err))
			}
		case CompareStatusQueued:
			// A fresh process has an empty queuedCreds stash, so an end
			// without a datasource ref has no password source left. Fail
			// those now — visibly, never silently dropped.
			if t.SourceRef == "" || t.TargetRef == "" {
				t.Status = CompareStatusFailed
				t.Error = errCompareCredsLost.Error()
				fin := time.Now()
				t.FinishedAt = &fin
				if err := s.persistCompareTask(t); err != nil {
					zap.L().Warn("failed to recover unrecoverable queued compare task", zap.String("id", t.ID), zap.Error(err))
				}
				continue
			}
			// Ref-backed queued tasks stay queued; the dispatch below hands
			// the slot to the FIFO head.
		}
	}
	// A1: restart recovery — hand the free slot to the earliest queued task.
	s.dispatchNextCompare()
}

// failCorruptCompareOnRecovery rewrites an unreadable compare task.json as an
// explicit failed record (P1-1). Best-effort: if the directory itself is
// broken the warn log is the only footprint left. Never touches passwords.
func (s *Server) failCorruptCompareOnRecovery(dir string, loadErr error) {
	if !compareIDRe.MatchString(dir) {
		// Not a server-generated id — leave unknown dirs alone (nothing can
		// own them, and rewriting arbitrary paths would be a foot-gun).
		return
	}
	fin := time.Now()
	task := &CompareTask{
		ID:         dir,
		Name:       "Compare " + dir,
		Status:     CompareStatusFailed,
		Error:      "task.json 无法读取（可能损坏），已标记失败：" + loadErr.Error(),
		CreatedAt:  fin,
		FinishedAt: &fin,
	}
	if err := s.persistCompareTask(task); err != nil {
		zap.L().Warn("failed to repair corrupt compare task.json",
			zap.String("id", dir), zap.Error(err))
		return
	}
	zap.L().Warn("repairing corrupt compare task.json as failed",
		zap.String("id", dir), zap.Error(loadErr))
}

// earliestQueuedCompare returns the FIFO head among the on-disk queued tasks.
// Ordering is QueueSeq (strict admission order) when present, else CreatedAt
// then id as a legacy fallback. Caller must hold s.compare.mu.
func (s *Server) earliestQueuedCompare() *CompareTask {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "compare"))
	if err != nil {
		return nil
	}
	var head *CompareTask
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.loadCompareTask(e.Name())
		if err != nil || t == nil || t.Status != CompareStatusQueued {
			if err != nil {
				// P1-1: an unreadable queued record must never vanish silently
				// (the FE would show 「排队中 · 第N位」 forever with no dispatch).
				zap.L().Warn("queued compare task.json unreadable, skipping for this dispatch",
					zap.String("dir", e.Name()), zap.Error(err))
			}
			continue
		}
		if head == nil || queuedBefore(t, head) {
			head = t
		}
	}
	return head
}

// queuedBefore orders two queued tasks. QueueSeq (persisted at admission under
// compare.mu) is the strict FIFO key; CreatedAt/id only serves records written
// before QueueSeq existed (random uuid id ⇒ stable, not true clock order).
func queuedBefore(a, b *CompareTask) bool {
	if a.QueueSeq != 0 && b.QueueSeq != 0 {
		if a.QueueSeq != b.QueueSeq {
			return a.QueueSeq < b.QueueSeq
		}
		return a.ID < b.ID
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// attachQueuedCompareCredentials makes a queued task runnable. Passwords come
// from the in-memory stash first (same-process dispatch keeps the exact
// submit-time credentials); failing that, from re-resolving its datasource
// refs (the restart-recovery path — the registry may even have fresher
// credentials). Caller must hold s.compare.mu.
func (s *Server) attachQueuedCompareCredentials(t *CompareTask) error {
	if creds, ok := s.compare.queuedCreds[t.ID]; ok {
		// A1: the stash entry is dropped only AFTER the running claim hits
		// disk (dispatchNextCompare), so a failed persist leaves the queued
		// record and its in-memory credentials consistent for a retry.
		t.Source.Password = creds.sourcePassword
		t.Target.Password = creds.targetPassword
		return nil
	}
	if t.SourceRef == "" || t.TargetRef == "" {
		return errCompareCredsLost
	}
	se, err := s.resolveDataSourceRef(t.SourceRef)
	if err != nil {
		return fmt.Errorf("source_ref: %w", err)
	}
	if !srcCapable(se.Type, source.CapCompare) {
		return fmt.Errorf("source_ref: 比对源端数据源类型必须是 postgres 或 mysql")
	}
	te, err := s.resolveDataSourceRef(t.TargetRef)
	if err != nil {
		return fmt.Errorf("target_ref: %w", err)
	}
	// Rerouted through the registry predicate (type-branch fixture keeps its
	// single sanctioned POST-path guard; same MS-12 predicate pattern as
	// ddl_compare's tidb checks).
	if !source.IsTiDB(te.Type) {
		return fmt.Errorf("target_ref: 目标端数据源类型必须是 tidb")
	}
	t.Source = dataSourceToSourceConfig(se)
	t.Target = dataSourceToTargetConfig(te)
	return nil
}

// dispatchNextCompare hands the free run slot to the FIFO-head queued task
// (A1). Everything — slot re-check, credential attach, status flip, slot
// claim — happens under compare.mu, the same critical section POST-enqueue
// and finish-slot-release use, so the single-flight invariant (#t4/B2) holds
// across queueing. A queued task that cannot be readied (unrecoverable or
// unresolvable credentials) fails explicitly and the next one is tried.
func (s *Server) dispatchNextCompare() {
	for {
		s.compare.mu.Lock()
		if s.compare.cancel != nil {
			s.compare.mu.Unlock()
			return
		}
		next := s.earliestQueuedCompare()
		if next == nil {
			s.compare.mu.Unlock()
			return
		}
		if err := s.attachQueuedCompareCredentials(next); err != nil {
			next.Status = CompareStatusFailed
			next.Error = err.Error()
			fin := time.Now()
			next.FinishedAt = &fin
			delete(s.compare.queuedCreds, next.ID)
			if perr := s.persistCompareTask(next); perr != nil {
				// The failure never hit disk — the task is still queued on
				// disk, so retrying it in this loop would spin forever. Stop;
				// the next finish/recovery retries the dispatch.
				s.compare.mu.Unlock()
				zap.L().Warn("failed to persist undispatchable queued compare task", zap.String("id", next.ID), zap.Error(perr))
				return
			}
			s.compare.mu.Unlock()
			zap.L().Warn("queued compare task cannot be dispatched", zap.String("id", next.ID), zap.Error(err))
			continue
		}
		now := time.Now()
		next.Status = CompareStatusRunning
		next.StartedAt = &now // M2: set before the goroutine spawns (no data race on the response)
		if err := s.persistCompareTask(next); err != nil {
			// The claim never hit disk — leave the on-disk copy queued (and its
			// in-memory credentials stashed) and stop; the next finish/recovery
			// retries the dispatch.
			s.compare.mu.Unlock()
			zap.L().Warn("failed to persist dispatched compare task", zap.String("id", next.ID), zap.Error(err))
			return
		}
		delete(s.compare.queuedCreds, next.ID) // the running claim is on disk; the stash is consumed
		ctx, cancel := context.WithCancel(context.Background())
		s.compare.cancel = cancel
		s.compare.runningID = next.ID
		s.compare.mu.Unlock()
		go s.runCompare(ctx, next)
		return
	}
}

// persistCompareTask writes task.json with passwords redacted (atomic temp+rename).
func (s *Server) persistCompareTask(task *CompareTask) error {
	dir := s.compareDir(task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	redacted := *task
	redacted.Source.Password = ""
	redacted.Target.Password = ""
	data, err := json.MarshalIndent(&redacted, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "task-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, "task.json"))
}

// compareOptionsBody is the persisted shape of the compare page's saved
// connection profile (dataDir/compare-options.json). Passwords are NEVER
// persisted nor returned: PUT ignores them, GET always returns them empty.
type compareOptionsBody struct {
	SourceType        string                  `json:"source_type"`
	Source            config.SourceConfig     `json:"source"` // password never stored
	Target            config.TargetConfig     `json:"target"` // password never stored
	Mode              string                  `json:"mode"`
	SampleRatio       float64                 `json:"sample_ratio"`
	ChecksumChunkSize int64                   `json:"checksum_chunk_size"`
	ChecksumParallel  int                     `json:"checksum_parallel"`          // deprecated (#t4)
	Parallel          int                     `json:"parallel"`                   // deprecated (#t4)
	Concurrency       int                     `json:"concurrency"`                // #t4 present-and-0 clears to auto
	QueryTimeout      string                  `json:"query_timeout"`              // #t94 per-query deadline (empty = validator default 30m)
	Watermark         *config.WatermarkFilter `json:"watermark,omitempty"`        // present-and-null clears
	IgnoreColumns     []string                `json:"ignore_columns,omitempty"`   // #ignore present-and-empty clears
	TableWatermarks   []config.TableWatermark `json:"table_watermarks,omitempty"` // #wm-override present-and-empty clears
}

// checksumChunkSizeFloorError is the shared #97 rejection message for all
// three webapi intake points (compare create / compare options PUT /
// migration task create); the floor matches the page control's :min and the
// released parameter contract, 0/absent keeps the runtime default.
func checksumChunkSizeFloorError(got int64) string {
	return fmt.Sprintf("checksum_chunk_size: 每窗口行数，不得小于 %d（当前 %d；0 或缺省 = 默认 %d）",
		config.MinChecksumChunkSize, got, config.DefaultChecksumChunkSize)
}

// normalizeWatermarkFilter validates and canonicalizes a watermark filter
// group (#t3): column passes the same identifier allow-list as incremental
// table/column names; op defaults to "<="; base_mode defaults to checksum.
// nil stays nil (filter off, zero regression). The filter is EITHER the
// single-value form (value) OR the half-open interval form
// (range_start/range_end): mixing the two, supplying only one interval
// endpoint, or supplying neither value form is rejected; an interval whose
// start does not order strictly before its end is rejected (start == end
// yields an empty half-open interval; order check only — no datetime format
// parsing, matching the front-end-only format contract).
func normalizeWatermarkFilter(wm *config.WatermarkFilter) (*config.WatermarkFilter, error) {
	if wm == nil {
		return nil, nil
	}
	out := *wm
	out.Column = strings.TrimSpace(out.Column)
	out.Value = strings.TrimSpace(out.Value)
	out.RangeStart = strings.TrimSpace(out.RangeStart)
	out.RangeEnd = strings.TrimSpace(out.RangeEnd)
	if out.Column == "" {
		return nil, fmt.Errorf("watermark: column 必须提供")
	}
	if !incIdentifierOK(out.Column) {
		return nil, fmt.Errorf("watermark.column: 非法列名 %q", out.Column)
	}
	rangeMode := out.RangeStart != "" || out.RangeEnd != ""
	singleMode := out.Value != ""
	switch {
	case rangeMode && singleMode:
		return nil, fmt.Errorf("watermark: 单值 value 与区间 range_start/range_end 互斥，不能同时提供")
	case rangeMode:
		if out.RangeStart == "" || out.RangeEnd == "" {
			return nil, fmt.Errorf("watermark: 区间模式必须同时提供 range_start 与 range_end")
		}
		if out.RangeStart >= out.RangeEnd {
			return nil, fmt.Errorf("watermark: range_start 不能大于等于 range_end（半开区间 [start,end) 恒为空）")
		}
	case !singleMode:
		return nil, fmt.Errorf("watermark: 必须提供 value 或完整的区间 range_start/range_end")
	}
	switch out.Op {
	case "", "<=", "<":
	default:
		return nil, fmt.Errorf(`watermark.op must be "<=" or "<"`)
	}
	switch out.BaseMode {
	case "", "quick", "sample", "checksum":
	default:
		return nil, fmt.Errorf(`watermark.base_mode must be one of quick/sample/checksum`)
	}
	if out.Op == "" {
		out.Op = "<="
	}
	if out.BaseMode == "" {
		out.BaseMode = "checksum"
	}
	return &out, nil
}

// normalizeTableWatermarks validates and canonicalizes the per-table
// watermark override list (#wm-override P1A): each entry's table name is
// trimmed and must be non-empty, duplicate table names are rejected (one
// override per table — last-write-wins would hide a config mistake), and
// each embedded filter group passes the SAME validation as the global group
// (identifier allow-list, single-value/interval mutual exclusion, full
// interval, op enum). The entry's BaseMode is forced to "" — an override
// NEVER carries a base algorithm (the task-level mode applies to every
// table); keeping the field would invite display/execution drift.
func normalizeTableWatermarks(tws []config.TableWatermark) ([]config.TableWatermark, error) {
	if len(tws) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(tws))
	out := make([]config.TableWatermark, 0, len(tws))
	for i := range tws {
		table := strings.TrimSpace(tws[i].Table)
		if table == "" {
			return nil, fmt.Errorf("table_watermarks[%d].table: 表名必须提供", i)
		}
		if seen[table] {
			return nil, fmt.Errorf("table_watermarks: 表 %q 重复配置例外水位", table)
		}
		seen[table] = true
		norm, werr := normalizeWatermarkFilter(&tws[i].WatermarkFilter)
		if werr != nil {
			return nil, fmt.Errorf("table_watermarks[%s]: %s", table, werr.Error())
		}
		norm.BaseMode = "" // override entries ignore the base algorithm
		out = append(out, config.TableWatermark{Table: table, WatermarkFilter: *norm})
	}
	return out, nil
}

// normalizeIgnoreColumns validates and canonicalizes the #ignore exclusion
// list: entries are trimmed, empties dropped, case-insensitive duplicates
// collapsed (first occurrence wins), every name passes the same identifier
// allow-list as watermark.column, and the list is capped at 64 entries.
// nil/empty stays nil (exclusion off, zero regression).
func normalizeIgnoreColumns(cols []string) ([]string, error) {
	if len(cols) == 0 {
		return nil, nil
	}
	const maxIgnoreColumns = 64
	out := make([]string, 0, len(cols))
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		t := strings.TrimSpace(c)
		if t == "" {
			continue
		}
		if !incIdentifierOK(t) {
			return nil, fmt.Errorf("ignore_columns: 非法列名 %q", t)
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	if len(out) > maxIgnoreColumns {
		return nil, fmt.Errorf("ignore_columns: 最多 %d 个列名", maxIgnoreColumns)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (s *Server) compareOptionsFile() string {
	return filepath.Join(s.dataDir, "compare-options.json")
}

func (s *Server) handleGetCompareOptions(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.loadCompareOptions())
}

func (s *Server) handlePutCompareOptions(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var req compareOptionsBody
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Partial-merge semantics (same as migration-options R1): fields
	// explicitly present in the body override saved values; absent fields
	// keep their saved values.
	var presence map[string]json.RawMessage
	if err := json.Unmarshal(raw, &presence); err != nil {
		presence = nil
	}
	if req.Mode != "" {
		switch req.Mode {
		case "quick", "sample", "checksum":
		default:
			s.writeError(w, http.StatusBadRequest, "mode must be one of quick/sample/checksum")
			return
		}
	}
	if req.SampleRatio < 0 || req.SampleRatio > 1 {
		s.writeError(w, http.StatusBadRequest, "sample_ratio must be within [0,1]")
		return
	}
	// #t4: unified budget — 0 means "auto" (clears the saved override);
	// anything above the hard cap is clamped (never a hard failure).
	if req.Concurrency < 0 {
		s.writeError(w, http.StatusBadRequest, "concurrency must be >= 0")
		return
	}
	if req.Concurrency > validator.MaxConcBudget {
		req.Concurrency = validator.MaxConcBudget
	}

	// Serialize load→merge→write so concurrent PUTs cannot lose updates.
	s.compare.optionsMu.Lock()
	defer s.compare.optionsMu.Unlock()

	merged := s.loadCompareOptions()
	apply := func(key string, set func()) {
		if _, ok := presence[key]; ok {
			set()
		}
	}
	apply("source_type", func() { merged.SourceType = strings.TrimSpace(req.SourceType) })
	// source/target merge field-by-field too: a body containing only
	// {"source":{"host":"x"}} must not wipe the remembered port/user/etc.
	apply("source", func() {
		var srcPresence map[string]json.RawMessage
		if raw, ok := presence["source"]; ok {
			_ = json.Unmarshal(raw, &srcPresence)
		}
		for _, f := range []struct {
			key string
			set func()
		}{
			{"type", func() { merged.Source.Type = req.Source.Type }},
			{"host", func() { merged.Source.Host = req.Source.Host }},
			{"port", func() { merged.Source.Port = req.Source.Port }},
			{"user", func() { merged.Source.User = req.Source.User }},
			{"database", func() { merged.Source.Database = req.Source.Database }},
			{"schema", func() { merged.Source.Schema = req.Source.Schema }},
			{"sslmode", func() { merged.Source.SSLMode = req.Source.SSLMode }},
		} {
			if _, ok := srcPresence[f.key]; ok {
				f.set()
			}
		}
	})
	apply("target", func() {
		var tgtPresence map[string]json.RawMessage
		if raw, ok := presence["target"]; ok {
			_ = json.Unmarshal(raw, &tgtPresence)
		}
		for _, f := range []struct {
			key string
			set func()
		}{
			{"host", func() { merged.Target.Host = req.Target.Host }},
			{"port", func() { merged.Target.Port = req.Target.Port }},
			{"user", func() { merged.Target.User = req.Target.User }},
			{"database", func() { merged.Target.Database = req.Target.Database }},
		} {
			if _, ok := tgtPresence[f.key]; ok {
				f.set()
			}
		}
	})
	apply("mode", func() { merged.Mode = req.Mode })
	apply("sample_ratio", func() { merged.SampleRatio = req.SampleRatio })
	// #97: the saved profile is exactly where a raw-API typo would live
	// long-term (options archive), so the same floor as the create path
	// applies; present-and-0 resets to the default window size (same
	// clear-by-0 semantics as concurrency).
	if _, ok := presence["checksum_chunk_size"]; ok && req.ChecksumChunkSize != 0 && req.ChecksumChunkSize < config.MinChecksumChunkSize {
		s.writeError(w, http.StatusBadRequest, checksumChunkSizeFloorError(req.ChecksumChunkSize))
		return
	}
	apply("checksum_chunk_size", func() {
		if req.ChecksumChunkSize == 0 {
			merged.ChecksumChunkSize = config.DefaultChecksumChunkSize
		} else {
			merged.ChecksumChunkSize = req.ChecksumChunkSize
		}
	})
	apply("checksum_parallel", func() { merged.ChecksumParallel = req.ChecksumParallel })
	apply("parallel", func() { merged.Parallel = req.Parallel })
	apply("concurrency", func() { merged.Concurrency = req.Concurrency })
	apply("query_timeout", func() { merged.QueryTimeout = strings.TrimSpace(req.QueryTimeout) })
	// #t3: present-and-null CLEARS the saved watermark group; present with
	// a body validates + normalizes it; absent keeps the saved value.
	if _, ok := presence["watermark"]; ok {
		norm, werr := normalizeWatermarkFilter(req.Watermark)
		if werr != nil {
			s.writeError(w, http.StatusBadRequest, werr.Error())
			return
		}
		merged.Watermark = norm
	}
	// #ignore: present validates + canonicalizes (present-and-empty clears);
	// absent keeps the saved list (partial-merge by presence).
	if _, ok := presence["ignore_columns"]; ok {
		norm, ierr := normalizeIgnoreColumns(req.IgnoreColumns)
		if ierr != nil {
			s.writeError(w, http.StatusBadRequest, ierr.Error())
			return
		}
		merged.IgnoreColumns = norm
	}
	// #wm-override: same presence-three-state merge as the exclusion list
	// (present-and-empty clears, absent keeps the saved overrides).
	if _, ok := presence["table_watermarks"]; ok {
		norm, terr := normalizeTableWatermarks(req.TableWatermarks)
		if terr != nil {
			s.writeError(w, http.StatusBadRequest, terr.Error())
			return
		}
		merged.TableWatermarks = norm
	}
	// Passwords never survive a save, regardless of what was sent.
	merged.Source.Password = ""
	merged.Target.Password = ""

	if err := s.writeCompareOptions(&merged); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("保存失败：%v", err))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// loadCompareOptions returns the persisted options or the zero value when
// nothing is saved yet or the file is corrupted (next PUT rewrites it).
func (s *Server) loadCompareOptions() compareOptionsBody {
	raw, err := os.ReadFile(s.compareOptionsFile())
	if err != nil {
		return compareOptionsBody{}
	}
	var opts compareOptionsBody
	if err := json.Unmarshal(raw, &opts); err != nil {
		zap.L().Warn("compare-options.json corrupted, ignoring saved options",
			zap.String("file", s.compareOptionsFile()), zap.Error(err))
		return compareOptionsBody{}
	}
	opts.Source.Password = ""
	opts.Target.Password = ""
	return opts
}

// writeCompareOptions persists the options atomically (temp + rename, same
// pattern as writeMigrationOptions).
func (s *Server) writeCompareOptions(opts *compareOptionsBody) error {
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	raw, err := json.MarshalIndent(opts, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dataDir, "compare-options-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.compareOptionsFile()); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (s *Server) handleCreateCompare(w http.ResponseWriter, r *http.Request) {
	var req CompareTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// F-02 datasource refs: resolve into a connection snapshot before any
	// validation/persistence (same semantics as migration tasks).
	if req.SourceRef != "" {
		e, err := s.resolveDataSourceRef(req.SourceRef)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
			return
		}
		if !srcCapable(e.Type, source.CapCompare) {
			// MS-08 absorbs this guard into the capability read (ruling seq
			// 410): e.Type is the RAW stored value (C1 shape 1 - empty is
			// rejected), postgres/mysql pass (mysql since MS-08), unknown
			// kinds resolve false. The PG-wire-only comment is obsolete -
			// the validator now speaks the MySQL wire via mysqlDialect.
			s.writeError(w, http.StatusBadRequest, "source_ref: 比对源端数据源类型必须是 postgres 或 mysql")
			return
		}
		req.Source = dataSourceToSourceConfig(e)
	}
	if req.TargetRef != "" {
		e, err := s.resolveDataSourceRef(req.TargetRef)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "target_ref: "+err.Error())
			return
		}
		if e.Type != "tidb" {
			s.writeError(w, http.StatusBadRequest, "target_ref: 目标端数据源类型必须是 tidb")
			return
		}
		req.Target = dataSourceToTargetConfig(e)
	}
	if req.Source.Host == "" || req.Target.Host == "" {
		s.writeError(w, http.StatusBadRequest, "source and target host are required")
		return
	}
	// P2-3 (inline path), MS-08 absorption: the NORMALIZED type feeds the
	// capability read (C1 shape 2 - empty defaults to postgres and passes,
	// byte-identical to the legacy `Type != "" && Type != "postgres"` guard
	// where empty was allowed through).
	if !srcCapable(req.Source.SourceType(), source.CapCompare) {
		s.writeError(w, http.StatusBadRequest, "source.type: 比对源端类型必须是 postgres 或 mysql")
		return
	}
	wm, werr := normalizeWatermarkFilter(req.Watermark)
	if werr != nil {
		s.writeError(w, http.StatusBadRequest, werr.Error())
		return
	}
	req.Watermark = wm
	twm, terr := normalizeTableWatermarks(req.TableWatermarks)
	if terr != nil {
		s.writeError(w, http.StatusBadRequest, terr.Error())
		return
	}
	// #wm-override: with an explicit table selection an override naming a
	// table OUTSIDE the selection is a config mistake — surface it at once
	// (exact string match, the same identity the engine resolves by). With
	// tables empty (= all tables) there is nothing static to check; a
	// non-existent override table simply never matches at runtime (no-op).
	if len(req.Tables) > 0 && len(twm) > 0 {
		set := make(map[string]bool, len(req.Tables))
		for _, t := range req.Tables {
			set[t] = true
		}
		for _, e := range twm {
			if !set[e.Table] {
				s.writeError(w, http.StatusBadRequest,
					fmt.Sprintf("table_watermarks: 例外水位表 %q 不在所选表清单中", e.Table))
				return
			}
		}
	}
	req.TableWatermarks = twm
	igCols, ierr := normalizeIgnoreColumns(req.IgnoreColumns)
	if ierr != nil {
		s.writeError(w, http.StatusBadRequest, ierr.Error())
		return
	}
	req.IgnoreColumns = igCols
	// A2 (#102): multi-db selection intake — names/wildcards validated and
	// canonicalized; the exclusion list only means something together with
	// a selection (a lone exclude list on a single-db task is a body
	// mistake, surfaced now instead of silently ignored).
	dbs, derr := normalizeCompareDBList(req.Databases, "databases")
	if derr != nil {
		s.writeError(w, http.StatusBadRequest, derr.Error())
		return
	}
	exclDBs, ederr := normalizeCompareDBList(req.ExcludeDatabases, "exclude_databases")
	if ederr != nil {
		s.writeError(w, http.StatusBadRequest, ederr.Error())
		return
	}
	if len(exclDBs) > 0 && len(dbs) == 0 {
		s.writeError(w, http.StatusBadRequest, "exclude_databases 仅在 databases 多库模式下有效")
		return
	}
	req.Databases = dbs
	req.ExcludeDatabases = exclDBs
	switch req.Mode {
	case "":
		// #t3: a watermark task's mode IS the filter's base algorithm.
		if wm != nil {
			req.Mode = wm.BaseMode
		} else {
			req.Mode = "sample"
		}
	case "quick", "sample", "checksum":
	default:
		s.writeError(w, http.StatusBadRequest, "mode must be one of quick/sample/checksum")
		return
	}
	if req.Name == "" {
		req.Name = "Compare " + time.Now().Format("2006-01-02 15:04:05")
	}

	// #t4: fold the deprecated parallel / checksum_parallel knobs into the
	// unified concurrency budget (an explicit concurrency wins; the result
	// is the effective value stored on the task, clamped to 1-8). Negative
	// values are rejected (same validation surface as PUT options).
	if req.Concurrency < 0 {
		s.writeError(w, http.StatusBadRequest, "concurrency must be >= 0")
		return
	}
	// F-12: the create path had no [0,1] gate (only PUT options did), so a
	// sample_ratio=100 body reached rand.Int63n with a negative span and
	// panicked the whole web process. Same message/semantics as PUT :236.
	if req.SampleRatio < 0 || req.SampleRatio > 1 {
		s.writeError(w, http.StatusBadRequest, "sample_ratio must be within [0,1]")
		return
	}
	// #97: checksum window floor — an explicit 1-999 value is the unit
	// confusion the released parameter contract calls out (批大小/字节/按库);
	// 0/absent keeps the runtime default.
	if req.ChecksumChunkSize != 0 && req.ChecksumChunkSize < config.MinChecksumChunkSize {
		s.writeError(w, http.StatusBadRequest, checksumChunkSizeFloorError(req.ChecksumChunkSize))
		return
	}
	conc := validator.ResolveConcurrency(req.Concurrency, req.Parallel, req.ChecksumParallel)

	// B2: single-running invariant — check + persist + claim must be one
	// atomic critical section, otherwise two concurrent POSTs both pass the
	// check and run simultaneously. A1: a busy slot no longer 409s — the
	// task joins a FIFO queue on disk (status=queued task.json); queue
	// admission shares this same critical section, so enqueue-vs-dispatch
	// can never race. Strict FIFO: while ANY queued task waits, a new POST
	// also queues instead of jumping the line.
	s.compare.mu.Lock()
	busy := s.compare.cancel != nil || s.earliestQueuedCompare() != nil

	now := time.Now()
	task := &CompareTask{
		ID:                   uuid.New().String()[:8],
		Name:                 req.Name,
		Source:               req.Source,
		Target:               req.Target,
		SourceRef:            req.SourceRef,
		TargetRef:            req.TargetRef,
		Mode:                 req.Mode,
		SampleRatio:          req.SampleRatio,
		ChecksumChunkSize:    req.ChecksumChunkSize,
		ChecksumParallel:     req.ChecksumParallel,
		Parallel:             req.Parallel,
		Concurrency:          conc,
		QueryTimeout:         strings.TrimSpace(req.QueryTimeout),
		Tables:               req.Tables,
		Watermark:            req.Watermark,
		TableWatermarks:      req.TableWatermarks,
		IgnoreColumns:        req.IgnoreColumns,
		PossiblyStuckTimeout: strings.TrimSpace(req.PossiblyStuckTimeout),
		Databases:            req.Databases,
		ExcludeDatabases:     req.ExcludeDatabases,
		CreatedAt:            now,
	}
	if busy {
		task.Status = CompareStatusQueued // StartedAt stays nil until dispatch
		// P2-1: strict FIFO admission order. CreatedAt alone is not enough —
		// two POSTs in the same clock tick tie, and the random uuid[:8]
		// tie-break is stable but not a real arrival order.
		task.QueueSeq = s.nextQueueSeq()
	} else {
		task.Status = CompareStatusRunning
		task.StartedAt = &now // M2: set before the goroutine spawns (no data race on the response)
	}
	if err := s.persistCompareTask(task); err != nil {
		s.compare.mu.Unlock()
		s.writeError(w, http.StatusInternalServerError, "failed to persist compare task: "+err.Error())
		return
	}

	if busy {
		// A1: passwords ride in memory only (task.json is redacted); stash
		// them for the same-process dispatch, then answer 202 + the queued
		// task (same redacted shape as the 201 response).
		if s.compare.queuedCreds == nil {
			s.compare.queuedCreds = make(map[string]compareQueuedCreds)
		}
		s.compare.queuedCreds[task.ID] = compareQueuedCreds{
			sourcePassword: task.Source.Password,
			targetPassword: task.Target.Password,
		}
		// P1-1: snapshot the 202 body HERE, under compare.mu and before any
		// goroutine can touch `task`. The self-heal dispatch below may hand
		// THIS task the slot, and runCompare then mutates the very same
		// struct (Status/TablesDone/...) — a copy taken after dispatch races
		// the runner. Slices are shared but never written by runCompare.
		resp := *task
		resp.Source.Password = ""
		resp.Target.Password = ""
		s.compare.mu.Unlock()
		// A1 self-heal: a queued record can outlive a transient dispatch
		// failure while the slot is actually free — kick a drain so the FIFO
		// head (not the task just admitted) runs first.
		s.dispatchNextCompare()
		s.writeJSON(w, http.StatusAccepted, &resp)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.compare.cancel = cancel
	s.compare.runningID = task.ID
	s.compare.mu.Unlock()

	// Passwords live only in memory for the run; persisted copies and the
	// HTTP response (M1) are redacted. Copy the response BEFORE spawning so
	// no field is shared with the goroutine at all.
	resp := *task
	resp.Source.Password = ""
	resp.Target.Password = ""

	go s.runCompare(ctx, task)

	s.writeJSON(w, http.StatusCreated, &resp)
}

// buildCompareRunParams derives the validator inputs from a compare task
// (L1: sample ratio must reach the sampling path; <=0 falls back to 0.01).
func buildCompareRunParams(task *CompareTask, reportFile string) (float64, common.ValidateOpts, config.CompareConfig) {
	sampleRatio := task.SampleRatio
	if sampleRatio <= 0 {
		sampleRatio = 0.01
	}
	// F-12 read-side clamp: tasks persisted before the create gate (or other
	// hand-built CompareTask values) must never feed the sampler an
	// out-of-range ratio — clamp to 1 and say so.
	if sampleRatio > 1 {
		zap.L().Warn("sample_ratio above 1 clamped to 1 (stale persisted task?)",
			zap.Float64("sample_ratio", sampleRatio))
		sampleRatio = 1
	}
	mode := task.Mode
	// #t3 belt-and-braces: a watermark task's mode is the filter's base
	// algorithm (create maps it; this keeps hand-built tasks honest too).
	if mode == "" && task.Watermark != nil && task.Watermark.BaseMode != "" {
		mode = task.Watermark.BaseMode
	}
	opts := common.ValidateOpts{
		Mode:        mode,
		SampleRatio: sampleRatio,
		Tables:      task.Tables,
		ReportFile:  reportFile,
	}
	compareCfg := config.CompareConfig{
		CompareMode:          mode,
		SampleRatio:          sampleRatio,
		ChecksumChunkSize:    task.ChecksumChunkSize,
		ChecksumParallel:     task.ChecksumParallel,
		Concurrency:          task.Concurrency,
		QueryTimeout:         task.QueryTimeout,
		Watermark:            task.Watermark,
		TableWatermarks:      task.TableWatermarks,
		IgnoreColumns:        task.IgnoreColumns,
		PossiblyStuckTimeout: task.PossiblyStuckTimeout,
	}
	return sampleRatio, opts, compareCfg
}

// inheritChunkedCountKnobs (BUG-CMP-COUNT-P1 A6) copies ONLY the two
// chunked-count knobs from the live config.yaml snapshot onto the compare
// task's override. The override is built field-by-field from task-level
// knobs, so the new keys would otherwise arrive as the zero value — and 0
// means DISABLED, silently turning chunked counting off on the UI path.
// config.Load's DefaultConfig prefill means a snapshot from a file
// without the keys carries 5M/1M; an explicit 0 in the file propagates as
// the runtime opt-out (including the UI). A snapshot failure (config file
// not wired on this server / read error) falls back to the explicit
// DefaultConfig values — never the zero value — with a Warn. No other
// Compare field is touched: the file's other knobs must not suddenly
// influence task-scoped compare behavior.
func inheritChunkedCountKnobs(cc *config.CompareConfig, snapshot func() (*config.Config, error), logger *zap.Logger) {
	cfg, err := snapshot()
	if err != nil {
		def := config.DefaultConfig()
		cc.ChunkedCountThreshold = def.Compare.ChunkedCountThreshold
		cc.ChunkedCountTarget = def.Compare.ChunkedCountTarget
		logger.Warn("compare task: config.yaml snapshot unavailable, chunked-count knobs fall back to defaults",
			zap.Error(err))
		return
	}
	cc.ChunkedCountThreshold = cfg.Compare.ChunkedCountThreshold
	cc.ChunkedCountTarget = cfg.Compare.ChunkedCountTarget
}

// compareAllTablesSkipped reports whether a finished report skipped EVERY
// table (watermark-active run where every table hit a residual skip path —
// no comparable columns, or all of them excluded): the run produced no
// comparison evidence at all, which must not read as "completed".
func compareAllTablesSkipped(rpt *reporter.Report) bool {
	if rpt == nil || len(rpt.Tables) == 0 {
		return false
	}
	for _, t := range rpt.Tables {
		if t.Status != reporter.StatusSkip {
			return false
		}
	}
	return true
}

// compareHeartbeatTick is the possibly-stuck scan cadence (#t95 P1). A
// package var so unit tests can shrink it (10ms) without touching the
// default 2s production cadence.
var compareHeartbeatTick = 2 * time.Second

// compareHeartbeat flags a running standalone compare task as possibly_stuck
// when NO progress event lands within the threshold (#t95 P1): a slow-but-
// moving run stays green, a stalled one is called out so the UI can tell
// 「慢」from「卡」. Any subsequent per-table/per-chunk progress event clears
// the flag. Stops on ctx cancel or once the task leaves the running state.
func (s *Server) compareHeartbeat(ctx context.Context, task *CompareTask, taskMu *sync.Mutex, threshold time.Duration) {
	ticker := time.NewTicker(compareHeartbeatTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			taskMu.Lock()
			if task.Status != CompareStatusRunning {
				taskMu.Unlock()
				return
			}
			status := task.Status
			stuck := time.Since(task.LastProgressAt) > threshold
			if stuck && !task.PossiblyStuck {
				task.PossiblyStuck = true
				if err := s.persistCompareTask(task); err != nil {
					zap.L().Warn("failed to persist possibly-stuck flag", zap.Error(err))
				}
				// #t95 B1 R2: carry the chunk progress as evidence so a
				// PossiblyStuck=true flag is never read in isolation — the
				// UI can show which chunk the task stalled on. Values are
				// captured under the lock (no race with progress writers).
				chunk, total := task.CurrentChunk, task.TotalChunks
				curTable := task.CurrentTable
				taskMu.Unlock()
				zap.L().Warn("compare task possibly stuck (no progress within threshold)",
					zap.String("compare_id", task.ID),
					zap.Duration("threshold", threshold))
				s.BroadcastProgress(task.ID, map[string]interface{}{
					"type": "compare", "status": status, "possibly_stuck": true,
					"current_table": curTable, "current_chunk": chunk, "total_chunks": total,
				})
			} else {
				taskMu.Unlock()
			}
		}
	}
}

// tableDoneProgress builds the OnTableDone WS broadcast payload
// (FEAT-CMP-TBL-DURATION): the per-table-done event now also carries the
// finished table's status + duration so the running panel can light each
// table's 耗时 the moment it completes. The persisted CompareTask schema is
// deliberately untouched — WS push only; report.json stays the persistent
// truth and a page refresh resetting the live list is accepted (D2).
func tableDoneProgress(task *CompareTask, done, total int, tr reporter.TableReport) map[string]interface{} {
	return map[string]interface{}{
		"type": "compare", "status": task.Status,
		"tables_done": done, "tables_total": total, "current_table": tr.TableName,
		"current_chunk": task.CurrentChunk, "total_chunks": task.TotalChunks,
		"possibly_stuck": task.PossiblyStuck,
		"table_status":   string(tr.Status), "table_duration": tr.Duration,
	}
}

func (s *Server) runCompare(ctx context.Context, task *CompareTask) {
	logger := zap.L()
	logger.Info("starting standalone comparison",
		zap.String("compare_id", task.ID), zap.String("mode", task.Mode))

	var taskMu sync.Mutex // serializes progress updates to task/persist/WS

	// #t95 P1: seed the heartbeat baseline before any work unit can land a
	// progress event; every per-table/per-chunk callback refreshes it.
	taskMu.Lock()
	task.LastProgressAt = time.Now()
	task.PossiblyStuck = false
	task.CurrentChunk = 0
	task.TotalChunks = 0
	taskMu.Unlock()

	finish := func(status, errMsg string) {
		taskMu.Lock()
		defer taskMu.Unlock()
		task.Status = status
		task.Error = errMsg
		task.CurrentTable = ""
		fin := time.Now()
		task.FinishedAt = &fin
		if err := s.persistCompareTask(task); err != nil {
			logger.Warn("failed to persist compare task", zap.Error(err))
		}
		// B2: only the task that owns the running slot may clear it.
		s.compare.mu.Lock()
		if s.compare.runningID == task.ID {
			s.compare.cancel = nil
			s.compare.runningID = ""
		}
		s.compare.mu.Unlock()
		// A1: the slot just freed — hand it to the FIFO-head queued task
		// (no-op when the queue is empty or someone else claimed first).
		s.dispatchNextCompare()
		prog := map[string]interface{}{
			"type": "compare", "status": task.Status,
			"tables_done": task.TablesDone, "tables_total": task.TablesTotal,
		}
		// A2 (#102): the terminal broadcast of a multi-db task carries the
		// database counters too (the FE's final list refresh keys off them).
		if task.DatabasesTotal > 0 {
			prog["databases_done"] = task.DatabasesDone
			prog["databases_total"] = task.DatabasesTotal
		}
		s.BroadcastProgress(task.ID, prog)
	}

	// #t95 P1: possibly-stuck heartbeat — flags the task when NO progress
	// event lands within the threshold (slow-but-moving stays green). Task-
	// scoped since A2: the multi-db loop runs per-database compare configs,
	// and the threshold knob is task-level either way.
	hbThreshold := (config.CompareConfig{PossiblyStuckTimeout: task.PossiblyStuckTimeout}).PossiblyStuckTimeoutDuration()
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go s.compareHeartbeat(heartbeatCtx, task, &taskMu, hbThreshold)

	// A2 (#102): multi-database tasks branch into the dedicated loop —
	// resolution, per-database sequential runs, cross-db summary; the
	// legacy single-database flow below stays byte-identical.
	if len(task.Databases) > 0 {
		s.runCompareMultiDB(ctx, task, &taskMu, finish, logger)
		return
	}

	report, err := s.executeCompareRun(ctx, task, &taskMu, task.Source, task.Target,
		filepath.Join(s.compareDir(task.ID), "report.json"),
		filepath.Join(s.compareDir(task.ID), "diff"),
		filepath.Join(s.dataDir, "spool", task.ID))
	// B1: check ctx FIRST regardless of err — Run swallows per-table ctx
	// cancellations into failed table reports and returns (rpt, nil), so a
	// mid-run cancel must not be reported as "completed".
	if ctx.Err() != nil {
		finish(CompareStatusCancelled, "cancelled")
		return
	}
	if err != nil {
		finish(CompareStatusFailed, err.Error())
		return
	}

	// Watermark active (global group OR per-table overrides, P3A) + every
	// table skipped ⇒ no evidence at all — fail the task explicitly instead
	// of a misleading "completed".
	if (task.Watermark != nil || len(task.TableWatermarks) > 0) && compareAllTablesSkipped(report) {
		finish(CompareStatusFailed, "所有表均被跳过（无可比列或列全被排除），未产生比对结果")
		return
	}

	// The report was saved by Run via ReportFile; announce completion.
	logger.Info("standalone comparison finished",
		zap.String("compare_id", task.ID),
		zap.String("overall", string(report.Status)))
	finish(CompareStatusCompleted, "")
}

func (s *Server) handleListCompares(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(s.dataDir, "compare")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeJSON(w, http.StatusOK, []*CompareTask{})
			return
		}
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tasks := []*CompareTask{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.loadCompareTask(e.Name())
		if err != nil || t == nil {
			continue
		}
		// MC-17 ⑥b: the list face carries no usernames — the FE renders
		// host:port/database only (verified: no consumer of source.user/
		// target.user on this route). The detail/report faces are unchanged.
		t.Source.User = ""
		t.Target.User = ""
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	s.writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) loadCompareTask(id string) (*CompareTask, error) {
	data, err := os.ReadFile(filepath.Join(s.compareDir(id), "task.json"))
	if err != nil {
		return nil, err
	}
	var t CompareTask
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Server) handleGetCompare(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	t, err := s.loadCompareTask(id)
	if err != nil || t == nil {
		s.writeError(w, http.StatusNotFound, "compare task not found")
		return
	}
	s.writeJSON(w, http.StatusOK, t)
}

// handleCompareReport serves the standalone compare report in the two
// family faces (MC-16): format=json serves the verbatim report.json bytes
// (byte-identical to the engine artifact, attachment compare-report-{id}
// .json); html (the default) renders the family report (attachment
// compare-report-{id}.html). A missing task.json is a 404 "task not found";
// a missing report.json is a 404 that names the causes — never a half-empty
// report; a report.json that exists but will not decode is a 500 with a
// generic body (no server-side paths — the raw *fs.PathError carries the
// absolute path; the original error goes to the server log). MC-14/15
// policy, applied to the standalone compare namespace.
//
// A2 multi-db (#102) branches first: without ?db= the response is the
// cross-database summary (format=json → verbatim summary.json bytes,
// format=csv → verbatim summary.csv, html → the family summary face);
// ?db=<name> (a name from the task's resolved list) serves that
// database's per-db report in the same json/html family faces. Legacy
// single-database tasks keep the exact pre-A2 contract (format=csv on a
// single-db task is a 404 naming the mismatch — there is no summary).
func (s *Server) handleCompareReport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	task, err := s.loadCompareTask(id)
	if err != nil || task == nil {
		s.writeError(w, http.StatusNotFound, "compare task not found")
		return
	}
	format := r.URL.Query().Get("format")
	db := r.URL.Query().Get("db")
	if len(task.Databases) > 0 {
		s.handleCompareMultiReport(w, task, format, db)
		return
	}
	if format == "csv" {
		s.writeError(w, http.StatusNotFound, "compare report not found — format=csv 仅多库任务提供（汇总 CSV）；单库任务请用 format=json/html")
		return
	}
	if db != "" {
		s.writeError(w, http.StatusNotFound, "compare report not found — ?db= 仅多库任务提供")
		return
	}
	reportPath := filepath.Join(s.compareDir(id), "report.json")
	if format == "json" {
		b, err := os.ReadFile(reportPath)
		if err != nil {
			s.writeCompareReportError(w, id, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-report-%s.json", id))
		w.Write(b)
		return
	}
	b, err := os.ReadFile(reportPath)
	if err != nil {
		s.writeCompareReportError(w, id, err)
		return
	}
	var rpt reporter.Report
	if err := json.Unmarshal(b, &rpt); err != nil {
		zap.L().Warn("decode compare report.json failed", zap.String("task", id), zap.Error(err))
		s.writeError(w, http.StatusInternalServerError, "read compare report failed — report.json exists but is unreadable (corrupt artifact); cause logged server-side")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-report-%s.html", id))
	w.Write([]byte(renderDataCompareReportHTML(task, &rpt)))
}

// compareDBSlugFor maps a database name from the task's resolved list onto
// its per-db subdirectory slug (the run ordinal the loop used). Unknown
// names (not in ResolvedDatabases) resolve false — the caller 404s.
func compareDBSlugFor(task *CompareTask, db string) (string, bool) {
	for i, name := range task.ResolvedDatabases {
		if name == db {
			return compareDBSlug(i, db), true
		}
	}
	return "", false
}

// handleCompareMultiReport serves the A2 faces: the cross-database summary
// (no db param) or one database's report (?db=<name>). Read errors follow
// the writeCompareReportError family policy (missing → 404 with the cause
// named, decode failure → 500 generic, original to the log).
func (s *Server) handleCompareMultiReport(w http.ResponseWriter, task *CompareTask, format, db string) {
	if db == "" {
		switch format {
		case "csv":
			b, err := os.ReadFile(filepath.Join(s.compareDir(task.ID), "summary.csv"))
			if err != nil {
				s.writeCompareReportError(w, task.ID, err)
				return
			}
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-summary-%s.csv", task.ID))
			w.Write(b)
			return
		case "json":
			b, err := os.ReadFile(filepath.Join(s.compareDir(task.ID), "summary.json"))
			if err != nil {
				s.writeCompareReportError(w, task.ID, err)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-summary-%s.json", task.ID))
			w.Write(b)
			return
		}
		// html (the default family face): decode summary.json and render.
		b, err := os.ReadFile(filepath.Join(s.compareDir(task.ID), "summary.json"))
		if err != nil {
			s.writeCompareReportError(w, task.ID, err)
			return
		}
		var sum CompareSummary
		if err := json.Unmarshal(b, &sum); err != nil {
			zap.L().Warn("decode compare summary.json failed", zap.String("task", task.ID), zap.Error(err))
			s.writeError(w, http.StatusInternalServerError, "read compare summary failed — summary.json exists but is unreadable (corrupt artifact); cause logged server-side")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-summary-%s.html", task.ID))
		w.Write([]byte(renderCompareSummaryHTML(task, &sum)))
		return
	}
	// Per-database report face. The name must be one the run resolved —
	// unknown names 404 before any filesystem touch (the {compareID}
	// allow-list twin policy, applied to the db selector).
	slug, ok := compareDBSlugFor(task, db)
	if !ok {
		s.writeError(w, http.StatusNotFound, "compare report not found — ?db= 必须是该任务解析出的库名之一")
		return
	}
	reportPath := filepath.Join(s.compareDir(task.ID), "db", slug, "report.json")
	if format == "json" {
		b, err := os.ReadFile(reportPath)
		if err != nil {
			s.writeCompareReportError(w, task.ID, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-report-%s-%s.json", task.ID, db))
		w.Write(b)
		return
	}
	b, err := os.ReadFile(reportPath)
	if err != nil {
		s.writeCompareReportError(w, task.ID, err)
		return
	}
	var rpt reporter.Report
	if err := json.Unmarshal(b, &rpt); err != nil {
		zap.L().Warn("decode per-db report.json failed", zap.String("task", task.ID), zap.String("db", db), zap.Error(err))
		s.writeError(w, http.StatusInternalServerError, "read compare report failed — report.json exists but is unreadable (corrupt artifact); cause logged server-side")
		return
	}
	// The family renderer reads task.Source/Target.Database for the meta
	// grid — a shallow copy with this database's names shows which pair
	// the report covers (the task-level fields stay the multi-db ones).
	dbTask := *task
	dbTask.Source.Database = db
	dbTask.Target.Database = db
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=compare-report-%s-%s.html", task.ID, db))
	w.Write([]byte(renderDataCompareReportHTML(&dbTask, &rpt)))
}

// writeCompareReportError maps a missing report.json → 404 with the cause
// set named (unfinished / failed before the compare completed / cleared)
// and every other read failure → 500 with a generic body (the original
// error goes to the server-side log instead — MC-14/15 policy).
func (s *Server) writeCompareReportError(w http.ResponseWriter, taskID string, err error) {
	if os.IsNotExist(err) {
		s.writeError(w, http.StatusNotFound, "compare report not found — the task has not produced report.json (unfinished, failed before the compare completed, or the artifact was cleared)")
		return
	}
	zap.L().Warn("read compare report failed", zap.String("task", taskID), zap.Error(err))
	s.writeError(w, http.StatusInternalServerError, "read compare report failed — report.json exists but is unreadable; cause logged server-side")
}

func (s *Server) handleCancelCompare(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	// P0-1: ONE Lock for the whole decision; EVERY return path Unlocks (the
	// pre-A1 code unlocked right after the snapshot, A1 moved it into the
	// branch and dropped it on the running path — a permanent compare.mu
	// leak that deadlocks the whole compare domain).
	s.compare.mu.Lock()
	cancel := s.compare.cancel
	running := s.compare.runningID
	if cancel == nil || running != id {
		// A1: cancelling a QUEUED task removes it from the queue at once —
		// same critical section as dispatch, so a task is either cancelled
		// or dispatched, never both.
		if t, err := s.loadCompareTask(id); err == nil && t != nil && t.Status == CompareStatusQueued {
			t.Status = CompareStatusCancelled
			t.Error = "cancelled" // same on-disk shape a running cancel leaves via finish()
			fin := time.Now()
			t.FinishedAt = &fin
			if err := s.persistCompareTask(t); err != nil {
				s.compare.mu.Unlock()
				s.writeError(w, http.StatusInternalServerError, "failed to persist compare task: "+err.Error())
				return
			}
			delete(s.compare.queuedCreds, id) // dequeue is on disk; drop the in-memory credentials
			s.compare.mu.Unlock()
			// A1 self-heal: with this task gone the slot may be idle while
			// another queued task still waits (transient dispatch failure) —
			// kick the FIFO drain.
			s.dispatchNextCompare()
			s.BroadcastProgress(id, map[string]interface{}{
				"type": "compare", "status": CompareStatusCancelled,
			})
			s.writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled", "task_id": id})
			return
		}
		s.compare.mu.Unlock()
		s.writeError(w, http.StatusConflict, "compare task is not running")
		return
	}
	// P0-1b: cancel() is a non-blocking context.CancelFunc — call it WHILE
	// still holding compare.mu, so the cancel targets exactly the task that
	// owns the slot right now (no TOCTOU window in which the run finishes,
	// dispatch hands the slot to a queued task, and we cancel that one
	// instead). The mutex is released before any HTTP write.
	cancel()
	s.compare.mu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "cancelling", "task_id": id})
}

func (s *Server) handleDeleteCompare(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	// A1: delete races dispatch — both read/move the same queued task.json.
	// The DECISION stays inside compare.mu, but the heavy os.RemoveAll does
	// not: a queued record is first flipped to cancelled on disk (so no
	// dispatch can pick it up), then the lock is dropped and the dir removed.
	// P2-3: never hold compare.mu across filesystem I/O.
	dir := s.compareDir(id)
	s.compare.mu.Lock()
	if s.compare.runningID == id {
		s.compare.mu.Unlock()
		s.writeError(w, http.StatusConflict, "cannot delete a running compare task; cancel it first")
		return
	}
	delete(s.compare.queuedCreds, id) // queued rows delete fine; only running blocks
	if t, err := s.loadCompareTask(id); err == nil && t != nil && t.Status == CompareStatusQueued {
		// Detach it from the queue before releasing the lock: dispatch only
		// takes status=queued, so this record can no longer be claimed while
		// the dir is being removed.
		t.Status = CompareStatusCancelled
		t.Error = "deleted"
		fin := time.Now()
		t.FinishedAt = &fin
		if perr := s.persistCompareTask(t); perr != nil {
			s.compare.mu.Unlock()
			s.writeError(w, http.StatusInternalServerError, "failed to detach queued compare task: "+perr.Error())
			return
		}
	}
	s.compare.mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A1 self-heal: deleting a queued head can leave the slot idle with others
	// waiting — kick the FIFO drain (no-op when a task is running).
	s.dispatchNextCompare()
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
