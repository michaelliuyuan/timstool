package webapi

// F-04 增量补齐 (timestamp-watermark incremental sync): pull-based,
// table-granular jobs that copy rows whose watermark column advanced past the
// per-table high-water mark. Completely independent of the log-based CDC
// pipeline; manual trigger only (v1); PostgreSQL sources only (v1); plain
// streamed SQL writes (no Lightning). Connection credentials never leave the
// server: jobs reference datasources by id and resolve F-02 snapshots.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"go.uber.org/zap"
)

// incMu serializes all read→modify→write cycles on incremental_jobs.json.
var incMu sync.Mutex

// incTableConfig is the per-table user configuration.
type incTableConfig struct {
	Table            string `json:"table"`
	WatermarkColumn  string `json:"watermark_column"`
	InitialWatermark string `json:"initial_watermark,omitempty"` // "" = MIN(col) → full backfill
}

// incTableState is the per-table runtime progress. Editing a job's config
// never touches state; state resets only via the API's explicit reset flag.
type incTableState struct {
	LastWatermark string     `json:"last_watermark,omitempty"`
	LastSyncAt    *time.Time `json:"last_sync_at,omitempty"`
	TotalRows     int64      `json:"total_rows"`
	Failed        string     `json:"failed,omitempty"` // last per-table error
}

type incJob struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	SourceRef        string                    `json:"source_ref"`
	TargetRef        string                    `json:"target_ref"`
	BatchSize        int                       `json:"batch_size"`
	Parallelism      int                       `json:"parallelism,omitempty"` // tables synced concurrently (normalized 1-16, default 4)
	StrictMode       bool                      `json:"strict_mode"`           // ">" instead of ">=" (may lose same-second late rows)
	ConflictStrategy string                    `json:"conflict_strategy"`     // replace | ignore | error
	Tables           []incTableConfig          `json:"tables"`
	States           map[string]*incTableState `json:"states"`
	History          []incRunRecord            `json:"history,omitempty"` // newest first, capped
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

type incTableResult struct {
	Table  string `json:"table"`
	FromWM string `json:"from_watermark"`
	ToWM   string `json:"to_watermark"`
	Rows   int64  `json:"rows"`
	Error  string `json:"error,omitempty"`
}

type incRunRecord struct {
	RunID      string           `json:"run_id"`
	StartedAt  time.Time        `json:"started_at"`
	DurationMs int64            `json:"duration_ms"`
	Tables     []incTableResult `json:"tables"`
	Status     string           `json:"status,omitempty"` // running|completed|failed (absent on legacy records = completed)
	Error      string           `json:"error,omitempty"`  // run-level error (e.g. interrupted by restart)
	// Log summary only (FEAT-INC-LOGS): the events themselves live in
	// dataDir/incremental_logs/<jobID>/<runID>.json so GET /jobs stays light.
	LogEvents  int64 `json:"log_events,omitempty"`
	LogDropped int64 `json:"log_dropped,omitempty"`
}

const incHistoryCap = 20

// incRunStatus values for incRunRecord.Status.
const (
	incRunStatusRunning   = "running"
	incRunStatusCompleted = "completed"
	incRunStatusFailed    = "failed"
)

// Table-sync parallelism bounds. Zero/absent values (legacy jobs, or clients
// that omit the field) normalize to the default; explicit out-of-range values
// are rejected with 400.
const (
	incParallelismDefault = 4
	incParallelismMax     = 16
)

// normalizeIncParallelism maps a raw parallelism value onto the valid range:
// 0/absent -> default (D2: legacy jobs load as the default, not serial).
func normalizeIncParallelism(v int) int {
	if v <= 0 {
		return incParallelismDefault
	}
	if v > incParallelismMax {
		return incParallelismMax
	}
	return v
}

// incRunning tracks jobs with a background run in flight (guarded by incMu).
// A running job rejects concurrent runs, edits and deletes (409): a PUT would
// swap the States map the engine is writing concurrently.
var incRunning = map[string]bool{}

// incRunCtxHook is a test seam: when set, every background run's context is
// passed through it (used to anchor that the run ctx carries no deadline).
var incRunCtxHook func(context.Context)

// incIdentifierRe is the server-side allow-list for table/column names: a
// conservative ASCII identifier (quoted anyway, but never accept anything the
// pattern rejects — belt and braces against SQL injection).
var incIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func incIdentifierOK(s string) bool { return incIdentifierRe.MatchString(s) }

// incQuoteMySQL quotes a MySQL/TiDB identifier.
func incQuoteMySQL(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// Source-side PG quoting/rendering/column-catalog/eligibility moved to
// WatermarkDialect (wm_dialect.go, MS-04 commit 2/3); all consumers dial the
// package-level incSourceDialect seam. incQuoteMySQL stays here: target-side
// write path, MS-09 seam (ruling seq 269).

// incCursorStep decides the keyset cursor after one fetched batch.
// saturated=true means the batch was full AND its max watermark equals the
// watermark we entered the batch with: a plain keyset retry would refetch the
// exact same batch forever (same-value livelock, e.g. same-second bulk
// INSERTs), so the caller must drain that value and jump past it.
func incCursorStep(entryWM, lastWM string, batchLen, batchSize int) (nextWM string, saturated, done bool) {
	if batchLen == 0 {
		return entryWM, false, true
	}
	if batchLen == batchSize && lastWM == entryWM {
		return lastWM, true, false
	}
	if batchLen < batchSize {
		return lastWM, false, true
	}
	return lastWM, false, false
}

// incJumpAfterDrain maps the MIN(watermark) > wm probe result to the next
// cursor: done=true when no value remains above the drained watermark.
func incJumpAfterDrain(nextMin sql.NullString) (string, bool) {
	if !nextMin.Valid || nextMin.String == "" {
		return "", true
	}
	return nextMin.String, false
}

// incBuildInsertSQL renders the batched target write with the conflict
// strategy prefix. nRows rows, len(colNames) placeholders each.
func incBuildInsertSQL(db, table string, colNames []string, nRows int, strategy string) string {
	prefix := "INSERT INTO"
	switch strategy {
	case "replace":
		prefix = "REPLACE INTO"
	case "ignore":
		prefix = "INSERT IGNORE INTO"
	}
	quoted := make([]string, len(colNames))
	for i, c := range colNames {
		quoted[i] = incQuoteMySQL(c)
	}
	oneRow := "(" + strings.Repeat("?, ", len(colNames))
	oneRow = oneRow[:len(oneRow)-2] + ")"
	return fmt.Sprintf("%s %s.%s (%s) VALUES %s",
		prefix, incQuoteMySQL(db), incQuoteMySQL(table), strings.Join(quoted, ", "),
		strings.Repeat(oneRow+", ", nRows-1)+oneRow)
}

// incTargetDSN renders the incremental write DSN with every pooled
// connection's session pinned to UTC (P-INC-TZ fix, MS-03 segment 2):
// go-sql-driver applies unknown DSN params as `SET <var>=<value>` on each
// new connection, so `time_zone='+00:00'` aligns the write session with the
// validator's read session (getTiDBConn -> TargetDialect.SessionInit SET
// time_zone='+00:00') and with the migration write path. Before this pin
// the write session inherited TiDB's system_time_zone (e.g. Asia/Shanghai):
// TIMESTAMP wall strings / time.Time binds (driver loc default UTC) were
// reinterpreted at the session offset, storing instants shifted by the
// offset and making post-incremental checksum compares read back −8h.
// TargetConfig.DSN() itself is intentionally untouched: its consumers
// (test-connection, CDC, validator pools) keep their own session setups.
func incTargetDSN(tc config.TargetConfig) string {
	return config.BuildMySQLDSN(tc.Host, tc.Port, tc.User, tc.Password, tc.Database,
		map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}, nil)
}

// incAdvanceWatermark implements the state rule: rows==0 keeps the current
// watermark (no speculative advance); otherwise the stream's MAX (= last row
// of the ORDER BY scan) becomes the new watermark.
func incAdvanceWatermark(current, streamMax string, rows int64) string {
	if rows == 0 {
		return current
	}
	return streamMax
}

// incMaxPlaceholders caps the bind-parameter count of ONE multi-row INSERT.
// MySQL/TiDB reject statements above 65535 placeholders (PG error 1390 on the
// MySQL wire, "Prepared statement contains too many placeholders"), so wide
// tables × large batches must be written in shards below that ceiling.
const incMaxPlaceholders = 65000

// incShardRows returns the max rows a single INSERT may carry for cols
// columns (rows*cols must stay within incMaxPlaceholders). cols<=0 or
// rows<=0 return rows unchanged — defensive: never shrink a batch when the
// column count is unknown.
func incShardRows(cols, rows int) int {
	if cols <= 0 || rows <= 0 {
		return rows
	}
	n := incMaxPlaceholders / cols
	if n < 1 {
		n = 1
	}
	if rows > n {
		return n
	}
	return rows
}

// incExecShardedInsert writes batch rows through incBuildInsertSQL in shards:
// one multi-row INSERT per shard, never exceeding incMaxPlaceholders bind
// params. Returns the rows written; a failing shard aborts with its error and
// already-written shards are kept (same "written batches are kept" semantics
// as the pre-sharding single-INSERT path). Used by BOTH write points: the
// main batch path and the same-value drain flush.
func incExecShardedInsert(ctx context.Context, db *sql.DB, database, table string, cols []string, batch [][]any, strategy string) (int64, error) {
	shard := incShardRows(len(cols), len(batch))
	var written int64
	for start := 0; start < len(batch); start += shard {
		end := start + shard
		if end > len(batch) {
			end = len(batch)
		}
		part := batch[start:end]
		args := make([]any, 0, len(part)*len(cols))
		for _, row := range part {
			args = append(args, row...)
		}
		insSQL := incBuildInsertSQL(database, table, cols, len(part), strategy)
		if _, err := db.ExecContext(ctx, insSQL, args...); err != nil {
			// Non-idempotent (error) strategy + partial writes: unlike the
			// pre-sharding single atomic INSERT, earlier shards are already
			// committed while the watermark did not advance — a plain rerun
			// would hit duplicate keys. Attach an actionable hint; the
			// idempotent strategies (replace/ignore) rerun cleanly and keep
			// the original error format.
			if written > 0 && strategy != "replace" && strategy != "ignore" {
				return written, fmt.Errorf("%w；已先行写入 %d 行且水位未推进，error 冲突策略直接重跑会撞主键，请清理目标端已写行或改用 replace/ignore 后重跑", err, written)
			}
			return written, err
		}
		written += int64(len(part))
	}
	return written, nil
}

func (s *Server) incrementalJobsFile() string { return s.dataDir + "/incremental_jobs.json" }

// cleanupIncrementalTempFiles removes crash-orphaned temp files at startup
// (they hold connection-watermark state; same hygiene as datasources).
func (s *Server) cleanupIncrementalTempFiles() {
	matches, err := filepath.Glob(s.dataDir + "/incremental-*.tmp")
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			zap.L().Warn("failed to remove orphaned incremental temp file", zap.String("file", m), zap.Error(err))
		}
	}
}

// markInterruptedIncrementalRuns rewrites status=running history stubs left
// behind by a crash/restart as failed, so the frontend never polls forever.
// Data-plane semantics are unchanged: already-written batches stay, watermarks
// do not advance, and REPLACE/IGNORE reruns are idempotent.
func (s *Server) markInterruptedIncrementalRuns() {
	incMu.Lock()
	defer incMu.Unlock()
	list := s.loadIncrementalJobs()
	dirty := false
	for i := range list {
		for j := range list[i].History {
			if list[i].History[j].Status == incRunStatusRunning {
				list[i].History[j].Status = incRunStatusFailed
				list[i].History[j].Error = "服务重启，运行中断"
				dirty = true
				// Pre-restart log events are unrecoverable: write the
				// single-event archive so the log endpoint still resolves.
				s.archiveInterruptedRun(list[i].ID, list[i].History[j].RunID)
			}
		}
	}
	if !dirty {
		return
	}
	if err := s.saveIncrementalJobs(list); err != nil {
		zap.L().Warn("failed to mark interrupted incremental runs", zap.Error(err))
	}
}

func (s *Server) loadIncrementalJobs() []incJob {
	raw, err := os.ReadFile(s.incrementalJobsFile())
	if err != nil {
		return nil
	}
	var list []incJob
	if err := json.Unmarshal(raw, &list); err != nil {
		zap.L().Warn("incremental_jobs.json corrupted, ignoring saved jobs",
			zap.String("file", s.incrementalJobsFile()), zap.Error(err))
		return nil
	}
	// Normalize a null/absent states map to an empty non-nil map: older or
	// hand-edited files may carry "states": null, and a nil map would panic
	// the background run engine on its first States write (syncOneTable).
	// Legacy jobs without a parallelism field normalize to the default (D2).
	for i := range list {
		if list[i].States == nil {
			list[i].States = map[string]*incTableState{}
		}
		list[i].Parallelism = normalizeIncParallelism(list[i].Parallelism)
	}
	return list
}

func (s *Server) saveIncrementalJobs(list []incJob) error {
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dataDir, "incremental-*.tmp")
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
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.incrementalJobsFile())
}

// --- validation ---

func validateIncJobBody(name string, sourceRef, targetRef string, tables []incTableConfig, strategy string, batchSize, parallelism int) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required")
	}
	if sourceRef == "" || targetRef == "" {
		return fmt.Errorf("source_ref and target_ref are required")
	}
	switch strategy {
	case "replace", "ignore", "error":
	case "":
		return fmt.Errorf("conflict_strategy is required")
	default:
		return fmt.Errorf("conflict_strategy must be one of replace/ignore/error")
	}
	if len(tables) == 0 {
		return fmt.Errorf("at least one table is required")
	}
	if batchSize < 1 || batchSize > 100000 {
		return fmt.Errorf("batch_size must be in 1-100000")
	}
	// 0/absent means "use the server default"; any other out-of-range value
	// is an explicit client mistake and gets a 400.
	if parallelism < 0 || parallelism > incParallelismMax {
		return fmt.Errorf("parallelism must be in 1-%d", incParallelismMax)
	}
	seen := map[string]bool{}
	for _, t := range tables {
		if !incIdentifierOK(t.Table) {
			return fmt.Errorf("invalid table name %q", t.Table)
		}
		if !incIdentifierOK(t.WatermarkColumn) {
			return fmt.Errorf("invalid watermark column %q", t.WatermarkColumn)
		}
		// #t1: case-insensitive dedup — PG unquoted identifiers fold to
		// lowercase, so ord_hdr and ORD_HDR are the same table. Quoted
		// case-sensitive twins are out of scope by this early check
		// (ruling tradeoff, see commit message).
		if seen[strings.ToLower(t.Table)] {
			return fmt.Errorf("duplicate table %q", t.Table)
		}
		seen[strings.ToLower(t.Table)] = true
	}
	return nil
}

// --- handlers ---

func (s *Server) handleListIncrementalJobs(w http.ResponseWriter, r *http.Request) {
	incMu.Lock()
	list := s.loadIncrementalJobs()
	incMu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateIncrementalJob(w http.ResponseWriter, r *http.Request) {
	var job incJob
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	job.Name = strings.TrimSpace(job.Name)
	if err := validateIncJobBody(job.Name, job.SourceRef, job.TargetRef, job.Tables, job.ConflictStrategy, job.BatchSize, job.Parallelism); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job.Parallelism = normalizeIncParallelism(job.Parallelism)
	// D4: PostgreSQL sources only in v1; target must be tidb (applier SQL is MySQL-flavoured).
	src, err := s.resolveDataSourceRef(job.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if !incSourceWatermarkCapable(src.Type) {
		// MS-04 absorbs this guard into the WatermarkDialect capability read
		// (ruling seq 82: baseline-frozen, only-decrease).
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	tgt, err := s.resolveDataSourceRef(job.TargetRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "target_ref: "+err.Error())
		return
	}
	if tgt.Type != "tidb" {
		s.writeError(w, http.StatusBadRequest, "目标数据源必须是 TiDB")
		return
	}

	now := time.Now()
	job.ID = uuid.New().String()[:8]
	job.States = map[string]*incTableState{}
	job.History = nil
	job.CreatedAt, job.UpdatedAt = now, now

	incMu.Lock()
	list := s.loadIncrementalJobs()
	for _, e := range list {
		if e.Name == job.Name {
			incMu.Unlock()
			s.writeError(w, http.StatusConflict, fmt.Sprintf("任务名称 %q 已存在", job.Name))
			return
		}
	}
	for {
		dup := false
		for _, e := range list {
			if e.ID == job.ID {
				dup = true
			}
		}
		if !dup {
			break
		}
		job.ID = uuid.New().String()[:8]
	}
	list = append(list, job)
	if err := s.saveIncrementalJobs(list); err != nil {
		incMu.Unlock()
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	incMu.Unlock()
	s.writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleUpdateIncrementalJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var job incJob
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	job.Name = strings.TrimSpace(job.Name)
	if err := validateIncJobBody(job.Name, job.SourceRef, job.TargetRef, job.Tables, job.ConflictStrategy, job.BatchSize, job.Parallelism); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job.Parallelism = normalizeIncParallelism(job.Parallelism)
	// D4 double-gate on update too (F-02 dual-path gate parity): a PUT must
	// not be able to swap refs past the create-side type checks.
	src, err := s.resolveDataSourceRef(job.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if !incSourceWatermarkCapable(src.Type) {
		// MS-04 absorbs this guard into the WatermarkDialect capability read
		// (ruling seq 82: baseline-frozen, only-decrease).
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	tgt, err := s.resolveDataSourceRef(job.TargetRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "target_ref: "+err.Error())
		return
	}
	if tgt.Type != "tidb" {
		s.writeError(w, http.StatusBadRequest, "目标数据源必须是 TiDB")
		return
	}

	incMu.Lock()
	defer incMu.Unlock()
	if incRunning[id] {
		s.writeError(w, http.StatusConflict, "该任务已有同步在进行中")
		return
	}
	list := s.loadIncrementalJobs()
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.writeError(w, http.StatusNotFound, "job not found")
		return
	}
	e := &list[idx]
	for i := range list {
		if i != idx && list[i].Name == job.Name {
			s.writeError(w, http.StatusConflict, fmt.Sprintf("任务名称 %q 已存在", job.Name))
			return
		}
	}
	// Config edits keep per-table state: state survives for tables whose name
	// is unchanged, resets for removed/new ones.
	oldStates := e.States
	if oldStates == nil {
		oldStates = map[string]*incTableState{}
	}
	states := map[string]*incTableState{}
	for _, t := range job.Tables {
		if st, ok := oldStates[t.Table]; ok {
			states[t.Table] = st
		} else {
			states[t.Table] = &incTableState{}
		}
	}
	e.Name, e.SourceRef, e.TargetRef = job.Name, job.SourceRef, job.TargetRef
	e.BatchSize, e.StrictMode, e.ConflictStrategy, e.Parallelism = job.BatchSize, job.StrictMode, job.ConflictStrategy, job.Parallelism
	e.Tables, e.States = job.Tables, states
	e.UpdatedAt = time.Now()
	if err := s.saveIncrementalJobs(list); err != nil {
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleDeleteIncrementalJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	incMu.Lock()
	defer incMu.Unlock()
	if incRunning[id] {
		s.writeError(w, http.StatusConflict, "该任务已有同步在进行中")
		return
	}
	list := s.loadIncrementalJobs()
	out := list[:0]
	found := false
	for _, e := range list {
		if e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err := s.saveIncrementalJobs(out); err != nil {
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	// Drop the job's log archives with it (delete can only happen outside a
	// run, so no live collector can be writing into the directory).
	if err := os.RemoveAll(filepath.Join(s.incrementalLogsRoot(), id)); err != nil {
		zap.L().Warn("failed to remove incremental log dir", zap.String("job", id), zap.Error(err))
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// incColumnView is the JSON shape of a column's watermark eligibility.
type incColumnView struct {
	Name       string `json:"name"`
	DataType   string `json:"data_type"`
	Comparable bool   `json:"comparable"`
	Indexed    bool   `json:"indexed"`
}

// queryIncColumns moved to WatermarkDialect (pgWatermarkDialect.QueryColumns,
// wm_dialect.go, MS-04 commit 2/3); callers dial incSourceDialect.QueryColumns.

// incKeyInfo discloses whether a source table has a dedup-capable key
// (FEAT-INC-KEY-WARN): REPLACE/IGNORE conflict strategies rely on a unique
// key on the target, which mirrors the source layout — warn when absent.
type incKeyInfo struct {
	HasPK     bool `json:"has_pk"`
	HasUnique bool `json:"has_unique"`
}

// incTableKeysSQL is a named const so tests can pin its structural guards
// (valid-index-only, non-partial, non-expression, partition exclusion) —
// real semantics are covered by isolation testing against a live PG.
// One catalog round trip covers the whole batch (≤ incColumnsBatchLimit).
const incTableKeysSQL = `
		SELECT tc.relname,
		       bool_or(i.indisprimary) AS has_pk,
		       bool_or(i.indisunique AND NOT i.indisprimary
		               AND i.indpred IS NULL AND i.indexprs IS NULL) AS has_unique
		FROM pg_index i
		JOIN pg_class tc ON tc.oid = i.indrelid
		JOIN pg_namespace ns ON ns.oid = tc.relnamespace
		WHERE ns.nspname = $1 AND tc.relname = ANY($2)
		  AND i.indisvalid AND tc.relispartition = false
		GROUP BY tc.relname`

// queryIncTableKeys returns the key disclosure per table. Tables absent
// from the map have no qualifying key (missing table, plain heap, only
// partial/expression/invalid indexes).
func queryIncTableKeys(ctx context.Context, db *sql.DB, schema string, tables []string) (map[string]incKeyInfo, error) {
	if len(tables) == 0 {
		return map[string]incKeyInfo{}, nil
	}
	// pgx stdlib encodes []string natively as a text[] argument for ANY($2).
	rows, err := db.QueryContext(ctx, incTableKeysSQL, schema, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]incKeyInfo{}
	for rows.Next() {
		var name string
		var ki incKeyInfo
		if err := rows.Scan(&name, &ki.HasPK, &ki.HasUnique); err != nil {
			return nil, err
		}
		out[name] = ki
	}
	return out, rows.Err()
}

// handleIncrementalColumns lists a table's columns with watermark eligibility
// and an index flag (no index on the watermark column ⇒ full scans ⇒ warn).
func (s *Server) handleIncrementalColumns(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("source_ref")
	table := chi.URLParam(r, "table")
	if ref == "" {
		s.writeError(w, http.StatusBadRequest, "source_ref is required")
		return
	}
	if !incIdentifierOK(table) {
		s.writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	e, err := s.resolveDataSourceRef(ref)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if !incSourceWatermarkCapable(e.Type) {
		// MS-04 absorbs this guard (ruling seq 82).
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	sc := dataSourceToSourceConfig(e)
	db, err := openPGTestConn(sc.DSN())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cols, err := incSourceDialect.QueryColumns(ctx, db, sc.Schema, table)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "list columns failed: "+err.Error())
		return
	}
	if len(cols) == 0 {
		s.writeError(w, http.StatusNotFound, "table not found in source schema")
		return
	}
	keys, err := queryIncTableKeys(ctx, db, sc.Schema, []string{table})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "query table keys failed: "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"columns":  cols,
		"key_info": keys[table], // absent ⇒ zero-value {false,false} ⇒ "no key"
	})
}

// incColumnsBatchLimit caps the batch endpoint: one information_schema round
// trip per table, so bound the fan-out.
const incColumnsBatchLimit = 200

// handleIncrementalColumnsBatch (#t1) returns the watermark-eligibility
// column list for many tables over ONE source connection — the batch-binding
// UI flow would otherwise open one connection per table.
func (s *Server) handleIncrementalColumnsBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceRef string   `json:"source_ref"`
		Tables    []string `json:"tables"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.SourceRef == "" {
		s.writeError(w, http.StatusBadRequest, "source_ref is required")
		return
	}
	if len(req.Tables) == 0 {
		s.writeError(w, http.StatusBadRequest, "tables is required")
		return
	}
	if len(req.Tables) > incColumnsBatchLimit {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("tables 数超过上限 %d", incColumnsBatchLimit))
		return
	}
	seen := map[string]bool{}
	for _, t := range req.Tables {
		if !incIdentifierOK(t) {
			s.writeError(w, http.StatusBadRequest, "invalid table name: "+t)
			return
		}
		if seen[t] {
			s.writeError(w, http.StatusBadRequest, "表重复: "+t)
			return
		}
		seen[t] = true
	}
	e, err := s.resolveDataSourceRef(req.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if !incSourceWatermarkCapable(e.Type) {
		// MS-04 absorbs this guard (ruling seq 82).
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	sc := dataSourceToSourceConfig(e)
	db, err := openPGTestConn(sc.DSN())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	type tableResult struct {
		Table   string          `json:"table"`
		Columns []incColumnView `json:"columns"`
		KeyInfo *incKeyInfo     `json:"key_info,omitempty"`
		Error   string          `json:"error,omitempty"`
	}
	// One catalog round trip covers the whole batch (FEAT-INC-KEY-WARN):
	// missing tables simply stay absent from the map ⇒ no key.
	keys, err := queryIncTableKeys(ctx, db, sc.Schema, req.Tables)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "query table keys failed: "+err.Error())
		return
	}
	out := make([]tableResult, 0, len(req.Tables))
	for _, t := range req.Tables {
		cols, err := incSourceDialect.QueryColumns(ctx, db, sc.Schema, t)
		if err != nil {
			out = append(out, tableResult{Table: t, Error: err.Error()})
			continue
		}
		if len(cols) == 0 {
			out = append(out, tableResult{Table: t, Error: "表在源库中不存在"})
			continue
		}
		ki := keys[t] // zero-value {false,false} when absent
		out = append(out, tableResult{Table: t, Columns: cols, KeyInfo: &ki})
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"tables": out})
}

// --- run engine ---

func incValueToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// runIncrementalJob executes one manual sync pass over the (sub)set of tables
// and returns the run record. Failures are per-table: one bad table never
// blocks the others. Runs execute in a background goroutine; concurrency is
// serialized per job via incRunning (409 on concurrent run/edit/delete).
// lg receives the run's log events (nil-safe).
func (s *Server) runIncrementalJob(ctx context.Context, job *incJob, subset map[string]bool, lg *incLogCollector) incRunRecord {
	if incRunCtxHook != nil {
		incRunCtxHook(ctx)
	}
	rec := incRunRecord{RunID: uuid.New().String()[:8], StartedAt: time.Now()}
	nTables := len(job.Tables)
	if len(subset) > 0 {
		nTables = len(subset)
	}
	lg.add(incLogLevelInfo, "", incLogPhaseStart,
		fmt.Sprintf("同步开始：%d 张表，批大小 %d，冲突策略 %s（strict=%v），并行度 %d", nTables, job.BatchSize, job.ConflictStrategy, job.StrictMode, normalizeIncParallelism(job.Parallelism)), "", 0, "", 0)
	src, err := s.resolveDataSourceRef(job.SourceRef)
	if err != nil {
		lg.add(incLogLevelError, "", incLogPhaseFail, "source_ref 解析失败: "+err.Error(), "", 0, "", 0)
		for _, t := range job.Tables {
			rec.Tables = append(rec.Tables, incTableResult{Table: t.Table, Error: "source_ref: " + err.Error()})
		}
		return rec
	}
	tgt, err := s.resolveDataSourceRef(job.TargetRef)
	if err != nil {
		lg.add(incLogLevelError, "", incLogPhaseFail, "target_ref 解析失败: "+err.Error(), "", 0, "", 0)
		for _, t := range job.Tables {
			rec.Tables = append(rec.Tables, incTableResult{Table: t.Table, Error: "target_ref: " + err.Error()})
		}
		return rec
	}
	sc := dataSourceToSourceConfig(src)
	tc := dataSourceToTargetConfig(tgt)

	pgDB, err := openPGTestConn(sc.DSN())
	if err != nil {
		lg.add(incLogLevelError, "", incLogPhaseFail, "连接源端失败: "+err.Error(), "", 0, "", 0)
		for _, t := range job.Tables {
			rec.Tables = append(rec.Tables, incTableResult{Table: t.Table, Error: "连接源端失败: " + err.Error()})
		}
		return rec
	}
	defer pgDB.Close()
	myDB, err := openMySQLTestConn(incTargetDSN(tc)) // P-INC-TZ: UTC-pinned write session
	if err != nil {
		lg.add(incLogLevelError, "", incLogPhaseFail, "连接目标端失败: "+err.Error(), "", 0, "", 0)
		for _, t := range job.Tables {
			rec.Tables = append(rec.Tables, incTableResult{Table: t.Table, Error: "连接目标端失败: " + err.Error()})
		}
		return rec
	}
	defer myDB.Close()

	// No run-level deadline: runs execute detached from any request and the
	// old shared 10-minute cap starved trailing tables after a big table
	// consumed the budget (root cause of the misleading "读取源表列信息失败:
	// context deadline exceeded" reports). Context cancellation is still
	// honored if a parent context ever supplies one.

	// --- worker pool execution model (FEAT-INC-PARALLEL) ---
	// Concurrency-safety contract:
	//   * job.States is only read here, in the scheduler, before any worker
	//     starts (concurrent map access would be a race); workers receive the
	//     pre-resolved *incTableState pointers and never touch the map.
	//   * rec.Tables results come back in original table order; each worker
	//     writes only its own index — no append, no lock, stable order.
	//   * Persistence stays single-pointed: workers never persist; jobs-file
	//     writes happen only in the run-start stub and the finalize path
	//     (positive ①).
	var tasks []incRunTableTask
	for _, t := range job.Tables {
		if len(subset) > 0 && !subset[t.Table] {
			continue
		}
		st := job.States[t.Table]
		if st == nil {
			st = &incTableState{}
			job.States[t.Table] = st
		}
		tasks = append(tasks, incRunTableTask{t: t, st: st})
	}
	workers := normalizeIncParallelism(job.Parallelism)
	rec.Tables = incRunTableTasks(tasks, workers,
		func(task incRunTableTask) incTableResult {
			res := s.syncOneTable(ctx, pgDB, myDB, sc, tc, job, task.t, task.st, lg)
			res.Table = task.t.Table
			return res
		},
		func(task incRunTableTask, dur time.Duration, r interface{}) incTableResult {
			zap.L().Error("incremental table sync panicked",
				zap.String("job", job.ID), zap.String("table", task.t.Table), zap.Any("panic", r))
			errMsg := "内部错误：表同步异常终止"
			task.st.Failed = errMsg
			lg.add(incLogLevelError, task.t.Table, incLogPhaseFail, errMsg, "", 0, "", dur.Milliseconds())
			return incTableResult{Table: task.t.Table, FromWM: task.st.LastWatermark, Error: errMsg}
		})

	failed := 0
	for _, res := range rec.Tables {
		if res.Error != "" {
			failed++
		}
	}
	rec.DurationMs = time.Since(rec.StartedAt).Milliseconds()
	if failed > 0 {
		lg.add(incLogLevelWarn, "", incLogPhaseDone,
			fmt.Sprintf("同步结束：%d/%d 张表失败，耗时 %d ms", failed, nTables, rec.DurationMs), "", 0, "", rec.DurationMs)
	} else {
		lg.add(incLogLevelInfo, "", incLogPhaseDone,
			fmt.Sprintf("同步结束：全部完成，耗时 %d ms", rec.DurationMs), "", 0, "", rec.DurationMs)
	}
	return rec
}

// incRunTableTask is one scheduled table sync: the config plus its
// pre-resolved state pointer (resolved by the scheduler before any worker
// starts, so concurrent workers never race on the job.States map).
type incRunTableTask struct {
	t  incTableConfig
	st *incTableState
}

// incRunTableTasks executes the tasks over a worker pool of at most `workers`
// goroutines and returns the results in the original task order (each worker
// writes only its own index — stable order, no locks). Each worker recovers
// its own panics via panicRes (adversarial must-fix: a panic must not cross
// goroutines and kill the process) and keeps draining so the run finishes.
func incRunTableTasks(tasks []incRunTableTask, workers int, runOne func(t incRunTableTask) incTableResult, panicRes func(t incRunTableTask, dur time.Duration, r interface{}) incTableResult) []incTableResult {
	out := make([]incTableResult, len(tasks))
	if len(tasks) == 0 {
		return out
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}
	if workers < 1 {
		workers = 1
	}
	taskCh := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range taskCh {
				func() {
					start := time.Now()
					defer func() {
						if r := recover(); r != nil {
							out[idx] = panicRes(tasks[idx], time.Since(start), r)
						}
					}()
					out[idx] = runOne(tasks[idx])
				}()
			}
		}()
	}
	for i := range tasks {
		taskCh <- i
	}
	close(taskCh)
	wg.Wait()
	return out
}

// syncOneTable syncs one table. st is the pre-resolved per-table state
// pointer (resolved by the scheduler — job.States is not consulted here so
// concurrent workers never race on the map).
func (s *Server) syncOneTable(ctx context.Context, pgDB, myDB *sql.DB, sc config.SourceConfig, tc config.TargetConfig, job *incJob, t incTableConfig, st *incTableState, lg *incLogCollector) incTableResult {
	tableStart := time.Now()
	res := incTableResult{Table: t.Table, FromWM: st.LastWatermark}

	wm := st.LastWatermark
	if wm == "" {
		wm = t.InitialWatermark
	}
	// Discover columns + validate the watermark type (D4 whitelist) at run
	// time against the live source — creation only validates identifiers.
	rows, err := pgDB.QueryContext(ctx, `
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`, sc.Schema, t.Table)
	if err != nil {
		res.Error = "读取源表列信息失败: " + err.Error()
		st.Failed = res.Error
		lg.add(incLogLevelError, t.Table, incLogPhaseFail, res.Error, "", 0, "", time.Since(tableStart).Milliseconds())
		return res
	}
	var cols []string
	var wmType string
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			rows.Close()
			res.Error = "读取源表列信息失败: " + err.Error()
			st.Failed = res.Error
			lg.add(incLogLevelError, t.Table, incLogPhaseFail, res.Error, "", 0, "", time.Since(tableStart).Milliseconds())
			return res
		}
		if name == t.WatermarkColumn {
			wmType = dataType
		}
		cols = append(cols, name)
	}
	rows.Close()
	if len(cols) == 0 {
		res.Error = fmt.Sprintf("源 schema %q 中不存在表 %q", sc.Schema, t.Table)
		st.Failed = res.Error
		lg.add(incLogLevelError, t.Table, incLogPhaseFail, res.Error, "", 0, "", time.Since(tableStart).Milliseconds())
		return res
	}
	if wmType == "" {
		res.Error = fmt.Sprintf("表 %q 不存在水位列 %q", t.Table, t.WatermarkColumn)
		st.Failed = res.Error
		lg.add(incLogLevelError, t.Table, incLogPhaseFail, res.Error, "", 0, "", time.Since(tableStart).Milliseconds())
		return res
	}
	if !incSourceDialect.WatermarkEligible(wmType) {
		res.Error = fmt.Sprintf("水位列 %q 类型 %q 不在白名单（timestamp/timestamptz/date/int/bigint）", t.WatermarkColumn, wmType)
		st.Failed = res.Error
		lg.add(incLogLevelError, t.Table, incLogPhaseFail, res.Error, "", 0, "", time.Since(tableStart).Milliseconds())
		return res
	}
	lg.add(incLogLevelInfo, t.Table, incLogPhaseStart,
		fmt.Sprintf("开始同步：%d 列，水位列 %s（%s）", len(cols), t.WatermarkColumn, wmType), "", 0, res.FromWM, 0)

	// Empty initial watermark ⇒ full backfill from MIN(watermark).
	minDerived := false
	if wm == "" {
		var minWM sql.NullString
		if err := pgDB.QueryRowContext(ctx,
			fmt.Sprintf("SELECT MIN(%s) FROM %s.%s", incSourceDialect.QuoteIdent(t.WatermarkColumn), incSourceDialect.QuoteIdent(sc.Schema), incSourceDialect.QuoteIdent(t.Table)),
		).Scan(&minWM); err != nil {
			res.Error = "计算初始水位失败: " + err.Error()
			st.Failed = res.Error
			return res
		}
		if !minWM.Valid || minWM.String == "" {
			// Empty table: nothing to do, and nothing to advance to
			// (re-probing MIN on every run is harmless — no state to keep).
			now := time.Now()
			st.LastSyncAt = &now
			st.Failed = ""
			res.ToWM = ""
			return res
		}
		wm = minWM.String
		minDerived = true
		res.FromWM = "(MIN) " + wm
		lg.add(incLogLevelInfo, t.Table, incLogPhaseStart,
			"初始水位为空，从 MIN(水位列) 全量回补", "", 0, wm, 0)
	}

	// Strict-mode >= exceptions: (a) a cursor derived from MIN(col) MUST scan
	// with >= first — a strict > would permanently skip every row sitting
	// exactly at MIN (adversarial ①, v2); (b) after a drain jump the
	// jumped-to value's rows are entirely unconsumed — a strict > would skip
	// them all, so the post-jump scan is also >=. Later scans restore the
	// job's strict semantics (the usual boundary-value tradeoff applies).
	geScan := minDerived
	total := int64(0)
	lastWM := wm
	batchNo := 0
	insertLogged := false
	for {
		entryWM := wm
		batchNo++
		scanStart := time.Now()
		selSQL := incSourceDialect.BuildSelectSQL(sc.Schema, t.Table, cols, t.WatermarkColumn, job.StrictMode && !geScan)
		srows, err := pgDB.QueryContext(ctx, selSQL, wm, job.BatchSize)
		if err != nil {
			res.Error = "查询源端失败: " + err.Error()
			st.Failed = res.Error
			lg.add(incLogLevelError, t.Table, incLogPhaseFail, res.Error, "", 0, entryWM, time.Since(tableStart).Milliseconds())
			return res
		}
		batch := [][]any{}
		for srows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := srows.Scan(ptrs...); err != nil {
				srows.Close()
				res.Error = "读取源端行失败: " + err.Error()
				st.Failed = res.Error
				lg.add(incLogLevelError, t.Table, incLogPhaseFail, res.Error, "", 0, entryWM, time.Since(tableStart).Milliseconds())
				return res
			}
			batch = append(batch, vals)
		}
		if err := srows.Err(); err != nil {
			srows.Close()
			res.Error = "遍历源端失败: " + err.Error()
			st.Failed = res.Error
			lg.add(incLogLevelError, t.Table, incLogPhaseFail, res.Error, "", 0, entryWM, time.Since(tableStart).Milliseconds())
			return res
		}
		srows.Close()
		if len(batch) == 0 {
			break
		}
		// D3 frequency control: first 5 batches log the rendered scan SQL in
		// full; afterwards every 50th logs a summary; drain/jump always full.
		if incLogShouldFullScan(batchNo) {
			lg.add(incLogLevelSQL, t.Table, incLogPhaseScan,
				fmt.Sprintf("第 %d 批扫描（%d 行）", batchNo, len(batch)),
				incRenderSelectSQL(sc.Schema, t.Table, cols, t.WatermarkColumn, job.StrictMode && !geScan, entryWM, job.BatchSize),
				int64(len(batch)), entryWM, time.Since(scanStart).Milliseconds())
		} else if incLogShouldSummaryScan(batchNo) {
			lg.add(incLogLevelInfo, t.Table, incLogPhaseScan,
				fmt.Sprintf("第 %d 批扫描（%d 行）", batchNo, len(batch)), "",
				int64(len(batch)), entryWM, time.Since(scanStart).Milliseconds())
		}

		written, wErr := incExecShardedInsert(ctx, myDB, tc.Database, t.Table, cols, batch, job.ConflictStrategy)
		if wErr != nil {
			res.Error = "写入目标端失败: " + wErr.Error()
			st.Failed = res.Error
			lg.add(incLogLevelError, t.Table, incLogPhaseWrite, res.Error, "", written, entryWM, time.Since(tableStart).Milliseconds())
			return res
		}
		total += written
		shard := incShardRows(len(cols), len(batch))
		nShards := (len(batch) + shard - 1) / shard
		if shard < 1 {
			nShards = 1
		}
		var insSQL string
		if !insertLogged {
			// INSERT statement shape, first occurrence per table only.
			insertLogged = true
			insSQL = incBuildInsertSQL(tc.Database, t.Table, cols, shard, job.ConflictStrategy)
		}
		lg.add(incLogLevelInfo, t.Table, incLogPhaseWrite,
			fmt.Sprintf("写入 %d 行（%d 片）", written, nShards), insSQL, written, "", time.Since(scanStart).Milliseconds())
		// ORDER BY watermark ⇒ the last row carries the batch MAX.
		lastWM = incValueToString(batch[len(batch)-1][wmIndex(cols, t.WatermarkColumn)])
		next, saturated, done := incCursorStep(entryWM, lastWM, len(batch), job.BatchSize)
		if saturated {
			// Same-value saturation: the keyset cannot advance inside this
			// value's window. Drain every remaining row at this watermark
			// (streamed, chunked writes), then jump to the next value.
			lg.add(incLogLevelSQL, t.Table, incLogPhaseDrain,
				fmt.Sprintf("整批同值饱和，进入泄流（水位 %s）", lastWM),
				incRenderDrainSQL(sc.Schema, t.Table, cols, t.WatermarkColumn, lastWM), 0, lastWM, 0)
			n, jump, tableDone, derr := s.incDrainWatermark(ctx, pgDB, myDB, sc, tc, job, t, cols, lastWM, lg)
			if derr != nil {
				res.Error = "泄流同值批次失败: " + derr.Error()
				st.Failed = res.Error
				lg.add(incLogLevelError, t.Table, incLogPhaseDrain, res.Error, "", n, lastWM, time.Since(tableStart).Milliseconds())
				return res
			}
			total += n
			lg.add(incLogLevelSQL, t.Table, incLogPhaseJump,
				fmt.Sprintf("泄流完成（%d 行），跳转下一水位", n),
				incRenderNextWatermarkSQL(sc.Schema, t.Table, t.WatermarkColumn, lastWM), n, jump, 0)
			if tableDone {
				break
			}
			wm = jump
			geScan = true
			continue
		}
		geScan = false
		wm = next
		if done {
			break
		}
	}

	now := time.Now()
	st.LastWatermark = incAdvanceWatermark(st.LastWatermark, lastWM, total)
	st.LastSyncAt = &now
	st.TotalRows += total
	st.Failed = ""
	res.ToWM = st.LastWatermark
	res.Rows = total
	lg.add(incLogLevelInfo, t.Table, incLogPhaseDone,
		fmt.Sprintf("表同步完成：%d 行，水位 → %s", total, st.LastWatermark), "", total, st.LastWatermark, time.Since(tableStart).Milliseconds())
	return res
}

func wmIndex(cols []string, wmCol string) int {
	for i, c := range cols {
		if c == wmCol {
			return i
		}
	}
	return -1
}

// incDrainWatermark consumes ALL rows whose watermark equals wm — the
// same-value saturation fix. The equality scan is unbounded and streamed;
// rows are written in BatchSize chunks with the job's conflict strategy
// (semantics identical to the main path). Afterwards the cursor jumps to
// MIN(watermark) > wm: no such value ⇒ the whole table is synced (done).
// Memory stays bounded regardless of how many rows share the value.
func (s *Server) incDrainWatermark(ctx context.Context, pgDB, myDB *sql.DB, sc config.SourceConfig, tc config.TargetConfig, job *incJob, t incTableConfig, cols []string, wm string, lg *incLogCollector) (rows int64, nextWM string, done bool, err error) {
	srows, qErr := pgDB.QueryContext(ctx, incSourceDialect.BuildDrainSQL(sc.Schema, t.Table, cols, t.WatermarkColumn), wm)
	if qErr != nil {
		return 0, "", false, qErr
	}
	batch := [][]any{}
	flushNo := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		flushNo++
		written, wErr := incExecShardedInsert(ctx, myDB, tc.Database, t.Table, cols, batch, job.ConflictStrategy)
		if wErr != nil {
			return wErr
		}
		rows += written
		if flushNo <= incLogFirstFullBatches || flushNo%incLogSummaryEveryNth == 0 {
			lg.add(incLogLevelInfo, t.Table, incLogPhaseWrite,
				fmt.Sprintf("泄流第 %d 片写入（%d 行）", flushNo, len(batch)), "", int64(len(batch)), wm, 0)
		}
		batch = batch[:0]
		return nil
	}
	for srows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if sErr := srows.Scan(ptrs...); sErr != nil {
			srows.Close()
			return rows, "", false, sErr
		}
		batch = append(batch, vals)
		if len(batch) >= job.BatchSize {
			if fErr := flush(); fErr != nil {
				srows.Close()
				return rows, "", false, fErr
			}
		}
	}
	if sErr := srows.Err(); sErr != nil {
		srows.Close()
		return rows, "", false, sErr
	}
	srows.Close()
	if fErr := flush(); fErr != nil {
		return rows, "", false, fErr
	}

	var next sql.NullString
	if qErr = pgDB.QueryRowContext(ctx,
		incSourceDialect.BuildNextWatermarkSQL(sc.Schema, t.Table, t.WatermarkColumn), wm,
	).Scan(&next); qErr != nil {
		return rows, "", false, qErr
	}
	jump, tableDone := incJumpAfterDrain(next)
	return rows, jump, tableDone, nil
}

func (s *Server) handleRunIncrementalJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Tables []string `json:"tables"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			s.writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	subset := map[string]bool{}
	for _, t := range body.Tables {
		if !incIdentifierOK(t) {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid table name %q", t))
			return
		}
		subset[t] = true
	}

	incMu.Lock()
	list := s.loadIncrementalJobs()
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		incMu.Unlock()
		s.writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if incRunning[id] {
		incMu.Unlock()
		s.writeError(w, http.StatusConflict, "该任务已有同步在进行中")
		return
	}
	job := list[idx]
	// Drop a running stub into history immediately and return 202: the run
	// itself executes detached from this request (client polls GET /jobs).
	stub := incRunRecord{
		RunID:     uuid.New().String()[:8],
		StartedAt: time.Now(),
		Status:    incRunStatusRunning,
	}
	list[idx].History = append([]incRunRecord{stub}, list[idx].History...)
	if len(list[idx].History) > incHistoryCap {
		list[idx].History = list[idx].History[:incHistoryCap]
	}
	if err := s.saveIncrementalJobs(list); err != nil {
		incMu.Unlock()
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	incRunning[id] = true
	incMu.Unlock()

	lg := incLogStartRun(stub.RunID)
	go func() {
		// A panic in the background run would kill the whole process (no
		// handler-level Recoverer here); recover and finalize as failed.
		defer func() {
			if r := recover(); r != nil {
				zap.L().Error("incremental run panicked",
					zap.String("job", id), zap.Any("panic", r))
				s.finalizeIncRunFailed(id, stub.RunID)
			}
		}()
		rec := s.runIncrementalJob(context.Background(), &job, subset, lg)
		rec.RunID = stub.RunID
		rec.Status = incRunStatusCompleted
		s.finalizeIncRun(id, stub.RunID, rec, job.States)
	}()

	s.writeJSON(w, http.StatusAccepted, map[string]string{"run_id": stub.RunID, "status": incRunStatusRunning})
}

// finalizeIncRun replaces the running stub (matched by RunID) with rec under
// incMu: reload the latest list, merge the engine's States and persist. Shared
// by the normal completion path and the panic path. If the job vanished (e.g.
// deleted mid-run) only the in-memory running flag is cleared.
func (s *Server) finalizeIncRun(id, runID string, rec incRunRecord, states map[string]*incTableState) {
	incMu.Lock()
	defer incMu.Unlock()
	delete(incRunning, id)
	// Log summary onto the record (events themselves go to the archive file,
	// keeping GET /jobs light — D5 independent-file variant).
	if c := incLogGetRun(runID); c != nil {
		_, dropped, seq := c.snapshot(0)
		rec.LogEvents = seq
		rec.LogDropped = dropped
	}
	list := s.loadIncrementalJobs()
	for i := range list {
		if list[i].ID != id {
			continue
		}
		if states != nil {
			list[i].States = states
		}
		for j := range list[i].History {
			if list[i].History[j].RunID == runID {
				list[i].History[j] = rec
				break
			}
		}
		list[i].UpdatedAt = time.Now()
		if err := s.saveIncrementalJobs(list); err != nil {
			zap.L().Warn("failed to persist incremental job state", zap.String("job", id), zap.Error(err))
		}
		break
	}
	// Detach the live collector and archive the ring (nil-safe when the run
	// predates FEAT-INC-LOGS or the collector was never started).
	s.archiveIncRunLogs(id, runID, rec.Status, incLogEndRun(runID))
}

// finalizeIncRunFailed closes a run that died unexpectedly: the stub is
// rewritten to failed with a generic error, States are left untouched.
func (s *Server) finalizeIncRunFailed(id, runID string) {
	if c := incLogGetRun(runID); c != nil {
		c.add(incLogLevelError, "", incLogPhaseFail, "内部错误：同步异常终止（panic 已恢复）", "", 0, "", 0)
	}
	s.finalizeIncRun(id, runID, incRunRecord{
		RunID:     runID,
		StartedAt: time.Now(),
		Status:    incRunStatusFailed,
		Error:     "内部错误：同步异常终止",
	}, nil)
}
