// compare_handler.go wires the MongoDB compare engine (M2, internal/
// mongocompare) into the task layer as a standalone compare task type (M3).
//
// Design (thread seq=65, review-approved direction):
//   - compare tasks do NOT go through orchestrator — this handler drives
//     mongocompare.CompareDatabases directly, mirroring the M1
//     source_handler.go purely-additive pattern (mysql/pg paths untouched).
//   - task kind is encoded inside ConfigJSON ({"kind":"mongocompare", ...}); the
//     store schema is not extended (red line: no store/orchestrator edits).
//   - v1 semantics: one source database vs one target database (src/dst db
//     names may differ, via source.database/target.database). Whole-instance
//     compare with database mapping is left for a later milestone.
//   - resume semantics (engine doc, engine.go): checkpoint reuse assumes the
//     data has not changed and bucket boundaries match; changes within the
//     same boundaries are surfaced as diffs (usually what is wanted).
//     AlgoVersion mismatch invalidates checkpoints engine-side; the task
//     layer only passes Resume through.
//   - OutDir is mandatory at this layer (engine skips all persistence when
//     it is empty, which would silently break resume): <dataDir>/compare/
//     <taskID>. It intentionally does NOT live under .checkpoint/<taskID>,
//     which handleStartTask wipes on every start.
//
// Ops note (seq=61-①): crashed live-test processes can leave orphan fixture
// databases named md_*_<pid>x<seq> (~15MB each) on shared smoke containers;
// the harness sweep deliberately never touches suffixed names, so orphans
// must be dropped manually.
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/mongocompare"
	"github.com/michaelliuyuan/timstool/internal/mongoconn"
	"github.com/michaelliuyuan/timstool/internal/source"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// compareEndpoint is one side of the comparison. Either a full URI or the
// discrete fields may be supplied; discrete fields win when both are given.
// datasource_id (M6) references a saved mongodb datasource: the server
// resolves it into this endpoint's snapshot at CREATE time (F-02 semantics —
// later edits/deletes of the datasource never affect the task), with any
// explicitly supplied discrete field overriding the stored profile.
type compareEndpoint struct {
	URI          string `json:"uri"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	User         string `json:"user"`
	Password     string `json:"password"`
	AuthDB       string `json:"auth_db"`
	TLS          bool   `json:"tls,omitempty"`
	Database     string `json:"database"`
	DatasourceID string `json:"datasource_id,omitempty"`
}

// dialConfig maps this endpoint onto the version-adaptive dial (M5):
// discrete fields build the structured config (M1 pattern — "no URI string
// escaping to get wrong"; passwords containing @ : / % must not be
// string-concatenated into a URI). A bare URI is passed through verbatim;
// its escaping is the caller's responsibility.
func (e compareEndpoint) dialConfig() (mongoconn.DialConfig, error) {
	cfg := mongoconn.DialConfig{URI: e.URI}
	if e.Host != "" {
		cfg.Host, cfg.Port, cfg.URI = e.Host, e.Port, ""
		cfg.User, cfg.Password = e.User, e.Password
		// authSource only when explicitly set, preserving the driver
		// default (admin / connection database) otherwise (M1 parity).
		cfg.AuthDB = strings.TrimSpace(e.AuthDB)
		cfg.TLS = e.TLS
	}
	if e.Host == "" && e.URI == "" {
		return cfg, fmt.Errorf("endpoint needs either host or uri")
	}
	return cfg, nil
}

// resolveCompareRef folds the referenced datasource into the endpoint (M6).
// The stored profile is the base; discrete fields already present on the
// endpoint override it (explicit wins). The password never travels to the
// browser, so the client cannot have filled it — the stored one is used
// unless the request explicitly carries one. Field-key mapping follows the
// two registries: datasource fields are camelCase per mongoMeta (authDb),
// the compare DTO is snake_case (auth_db).
func (s *Server) resolveCompareRef(e *compareEndpoint) error {
	if e.DatasourceID == "" {
		return nil
	}
	entry, err := s.resolveDataSourceRef(e.DatasourceID)
	if err != nil {
		return fmt.Errorf("datasource %q: %v", e.DatasourceID, err)
	}
	if !source.IsMongoDB(entry.Type) {
		return fmt.Errorf("datasource %q is %q, compare requires a mongodb datasource", e.DatasourceID, entry.Type)
	}
	if e.Host == "" {
		e.Host = stringField(entry.Fields, "host")
	}
	if e.Port == 0 {
		if p := stringField(entry.Fields, "port"); p != "" {
			if n, err := dsParsePort(p); err == nil {
				e.Port = n
			}
		}
	}
	if e.User == "" {
		e.User = stringField(entry.Fields, "user")
	}
	if e.Password == "" {
		e.Password = stringField(entry.Fields, "password")
	}
	if strings.TrimSpace(e.AuthDB) == "" {
		e.AuthDB = stringField(entry.Fields, "authDb")
	}
	if !e.TLS {
		switch strings.ToLower(strings.TrimSpace(stringField(entry.Fields, "tls"))) {
		case "true", "1", "on", "yes":
			e.TLS = true
		}
	}
	// The ref has served its purpose: everything below works on the folded
	// snapshot, and the persisted spec carries no live reference back to
	// the (mutable, deletable) datasource registry entry.
	e.DatasourceID = ""
	return nil
}

// compareTaskSpec is the persisted compare-task configuration (stored under
// ConfigJSON key "compare"; all JSON tags snake_case per task-API convention).
// Resume is intentionally NOT persisted here: a fresh start wipes the task's
// OutDir (a retry after cancel/failure wants a clean compare); resume is an
// explicit start-time parameter for the checkpoint-continuation case only
// (review ruling seq=68, semantic pitfall 1).
//
// 第9单 ①: the optional watermark window rides the SAME filter group as the
// pg/mysql compare side (config.WatermarkFilter / config.TableWatermark —
// dialect-free data structures); Table = collection name within the task's
// source database. nil/inactive = full comparison, byte-identical legacy
// engine behavior (red line).
type compareTaskSpec struct {
	Source          compareEndpoint         `json:"source"`
	Target          compareEndpoint         `json:"target"`
	Buckets         int                     `json:"buckets"`
	ParallelColls   int                     `json:"parallel_colls"`
	ParallelBuckets int                     `json:"parallel_buckets"`
	BatchSize       int                     `json:"batch_size"`
	Levels          []string                `json:"levels"`
	Watermark       *config.WatermarkFilter `json:"watermark,omitempty"`
	TableWatermarks []config.TableWatermark `json:"table_watermarks,omitempty"`
	// coll-select ①: optional collection whitelist within the source
	// database. Absent/empty = compare every collection (legacy). The FE
	// omits the field when everything is selected so legacy payloads stay
	// byte-identical.
	Collections []string `json:"collections,omitempty"`
}

// compareTaskEnvelope is the ConfigJSON shape for compare tasks. The kind
// discriminator lets handleStartTask/handleResumeTask route without touching
// the store schema.
type compareTaskEnvelope struct {
	Kind    string          `json:"kind"`
	Compare compareTaskSpec `json:"compare"`
}

const compareTaskKind = "mongocompare"

// parseCompareTask returns the spec when cfgJSON is a compare task.
func parseCompareTask(cfgJSON string) (*compareTaskSpec, bool) {
	var env compareTaskEnvelope
	if err := json.Unmarshal([]byte(cfgJSON), &env); err != nil {
		return nil, false
	}
	if env.Kind != compareTaskKind {
		return nil, false
	}
	return &env.Compare, true
}

// validateCompareParams enforces the MC-9 C4 boundaries: buckets 1..4096,
// parallel_colls 1..32, parallel_buckets 1..32, batch_size 100..100000
// (0 = unset → engine default), and levels containing "diff" without "hash"
// is an explicit rejection (diff drills into mismatched hash buckets —
// without the hash level it can never fire and would silently pass as
// "compared").
func validateCompareParams(spec *compareTaskSpec) error {
	for _, c := range []struct {
		name string
		v    int
		min  int
		max  int
	}{
		{"buckets", spec.Buckets, 1, 4096},
		{"parallel_colls", spec.ParallelColls, 1, 32},
		{"parallel_buckets", spec.ParallelBuckets, 1, 32},
		{"batch_size", spec.BatchSize, 100, 100000},
	} {
		if c.v != 0 && (c.v < c.min || c.v > c.max) {
			return fmt.Errorf("%s must be %d..%d (0 = default)", c.name, c.min, c.max)
		}
	}
	hasHash, hasDiff := false, false
	for _, l := range spec.Levels {
		switch l {
		case string(mongocompare.LevelHash):
			hasHash = true
		case string(mongocompare.LevelDiff):
			hasDiff = true
		}
	}
	if hasDiff && !hasHash {
		return fmt.Errorf(`levels containing "diff" must also contain "hash"`)
	}
	return nil
}

// handleCreateCompareTask creates a standalone mongodb compare task.
// Route: POST /api/v1/mongocompare/tasks (registered next to POST /tasks).
func (s *Server) handleCreateCompareTask(w http.ResponseWriter, r *http.Request) {
	var req compareTaskSpec
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Resolve datasource refs into connection snapshots FIRST (F-02: the
	// snapshot is taken at creation; later datasource edits/deletes never
	// affect the task). The resolved spec — not the ref — is persisted.
	if err := s.resolveCompareRef(&req.Source); err != nil {
		s.writeError(w, http.StatusBadRequest, "source: "+err.Error())
		return
	}
	if err := s.resolveCompareRef(&req.Target); err != nil {
		s.writeError(w, http.StatusBadRequest, "target: "+err.Error())
		return
	}
	if _, err := req.Source.dialConfig(); err != nil {
		s.writeError(w, http.StatusBadRequest, "source: "+err.Error())
		return
	}
	if _, err := req.Target.dialConfig(); err != nil {
		s.writeError(w, http.StatusBadRequest, "target: "+err.Error())
		return
	}
	if req.Source.Database == "" {
		s.writeError(w, http.StatusBadRequest, "source.database is required")
		return
	}
	if req.Target.Database == "" {
		req.Target.Database = req.Source.Database
	}
	for _, l := range req.Levels {
		switch mongocompare.Level(l) {
		case mongocompare.LevelSchema, mongocompare.LevelIndex, mongocompare.LevelCount, mongocompare.LevelHash, mongocompare.LevelDiff:
		default:
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown level %q", l))
			return
		}
	}
	if err := validateCompareParams(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// coll-select ①: normalize the optional whitelist — trim, drop empties,
	// dedupe preserving order. A system.* selection is rejected outright:
	// the engine excludes system namespaces by design, so a payload asking
	// for one means a client bug or a misunderstood payload — refuse loudly
	// instead of silently comparing less than was asked.
	// 对抗席 F5: an EXPLICITLY provided-but-empty list (["", "  "]) must
	// 400 — collapsing it to nil (= full database) would silently widen the
	// scope from "compare nothing" to "compare everything". Absence of the
	// field stays the legacy full-database contract.
	if req.Collections != nil {
		seen := make(map[string]bool, len(req.Collections))
		cols := req.Collections[:0]
		for _, c := range req.Collections {
			c = strings.TrimSpace(c)
			if c == "" || seen[c] {
				continue
			}
			if strings.HasPrefix(c, "system.") {
				s.writeError(w, http.StatusBadRequest, fmt.Sprintf("collections: %q is a system namespace and can never be compared", c))
				return
			}
			seen[c] = true
			cols = append(cols, c)
		}
		if len(cols) == 0 {
			s.writeError(w, http.StatusBadRequest, "collections: list is empty after normalization (omit the field to compare the whole database)")
			return
		}
		if len(cols) > 10000 {
			s.writeError(w, http.StatusBadRequest, "collections: too many entries (max 10000)")
			return
		}
		req.Collections = cols
		// 对抗席 F3: a watermark override pointing outside the whitelist
		// can never fire (wmFor is a pure name match) — refuse here so the
		// exception is never silently lost while the confirm dialog claimed
		// it was active.
		for _, tw := range req.TableWatermarks {
			// Trim before matching (对抗席 N2): normalizeTableWatermarks
			// trims later — comparing the raw name would edge-reject a
			// legitimately padded override.
			if !seen[strings.TrimSpace(tw.Table)] {
				s.writeError(w, http.StatusBadRequest, fmt.Sprintf("table_watermarks: override for %q is not in the selected collections", tw.Table))
				return
			}
		}
	}
	// 第9单 ①: watermark normalization REUSES the webapi-generic validators
	// the pg/mysql side runs (identifier allow-list, single-value/interval
	// mutual exclusion, op enum, duplicate-collection rejection). BaseMode is
	// cleared afterwards — the mongo engine has no quick/sample/checksum
	// dimension, and keeping the field would invite display drift.
	if req.Watermark != nil {
		norm, werr := normalizeWatermarkFilter(req.Watermark)
		if werr != nil {
			s.writeError(w, http.StatusBadRequest, werr.Error())
			return
		}
		norm.BaseMode = ""
		req.Watermark = norm
		// 对抗票 P2（攻击面 d）: the shared normalizer orders range
		// endpoints as raw STRINGS; mongo endpoints live in typed domains
		// (ObjectId bytes / int64) where string order can differ and an
		// inverted pair silently compares nothing — re-gate in the type
		// domain (pg/mysql keeps the string-only contract untouched).
		if oerr := mongocompare.WmRangeOrderErr(norm); oerr != nil {
			s.writeError(w, http.StatusBadRequest, oerr.Error())
			return
		}
	}
	if req.TableWatermarks != nil {
		norm, terr := normalizeTableWatermarks(req.TableWatermarks)
		if terr != nil {
			s.writeError(w, http.StatusBadRequest, terr.Error())
			return
		}
		for i := range norm {
			if oerr := mongocompare.WmRangeOrderErr(&norm[i].WatermarkFilter); oerr != nil {
				s.writeError(w, http.StatusBadRequest, fmt.Sprintf("table_watermarks[%s]: %s", norm[i].Table, oerr.Error()))
				return
			}
		}
		req.TableWatermarks = norm
	}

	task := &store.Task{
		ID:   newTaskID(),
		Name: fmt.Sprintf("MongoDB Compare %s", time.Now().Format("2006-01-02 15:04:05")),
	}
	env := compareTaskEnvelope{Kind: compareTaskKind, Compare: req}
	cfgBytes, err := json.Marshal(env)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	task.ConfigJSON = string(cfgBytes)

	if err := s.store.CreateTask(task); err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, newTaskEnvelope(task))
}

// handleStartCompareTask is the token-gated compare branch of
// handleStartTask (MC-10 ①③): the gate has already run by the time this
// handler is invoked. Stored params are re-validated BEFORE any state
// write — a legacy task created before the C4 boundaries fails loud here
// (explicit 400, zero status mutation) instead of silently running with
// out-of-range knobs.
func (s *Server) handleStartCompareTask(taskID string, spec *compareTaskSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var startBody struct {
			Resume bool `json:"resume"`
		}
		_ = json.NewDecoder(r.Body).Decode(&startBody)
		if err := validateCompareParams(spec); err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.store.UpdateTaskStatus(taskID, store.TaskStatusRunning); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.store.ResetTaskForRerun(taskID)
		s.logCollector.RemoveBuffer(taskID)
		ctx, cancel := context.WithCancel(context.Background())
		s.runningTasks[taskID] = cancel
		s.startCompareTask(ctx, taskID, spec, startBody.Resume)
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "started", "task_id": taskID})
	}
}

// handleResumeCompareTask is the token-gated compare branch of
// handleResumeTask (MC-10 ①③): checkpoint-continuation resume with the
// same pre-write param re-validation as start.
func (s *Server) handleResumeCompareTask(taskID string, spec *compareTaskSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := validateCompareParams(spec); err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.store.UpdateTaskStatus(taskID, store.TaskStatusRunning)
		ctx, cancel := context.WithCancel(context.Background())
		s.runningTasks[taskID] = cancel
		s.startCompareTask(ctx, taskID, spec, true)
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
	}
}

// handlePauseCompareTask / handleCancelCompareTask / handleDeleteCompareTask
// are the token-gated compare branches of the shared lifecycle handlers
// (MC-10 ①): kind-gated wrappers around the exact migration-path bodies, so
// compare and migration semantics stay byte-identical behind the gate.
func (s *Server) handlePauseCompareTask(taskID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.store.UpdateTaskStatus(taskID, store.TaskStatusPaused)
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
	}
}

func (s *Server) handleCancelCompareTask(taskID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cancel := s.runningCancel(taskID); cancel != nil {
			cancel()
		} else {
			s.store.UpdateTaskStatus(taskID, store.TaskStatusCancelled)
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	}
}

func (s *Server) handleDeleteCompareTask(taskID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.store.DeleteTask(taskID)
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

// startCompareTask is the compare branch of task start/resume: it resets the
// task's compare artifacts (see resetCompareArtifacts) and spawns the run.
func (s *Server) startCompareTask(ctx context.Context, taskID string, spec *compareTaskSpec, resume bool) {
	s.resetCompareArtifacts(taskID, resume)
	go s.runMongoCompare(ctx, taskID, spec, resume)
}

// resetCompareArtifacts prepares the task's compare OutDir for a run. A
// fresh start wipes the whole dir (retry wants a clean compare); a resume
// keeps engine checkpoints and diff artifacts for continuation (review
// ruling seq=68) but drops the previous run's summary.json — the report is
// a final-state artifact, so a mid-run /report 404s instead of serving the
// stale previous round (adversarial MC-14 Level-2 #1, fixed in MC-15). A
// failed drop (AV lock, disk error) is logged and carried — the run itself
// still overwrites the summary at finalize.
func (s *Server) resetCompareArtifacts(taskID string, resume bool) {
	dir := filepath.Join(s.dataDir, "compare", taskID)
	if !resume {
		_ = os.RemoveAll(dir)
		return
	}
	if err := os.Remove(filepath.Join(dir, "summary.json")); err != nil && !os.IsNotExist(err) {
		zap.L().Warn("failed to drop stale compare summary on resume", zap.String("task", taskID), zap.Error(err))
	}
}

func (s *Server) runMongoCompare(ctx context.Context, taskID string, spec *compareTaskSpec, resume bool) {
	s.logCollector.GetBuffer(taskID)
	s.logCollector.Append(taskID, "INFO", "MongoDB compare task started", "")

	outDir := filepath.Join(s.dataDir, "compare", taskID)

	srcClient, err := connectMongo(ctx, spec.Source)
	if err != nil {
		s.failCompare(taskID, "connect source: "+err.Error())
		return
	}
	defer srcClient.Disconnect(context.Background())
	dstClient, err := connectMongo(ctx, spec.Target)
	if err != nil {
		s.failCompare(taskID, "connect target: "+err.Error())
		return
	}
	defer dstClient.Disconnect(context.Background())

	cfg := mongocompare.Config{
		Buckets:         spec.Buckets,
		ParallelColls:   spec.ParallelColls,
		ParallelBuckets: spec.ParallelBuckets,
		BatchSize:       spec.BatchSize,
		Resume:          resume,
		OutDir:          outDir,
		Watermark:       spec.Watermark,
		TableWatermarks: spec.TableWatermarks,
		OnlyCollections: spec.Collections,
	}
	for _, l := range spec.Levels {
		cfg.Levels = append(cfg.Levels, mongocompare.Level(l))
	}

	// Lightweight progress: poll completed checkpoint files until the run
	// finishes (the engine has no streaming callback; totals are only known
	// in the final summary, so mid-run progress reports counts, not ratio).
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	go s.pollCompareProgress(pollCtx, taskID, outDir)

	summary, err := mongocompare.CompareDatabases(ctx, srcClient, dstClient, spec.Source.Database, spec.Target.Database, cfg)
	pollCancel()

	if ctx.Err() == context.Canceled {
		s.logCollector.Append(taskID, "WARN", "MongoDB compare task cancelled", "")
		s.store.UpdateTaskStatus(taskID, store.TaskStatusCancelled)
		return
	}
	if err != nil {
		s.failCompare(taskID, "compare failed: "+err.Error())
		return
	}

	// 第9单 ⑦ (对标 compare.go:816 的 all-skipped 门): a watermark task where
	// NOT ONE collection ran inside its window (every applicable collection
	// degraded / failed precheck / TTL-skipped) produced no watermark-window
	// evidence at all — fail loudly instead of a misleading "completed".
	if (spec.Watermark != nil || len(spec.TableWatermarks) > 0) &&
		len(summary.Collections) > 0 && !mongocompare.WmAnyWindowed(summary.Collections) {
		s.failCompare(taskID, "所有集合均未能应用水位过滤（水位列不可用或预检失败），未产生水位窗口比对结果")
		return
	}

	result := map[string]any{
		"kind":         compareTaskKind,
		"overall":      summary.Overall,
		"counts":       summary.Counts,
		"duration_sec": summary.DurationSec,
		"summary_path": filepath.Join(outDir, "summary.json"),
		"diff_dir":     filepath.Join(outDir, "diff"),
	}
	s.store.SetTaskResult(taskID, result)

	total := len(summary.Collections)
	done := summary.Counts["equal"] + summary.Counts["diff"] + summary.Counts["error"] + summary.Counts["ttl_skip"]
	_ = s.store.UpdateTaskProgress(taskID, "compare", 1.0, done, total, int64(done), int64(total))

	status := store.TaskStatusCompleted
	if summary.Overall == mongocompare.OverallError {
		status = store.TaskStatusFailed
	}
	s.logCollector.Append(taskID, "INFO", fmt.Sprintf("MongoDB compare finished: overall=%s counts=%v", summary.Overall, summary.Counts), "")
	s.store.UpdateTaskStatus(taskID, status)
}

func (s *Server) failCompare(taskID, msg string) {
	s.logCollector.Append(taskID, "ERROR", msg, "")
	s.store.SetTaskError(taskID, msg)
}

// pollCompareProgress reports completed bucket-checkpoint files as a coarse
// heartbeat every 2s (mirrors pollProgress cadence).
func (s *Server) pollCompareProgress(ctx context.Context, taskID, outDir string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			entries, err := os.ReadDir(filepath.Join(outDir, ".checkpoints"))
			if err != nil {
				continue
			}
			done := len(entries)
			s.BroadcastProgress(taskID, map[string]any{
				"phase":        "compare",
				"progress":     0, // ratio unknown until summary; clients treat 0 as indeterminate-running
				"buckets_done": done,
			})
		}
	}
}

func connectMongo(ctx context.Context, e compareEndpoint) (mongoconn.Client, error) {
	cfg, err := e.dialConfig()
	if err != nil {
		return nil, err
	}
	client, err := mongoconn.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return client, nil
}

// newTaskID mirrors the id scheme used by handleCreateTask.
func newTaskID() string {
	return uuid.New().String()[:8]
}
