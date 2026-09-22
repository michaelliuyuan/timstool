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

// Standalone compare tasks (鐙珛鏁版嵁姣斿): creation validation, persistence
// with password redaction, list/get/report roundtrip, concurrency 409, and
// delete rules. Handlers are invoked directly (same pattern as the migration
// options tests); the actual validation run needs live DBs and is covered by
// remote verification.

func compareURL(id string) string { return "/api/v1/compare/tasks/" + id }

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
	}
	for _, c := range cases {
		w, req := doReq("POST", "/api/v1/compare/tasks", c.body)
		s.handleCreateCompare(w, req)
		if w.Code != c.code {
			t.Fatalf("%s: status = %d body=%s", c.name, w.Code, w.Body.String())
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

func TestCompareTask_ConcurrentRejected(t *testing.T) {
	s, _ := newTestServer(t)
	s.compare.cancel = func() {}
	s.compare.runningID = "other"
	defer func() { s.compare.cancel = nil; s.compare.runningID = "" }()

	w, req := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s"},"target":{"host":"t"}}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while running, got %d", w.Code)
	}
}

func TestCompareTask_ListGetReportDelete(t *testing.T) {
	s, _ := newTestServer(t)

	// Seed a finished compare task + report on disk directly.
	dir := s.compareDir("cmpabcd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	taskJSON := `{"id":"cmpabcd","name":"seed","status":"completed",
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
	if len(list) != 1 || list[0].ID != "cmpabcd" || list[0].TablesTotal != 3 {
		t.Fatalf("unexpected list: %+v", list)
	}

	// Get detail.
	w, req = doReq("GET", compareURL("cmpabcd"), "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d", w.Code)
	}

	// Report raw roundtrip.
	w, req = doReq("GET", compareURL("cmpabcd")+"/report", "")
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
	w, req = doReq("DELETE", compareURL("cmpabcd"), "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("compare dir still exists after delete")
	}

	// Delete while running → 409.
	s.compare.runningID = "cmpabcd"
	defer func() { s.compare.runningID = "" }()
	w, req = doReq("DELETE", compareURL("cmpabcd"), "")
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
	if opts.Mode != "sample" || len(opts.Tables) != 1 || opts.ReportFile != "/tmp/r.json" {
		t.Fatalf("unexpected opts: %+v", opts)
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
