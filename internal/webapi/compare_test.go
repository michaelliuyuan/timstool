package webapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Standalone compare tasks (独立数据比对): creation validation, persistence
// with password redaction, list/get/report roundtrip, concurrency 409, and
// delete rules. Handlers are invoked directly (same pattern as the migration
// options tests); the actual validation run needs live DBs and is covered by
// remote verification.

func compareURL(id string) string { return "/api/v1/compare/tasks/" + id }

func TestCompareOptions_RoundtripAndRedaction(t *testing.T) {
	s, _ := newTestServer(t)

	// PUT a full profile including passwords; they must be ignored.
	w, req := doReq("PUT", "/api/v1/compare/options",
		`{"source_type":"postgres","source":{"host":"pg","port":5433,"user":"u","password":"secret1","database":"d","schema":"public","sslmode":"disable"},
		  "target":{"host":"tidb","port":4000,"user":"root","password":"secret2","database":"d"},
		  "mode":"checksum","sample_ratio":0.1,"checksum_chunk_size":20000,"checksum_parallel":4,"parallel":2}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put status = %d body=%s", w.Code, w.Body.String())
	}

	// On-disk file must not contain either password.
	raw, err := os.ReadFile(s.compareOptionsFile())
	if err != nil {
		t.Fatalf("compare-options.json missing: %v", err)
	}
	if strings.Contains(string(raw), "secret1") || strings.Contains(string(raw), "secret2") {
		t.Fatalf("password persisted in compare-options.json: %s", raw)
	}

	// GET returns the saved profile with empty passwords.
	w, req = doReq("GET", "/api/v1/compare/options", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d", w.Code)
	}
	var opts compareOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if opts.Source.Host != "pg" || opts.Source.Port != 5433 || opts.Source.Password != "" {
		t.Fatalf("bad source roundtrip: %+v", opts.Source)
	}
	if opts.Target.Host != "tidb" || opts.Target.Password != "" {
		t.Fatalf("bad target roundtrip: %+v", opts.Target)
	}
	if opts.SourceType != "postgres" || opts.Mode != "checksum" || opts.SampleRatio != 0.1 ||
		opts.ChecksumChunkSize != 20000 || opts.ChecksumParallel != 4 || opts.Parallel != 2 {
		t.Fatalf("bad prefs roundtrip: %+v", opts)
	}
	if strings.Contains(w.Body.String(), "secret1") || strings.Contains(w.Body.String(), "secret2") {
		t.Fatalf("password leaked in GET response: %s", w.Body.String())
	}
}

func TestCompareOptions_PartialMerge(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("PUT", "/api/v1/compare/options",
		`{"source":{"host":"pg","port":5432},"target":{"host":"tidb","port":4000},"mode":"sample"}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put1 status = %d", w.Code)
	}

	// Partial PUT: only source.host present; everything else keeps saved value.
	w, req = doReq("PUT", "/api/v1/compare/options", `{"source":{"host":"pg2"}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put2 status = %d body=%s", w.Code, w.Body.String())
	}

	opts := s.loadCompareOptions()
	if opts.Source.Host != "pg2" {
		t.Fatalf("present field not applied: %+v", opts.Source)
	}
	if opts.Source.Port != 5432 || opts.Target.Host != "tidb" || opts.Mode != "sample" {
		t.Fatalf("absent fields not preserved: %+v", opts)
	}
}

func TestCompareOptions_LegacyFileAndValidation(t *testing.T) {
	s, _ := newTestServer(t)

	// Old/partial file (e.g. only source) loads fine.
	if err := os.WriteFile(s.compareOptionsFile(), []byte(`{"source":{"host":"old"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := s.loadCompareOptions()
	if opts.Source.Host != "old" {
		t.Fatalf("legacy file not loaded: %+v", opts)
	}

	// Corrupted file falls back to zero value (next PUT rewrites it).
	if err := os.WriteFile(s.compareOptionsFile(), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.loadCompareOptions().Source.Host; got != "" {
		t.Fatalf("corrupted file not ignored: %q", got)
	}

	// Invalid mode / sample_ratio rejected.
	cases := []struct{ name, body string }{
		{"bad mode", `{"mode":"full"}`},
		{"bad ratio", `{"sample_ratio":1.5}`},
	}
	for _, c := range cases {
		w, req := doReq("PUT", "/api/v1/compare/options", c.body)
		s.router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

func TestCompareTask_Validation(t *testing.T) {
	s, _ := newTestServer(t)

	cases := []struct {
		name string
		body string
		code int
	}{
		{"missing source host", `{"target":{"host":"t"}}`, http.StatusBadRequest},
		{"missing target host", `{"source":{"host":"s"}}`, http.StatusBadRequest},
		{"bad mode", `{"source":{"host":"s"},"target":{"host":"t"},"mode":"full"}`, http.StatusBadRequest},
		{"bad body", `{`, http.StatusBadRequest},
		// F-12: the create path must gate sample_ratio exactly like PUT
		// options — 100 used to sail through and panic the sampler.
		{"ratio 100", `{"source":{"host":"s"},"target":{"host":"t"},"sample_ratio":100}`, http.StatusBadRequest},
		{"ratio negative", `{"source":{"host":"s"},"target":{"host":"t"},"sample_ratio":-0.5}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		w, req := doReq("POST", "/api/v1/compare/tasks", c.body)
		s.handleCreateCompare(w, req)
		if w.Code != c.code {
			t.Fatalf("%s: status = %d body=%s", c.name, w.Code, w.Body.String())
		}
	}

	// F-12 boundary: ratio=1 is legal and must clear the gate (the task then
	// fails asynchronously at connect time — only the 400 gate is pinned).
	w, req := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s","type":"postgres","port":5432},"target":{"host":"t"},"sample_ratio":1}`)
	s2, _ := newTestServer(t)
	s2.handleCreateCompare(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("ratio 1: status = %d body=%s", w.Code, w.Body.String())
	}
}

// MS-08 absorption anchor: the inline source guard reads the capability via
// the NORMALIZED type (C1 shape 2 - empty defaults to postgres and passes);
// mysql passes since MS-08, unsupported kinds still 400.
func TestCompareTask_SourceTypeGuardCapability(t *testing.T) {
	s, _ := newTestServer(t)

	// Unsupported kinds are rejected with the updated bilingual message.
	for _, bad := range []string{`"tidb"`, `"oracle"`, `"cockroachdb"`} {
		body := `{"source":{"host":"s","type":` + bad + `},"target":{"host":"t"}}`
		w, req := doReq("POST", "/api/v1/compare/tasks", body)
		s.handleCreateCompare(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("type %s: status = %d body=%s", bad, w.Code, w.Body.String())
		}
		if msg := w.Body.String(); !strings.Contains(msg, "postgres") || !strings.Contains(msg, "mysql") {
			t.Fatalf("type %s: 400 body must name postgres/mysql: %s", bad, msg)
		}
	}

	// mysql + empty (normalized postgres) both PASS the gate; the created
	// tasks fail later at connection time (no live DB in unit tests), which
	// the create handler reports asynchronously - the 400 gate itself is
	// what this anchor pins. A fresh server per case avoids the single-
	// running-task mutex (409) from the previous case's async run.
	for _, ok := range []string{`"mysql"`, `""`} {
		s2, _ := newTestServer(t)
		body := `{"source":{"host":"s","type":` + ok + `,"port":3306},"target":{"host":"t"}}`
		w, req := doReq("POST", "/api/v1/compare/tasks", body)
		s2.handleCreateCompare(w, req)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("type %s: unexpected gate status %d body=%s", ok, w.Code, w.Body.String())
		}
		if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "source.type") {
			t.Fatalf("type %s: wrongly 400'd at the source gate: %s", ok, w.Body.String())
		}
	}
}

func TestCompareTask_PersistRedactsPasswords(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/compare/tasks",
		`{"name":"cmp","source":{"host":"s","port":5432,"user":"u","password":"secret1","database":"d"},
		  "target":{"host":"t","port":4000,"user":"root","password":"secret2","database":"d"},
		  "mode":"checksum","tables":["a","b"],"parallel":2}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	var created CompareTask
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}

	// M1: the HTTP response must not echo passwords either.
	if strings.Contains(w.Body.String(), "secret1") || strings.Contains(w.Body.String(), "secret2") {
		t.Fatalf("password leaked in create response: %s", w.Body.String())
	}

	// Wait for the background run to finish (unreachable DBs -> failed) so the
	// file is stable, then assert no password ever hit the disk.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.compare.mu.Lock()
		idle := s.compare.cancel == nil
		s.compare.mu.Unlock()
		if idle {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// On-disk task.json must not contain either password.
	raw, err := os.ReadFile(filepath.Join(s.compareDir(created.ID), "task.json"))
	if err != nil {
		t.Fatalf("task.json missing: %v", err)
	}
	if strings.Contains(string(raw), "secret1") || strings.Contains(string(raw), "secret2") {
		t.Fatalf("password leaked to disk: %s", raw)
	}
}

// A1: a busy slot no longer 409s — the second POST gets 202 + a queued
// task; task.json on disk IS the queue record (status=queued, no started_at).
// The old 409 contract is dead by design.
func TestCompareTask_BusySlotQueues(t *testing.T) {
	s, _ := newTestServer(t)
	s.compare.cancel = func() {}
	s.compare.runningID = "other"
	defer func() { s.compare.cancel = nil; s.compare.runningID = "" }()

	w, req := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s","user":"u","password":"secret1"},"target":{"host":"t","password":"secret2"}}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 queued while running, got %d body=%s", w.Code, w.Body.String())
	}
	var queued CompareTask
	if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil {
		t.Fatalf("decode queued: %v", err)
	}
	if queued.Status != CompareStatusQueued {
		t.Fatalf("queued status = %q", queued.Status)
	}
	if queued.StartedAt != nil {
		t.Fatalf("queued task must not carry started_at: %+v", queued.StartedAt)
	}
	// M1: the queued response is password-redacted, and the on-disk queue
	// record must be too (stash lives in memory only).
	if strings.Contains(w.Body.String(), "secret1") || strings.Contains(w.Body.String(), "secret2") {
		t.Fatalf("password leaked in queued response: %s", w.Body.String())
	}
	t2, err := s.loadCompareTask(queued.ID)
	if err != nil || t2 == nil {
		t.Fatalf("reload queued task: %v", err)
	}
	if t2.Status != CompareStatusQueued {
		t.Fatalf("on-disk queued status = %q", t2.Status)
	}
	raw, err := os.ReadFile(filepath.Join(s.compareDir(queued.ID), "task.json"))
	if err != nil {
		t.Fatalf("task.json missing: %v", err)
	}
	if strings.Contains(string(raw), "secret1") || strings.Contains(string(raw), "secret2") {
		t.Fatalf("password leaked to disk: %s", raw)
	}
	// The submitted passwords must be stashed in memory for the dispatch.
	if c, ok := s.compare.queuedCreds[queued.ID]; !ok || c.sourcePassword != "secret1" || c.targetPassword != "secret2" {
		t.Fatalf("queued credentials not stashed: %+v", s.compare.queuedCreds)
	}
}

// A1 fairness: while ANY queued task waits, a new POST also queues (202)
// even though the slot itself is free — no line jumping.
func TestCompareTask_PostQueuesBehindWaitingQueue(t *testing.T) {
	s, _ := newTestServer(t)
	s.compare.cancel = func() {}
	s.compare.runningID = "other"
	w, req := doReq("POST", "/api/v1/compare/tasks", `{"source":{"host":"s"},"target":{"host":"t"}}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first post status = %d body=%s", w.Code, w.Body.String())
	}

	// Slot frees (no dispatch fires in this hand-made scenario) — a new POST
	// must STILL queue behind the waiting task.
	s.compare.cancel = nil
	s.compare.runningID = ""
	w, req = doReq("POST", "/api/v1/compare/tasks", `{"source":{"host":"s2"},"target":{"host":"t2"}}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("second post status = %d body=%s", w.Code, w.Body.String())
	}
	var second CompareTask
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if second.Status != CompareStatusQueued {
		t.Fatalf("second status = %q, want queued (FIFO fairness)", second.Status)
	}
	// The admission decision is what this pins. The slot really is free here,
	// so the self-heal drain now hands it to the FIFO head — wait until the
	// queue is drained so the temp-dir cleanup never races a goroutine.
	compareWaitQueueDrained(t, s)
}

// compareWaitQueueDrained polls until no queued task remains and the running
// slot is idle (bounded), so a test's temp-dir cleanup cannot race a dispatch.
func compareWaitQueueDrained(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s.compare.mu.Lock()
		idle := s.compare.cancel == nil
		s.compare.mu.Unlock()
		if idle && s.earliestQueuedCompare() == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("compare queue never drained")
}

// A1: queued tasks can be cancelled (removed from the queue at once) and
// deleted; cancelling a non-queued, non-running task still 409s.
func TestCompareTask_QueuedCancelAndDelete(t *testing.T) {
	s, _ := newTestServer(t)
	s.compare.cancel = func() {}
	s.compare.runningID = "other"
	defer func() { s.compare.cancel = nil; s.compare.runningID = "" }()

	w, req := doReq("POST", "/api/v1/compare/tasks", `{"source":{"host":"s"},"target":{"host":"t"}}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("queue status = %d body=%s", w.Code, w.Body.String())
	}
	var queued CompareTask
	if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil {
		t.Fatalf("decode queued: %v", err)
	}

	// Cancel the queued task → immediate cancelled (not "cancelling").
	// (Routed through the real router: the handler reads {compareID} via chi.)
	w, req = doReq("POST", compareURL(queued.ID)+"/cancel", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"cancelled"`) {
		t.Fatalf("queued cancel status=%d body=%s", w.Code, w.Body.String())
	}
	t2, err := s.loadCompareTask(queued.ID)
	if err != nil || t2 == nil {
		t.Fatalf("reload cancelled: %v", err)
	}
	if t2.Status != CompareStatusCancelled || t2.FinishedAt == nil {
		t.Fatalf("queued task not cancelled on disk: %+v", t2)
	}
	if _, ok := s.compare.queuedCreds[queued.ID]; ok {
		t.Fatalf("stashed credentials survive queued cancel")
	}

	// Cancel again → 409 (neither running nor queued).
	w, req = doReq("POST", compareURL(queued.ID)+"/cancel", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("double cancel status = %d", w.Code)
	}

	// Delete a fresh queued task → 200, dir gone (running still blocks).
	w, req = doReq("POST", "/api/v1/compare/tasks", `{"source":{"host":"s"},"target":{"host":"t"}}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("requeue status = %d body=%s", w.Code, w.Body.String())
	}
	var q2 CompareTask
	if err := json.Unmarshal(w.Body.Bytes(), &q2); err != nil {
		t.Fatalf("decode q2: %v", err)
	}
	w, req = doReq("DELETE", compareURL(q2.ID), "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("queued delete status = %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(s.compareDir(q2.ID)); !os.IsNotExist(err) {
		t.Fatalf("queued compare dir still exists after delete")
	}
	if _, ok := s.compare.queuedCreds[q2.ID]; ok {
		t.Fatalf("stashed credentials survive queued delete")
	}
}

// compareWaitSlotIdle polls until the running slot is free (bounded).
func compareWaitSlotIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s.compare.mu.Lock()
		idle := s.compare.cancel == nil
		s.compare.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("compare slot never went idle")
}

// A1 end-to-end: two tasks queued behind a fake slot are dispatched FIFO —
// the inline credentials ride the in-memory stash — and the first task's
// real finish() hands the slot to the second automatically.
func TestCompareTask_FifoAutoDispatch(t *testing.T) {
	s, _ := newTestServer(t)
	s.compare.cancel = func() {}
	s.compare.runningID = "other"

	for _, name := range []string{"first", "second"} {
		w, req := doReq("POST", "/api/v1/compare/tasks",
			`{"name":"`+name+`","source":{"host":"127.0.0.1","port":1},"target":{"host":"127.0.0.1","port":2}}`)
		s.handleCreateCompare(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("%s: queue status = %d body=%s", name, w.Code, w.Body.String())
		}
	}

	// Release the fake slot and kick the first dispatch by hand (in
	// production the running task's finish() does exactly this).
	s.compare.cancel = nil
	s.compare.runningID = ""
	s.dispatchNextCompare()

	// Both run sequentially against unreachable ports and fail fast; the
	// first finish dispatches the second without any external nudging.
	compareWaitSlotIdle(t, s)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		list := s.listCompareTasksForTest()
		var first, second *CompareTask
		for _, tsk := range list {
			if tsk.Name == "first" {
				first = tsk
			}
			if tsk.Name == "second" {
				second = tsk
			}
		}
		if first != nil && second != nil &&
			first.Status != CompareStatusQueued && first.Status != CompareStatusRunning &&
			second.Status != CompareStatusQueued && second.Status != CompareStatusRunning {
			if second.StartedAt == nil {
				t.Fatalf("second task never dispatched: %+v", second)
			}
			if first.StartedAt == nil || second.StartedAt.Before(*first.StartedAt) {
				t.Fatalf("FIFO order violated: first=%v second=%v", first.StartedAt, second.StartedAt)
			}
			if second.Status != CompareStatusFailed {
				t.Fatalf("second task status = %q, want failed (unreachable DB)", second.Status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("queued tasks never drained: %+v", s.listCompareTasksForTest())
}

// listCompareTasksForTest loads every on-disk compare task (test helper).
func (s *Server) listCompareTasksForTest() []*CompareTask {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "compare"))
	if err != nil {
		return nil
	}
	var out []*CompareTask
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if t, err := s.loadCompareTask(e.Name()); err == nil && t != nil {
			out = append(out, t)
		}
	}
	return out
}

func TestCompareTask_ListGetReportDelete(t *testing.T) {
	s, _ := newTestServer(t)

	// Seed a finished compare task + report on disk directly. The id must
	// satisfy the MC-17 ⑥a allow-list (^[0-9a-f]{8}$) — these calls go
	// through the real router, which 404s non-conforming ids before disk.
	dir := s.compareDir("a1b2c3d4")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	taskJSON := `{"id":"a1b2c3d4","name":"seed","status":"completed",
		"source":{"host":"s","database":"d"},"target":{"host":"t","database":"d"},
		"mode":"quick","tables_done":3,"tables_total":3,
		"created_at":"` + now + `"}`
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(taskJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	reportJSON := `{"tool":"timstool-migrator","overall_status":"pass","tables":[]}`
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte(reportJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// List contains the seeded task.
	w, req := doReq("GET", "/api/v1/compare/tasks", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var list []*CompareTask
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "a1b2c3d4" || list[0].TablesTotal != 3 {
		t.Fatalf("unexpected list: %+v", list)
	}

	// Get detail.
	w, req = doReq("GET", compareURL("a1b2c3d4"), "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d", w.Code)
	}

	// Report raw roundtrip (MC-16: explicit format=json — the no-param
	// default is now the server-rendered html family report).
	w, req = doReq("GET", compareURL("a1b2c3d4")+"/report?format=json", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"overall_status":"pass"`) {
		t.Fatalf("report status=%d body=%s", w.Code, w.Body.String())
	}

	// Unknown id → 404.
	w, req = doReq("GET", compareURL("nope"), "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}

	// Delete: not running → allowed and removed.
	w, req = doReq("DELETE", compareURL("a1b2c3d4"), "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("compare dir still exists after delete")
	}

	// Delete while running → 409.
	s.compare.runningID = "a1b2c3d4"
	defer func() { s.compare.runningID = "" }()
	w, req = doReq("DELETE", compareURL("a1b2c3d4"), "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for running delete, got %d", w.Code)
	}
}

func TestCompareTask_BuildRunParamsSampleRatio(t *testing.T) {
	// L1: sample ratio must reach ValidateOpts (sampling path), with a sane
	// fallback when the request left it at zero.
	task := &CompareTask{Mode: "sample", SampleRatio: 0, Tables: []string{"a"}}
	ratio, opts, cfg := buildCompareRunParams(task, "/tmp/r.json")
	if ratio != 0.01 || opts.SampleRatio != 0.01 || cfg.SampleRatio != 0.01 {
		t.Fatalf("zero ratio not defaulted: ratio=%v opts=%+v cfg=%+v", ratio, opts, cfg)
	}
	task.SampleRatio = 0.25
	_, opts, _ = buildCompareRunParams(task, "/tmp/r.json")
	if opts.SampleRatio != 0.25 {
		t.Fatalf("explicit ratio not propagated: %+v", opts)
	}
	// F-12 read-side clamp: a stale persisted task with ratio > 1 must be
	// clamped to 1, never reach the sampler out of range.
	task.SampleRatio = 100
	ratio, opts, cfg = buildCompareRunParams(task, "/tmp/r.json")
	if ratio != 1 || opts.SampleRatio != 1 || cfg.SampleRatio != 1 {
		t.Fatalf("ratio 100 not clamped: ratio=%v opts=%+v cfg=%+v", ratio, opts, cfg)
	}
	if opts.Mode != "sample" || len(opts.Tables) != 1 || opts.ReportFile != "/tmp/r.json" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
}

// P0-2 anchor: query_timeout must reach the validator's CompareConfig (the
// validator's QueryTimeoutDuration() then binds it per query); an empty
// task keeps the empty value (the validator falls back to its 5m default).
func TestCompareTask_BuildRunParamsQueryTimeout(t *testing.T) {
	_, _, cfg := buildCompareRunParams(&CompareTask{QueryTimeout: "90s"}, "")
	if cfg.QueryTimeout != "90s" {
		t.Fatalf("query_timeout not propagated to CompareConfig: %+v", cfg)
	}
	_, _, cfg = buildCompareRunParams(&CompareTask{}, "")
	if cfg.QueryTimeout != "" {
		t.Fatalf("empty query_timeout must stay empty (validator binds its 30m default), got %q", cfg.QueryTimeout)
	}
	// The JSON wire name is query_timeout on the request face.
	var req CompareTaskRequest
	if err := json.Unmarshal([]byte(`{"query_timeout":"3m"}`), &req); err != nil {
		t.Fatalf("decode CompareTaskRequest: %v", err)
	}
	if req.QueryTimeout != "3m" {
		t.Fatalf("request query_timeout = %q, want 3m", req.QueryTimeout)
	}
}

func TestCompareTask_RecoverStaleRunning(t *testing.T) {
	s, _ := newTestServer(t)
	dir := s.compareDir("stale01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	running := `{"id":"stale01","name":"orphan","status":"running",
		"source":{"host":"s"},"target":{"host":"t"},"mode":"quick",
		"created_at":"` + now + `"}`
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(running), 0o644); err != nil {
		t.Fatal(err)
	}

	s.recoverStaleCompares()

	t2, err := s.loadCompareTask("stale01")
	if err != nil || t2 == nil {
		t.Fatalf("reload stale task: %v", err)
	}
	if t2.Status != CompareStatusFailed || t2.Error != "interrupted by service restart" {
		t.Fatalf("stale running task not recovered: %+v", t2)
	}
	if t2.FinishedAt == nil {
		t.Fatalf("recovered task missing finished_at")
	}
}

// A1 restart recovery: queued tasks survive a restart in FIFO order.
// Ref-backed tasks re-resolve credentials from the datasource registry and
// are re-dispatched; a ref pointing at a MISSING datasource fails explicitly
// and the queue moves on; inline-credential tasks fail with the explicit
// creds-lost message (passwords are NEVER persisted — the red line).
func TestCompareTask_RecoverQueuedOnRestart(t *testing.T) {
	s, _ := newTestServer(t)

	// Seed the datasource registry: one postgres source + one tidb target.
	ds := `[{"id":"dsrc0001","name":"src","type":"postgres",
	  "fields":{"host":"127.0.0.1","port":"5433","user":"u","password":"pw-src","database":"d"}},
	 {"id":"dtgt0001","name":"tgt","type":"tidb",
	  "fields":{"host":"127.0.0.1","port":"4400","user":"root","password":"pw-tgt","database":"d"}}]`
	if err := os.WriteFile(s.datasourcesFile(), []byte(ds), 0o600); err != nil {
		t.Fatal(err)
	}

	seed := func(id, name, srcRef, tgtRef, created, body string) {
		t.Helper()
		dir := s.compareDir(id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		taskJSON := `{"id":"` + id + `","name":"` + name + `","status":"queued"` +
			`,"source_ref":"` + srcRef + `","target_ref":"` + tgtRef + `"` +
			body + `,"created_at":"` + created + `"}`
		if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(taskJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// FIFO order is earliest CreatedAt first: ref task oldest, then the
	// broken-ref task, then the inline task.
	base := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	mid := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339Nano)
	late := time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339Nano)

	// Recoverable: both refs exist.
	seed("aa000001", "ref-task", "dsrc0001", "dtgt0001", base,
		`,"source":{"host":"old"},"target":{"host":"old"},"mode":"quick"`)
	// Refs present but the source datasource is gone → explicit failure,
	// queue moves on to the next head.
	seed("bb000002", "broken-ref", "gone0001", "dtgt0001", mid,
		`,"source":{"host":"old"},"target":{"host":"old"},"mode":"quick"`)
	// Inline credentials, no refs → explicit creds-lost failure (never a
	// silent drop, never a password on disk).
	seed("cc000003", "inline-task", "", "", late,
		`,"source":{"host":"old"},"target":{"host":"old"},"mode":"quick"`)

	s.recoverStaleCompares()

	// The inline task failed immediately, without ever running.
	inline, err := s.loadCompareTask("cc000003")
	if err != nil || inline == nil {
		t.Fatalf("reload inline: %v", err)
	}
	if inline.Status != CompareStatusFailed || inline.Error != errCompareCredsLost.Error() {
		t.Fatalf("inline queued task not failed explicitly: %+v", inline)
	}
	if inline.StartedAt != nil {
		t.Fatalf("inline task must never start: %+v", inline.StartedAt)
	}

	// The ref task was dispatched (it runs against unreachable ports, then
	// fails; its finish() fails the broken-ref task via the dispatch loop).
	compareWaitSlotIdle(t, s)
	deadline := time.Now().Add(20 * time.Second)
	var broken, ref *CompareTask
	for time.Now().Before(deadline) {
		broken, _ = s.loadCompareTask("bb000002")
		ref, _ = s.loadCompareTask("aa000001")
		if broken != nil && broken.Status == CompareStatusFailed &&
			ref != nil && ref.Status == CompareStatusFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The broken-ref task failed with the resolution error, never ran.
	if broken == nil || broken.Status != CompareStatusFailed || !strings.Contains(broken.Error, "source_ref") {
		t.Fatalf("broken-ref task not failed with source_ref error: %+v", broken)
	}
	if broken.StartedAt != nil {
		t.Fatalf("broken-ref task must never start: %+v", broken.StartedAt)
	}

	if ref == nil || ref.StartedAt == nil {
		t.Fatalf("ref queued task never dispatched after restart: %+v", ref)
	}
	if ref.Status != CompareStatusFailed {
		t.Fatalf("ref task status = %q, want failed (unreachable DB)", ref.Status)
	}
	if ref.Error == errCompareCredsLost.Error() {
		t.Fatalf("ref-backed task must not hit the creds-lost path: %+v", ref)
	}
	if ref.SourceRef != "dsrc0001" || ref.TargetRef != "dtgt0001" {
		t.Fatalf("refs not preserved on the dispatched task: %+v", ref)
	}
	// The re-resolved snapshot (registry ports), not the stale seeded host.
	if ref.Source.Host != "127.0.0.1" || ref.Source.Port != 5433 {
		t.Fatalf("source not re-resolved from the registry: %+v", ref.Source)
	}
	if ref.Target.Port != 4400 {
		t.Fatalf("target not re-resolved from the registry: %+v", ref.Target)
	}
	// The re-resolved passwords ran in memory only — never hit the disk.
	raw, err := os.ReadFile(filepath.Join(s.compareDir("aa000001"), "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "pw-src") || strings.Contains(string(raw), "pw-tgt") {
		t.Fatalf("password leaked to disk during recovery dispatch: %s", raw)
	}
}
