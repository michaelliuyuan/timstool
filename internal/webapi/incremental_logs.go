package webapi

// FEAT-INC-LOGS: structured, observable run logs for the timestamp-watermark
// incremental sync. Events flow through an in-memory per-run ring collector
// while the run is in flight, then are archived to
// dataDir/incremental_logs/<jobID>/<runID>.json on finalize. The endpoint
// serves memory first, archive fallback — the frontend polls one protocol
// (after=seq) regardless of run state.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// incLogEvent levels.
const (
	incLogLevelInfo  = "info"
	incLogLevelSQL   = "sql"
	incLogLevelWarn  = "warn"
	incLogLevelError = "error"
)

// incLogEvent phases (lifecycle positions).
const (
	incLogPhaseStart       = "start"
	incLogPhaseScan        = "scan"
	incLogPhaseWrite       = "write"
	incLogPhaseDrain       = "drain"
	incLogPhaseJump        = "jump"
	incLogPhaseDone        = "done"
	incLogPhaseFail        = "fail"
	incLogPhaseInterrupted = "interrupted"
)

// incLogRingCap bounds the events kept per run (memory and archive alike).
const incLogRingCap = 1000

// incLogEvent is one observable step of a run. SQL strings are display-only
// renders (see incRenderSelectSQL) and never contain row data — only the
// statement shape plus the watermark/limit parameters that were bound.
type incLogEvent struct {
	Seq    int64     `json:"seq"`
	Ts     time.Time `json:"ts"`
	Level  string    `json:"level"`
	Table  string    `json:"table,omitempty"`
	Phase  string    `json:"phase,omitempty"`
	Msg    string    `json:"msg,omitempty"`
	SQL    string    `json:"sql,omitempty"`
	Rows   int64     `json:"rows,omitempty"`
	WM     string    `json:"wm,omitempty"`
	CostMs int64     `json:"cost_ms,omitempty"`
}

// incLogCollector accumulates events for one run. Single writer (the run
// goroutine) plus concurrent snapshot readers; guarded by its own mutex —
// deliberately NOT incMu, so log reads never contend the jobs-file cycle.
type incLogCollector struct {
	mu      sync.Mutex
	events  []incLogEvent
	dropped int64
	seq     int64
}

// add appends one event. Nil-receiver safe so call sites (and tests) can pass
// a nil collector without guards.
func (c *incLogCollector) add(level, table, phase, msg, sql string, rows int64, wm string, costMs int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	if len(c.events) >= incLogRingCap {
		// Evict the oldest: seq values are assigned before eviction, so the
		// gap between the first surviving seq and 1 IS the dropped count.
		c.events = c.events[1:]
		c.dropped++
	}
	c.events = append(c.events, incLogEvent{
		Seq: c.seq, Ts: time.Now(), Level: level, Table: table, Phase: phase,
		Msg: msg, SQL: sql, Rows: rows, WM: wm, CostMs: costMs,
	})
}

// snapshot returns all events with Seq > after plus the total dropped count.
// The returned slice is always non-nil (JSON marshals as [] not null).
func (c *incLogCollector) snapshot(after int64) ([]incLogEvent, int64, int64) {
	if c == nil {
		return []incLogEvent{}, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []incLogEvent{}
	for _, e := range c.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out, c.dropped, c.seq
}

// Registry of live collectors keyed by run ID (own lock; see type comment).
var (
	incLogMu      sync.Mutex
	incLogLiveRun = map[string]*incLogCollector{}
)

func incLogStartRun(runID string) *incLogCollector {
	c := &incLogCollector{events: []incLogEvent{}}
	incLogMu.Lock()
	incLogLiveRun[runID] = c
	incLogMu.Unlock()
	return c
}

func incLogGetRun(runID string) *incLogCollector {
	incLogMu.Lock()
	defer incLogMu.Unlock()
	return incLogLiveRun[runID]
}

// incLogEndRun detaches the collector (run finished) and returns it for
// archiving.
func incLogEndRun(runID string) *incLogCollector {
	incLogMu.Lock()
	defer incLogMu.Unlock()
	c := incLogLiveRun[runID]
	delete(incLogLiveRun, runID)
	return c
}

// incLogArchive is the on-disk shape under incremental_logs/<jobID>/.
type incLogArchive struct {
	RunID   string        `json:"run_id"`
	Status  string        `json:"status"`
	Dropped int64         `json:"dropped"`
	Events  []incLogEvent `json:"events"`
}

func (s *Server) incrementalLogsRoot() string {
	return filepath.Join(s.dataDir, "incremental_logs")
}

func (s *Server) incrementalLogFile(jobID, runID string) string {
	return filepath.Join(s.incrementalLogsRoot(), jobID, runID+".json")
}

// archiveIncRunLogs persists the collector's ring to the run's archive file.
// Event count is already bounded by the ring cap — no second truncation.
func (s *Server) archiveIncRunLogs(jobID, runID, status string, c *incLogCollector) {
	if c == nil {
		return
	}
	events, dropped, _ := c.snapshot(0)
	arch := incLogArchive{RunID: runID, Status: status, Dropped: dropped, Events: events}
	dir := filepath.Join(s.incrementalLogsRoot(), jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		zap.L().Warn("failed to create incremental log dir", zap.String("dir", dir), zap.Error(err))
		return
	}
	raw, err := json.MarshalIndent(arch, "", "  ")
	if err != nil {
		zap.L().Warn("failed to marshal incremental log archive", zap.String("run", runID), zap.Error(err))
		return
	}
	if err := os.WriteFile(s.incrementalLogFile(jobID, runID), raw, 0o644); err != nil {
		zap.L().Warn("failed to write incremental log archive", zap.String("run", runID), zap.Error(err))
	}
}

// archiveInterruptedRun writes the single-event archive for a run whose
// process died mid-flight: pre-restart events are unrecoverable, so the file
// starts a fresh seq space with one terminal event and says so.
func (s *Server) archiveInterruptedRun(jobID, runID string) {
	arch := incLogArchive{
		RunID:  runID,
		Status: incRunStatusFailed,
		Events: []incLogEvent{{
			Seq: 1, Ts: time.Now(), Level: incLogLevelWarn, Phase: incLogPhaseInterrupted,
			Msg: "服务重启，运行中断（重启前的事件不可恢复）",
		}},
	}
	dir := filepath.Join(s.incrementalLogsRoot(), jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(arch, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(s.incrementalLogFile(jobID, runID), raw, 0o644); err != nil {
		zap.L().Warn("failed to write interrupted run log archive", zap.String("run", runID), zap.Error(err))
	}
}

// sweepIncrementalLogDirs removes archive directories of deleted jobs
// (startup hygiene; delete also removes its own dir inline).
func (s *Server) sweepIncrementalLogDirs() {
	entries, err := os.ReadDir(s.incrementalLogsRoot())
	if err != nil {
		return
	}
	live := map[string]bool{}
	incMu.Lock()
	for _, j := range s.loadIncrementalJobs() {
		live[j.ID] = true
	}
	incMu.Unlock()
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.incrementalLogsRoot(), e.Name())); err != nil {
			zap.L().Warn("failed to sweep incremental log dir", zap.String("dir", e.Name()), zap.Error(err))
		}
	}
}

// handleIncrementalRunLogs serves GET /incremental/jobs/{id}/runs/{run_id}/logs
// ?after=<seq>. Live collector first, archive fallback; unknown job / unknown
// run / deleted job all 404. Pure GET, no side effects.
func (s *Server) handleIncrementalRunLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	runID := chi.URLParam(r, "run_id")
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			s.writeError(w, http.StatusBadRequest, "invalid after parameter")
			return
		}
		after = v
	}

	incMu.Lock()
	list := s.loadIncrementalJobs()
	incMu.Unlock()
	jobFound := false
	for i := range list {
		if list[i].ID == id {
			jobFound = true
			break
		}
	}
	if !jobFound {
		s.writeError(w, http.StatusNotFound, "job not found")
		return
	}

	if c := incLogGetRun(runID); c != nil {
		events, dropped, lastSeq := c.snapshot(after)
		s.writeJSON(w, http.StatusOK, map[string]any{
			"events": events, "status": incRunStatusRunning, "dropped": dropped, "last_seq": lastSeq,
		})
		return
	}

	raw, err := os.ReadFile(s.incrementalLogFile(id, runID))
	if err != nil {
		s.writeError(w, http.StatusNotFound, "run logs not found")
		return
	}
	var arch incLogArchive
	if err := json.Unmarshal(raw, &arch); err != nil {
		s.writeError(w, http.StatusInternalServerError, "log archive corrupted: "+err.Error())
		return
	}
	events := []incLogEvent{}
	for _, e := range arch.Events {
		if e.Seq > after {
			events = append(events, e)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"events": events, "status": arch.Status, "dropped": arch.Dropped, "last_seq": int64(len(arch.Events)),
	})
}

// ---- display-only SQL renders ----
//
// These inline the bound parameters into the exact statement shapes that
// incBuildSelectSQL / incBuildDrainSQL / incBuildNextWatermarkSQL produce with
// $1 placeholders. The REAL execution path never interpolates values; the
// renders exist solely so the log can show what ran. Keep them decoupled from
// the builders (no shared mutable state) — changing one must not drift the
// other silently, which is why the anchors pin both sides.

func incQuoteSQLLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// incRenderSelectSQL renders the keyset scan with wm and limit inlined.
// MS-04 note: these log-preview renderers are SAME-SOURCE different call
// sites of the dialect renderers (identifier fragments via
// incSourceDialect.QuoteIdent; overall SQL differs from the parameterized
// main-flow shapes by design - inline wm/limit for display). Do NOT copy
// them again; the quote fragments are byte-anchored to the dialect in
// incremental_test.go (TestIncLogsRenderersQuoteFragments).
func incRenderSelectSQL(d WatermarkDialect, schema, table string, cols []string, wmCol string, strict bool, wm string, limit int) string {
	op := ">="
	if strict {
		op = ">"
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = d.QuoteIdent(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s %s %s ORDER BY %s LIMIT %d",
		strings.Join(quoted, ", "), d.QuoteIdent(schema), d.QuoteIdent(table),
		d.QuoteIdent(wmCol), op, incQuoteSQLLiteral(wm), d.QuoteIdent(wmCol), limit)
}

// incRenderDrainSQL renders the same-value drain scan with wm inlined.
func incRenderDrainSQL(d WatermarkDialect, schema, table string, cols []string, wmCol, wm string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = d.QuoteIdent(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s = %s",
		strings.Join(quoted, ", "), d.QuoteIdent(schema), d.QuoteIdent(table),
		d.QuoteIdent(wmCol), incQuoteSQLLiteral(wm))
}

// incRenderNextWatermarkSQL renders the post-drain jump probe with wm inlined.
func incRenderNextWatermarkSQL(d WatermarkDialect, schema, table, wmCol, wm string) string {
	return fmt.Sprintf("SELECT MIN(%s) FROM %s.%s WHERE %s > %s",
		d.QuoteIdent(wmCol), d.QuoteIdent(schema), d.QuoteIdent(table),
		d.QuoteIdent(wmCol), incQuoteSQLLiteral(wm))
}

// incLogScanBatchFull / incLogScanBatchSummary are the D3 frequency controls:
// the first incLogFirstFullBatches batches of each table log the rendered SQL
// in full; afterwards every incLogSummaryEveryNth batch logs a summary only.
// drain/jump transitions are always logged in full (see the engine hooks).
const (
	incLogFirstFullBatches = 5
	incLogSummaryEveryNth  = 50
)

// incLogShouldFullScan reports whether batch n (1-based) gets a full-SQL
// event (the first incLogFirstFullBatches batches); incLogShouldSummaryScan
// reports whether it gets a SQL-less summary (every incLogSummaryEveryNth
// batch afterwards — the 50th, 100th, … boundary is the off-by-one hot spot
// the anchors pin).
func incLogShouldFullScan(n int) bool { return n <= incLogFirstFullBatches }
func incLogShouldSummaryScan(n int) bool {
	return n > incLogFirstFullBatches && n%incLogSummaryEveryNth == 0
}
