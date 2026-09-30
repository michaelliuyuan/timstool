package webapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// FEAT-INC-PARALLEL anchors: parallelism validation/normalization, worker-pool
// concurrency bounds, order stability, no-deadline run context, per-worker
// panic recovery, concurrent collector seq monotonicity, and rerun
// convergence after partial failure.

// A1: parallelism round-trip (absent → 4, explicit accepted), out-of-range
// 400 on create and update, and legacy on-disk jobs normalize to the default.
func TestIncParallelismValidationAndNormalize(t *testing.T) {
	if got := normalizeIncParallelism(0); got != incParallelismDefault {
		t.Fatalf("0 must normalize to default %d, got %d", incParallelismDefault, got)
	}
	if got := normalizeIncParallelism(-3); got != incParallelismDefault {
		t.Fatalf("negative must normalize to default, got %d", got)
	}
	if got := normalizeIncParallelism(99); got != incParallelismMax {
		t.Fatalf("over-max must clamp to %d, got %d", incParallelismMax, got)
	}
	if got := normalizeIncParallelism(7); got != 7 {
		t.Fatalf("in-range must pass through, got %d", got)
	}

	s, _ := newTestServer(t)
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "pl-src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "pl-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)

	body := func(par string) string {
		if par == "" {
			return `{"name": "pj", "source_ref": "` + srcID + `", "target_ref": "` + tgtID +
				`", "batch_size": 500, "conflict_strategy": "replace", "tables": [{"table": "users", "watermark_column": "update_time"}]}`
		}
		return `{"name": "pj", "source_ref": "` + srcID + `", "target_ref": "` + tgtID +
			`", "batch_size": 500, "parallelism": ` + par + `, "conflict_strategy": "replace", "tables": [{"table": "users", "watermark_column": "update_time"}]}`
	}

	// Out-of-range explicit values → 400 on create.
	for _, bad := range []string{"-1", "17", "100"} {
		w, req = doReq("POST", "/api/v1/incremental/jobs", body(bad))
		s.handleCreateIncrementalJob(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "parallelism") {
			t.Fatalf("parallelism %s must be 400: %d %s", bad, w.Code, w.Body.String())
		}
	}
	// Absent → server default 4; explicit 9 round-trips.
	w, req = doReq("POST", "/api/v1/incremental/jobs", body(""))
	s.handleCreateIncrementalJob(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if got := dsBody(t, w)["parallelism"].(float64); got != 4 {
		t.Fatalf("absent parallelism must default to 4, got %v", got)
	}
	id := dsBody(t, w)["id"].(string)
	w, req = doReq("PUT", "/api/v1/incremental/jobs/"+id, body("9"))
	req = withChiParam(req, "id", id)
	s.handleUpdateIncrementalJob(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if got := dsBody(t, w)["parallelism"].(float64); got != 9 {
		t.Fatalf("parallelism=9 must round-trip, got %v", got)
	}

	// Legacy job on disk without the field → loaded as 4 (D2).
	incMu.Lock()
	list := s.loadIncrementalJobs()
	for i := range list {
		if list[i].ID == id {
			list[i].Parallelism = 0
		}
	}
	if err := s.saveIncrementalJobs(list); err != nil {
		t.Fatal(err)
	}
	list = s.loadIncrementalJobs()
	incMu.Unlock()
	for _, e := range list {
		if e.ID == id && e.Parallelism != incParallelismDefault {
			t.Fatalf("legacy job must load with parallelism=%d, got %d", incParallelismDefault, e.Parallelism)
		}
	}
}

// A2: the pool's in-flight concurrency never exceeds the configured worker
// count, and with parallelism=2 it genuinely reaches 2 (not stuck at 1).
func TestIncPoolConcurrencyBound(t *testing.T) {
	for _, tc := range []struct {
		workers, tables int
	}{
		{2, 8}, {4, 6}, {3, 2}, // workers > tables case included
	} {
		var inFlight, peak int64
		var mu sync.Mutex
		tasks := make([]incRunTableTask, tc.tables)
		for i := range tasks {
			tasks[i] = incRunTableTask{t: incTableConfig{Table: "t" + string(rune('a'+i))}, st: &incTableState{}}
		}
		gate := make(chan struct{})
		var gateOnce sync.Once
		release := func() { gateOnce.Do(func() { close(gate) }) }
		var started int64
		results := incRunTableTasks(tasks, tc.workers, func(t incRunTableTask) incTableResult {
			n := atomic.AddInt64(&inFlight, 1)
			mu.Lock()
			if n > peak {
				peak = n
			}
			mu.Unlock()
			if atomic.AddInt64(&started, 1) >= int64(tc.workers) && tc.workers < tc.tables {
				release() // release the first `workers` tasks together (double-close safe)
			}
			if tc.workers < tc.tables {
				<-gate
			}
			atomic.AddInt64(&inFlight, -1)
			return incTableResult{Table: t.t.Table, Rows: 1}
		}, func(t incRunTableTask, dur time.Duration, r interface{}) incTableResult {
			return incTableResult{Table: t.t.Table, Error: "panic"}
		})
		if len(results) != tc.tables {
			t.Fatalf("workers=%d tables=%d: lost results (%d)", tc.workers, tc.tables, len(results))
		}
		if peak > int64(tc.workers) {
			t.Fatalf("workers=%d: peak in-flight %d exceeded bound", tc.workers, peak)
		}
		if tc.workers < tc.tables && peak < int64(tc.workers) {
			t.Fatalf("workers=%d tables=%d: pool never reached parallelism (peak %d)", tc.workers, tc.tables, peak)
		}
	}
}

// A4: results come back in original table order regardless of completion
// order, and all results are present (run with -race in CI).
func TestIncPoolOrderStable(t *testing.T) {
	tasks := make([]incRunTableTask, 12)
	names := make([]string, 12)
	for i := range tasks {
		names[i] = "table_" + string(rune('A'+i))
		tasks[i] = incRunTableTask{t: incTableConfig{Table: names[i]}, st: &incTableState{}}
	}
	// Later tables finish first: completion order is inverted vs task order.
	var wgCount int64
	results := incRunTableTasks(tasks, 6, func(t incRunTableTask) incTableResult {
		idx := 0
		for i, n := range names {
			if n == t.t.Table {
				idx = i
			}
		}
		time.Sleep(time.Duration(12-idx) * time.Millisecond) // inverted latency
		atomic.AddInt64(&wgCount, 1)
		return incTableResult{Table: t.t.Table, Rows: int64(idx + 1)}
	}, func(t incRunTableTask, dur time.Duration, r interface{}) incTableResult {
		return incTableResult{Table: t.t.Table, Error: "panic"}
	})
	if len(results) != 12 || atomic.LoadInt64(&wgCount) != 12 {
		t.Fatalf("results=%d executed=%d, want 12/12", len(results), wgCount)
	}
	for i, res := range results {
		if res.Table != names[i] {
			t.Fatalf("order unstable at %d: got %s want %s", i, res.Table, names[i])
		}
	}
}

// A8: a panicking worker marks only that table failed; the pool, the other
// tables and the panicRes callback all complete (per-worker recover).
func TestIncPoolWorkerPanicIsolated(t *testing.T) {
	tasks := []incRunTableTask{
		{t: incTableConfig{Table: "ok1"}, st: &incTableState{}},
		{t: incTableConfig{Table: "boom"}, st: &incTableState{}},
		{t: incTableConfig{Table: "ok2"}, st: &incTableState{}},
		{t: incTableConfig{Table: "ok3"}, st: &incTableState{}},
	}
	results := incRunTableTasks(tasks, 3, func(t incRunTableTask) incTableResult {
		if t.t.Table == "boom" {
			panic("synthetic table sync panic")
		}
		return incTableResult{Table: t.t.Table, Rows: 5}
	}, func(t incRunTableTask, dur time.Duration, r interface{}) incTableResult {
		t.st.Failed = "内部错误：表同步异常终止"
		return incTableResult{Table: t.t.Table, Error: "内部错误：表同步异常终止"}
	})
	wantErrs := map[string]bool{"ok1": false, "boom": true, "ok2": false, "ok3": false}
	for _, res := range results {
		if (res.Error != "") != wantErrs[res.Table] {
			t.Fatalf("table %s error state wrong: %+v", res.Table, res)
		}
	}
	if tasks[1].st.Failed == "" {
		t.Fatal("panic must record st.Failed on the panicked table")
	}
	for _, i := range []int{0, 2, 3} {
		if tasks[i].st.Failed != "" {
			t.Fatalf("healthy table %s must not be marked failed", tasks[i].t.Table)
		}
	}
}

// A5: concurrent collector adds keep seq strictly monotonic (the pool fans in
// log events from every worker).
func TestIncPoolConcurrentLogSeqMonotonic(t *testing.T) {
	c := &incLogCollector{}
	tasks := make([]incRunTableTask, 8)
	for i := range tasks {
		tasks[i] = incRunTableTask{t: incTableConfig{Table: "t" + string(rune('a'+i))}, st: &incTableState{}}
	}
	incRunTableTasks(tasks, 8, func(t incRunTableTask) incTableResult {
		for j := 0; j < 100; j++ {
			c.add(incLogLevelInfo, t.t.Table, incLogPhaseScan, "evt", "", 0, "", 0)
		}
		return incTableResult{Table: t.t.Table}
	}, func(t incRunTableTask, dur time.Duration, r interface{}) incTableResult {
		return incTableResult{Table: t.t.Table, Error: "panic"}
	})
	events, dropped, lastSeq := c.snapshot(0)
	if dropped != 0 {
		t.Fatalf("800 events must fit cap 1000, dropped=%d", dropped)
	}
	if len(events) != 800 || lastSeq != 800 {
		t.Fatalf("events=%d lastSeq=%d, want 800/800", len(events), lastSeq)
	}
	prev := int64(0)
	for _, e := range events {
		if e.Seq <= prev {
			t.Fatalf("seq must be strictly monotonic: %d after %d", e.Seq, prev)
		}
		prev = e.Seq
	}
}

// A7: after a partially failed parallel run, a rerun only rescans the failed
// table — successful tables keep their watermark and cost 0 rows.
func TestIncPoolRerunConvergesAfterPartialFailure(t *testing.T) {
	tables := []string{"a", "b", "c", "d"}
	states := map[string]*incTableState{}
	failTable := "c"

	runOnce := func() (rows map[string]int64, errs map[string]bool) {
		rows, errs = map[string]int64{}, map[string]bool{}
		var mu sync.Mutex
		tasks := make([]incRunTableTask, len(tables))
		for i, name := range tables {
			st := states[name]
			if st == nil {
				st = &incTableState{}
				states[name] = st
			}
			tasks[i] = incRunTableTask{t: incTableConfig{Table: name}, st: st}
		}
		results := incRunTableTasks(tasks, 4, func(t incRunTableTask) incTableResult {
			// Model: a table with an advanced watermark is already synced —
			// 0 rows, instant; a fresh one syncs 10 rows and advances. One
			// designated table fails without advancing.
			if t.st.LastWatermark != "" {
				return incTableResult{Table: t.t.Table, Rows: 0, ToWM: t.st.LastWatermark}
			}
			if t.t.Table == failTable && t.st.Failed != "done" {
				return incTableResult{Table: t.t.Table, Error: "synthetic failure"}
			}
			t.st.LastWatermark = "w9"
			return incTableResult{Table: t.t.Table, Rows: 10, ToWM: "w9"}
		}, func(t incRunTableTask, dur time.Duration, r interface{}) incTableResult {
			return incTableResult{Table: t.t.Table, Error: "panic"}
		})
		for _, res := range results {
			mu.Lock()
			rows[res.Table] = res.Rows
			errs[res.Table] = res.Error != ""
			mu.Unlock()
		}
		return rows, errs
	}

	rows, errs := runOnce()
	if errs["a"] || errs["b"] || errs["d"] || !errs["c"] {
		t.Fatalf("first run: expected only c to fail: %v", errs)
	}
	if rows["a"] != 10 || rows["c"] != 0 {
		t.Fatalf("first run rows wrong: %v", rows)
	}

	// Rerun: only c rescans; healthy tables 0 rows, watermarks unchanged.
	failTable = "" // c's failure is transient
	states["c"].Failed = ""
	rows, errs = runOnce()
	for _, e := range errs {
		if e {
			t.Fatalf("second run must be all-green: %v", errs)
		}
	}
	if rows["a"] != 0 || rows["b"] != 0 || rows["d"] != 0 {
		t.Fatalf("healthy tables must rescan 0 rows on rerun: %v", rows)
	}
	if rows["c"] != 10 {
		t.Fatalf("failed table must rescan on rerun: %v", rows)
	}
}

// A3: the background run context carries no deadline (the 10-minute
// run-level cap is gone). Broken-datasource run completes fast; the seam
// captures the ctx the engine actually received.
func TestIncRunContextHasNoDeadline(t *testing.T) {
	s, _ := newTestServer(t)
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "nd-src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "nd-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/incremental/jobs", `{"name": "ndj", "source_ref": "`+srcID+
		`", "target_ref": "`+tgtID+`", "batch_size": 500, "conflict_strategy": "replace", "tables": [{"table": "users", "watermark_column": "update_time"}]}`)
	s.handleCreateIncrementalJob(w, req)
	id := dsBody(t, w)["id"].(string)

	// Break the source ref so the run fails fast without a live DB.
	w, req = doReq("DELETE", "/api/v1/datasources/"+srcID, "")
	req = withChiParam(req, "id", srcID)
	s.handleDeleteDataSource(w, req)

	var gotCtx atomic.Value
	incRunCtxHook = func(c context.Context) { gotCtx.Store(c) }
	defer func() { incRunCtxHook = nil }()

	w, req = doReq("POST", "/api/v1/incremental/jobs/"+id+"/run", "")
	req = withChiParam(req, "id", id)
	s.handleRunIncrementalJob(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("run must be 202: %d %s", w.Code, w.Body.String())
	}
	rec := waitForIncRun(t, s, id)
	if rec.Status != incRunStatusCompleted {
		t.Fatalf("run must complete: %+v", rec)
	}
	ctx, ok := gotCtx.Load().(context.Context)
	if !ok || ctx == nil {
		t.Fatal("ctx seam never captured a context")
	}
	if dl, ok := ctx.Deadline(); ok {
		t.Fatalf("run context must carry no deadline, got %v", dl)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("run context must not be pre-cancelled: %v", err)
	}
}
