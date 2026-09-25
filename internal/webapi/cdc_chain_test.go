package webapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/orchestrator"
	"go.uber.org/zap"
)

// CDC chain (P1 task 1) + REPLICA IDENTITY FULL assist (P1 task 2) tests.
// All DB access is mocked (cdcChainProbe / replicaIdentityExec fakes).

type fakeChainProber struct {
	pubErr     error
	pubCreated bool
	slotLSN    string
	slotReused bool
	slotErr    error
}

func (f *fakeChainProber) EnsurePublication(cfg *config.Config, name string) (bool, error) {
	return f.pubCreated, f.pubErr
}

func (f *fakeChainProber) EnsureSlot(cfg *config.Config, name string) (string, bool, error) {
	if f.slotErr != nil {
		return "", false, f.slotErr
	}
	return f.slotLSN, f.slotReused, nil
}

func TestCDCChain_PrepareSuccessAndDefaults(t *testing.T) {
	s, _, _ := newCDCServer(t)
	s.cdcChainProbe = &fakeChainProber{slotLSN: "0/3D0000A0"}

	cfg := &config.Config{}
	cfg.Source.Type = "postgres"
	lsn, reused, err := s.prepareCDCChain(cfg)
	if err != nil || lsn != "0/3D0000A0" || reused {
		t.Fatalf("prepare = %q,%v,%v", lsn, reused, err)
	}
	if sn := chainSlotName(cfg); sn != "pg2tidb_cdc" {
		t.Fatalf("default slot name = %q", sn)
	}
	if pn := chainPublicationName(cfg); pn != "pg2tidb_pub" {
		t.Fatalf("default publication name = %q", pn)
	}
}

func TestCDCChain_PrepareSlotFailureAborts(t *testing.T) {
	s, _, _ := newCDCServer(t)
	s.cdcChainProbe = &fakeChainProber{slotErr: fmt.Errorf("wal_level != logical")}

	if _, _, err := s.prepareCDCChain(&config.Config{}); err == nil ||
		!strings.Contains(err.Error(), "预建 replication slot 失败") {
		t.Fatalf("err = %v", err)
	}
}

func TestCDCChain_StartTaskAbortsWhenPrepareFails(t *testing.T) {
	s, _, _ := newCDCServer(t)
	s.cdcChainProbe = &fakeChainProber{slotErr: fmt.Errorf("boom")}

	// Create a chained task via the API, then start it: the 409 must abort
	// before any migration goroutine is spawned.
	body := `{"name":"chain1","source":{"host":"pg","port":5432,"user":"u","password":"p","database":"d"},
		"target":{"host":"t","port":4000,"user":"u","password":"p","database":"d"},
		"opts":{"cdc_chain":true}}`
	w, req := doReq("POST", "/api/v1/tasks", body)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	taskID := ExtractTaskID(t, w.Body.String())

	w, req = doReq("POST", "/api/v1/tasks/"+taskID+"/start", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("start status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "预建失败") {
		t.Fatalf("body = %s", w.Body.String())
	}
	// Task must not be running.
	task := GetTaskForTest(t, s, taskID)
	if task.Status == "running" {
		t.Fatal("task must not be running after chain prepare failure")
	}
	if task.Error == "" {
		t.Fatal("task error must be recorded")
	}
}

func TestCDCChain_StartAfterSuccessFailedSupervisorIsLoud(t *testing.T) {
	s, _, _ := newCDCServer(t)
	// nil supervisor → loud failure path: log entry + broadcast, no panic.
	cfg := &config.Config{}
	cfg.Migration.ChainStartLSN = "0/1"
	s.startCDCChainAfterSuccess("taskX", cfg)
	var logText string
	for _, e := range s.logCollector.GetBuffer("taskX").GetAll() {
		logText += e.Message + "\n"
	}
	if !strings.Contains(logText, "CDC 自动衔接失败") {
		t.Fatalf("log = %s", logText)
	}
}

func TestCDCChain_ConflictStrategyDefault(t *testing.T) {
	if cs := chainConflictStrategy(&config.Config{}); cs != "replace" {
		t.Fatalf("default = %q", cs)
	}
	cfg := &config.Config{}
	cfg.CDC.ConflictStrategy = "upsert"
	if cs := chainConflictStrategy(cfg); cs != "upsert" {
		t.Fatalf("override = %q", cs)
	}
}

func TestCDCChain_SeedCheckpoint(t *testing.T) {
	s, _, _ := newCDCServer(t)
	cfg := &config.Config{}
	cfg.Migration.ChainStartLSN = "0/3D0000A0"

	if err := s.seedChainCheckpoint("taskX", cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// File written with the numeric LSN + slot name; the child runner will
	// load it and replay from that exact point.
	cp, err := cdc.NewCheckpointManager(chainCheckpointPathForTest(s)).Load()
	if err != nil || cp == nil {
		t.Fatalf("load: %v %v", cp, err)
	}
	if cp.LSN.String() != "0/3D0000A0" {
		t.Fatalf("lsn = %s", cp.LSN.String())
	}

	// Existing checkpoint at/past the chain point wins: content unchanged
	// (chain LSN 0/1000000 < existing 0/3D0000A0).
	cfg.Migration.ChainStartLSN = "0/1000000"
	if err := s.seedChainCheckpoint("taskX", cfg); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	cp2, _ := cdc.NewCheckpointManager(chainCheckpointPathForTest(s)).Load()
	if cp2.LSN.String() != "0/3D0000A0" {
		t.Fatalf("existing checkpoint clobbered: %s", cp2.LSN.String())
	}
}

func TestCDCChain_SeedAdvancesStaleCheckpoint(t *testing.T) {
	s, _, _ := newCDCServer(t)
	// A stale checkpoint BEHIND the chain point must be advanced to the chain
	// point (never replay from an unnecessarily early position).
	mgr := cdc.NewCheckpointManager(chainCheckpointPathForTest(s))
	mgr.SetSlotName("pg2tidb_cdc")
	mgr.Update(1) // tiny old LSN
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Migration.ChainStartLSN = "0/3D0000A0"
	if err := s.seedChainCheckpoint("taskX", cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cp, _ := cdc.NewCheckpointManager(chainCheckpointPathForTest(s)).Load()
	if cp.LSN.String() != "0/3D0000A0" {
		t.Fatalf("stale checkpoint not advanced: %s", cp.LSN.String())
	}
}

func TestCDCChain_SeedCheckpointEdgeCases(t *testing.T) {
	s, _, _ := newCDCServer(t)

	// empty LSN: no-op, no file created
	cfg := &config.Config{}
	if err := s.seedChainCheckpoint("t", cfg); err != nil {
		t.Fatalf("empty lsn: %v", err)
	}
	if _, err := os.Stat(chainCheckpointPathForTest(s)); !os.IsNotExist(err) {
		t.Fatal("file must not be created for empty LSN")
	}
	// bad LSN: error
	cfg.Migration.ChainStartLSN = "not-a-lsn"
	if err := s.seedChainCheckpoint("t", cfg); err == nil {
		t.Fatal("bad lsn must error")
	}
}

func TestCDCChain_OnlyValidateFailedDemotion(t *testing.T) {
	mk := func(phase orchestrator.Phase, ok bool) orchestrator.PipelineResult {
		return orchestrator.PipelineResult{Phase: phase, Success: ok}
	}
	allOK := []orchestrator.PipelineResult{mk("schema", true), mk("data", true), mk(orchestrator.PhaseValidate, true)}
	valFail := []orchestrator.PipelineResult{mk("schema", true), mk("data", true), mk(orchestrator.PhaseValidate, false)}
	dataFail := []orchestrator.PipelineResult{mk("schema", true), mk("data", false), mk(orchestrator.PhaseValidate, false)}
	bothFail := []orchestrator.PipelineResult{mk("schema", true), mk("data", false), mk(orchestrator.PhaseValidate, true)}

	if onlyValidateFailed(allOK, true) {
		t.Fatal("all-success must not demote (nothing to demote)")
	}
	if !onlyValidateFailed(valFail, true) {
		t.Fatal("validate-only failure with chain must demote")
	}
	if onlyValidateFailed(dataFail, true) {
		t.Fatal("data failure must never demote")
	}
	if onlyValidateFailed(bothFail, true) {
		t.Fatal("non-validate failure blocks demotion")
	}
	if onlyValidateFailed(valFail, false) {
		t.Fatal("no chain → no demotion")
	}
}

// Regression (P1 deviation B BLOCKER): when the orchestrator aborts on a
// validate-phase failure it returns that failure as err — runMigration must
// still reach the chained demotion branch instead of failing fast.
func TestCDCChain_ValidateErrDemotionInRunMigration(t *testing.T) {
	s, _, _ := newCDCServer(t)
	body := `{"name":"chain-demote","source":{"host":"pg","port":5432,"user":"u","password":"p","database":"d"},
		"target":{"host":"t","port":4000,"user":"u","password":"p","database":"d"},
		"opts":{"cdc_chain":true}}`
	w, req := doReq("POST", "/api/v1/tasks", body)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	taskID := ExtractTaskID(t, w.Body.String())

	orig := runPipeline
	defer func() { runPipeline = orig }()

	mkCfg := func() config.Config {
		cfg := config.Config{}
		cfg.Migration.CDCChain = true
		cfg.Migration.ChainStartLSN = "0/1"
		cfg.Migration.CheckpointDir = t.TempDir()
		return cfg
	}

	// Case 1: pipeline aborts on validate-only failure (err non-nil).
	runPipeline = func(ctx context.Context, cfg config.Config, pc orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
		rs := []orchestrator.PipelineResult{
			{Phase: "schema", Success: true},
			{Phase: "data", Success: true},
			{Phase: orchestrator.PhaseValidate, Success: false},
		}
		return rs, fmt.Errorf("data validation failed: 1/2 tables failed")
	}
	s.runMigration(context.Background(), taskID, mkCfg(), 1)
	task := GetTaskForTest(t, s, taskID)
	if task.Status != "completed" {
		t.Fatalf("validate-only failure must demote to completed, got %q (err=%q)", task.Status, task.Error)
	}
	var logText string
	for _, e := range s.logCollector.GetBuffer(taskID).GetAll() {
		logText += e.Message + "\n"
	}
	if !strings.Contains(logText, "降级为告警") || !strings.Contains(logText, "data validation failed") {
		t.Fatalf("demotion WARN missing or lacks err detail: %s", logText)
	}

	// Case 2: pipeline aborts on a data-phase failure — must stay failed.
	taskID2 := taskID
	{
		body2 := `{"name":"chain-demote2","source":{"host":"pg","port":5432,"user":"u","password":"p","database":"d"},
			"target":{"host":"t","port":4000,"user":"u","password":"p","database":"d"},
			"opts":{"cdc_chain":true}}`
		w2, req2 := doReq("POST", "/api/v1/tasks", body2)
		s.router.ServeHTTP(w2, req2)
		if w2.Code != http.StatusCreated {
			t.Fatalf("create2 status = %d", w2.Code)
		}
		taskID2 = ExtractTaskID(t, w2.Body.String())
	}
	runPipeline = func(ctx context.Context, cfg config.Config, pc orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
		rs := []orchestrator.PipelineResult{
			{Phase: "schema", Success: true},
			{Phase: "data", Success: false},
		}
		return rs, fmt.Errorf("data migration failed: boom")
	}
	s.runMigration(context.Background(), taskID2, mkCfg(), 1)
	task2 := GetTaskForTest(t, s, taskID2)
	if task2.Status != "failed" {
		t.Fatalf("data failure must stay failed, got %q", task2.Status)
	}
	if task2.Error == "" {
		t.Fatal("task error must be recorded for data failure")
	}

	// Case 3: non-chained validate-only failure with err — must stay failed.
	{
		body3 := `{"name":"chain-demote3","source":{"host":"pg","port":5432,"user":"u","password":"p","database":"d"},
			"target":{"host":"t","port":4000,"user":"u","password":"p","database":"d"}}`
		w3, req3 := doReq("POST", "/api/v1/tasks", body3)
		s.router.ServeHTTP(w3, req3)
		if w3.Code != http.StatusCreated {
			t.Fatalf("create3 status = %d", w3.Code)
		}
		taskID3 := ExtractTaskID(t, w3.Body.String())
		runPipeline = func(ctx context.Context, cfg config.Config, pc orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
			rs := []orchestrator.PipelineResult{
				{Phase: "data", Success: true},
				{Phase: orchestrator.PhaseValidate, Success: false},
			}
			return rs, fmt.Errorf("data validation failed: 1/2 tables failed")
		}
		cfg3 := mkCfg()
		cfg3.Migration.CDCChain = false
		s.runMigration(context.Background(), taskID3, cfg3, 1)
		task3 := GetTaskForTest(t, s, taskID3)
		if task3.Status != "failed" {
			t.Fatalf("non-chained validate failure must stay failed, got %q", task3.Status)
		}
	}
}

// chainCheckpointPathForTest resolves the CDC config's checkpoint file path.
func chainCheckpointPathForTest(s *Server) string {
	cfg, err := func() (*config.Config, error) {
		cdcCfgMu.Lock()
		defer cdcCfgMu.Unlock()
		return s.loadCDCConfig()
	}()
	if err != nil {
		return ""
	}
	if cfg.CDC.CheckpointFile != "" {
		return cfg.CDC.CheckpointFile
	}
	return ".cdc_checkpoint.json"
}

// --- REPLICA IDENTITY FULL (P1 task 2) ---

type fakeReplicaExec struct {
	canAlter map[string]bool
	alterErr map[string]error
	alterLog []string
}

func (f *fakeReplicaExec) CanAlter(cfg *config.Config, schema, table string) (bool, error) {
	ok, exists := f.canAlter[schema+"."+table]
	if !exists {
		return false, fmt.Errorf("表 %s.%s 不存在", schema, table)
	}
	return ok, nil
}

func (f *fakeReplicaExec) AlterFull(cfg *config.Config, schema, table string) error {
	f.alterLog = append(f.alterLog, schema+"."+table)
	if err, ok := f.alterErr[schema+"."+table]; ok {
		return err
	}
	return nil
}

func TestCDCReplicaIdentity_ConfirmAndValidation(t *testing.T) {
	s, _, _ := newCDCServer(t)
	s.replicaIdentityExec = &fakeReplicaExec{}

	// wrong confirm
	w, req := doReq("POST", "/api/v1/cdc/replica-identity", `{"confirm":"NO","tables":["a.b"]}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	// empty tables
	w, req = doReq("POST", "/api/v1/cdc/replica-identity", `{"confirm":"ALTER","tables":[]}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestCDCReplicaIdentity_PerTableResults(t *testing.T) {
	s, _, _ := newCDCServer(t)
	fe := &fakeReplicaExec{canAlter: map[string]bool{
		"public.ok1":    true,
		"public.locked": false,
	}}
	s.replicaIdentityExec = fe

	body := `{"confirm":"ALTER","tables":["public.ok1","public.locked","public.bad name","ghost"]}`
	w, req := doReq("POST", "/api/v1/cdc/replica-identity", body)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	// ok1 executed
	if !strings.Contains(out, `"table":"public.ok1"`) || !strings.Contains(fe.alterLog[0], "public.ok1") {
		t.Fatalf("ok1 not executed: %s / %v", out, fe.alterLog)
	}
	// locked: no privilege, SQL still returned for manual exec
	if !strings.Contains(out, "无权执行") || !strings.Contains(out, `ALTER TABLE public.locked REPLICA IDENTITY FULL;`) {
		t.Fatalf("locked handling wrong: %s", out)
	}
	// illegal identifier refused
	if !strings.Contains(out, "非法表名") {
		t.Fatalf("illegal ident not refused: %s", out)
	}
	// non-existent table (ghost → public.ghost) reports error
	if !strings.Contains(out, "不存在") {
		t.Fatalf("missing table not reported: %s", out)
	}
	if !strings.Contains(out, `"ok":false`) {
		t.Fatalf("aggregate ok must be false: %s", out)
	}
}

func TestCDCReplicaIdentity_AllOK(t *testing.T) {
	s, _, _ := newCDCServer(t)
	fe := &fakeReplicaExec{canAlter: map[string]bool{"public.a": true, "public.b": true}}
	s.replicaIdentityExec = fe

	w, req := doReq("POST", "/api/v1/cdc/replica-identity", `{"confirm":"ALTER","tables":["a","b"]}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(fe.alterLog) != 2 {
		t.Fatalf("alterLog = %v", fe.alterLog)
	}
	// bare names default to public schema
	for _, x := range fe.alterLog {
		if !strings.HasPrefix(x, "public.") {
			t.Fatalf("schema default wrong: %v", fe.alterLog)
		}
	}
}

func TestCDCPrecheck_NoPKStructuredList(t *testing.T) {
	s, p, _ := newCDCServer(t)
	p.noPK = []string{"public.t1 (无主键)", "public.t2 (REPLICA IDENTITY d)"}

	w, req := doReq("GET", "/api/v1/cdc/precheck", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"no_pk_tables_list":["public.t1","public.t2"]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured list missing (%s): %s", want, body)
		}
	}
}

// --- helpers ---

func ExtractTaskID(t *testing.T, body string) string {
	t.Helper()
	// body is {"id":"xxxxxxxx",...}
	i := strings.Index(body, `"id":"`)
	if i < 0 {
		t.Fatalf("no id in %s", body)
	}
	rest := body[i+6:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("bad id in %s", body)
	}
	return rest[:j]
}

func GetTaskForTest(t *testing.T, s *Server, id string) (t0 *taskView) {
	t.Helper()
	task, err := s.store.GetTask(id)
	if err != nil || task == nil {
		t.Fatalf("get task: %v %v", task, err)
	}
	return &taskView{Status: string(task.Status), Error: task.Error}
}

type taskView struct {
	Status string
	Error  string
}

var _ = time.Second
var _ = zap.NewNop()
