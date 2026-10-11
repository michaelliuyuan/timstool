package webapi

// FEAT-INC-LOGS anchors: seq/after protocol, ring eviction + dropped, the
// D3 frequency boundaries (50/100 off-by-one), display-only renders, archive
// round-trip, interrupted-run archive, endpoint 404/400 faces and collector
// isolation.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// withChiParams attaches several chi URL params on ONE route context
// (withChiParam resets the context per call, losing earlier params).
func withChiParams(req *http.Request, kv ...string) *http.Request {
	rctx := chi.NewRouteContext()
	for i := 0; i+1 < len(kv); i += 2 {
		rctx.URLParams.Add(kv[i], kv[i+1])
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestIncLogSeqMonotonicAndAfter(t *testing.T) {
	c := &incLogCollector{events: []incLogEvent{}}
	for i := 0; i < 5; i++ {
		c.add(incLogLevelInfo, "t1", incLogPhaseScan, "m", "", int64(i), "w", 0)
	}
	events, dropped, lastSeq := c.snapshot(0)
	if len(events) != 5 || dropped != 0 || lastSeq != 5 {
		t.Fatalf("full snapshot: len=%d dropped=%d lastSeq=%d", len(events), dropped, lastSeq)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq must be monotonic from 1: %+v", events)
		}
	}
	events, _, _ = c.snapshot(3)
	if len(events) != 2 || events[0].Seq != 4 || events[1].Seq != 5 {
		t.Fatalf("after=3 must return only seq>3: %+v", events)
	}
	// Marshal-level: events must never serialize as null (B1 lesson class).
	raw, err := json.Marshal(map[string]any{"events": events})
	if err != nil || string(raw) == `{"events":null}` {
		t.Fatalf("events must marshal as []: %v %s", err, raw)
	}
}

func TestIncLogRingEvictionDropped(t *testing.T) {
	c := &incLogCollector{events: []incLogEvent{}}
	for i := 0; i < incLogRingCap+50; i++ {
		c.add(incLogLevelInfo, "", incLogPhaseScan, "", "", 0, "", 0)
	}
	events, dropped, _ := c.snapshot(0)
	if len(events) != incLogRingCap {
		t.Fatalf("ring must cap at %d: %d", incLogRingCap, len(events))
	}
	if dropped != 50 {
		t.Fatalf("dropped must count evictions: %d", dropped)
	}
	if events[0].Seq != 51 {
		t.Fatalf("first surviving seq must be 51 (dropped gap): %d", events[0].Seq)
	}
	// after below the first surviving seq replays from the first survivor.
	events, dropped, _ = c.snapshot(10)
	if len(events) != incLogRingCap || dropped != 50 || events[0].Seq != 51 {
		t.Fatalf("stale after must replay from first event with dropped: len=%d dropped=%d first=%d",
			len(events), dropped, events[0].Seq)
	}
}

func TestIncLogFrequencyBoundaries(t *testing.T) {
	for n := 1; n <= incLogFirstFullBatches; n++ {
		if !incLogShouldFullScan(n) || incLogShouldSummaryScan(n) {
			t.Fatalf("batch %d must be full-SQL (not summary)", n)
		}
	}
	for _, n := range []int{6, 7, 25, 49, 51, 99, 101} {
		if incLogShouldFullScan(n) || incLogShouldSummaryScan(n) {
			t.Fatalf("batch %d must be neither full nor summary", n)
		}
	}
	for _, n := range []int{50, 100, 150} {
		if incLogShouldFullScan(n) || !incLogShouldSummaryScan(n) {
			t.Fatalf("K-multiple batch %d must be summary (and not full)", n)
		}
	}
}

func TestIncLogRenderSelectSQL(t *testing.T) {
	got := incRenderSelectSQL(incSourceDialect, "public", "users", []string{"id", "update_time"}, "update_time", false, "2026-09-29 00:00:00", 1000)
	want := `SELECT "id", "update_time" FROM "public"."users" WHERE "update_time" >= '2026-09-29 00:00:00' ORDER BY "update_time" LIMIT 1000`
	if got != want {
		t.Fatalf("render mismatch:\n got: %s\nwant: %s", got, want)
	}
	got = incRenderSelectSQL(incSourceDialect, "s", "t", []string{"w"}, "w", true, "x'y", 5)
	want = `SELECT "w" FROM "s"."t" WHERE "w" > 'x''y' ORDER BY "w" LIMIT 5`
	if got != want {
		t.Fatalf("strict + quote-escape mismatch:\n got: %s\nwant: %s", got, want)
	}
	got = incRenderDrainSQL(incSourceDialect, "s", "t", []string{"a", "w"}, "w", "w1")
	want = `SELECT "a", "w" FROM "s"."t" WHERE "w" = 'w1'`
	if got != want {
		t.Fatalf("drain render mismatch:\n got: %s\nwant: %s", got, want)
	}
	got = incRenderNextWatermarkSQL(incSourceDialect, "s", "t", "w", "w1")
	want = `SELECT MIN("w") FROM "s"."t" WHERE "w" > 'w1'`
	if got != want {
		t.Fatalf("jump render mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// seedLogJob writes a minimal one-job registry so endpoint tests have a job.
func seedLogJob(t *testing.T, s *Server, id string) {
	t.Helper()
	incMu.Lock()
	defer incMu.Unlock()
	list := []incJob{{
		ID: id, Name: "logjob", States: map[string]*incTableState{},
		History: []incRunRecord{{RunID: "archived1", StartedAt: time.Now(), Status: incRunStatusCompleted}},
	}}
	if err := s.saveIncrementalJobs(list); err != nil {
		t.Fatal(err)
	}
}

func TestIncLogArchiveRoundTripAndEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	seedLogJob(t, s, "jobA")

	c := &incLogCollector{events: []incLogEvent{}}
	c.add(incLogLevelSQL, "t1", incLogPhaseScan, "q", "SELECT 1", 10, "w0", 3)
	c.add(incLogLevelInfo, "t1", incLogPhaseDone, "d", "", 10, "w1", 9)
	s.archiveIncRunLogs("jobA", "archived1", incRunStatusCompleted, c)

	raw, err := os.ReadFile(s.incrementalLogFile("jobA", "archived1"))
	if err != nil {
		t.Fatalf("archive file must exist: %v", err)
	}
	var arch incLogArchive
	if err := json.Unmarshal(raw, &arch); err != nil {
		t.Fatal(err)
	}
	if arch.Status != incRunStatusCompleted || len(arch.Events) != 2 || arch.Events[1].Seq != 2 {
		t.Fatalf("archive round-trip: %+v", arch)
	}

	// Endpoint serves the archive once no live collector exists.
	w, req := doReq("GET", "/api/v1/incremental/jobs/jobA/runs/archived1/logs?after=1", "")
	req = withChiParams(req, "id", "jobA", "run_id", "archived1")
	s.handleIncrementalRunLogs(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("logs endpoint: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Events  []incLogEvent `json:"events"`
		Status  string        `json:"status"`
		Dropped int64         `json:"dropped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != incRunStatusCompleted || len(resp.Events) != 1 || resp.Events[0].Seq != 2 {
		t.Fatalf("after=1 must filter to seq 2: %+v", resp)
	}

	// Live collector takes precedence: same runID serves from memory as running.
	live := incLogStartRun("archived1")
	defer incLogEndRun("archived1")
	live.add(incLogLevelInfo, "", incLogPhaseStart, "live", "", 0, "", 0)
	w, req = doReq("GET", "/api/v1/incremental/jobs/jobA/runs/archived1/logs", "")
	req = withChiParams(req, "id", "jobA", "run_id", "archived1")
	s.handleIncrementalRunLogs(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != incRunStatusRunning || len(resp.Events) != 1 {
		t.Fatalf("live collector must serve as running: %+v", resp)
	}
}

func TestIncLogEndpointNotFoundFaces(t *testing.T) {
	s, _ := newTestServer(t)
	seedLogJob(t, s, "jobB")

	cases := []struct {
		job, run, after string
		wantCode        int
	}{
		{"nope", "r", "", http.StatusNotFound},         // unknown job
		{"jobB", "nosuchrun", "", http.StatusNotFound}, // unknown run (no archive)
		{"jobB", "r", "-1", http.StatusBadRequest},     // invalid after
		{"jobB", "r", "abc", http.StatusBadRequest},    // non-numeric after
	}
	for _, tc := range cases {
		w, req := doReq("GET", "/api/v1/incremental/jobs/"+tc.job+"/runs/"+tc.run+"/logs?after="+tc.after, "")
		req = withChiParams(req, "id", tc.job, "run_id", tc.run)
		s.handleIncrementalRunLogs(w, req)
		if w.Code != tc.wantCode {
			t.Fatalf("job=%s run=%s after=%s: got %d want %d (%s)",
				tc.job, tc.run, tc.after, w.Code, tc.wantCode, w.Body.String())
		}
	}
}

func TestIncLogInterruptedRunArchive(t *testing.T) {
	s, _ := newTestServer(t)
	incMu.Lock()
	list := []incJob{{
		ID: "jobC", Name: "int", States: map[string]*incTableState{},
		History: []incRunRecord{{RunID: "deadrun", StartedAt: time.Now(), Status: incRunStatusRunning}},
	}}
	if err := s.saveIncrementalJobs(list); err != nil {
		incMu.Unlock()
		t.Fatal(err)
	}
	incMu.Unlock()

	s.markInterruptedIncrementalRuns()

	raw, err := os.ReadFile(s.incrementalLogFile("jobC", "deadrun"))
	if err != nil {
		t.Fatalf("interrupted run must leave an archive: %v", err)
	}
	var arch incLogArchive
	if err := json.Unmarshal(raw, &arch); err != nil {
		t.Fatal(err)
	}
	if len(arch.Events) != 1 || arch.Events[0].Phase != incLogPhaseInterrupted || arch.Events[0].Seq != 1 {
		t.Fatalf("interrupted archive must be a single seq-1 event: %+v", arch)
	}
	if arch.Status != incRunStatusFailed {
		t.Fatalf("interrupted archive status: %s", arch.Status)
	}
}

func TestIncLogDeleteJobSweepsDir(t *testing.T) {
	s, _ := newTestServer(t)
	if err := os.MkdirAll(filepath.Join(s.incrementalLogsRoot(), "gonejob"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.sweepIncrementalLogDirs()
	if _, err := os.Stat(filepath.Join(s.incrementalLogsRoot(), "gonejob")); !os.IsNotExist(err) {
		t.Fatal("orphan log dir must be swept at startup")
	}
}

func TestIncLogCollectorIsolation(t *testing.T) {
	a, b := &incLogCollector{events: []incLogEvent{}}, &incLogCollector{events: []incLogEvent{}}
	var wg sync.WaitGroup
	for _, c := range []*incLogCollector{a, a, b, b} {
		wg.Add(1)
		go func(c *incLogCollector) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				c.add(incLogLevelInfo, "", incLogPhaseScan, "", "", 0, "", 0)
			}
		}(c)
	}
	wg.Wait()
	ea, _, sa := a.snapshot(0)
	eb, _, sb := b.snapshot(0)
	if sa != 400 || sb != 400 || len(ea) != 400 || len(eb) != 400 {
		t.Fatalf("collectors must be isolated per run: a=%d/%d b=%d/%d", len(ea), sa, len(eb), sb)
	}
	prev := int64(0)
	for _, e := range ea {
		if e.Seq <= prev {
			t.Fatalf("seq must stay monotonic under concurrency: %+v", e)
		}
		prev = e.Seq
	}
}

// Nil collector safety: the engine call sites pass lg straight through.
func TestIncLogNilCollectorSafe(t *testing.T) {
	var c *incLogCollector
	c.add(incLogLevelInfo, "", incLogPhaseStart, "nil-safe", "", 0, "", 0)
	if _, dropped, seq := c.snapshot(0); dropped != 0 || seq != 0 {
		t.Fatal("nil collector must be a no-op")
	}
}
