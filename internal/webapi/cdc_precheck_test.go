package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/store"
	"gopkg.in/yaml.v3"
)

// CDC config / precheck / reset tests (A1/A2/A3). DB probes are mocked via a
// fake cdcDBProber; config.yaml is a temp file.

type fakeProber struct {
	version    string
	pgErr      error
	walLevel   string
	walErr     error
	replRole   bool
	replErr    error
	noPK       []string
	noPKErr    error
	slotExists bool
	slotActive bool
	slotLSN    string
	slotErr    error
	lag        int64
	currentLSN string
	targetErr  error
}

func (f *fakeProber) PingPG(cfg *config.Config) (string, error) { return f.version, f.pgErr }
func (f *fakeProber) WalLevel(cfg *config.Config) (string, error) {
	return f.walLevel, f.walErr
}
func (f *fakeProber) HasReplicationRole(cfg *config.Config) (bool, error) {
	return f.replRole, f.replErr
}
func (f *fakeProber) NoPKTables(cfg *config.Config) ([]string, error) { return f.noPK, f.noPKErr }
func (f *fakeProber) Slot(cfg *config.Config, slot string) (string, bool, bool, error) {
	if f.slotErr != nil {
		return "", false, false, f.slotErr
	}
	return f.slotLSN, f.slotActive, f.slotExists, nil
}
func (f *fakeProber) WalLag(cfg *config.Config, lsn string) (int64, string, error) {
	return f.lag, f.currentLSN, nil
}
func (f *fakeProber) PingTarget(cfg *config.Config) error { return f.targetErr }

// newCDCServer wires a test server with a temp config.yaml + fake prober.
func newCDCServer(t *testing.T) (*Server, *fakeProber, string) {
	t.Helper()
	s, _ := newTestServer(t)
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.DefaultConfig()
	cfg.Source.Host = "pghost"
	cfg.Source.Port = 5433
	cfg.Source.Password = "pgsecret"
	cfg.Target.Host = "tidbhost"
	cfg.Target.Password = "tidbsecret"
	cfg.CDC.CheckpointFile = filepath.Join(t.TempDir(), "cp.json")
	raw, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(cfgFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s.SetCDCConfigFile(cfgFile)
	p := &fakeProber{version: "16.2", walLevel: "logical", replRole: true}
	s.cdcProbe = p
	return s, p, cfgFile
}

func TestCDCConfig_GetRedacted(t *testing.T) {
	s, _, _ := newCDCServer(t)

	w, req := doReq("GET", "/api/v1/cdc/config", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "pgsecret") || strings.Contains(body, "tidbsecret") {
		t.Fatalf("password leaked: %s", body)
	}
	if !strings.Contains(body, `"has_password":true`) {
		t.Fatalf("has_password missing: %s", body)
	}
	if !strings.Contains(body, "pghost") || !strings.Contains(body, "pg2tidb_cdc") {
		t.Fatalf("summary fields missing: %s", body)
	}
}

func TestCDCConfig_PutMergeKeepsEmptyPassword(t *testing.T) {
	s, _, cfgFile := newCDCServer(t)

	// Change host + port; empty password must NOT wipe the stored one.
	w, req := doReq("PUT", "/api/v1/cdc/config",
		`{"source":{"host":"newpg","port":5434},"target":{"host":"newtidb","password":""}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put status = %d body=%s", w.Code, w.Body.String())
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.Host != "newpg" || cfg.Source.Port != 5434 {
		t.Fatalf("fields not applied: %+v", cfg.Source)
	}
	if cfg.Source.Password != "pgsecret" || cfg.Target.Password != "tidbsecret" {
		t.Fatalf("empty password wiped stored value: %q / %q", cfg.Source.Password, cfg.Target.Password)
	}
	if cfg.Target.Host != "newtidb" {
		t.Fatalf("target not applied: %+v", cfg.Target)
	}
	// cdc section untouched
	if cfg.CDC.SlotName != "pg2tidb_cdc" {
		t.Fatalf("cdc section changed: %+v", cfg.CDC)
	}

	// A non-empty password updates it.
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"source":{"password":"newsecret"}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put2 status = %d", w.Code)
	}
	cfg, _ = config.Load(cfgFile)
	if cfg.Source.Password != "newsecret" {
		t.Fatalf("password not updated: %q", cfg.Source.Password)
	}

	// Reject wiping hosts.
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"source":{"host":""}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty host accepted: %d", w.Code)
	}
}

func TestCDCConfig_ImportFromLatestCompleted(t *testing.T) {
	s, _, cfgFile := newCDCServer(t)

	// Two tasks: latest (created later) completed, older completed too.
	mk := func(name string) *store.Task {
		return &store.Task{ID: name, Name: name, Status: store.TaskStatusCompleted}
	}
	oldT := mk("older")
	if err := s.store.CreateTask(oldT); err != nil {
		t.Fatal(err)
	}
	taskCfg := config.DefaultConfig()
	taskCfg.Source = config.SourceConfig{Type: "postgres", Host: "fromtask", Port: 5433, User: "u", Password: "taskpw", Database: "db", Schema: "public"}
	taskCfg.Target = config.TargetConfig{Host: "tasktidb", Port: 4000, User: "root", Password: "tpw", Database: "db"}
	cfgBytes, _ := json.Marshal(taskCfg)
	newT := mk("newer")
	newT.ConfigJSON = string(cfgBytes)
	if err := s.store.CreateTask(newT); err != nil {
		t.Fatal(err)
	}

	w, req := doReq("POST", "/api/v1/cdc/config/import", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("import status = %d body=%s", w.Code, w.Body.String())
	}
	cfg, _ := config.Load(cfgFile)
	if cfg.Source.Host != "fromtask" || cfg.Target.Host != "tasktidb" {
		t.Fatalf("import not applied: %+v / %+v", cfg.Source, cfg.Target)
	}
	if cfg.Source.Password != "taskpw" || cfg.Target.Password != "tpw" {
		t.Fatalf("imported passwords missing (CDC child needs them): %+v", cfg.Source)
	}
	if cfg.CDC.SlotName != "pg2tidb_cdc" {
		t.Fatalf("cdc section clobbered by import: %+v", cfg.CDC)
	}

	// No completed tasks → 409 with guidance.
	s2, _, _ := newCDCServer(t)
	w, req = doReq("POST", "/api/v1/cdc/config/import", "")
	s2.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("no-task import status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestCDCPrecheck_AllOKAndFailures(t *testing.T) {
	s, p, _ := newCDCServer(t)
	p.slotExists = true
	p.slotActive = false
	p.slotLSN = "0/2000000"
	p.lag = 1 << 20
	p.currentLSN = "0/3000000"

	w, req := doReq("GET", "/api/v1/cdc/precheck", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp cdcPrecheckResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.WarnOnly {
		t.Fatalf("expected warn_only, items=%+v", resp.Items)
	}
	if len(resp.Items) != 7 {
		t.Fatalf("expected 7 items, got %d: %+v", len(resp.Items), resp.Items)
	}
	if !resp.Slot.Exists || resp.Slot.RestartLSN != "0/2000000" || resp.Slot.LagBytes != 1<<20 {
		t.Fatalf("slot info wrong: %+v", resp.Slot)
	}
	// No checkpoint but slot exists → resume-from-slot conclusion.
	if !strings.Contains(resp.Conclusion, "restart_lsn=0/2000000") {
		t.Fatalf("conclusion wrong: %s", resp.Conclusion)
	}

	// Failure path: bad wal_level + unreachable source.
	p2 := &fakeProber{version: "9.6", walLevel: "replica", replRole: false, targetErr: fmt.Errorf("refused")}
	s2, _, _ := newCDCServer(t)
	s2.cdcProbe = p2
	w, req = doReq("GET", "/api/v1/cdc/precheck", "")
	s2.router.ServeHTTP(w, req)
	var resp2 cdcPrecheckResponse
	json.Unmarshal(w.Body.Bytes(), &resp2)
	if resp2.WarnOnly {
		t.Fatalf("expected failures to block")
	}
	for _, it := range resp2.Items {
		switch it.Item {
		case "source_conn", "wal_level", "repl_role", "target_conn":
			if it.Level != "fail" {
				t.Fatalf("%s should fail: %+v", it.Item, it)
			}
		}
	}
}

func TestCDCPrecheck_NoPKWarnAndGapHint(t *testing.T) {
	s, p, cfgFile := newCDCServer(t)
	p.noPK = []string{"public.a", "public.b"}

	// No completed migration → gap hint not appended (fresh start).
	w, req := doReq("GET", "/api/v1/cdc/precheck", "")
	s.router.ServeHTTP(w, req)
	var resp cdcPrecheckResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.WarnOnly != true {
		t.Fatalf("no-pk must be warn not fail")
	}
	found := false
	for _, it := range resp.Items {
		if it.Item == "no_pk_tables" && it.Level == "warn" && strings.Contains(it.Detail, "2 张表") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no_pk warn missing: %+v", resp.Items)
	}
	if strings.Contains(resp.Conclusion, "增量不会被捕获") {
		t.Fatalf("gap hint without base migration: %s", resp.Conclusion)
	}

	// With a completed migration → gap hint appears for a fresh slot.
	task := &store.Task{ID: "done", Name: "done", Status: store.TaskStatusCompleted, ConfigJSON: "{}"}
	if err := s.store.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	w, _ = doReq("GET", "/api/v1/cdc/precheck", "")
	s.router.ServeHTTP(w, req)
	var resp2 cdcPrecheckResponse
	json.Unmarshal(w.Body.Bytes(), &resp2)
	if !strings.Contains(resp2.Conclusion, "增量不会被捕获") {
		t.Fatalf("gap hint missing: %s", resp2.Conclusion)
	}
	_ = cfgFile
}

func TestCDCResetCheckpoint(t *testing.T) {
	s, _, cfgFile := newCDCServer(t)
	cfg, _ := config.Load(cfgFile)
	cpPath := cfg.CDC.CheckpointFile
	if err := os.WriteFile(cpPath, []byte(`{"lsn":"0/1000","slot_name":"pg2tidb_cdc"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Missing confirmation → 400.
	w, req := doReq("POST", "/api/v1/cdc/checkpoint/reset", `{"confirm":"yes"}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed reset = %d", w.Code)
	}

	// Confirmed → file gone, guidance mentions the slot.
	w, req = doReq("POST", "/api/v1/cdc/checkpoint/reset", `{"confirm":"DELETE"}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reset = %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(cpPath); !os.IsNotExist(err) {
		t.Fatalf("checkpoint file still present")
	}
	if !strings.Contains(w.Body.String(), "pg_drop_replication_slot") {
		t.Fatalf("slot guidance missing: %s", w.Body.String())
	}

	// Gone → 404.
	w, req = doReq("POST", "/api/v1/cdc/checkpoint/reset", `{"confirm":"DELETE"}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("second reset = %d", w.Code)
	}
}

func TestCDCSlot_Live(t *testing.T) {
	s, p, _ := newCDCServer(t)
	p.slotExists = true
	p.slotActive = true
	p.slotLSN = "0/ABCDEF"
	p.lag = 2048
	p.currentLSN = "0/ABCFFF"

	w, req := doReq("GET", "/api/v1/cdc/slot", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"restart_lsn":"0/ABCDEF"`) || !strings.Contains(body, `"lag_bytes":2048`) {
		t.Fatalf("slot response wrong: %s", body)
	}
	if !strings.Contains(body, `"exists":false`) && strings.Contains(body, `"checkpoint":{"exists":false`) {
		// checkpoint absent is fine; just ensure no 5xx and slot parsed
		t.Fatalf("unexpected: %s", body)
	}
}
