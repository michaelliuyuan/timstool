package webapi

// MS-11e pen 5 anchors: the MS-11b tail-batch P3 items on the CDC start
// order, the start-gate config snapshot (TOCTOU), and the sentinel-400
// normalization across both import paths. Source-form anchors read the
// handler source directly (order/locking are structural facts a router
// black-box cannot pin deterministically); behavioral anchors drive the
// router wherever a fixture allows it.

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/store"
)

func srcOf(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// E1 behavioral: a wired supervisor with cdc.enable=false answers the start
// with a 400 config error (remediation: enable it), never a 409 conflict —
// even the supervisor's own ErrCDCDisabled 409 is pre-empted by the gate.
func TestCDCStartDisabledEnableIs400ConfigError(t *testing.T) {
	s, _, _ := newCDCServer(t)
	s.cdcSupervisor = newTestSupervisor(t, false) // wired, enable=false
	w, req := doReq("POST", "/api/v1/cdc/start", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("enable=false start = %d (want 400), body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cdc.enable") {
		t.Fatalf("remediation must name cdc.enable: %s", w.Body.String())
	}
}

// E1 order (form): both config-error gates (enable, server_id) must appear
// BEFORE the dual-increment mutex 409 in the start handler — config errors
// precede conflicts.
func TestCDCStartConfigGatesPrecedeMutex409(t *testing.T) {
	src := srcOf(t, "cdc_handler.go")
	enableIdx := strings.Index(src, "MS-11e E1")
	serverIDIdx := strings.Index(src, "MySQL CDC 需要 cdc.server_id")
	mutexIdx := strings.Index(src, "blockCDCStartForIncremental()")
	if enableIdx < 0 || serverIDIdx < 0 || mutexIdx < 0 {
		t.Fatalf("gate/mutex landmarks missing in handler source")
	}
	if !(enableIdx < mutexIdx && serverIDIdx < mutexIdx) {
		t.Fatalf("config gates must precede the mutex 409: enable=%d server_id=%d mutex=%d",
			enableIdx, serverIDIdx, mutexIdx)
	}
}

// E2 (form): the start gate reads config.yaml through cdcCfgSnapshot, which
// takes cdcCfgMu — the unlocked read was a TOCTOU stale-cfg hazard against
// concurrent PUT /cdc/config writes.
func TestCDCStartGateReadsLockedSnapshot(t *testing.T) {
	hSrc := srcOf(t, "cdc_handler.go")
	if !strings.Contains(hSrc, "s.cdcCfgSnapshot()") {
		t.Fatalf("start gate must read via cdcCfgSnapshot (locked), not loadCDCConfig")
	}
	cSrc := srcOf(t, "cdc_config.go")
	snapIdx := strings.Index(cSrc, "func (s *Server) cdcCfgSnapshot()")
	lockIdx := strings.Index(cSrc[snapIdx:], "cdcCfgMu.Lock()")
	if snapIdx < 0 || lockIdx < 0 {
		t.Fatalf("cdcCfgSnapshot must hold cdcCfgMu around the load")
	}
}

// E4: the MariaDB 4-column master-status scan shape (file, pos + two *any
// discard sinks) — non-nil dests, exactly four.
func TestScanMasterStatus4MariaDBShape(t *testing.T) {
	row := &fakeRowScan{}
	if _, _, err := scanMasterStatus4(row); err != nil {
		t.Fatal(err)
	}
	if len(row.capture) != 4 {
		t.Fatalf("4-column scan dests = %d, want 4", len(row.capture))
	}
	for i, d := range row.capture[2:] {
		if d == nil {
			t.Fatalf("dest %d is nil — nil dest form must not return", i+2)
		}
	}
}

// E5 dual-path: a hand-broken scalar `cdc:` section must surface as 400 on
// BOTH import endpoints (sentinel normalized with PUT /cdc/config), never
// a 500 "写入失败".
func brokenCDCConfigFile(t *testing.T, s *Server) {
	t.Helper()
	plain := t.TempDir() + "\\config.yaml"
	if err := os.WriteFile(plain, []byte("source:\n  host: a\n  port: 1\ntarget:\n  host: b\n  port: 2\nCDC: oops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.SetCDCConfigFile(plain)
}

func TestCDCImportTaskSentinelIs400(t *testing.T) {
	s, _, _ := newCDCServer(t)
	task := &store.Task{ID: "t1", Name: "t1", Status: store.TaskStatusCompleted}
	taskCfg := config.DefaultConfig()
	taskCfg.Source.Host = "fromtask"
	taskCfg.Target.Host = "tasktidb"
	task.ConfigJSON = `{"source":{"host":"fromtask"},"target":{"host":"tasktidb"}}`
	if err := s.store.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	brokenCDCConfigFile(t, s)
	w, req := doReq("POST", "/api/v1/cdc/config/import", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("import with broken cdc section = %d (want 400): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cdc") {
		t.Fatalf("refusal must name the cdc section: %s", w.Body.String())
	}
}

func TestCDCImportFromDataSourceSentinelIs400(t *testing.T) {
	s, _ := newTestServer(t)
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "e5-src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "e5-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)

	brokenCDCConfigFile(t, s)
	w, req = doReq("POST", "/api/v1/cdc/import-from-datasource",
		`{"source_ref": "`+srcID+`", "target_ref": "`+tgtID+`"}`)
	s.handleImportCDCFromDataSource(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("import-from-datasource with broken cdc section = %d (want 400): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cdc") {
		t.Fatalf("refusal must name the cdc section: %s", w.Body.String())
	}
}
