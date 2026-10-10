package webapi

// MS-11p anchors: the validate-dumpling handler (four path shapes + the
// --version live probe, its failure and timeout shapes), the migration
// options use_dumpling/dumpling_path persistence (partial-merge + save-time
// validation), and the task-creation mapping into the persisted task config.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/dumpling"
)

func fakeDumplingScript(t *testing.T, name string, slow bool) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, name+".cmd")
		content := "@echo off\r\necho tidb-dumpling v9.9.9-fake\r\n"
		if slow {
			// Busy-loop INSIDE cmd.exe (no grandchild): a spawned child
			// would inherit the probe pipes and hold them past the kill.
			content = "@echo off\r\nfor /l %%i in (1,1,50000000) do rem\r\n"
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(dir, name)
	content := "#!/bin/sh\necho 'tidb-dumpling v9.9.9-fake'\n"
	if slow {
		content = "#!/bin/sh\nsleep 30\n"
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func postValidateDumpling(t *testing.T, h http.HandlerFunc, body string) validateDumplingResponse {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/validate-dumpling", strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	var resp validateDumplingResponse
	if w.Code != 200 || json.NewDecoder(w.Body).Decode(&resp) != nil {
		t.Fatalf("validate-dumpling: code=%d body=%s", w.Code, w.Body.String())
	}
	return resp
}

// TestHandleValidateDumplingFourShapesPlusLiveProbe: the Lightning-gate
// mirror (exists/dir/x-bit/missing) plus the MS-11p enhancement — a passing
// path must yield a REAL --version string, and a discovery hit (empty path)
// is probed too.
func TestHandleValidateDumplingFourShapesPlusLiveProbe(t *testing.T) {
	s, _ := newTestServer(t)

	exe := fakeDumplingScript(t, "tidb-dumpling", false)

	// Shape 1: explicit runnable path → success + live version string.
	resp := postValidateDumpling(t, s.handleValidateDumpling, fmt.Sprintf(`{"path":%q}`, exe))
	if !resp.Success || resp.ResolvedPath != exe {
		t.Fatalf("shape1: success=%v resolved=%q msg=%q", resp.Success, resp.ResolvedPath, resp.Message)
	}
	if resp.Version != "tidb-dumpling v9.9.9-fake" {
		t.Fatalf("shape1: version = %q, want the live probe output (no embedded fallback)", resp.Version)
	}

	// Shape 2: missing path.
	resp = postValidateDumpling(t, s.handleValidateDumpling, `{"path":"/no/such/dumpling"}`)
	if resp.Success {
		t.Fatalf("shape2: success=true, want fail (missing)")
	}

	// Shape 3: directory.
	resp = postValidateDumpling(t, s.handleValidateDumpling, fmt.Sprintf(`{"path":%q}`, t.TempDir()))
	if resp.Success {
		t.Fatalf("shape3: success=true, want fail (directory)")
	}

	// Shape 4: no x-bit (unix only).
	if runtime.GOOS != "windows" {
		noexec := filepath.Join(t.TempDir(), "noexec")
		if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		resp = postValidateDumpling(t, s.handleValidateDumpling, fmt.Sprintf(`{"path":%q}`, noexec))
		if resp.Success {
			t.Fatalf("shape4: success=true, want fail (no x-bit)")
		}
	}

	// Empty path + discovery miss → fail with the auto-discovery message.
	prevFind := webapiFindDumpling
	webapiFindDumpling = func(cfg string) string { return "" }
	defer func() { webapiFindDumpling = prevFind }()
	resp = postValidateDumpling(t, s.handleValidateDumpling, `{"path":""}`)
	if resp.Success || !strings.Contains(resp.Message, "自动发现失败") {
		t.Fatalf("discovery miss: success=%v msg=%q", resp.Success, resp.Message)
	}

	// Empty path + discovery hit → success carries the discovered path AND
	// the live version string (discovery does not skip 探真).
	webapiFindDumpling = func(cfg string) string { return exe }
	resp = postValidateDumpling(t, s.handleValidateDumpling, `{"path":"  "}`)
	if !resp.Success || resp.ResolvedPath != exe || resp.Version != "tidb-dumpling v9.9.9-fake" {
		t.Fatalf("discovery hit: success=%v resolved=%q version=%q msg=%q", resp.Success, resp.ResolvedPath, resp.Version, resp.Message)
	}
	if !strings.Contains(resp.Message, "自动发现") {
		t.Fatalf("discovery hit message = %q, want 自动发现 note", resp.Message)
	}
}

// TestHandleValidateDumplingProbeFailure: a path that passes the stat checks
// but cannot execute (corrupt download / wrong arch) must FAIL validation —
// the probe is the evidence, not the stat.
func TestHandleValidateDumplingProbeFailure(t *testing.T) {
	s, _ := newTestServer(t)
	garbage := filepath.Join(t.TempDir(), "garbage-dumpling")
	if err := os.WriteFile(garbage, []byte("this is not a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	resp := postValidateDumpling(t, s.handleValidateDumpling, fmt.Sprintf(`{"path":%q}`, garbage))
	if resp.Success {
		t.Fatalf("probe failure: success=true msg=%q, want fail (cannot execute)", resp.Message)
	}
	if !strings.Contains(resp.Message, "探真失败") {
		t.Fatalf("probe failure message = %q, want 探真失败 wording", resp.Message)
	}
}

// TestHandleValidateDumplingProbeTimeout: a wedged binary fails at the
// (shrunken) probe timeout — the wizard request can never hang.
func TestHandleValidateDumplingProbeTimeout(t *testing.T) {
	s, _ := newTestServer(t)
	restore := dumpling.SetVersionTimeout(300 * time.Millisecond)
	defer restore()
	hung := fakeDumplingScript(t, "tidb-dumpling-hung", true)
	resp := postValidateDumpling(t, s.handleValidateDumpling, fmt.Sprintf(`{"path":%q}`, hung))
	if resp.Success {
		t.Fatalf("probe timeout: success=true, want fail")
	}
	if !strings.Contains(resp.Message, "探真失败") || !strings.Contains(resp.Message, "timed out") {
		t.Fatalf("probe timeout message = %q, want 探真失败+timed out", resp.Message)
	}
}

// TestMigrationOptionsDumplingFields: partial-merge keeps absent fields,
// saves explicit ones, and rejects a broken explicit path at save time
// (semantics A pushed early: fail at save, not mid-task).
func TestMigrationOptionsDumplingFields(t *testing.T) {
	s, _ := newTestServer(t)

	// Save an explicit dumpling path with the switch on.
	exe := fakeDumplingScript(t, "tidb-dumpling", false)
	w, req := doReq("PUT", "/api/v1/migration-options",
		fmt.Sprintf(`{"use_dumpling":true,"dumpling_path":%q}`, exe))
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid dumpling path: status=%d body=%s", w.Code, w.Body.String())
	}

	// Partial PUT (only temp_dir) must NOT wipe the dumpling memory.
	w, req = doReq("PUT", "/api/v1/migration-options", `{"temp_dir":"/tmp/keep"}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial PUT: status=%d body=%s", w.Code, w.Body.String())
	}
	w, req = doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	var got migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.UseDumpling || got.DumplingPath != exe || got.TempDir != "/tmp/keep" {
		t.Fatalf("merged options = %+v, want dumpling memory kept + temp_dir updated", got)
	}

	// Broken explicit path + switch on → 400 at save time.
	w, req = doReq("PUT", "/api/v1/migration-options",
		`{"use_dumpling":true,"dumpling_path":"/no/such/dumpling"}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad dumpling path: status=%d, want 400", w.Code)
	}
	// Directory as path → 400.
	w, req = doReq("PUT", "/api/v1/migration-options",
		fmt.Sprintf(`{"use_dumpling":true,"dumpling_path":%q}`, t.TempDir()))
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("dir dumpling path: status=%d, want 400", w.Code)
	}

	// Turning the switch off clears the persisted path use (path kept as
	// memory is fine, but a task created with switch off must not see it).
	w, req = doReq("PUT", "/api/v1/migration-options", `{"use_dumpling":false}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("switch-off PUT: status=%d", w.Code)
	}
	w, req = doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	got = migrationOptionsBody{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UseDumpling {
		t.Fatalf("switch off not persisted: %+v", got)
	}
}

// TestCreateTaskMapsDumplingOpts: the wizard's use_dumpling/dumpling_path
// ride into the persisted task config (ConfigJSON) — the report and the
// engine read them from there.
func TestCreateTaskMapsDumplingOpts(t *testing.T) {
	s, st := newTestServer(t)

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "ms11p-src", "type": "mysql",
		"fields": {"host": "10.0.0.1", "port": 3306, "user": "mu", "password": "pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "ms11p-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)

	w, req = doReq("POST", "/api/v1/tasks", fmt.Sprintf(`{
		"name": "ms11p", "source_ref": %q, "target_ref": %q,
		"opts": {"use_dumpling": true, "dumpling_path": "/opt/dumpling/tidb-dumpling"}
	}`, srcID, tgtID))
	s.handleCreateTask(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("create task: status=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTask(created.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetTask: %v %v", err, stored)
	}
	var cfg config.Config
	if err := json.Unmarshal([]byte(stored.ConfigJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Migration.UseDumpling || cfg.Migration.DumplingPath != "/opt/dumpling/tidb-dumpling" {
		t.Fatalf("task config dumpling = %v/%q, want mapped into ConfigJSON", cfg.Migration.UseDumpling, cfg.Migration.DumplingPath)
	}
}
