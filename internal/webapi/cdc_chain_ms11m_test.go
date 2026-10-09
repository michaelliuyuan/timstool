package webapi

// MS-11m: CDC chain foreign-source guard. CDC is a system-wide single chain
// (one config.yaml cdc source, one supervisor, one checkpoint file) while
// startCDCChainAfterSuccess is per-task — these tests pin the guard that a
// foreign chain (a) never silently swallows the chaining intent and (b) never
// lets a PG LSN cross-pollute a MySQL-form (slotless) checkpoint.

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/michaelliuyuan/timstool/internal/cdc"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"gopkg.in/yaml.v3"
)

// rewriteCDCCfgSource swaps the CDC config.yaml source in place (load →
// modify → save, under the same mutex the handlers use).
func rewriteCDCCfgSource(t *testing.T, s *Server, cfgFile string, sc config.SourceConfig) {
	t.Helper()
	cdcCfgMu.Lock()
	defer cdcCfgMu.Unlock()
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Source = sc
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func ms11mTaskPG() *config.Config {
	cfg := &config.Config{}
	cfg.Source.Type = "postgres"
	cfg.Source.Host = "pghost"
	cfg.Source.Port = 5433
	return cfg
}

func ms11mLogText(s *Server, taskID string) (text string, hasWarn bool) {
	for _, e := range s.logCollector.GetBuffer(taskID).GetAll() {
		text += e.Message + "\n"
		if e.Level == "WARN" {
			hasWarn = true
		}
	}
	return text, hasWarn
}

// Case ① same-source running → legacy skip message, guard silent.
func TestMS11m_SameSourceRunningSkipsUnchanged(t *testing.T) {
	s, _, _ := newCDCServer(t) // CDC config source = postgres@pghost:5433
	sup := newTestSupervisor(t, true)
	sup.SetFactory(func() (supervisedProcess, error) { return newFakeProc(4242), nil })
	s.cdcSupervisor = sup
	if _, err := sup.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !waitForState(t, sup, StateRunning, 2*time.Second) {
		t.Fatal("never reached running")
	}

	s.startCDCChainAfterSuccess("taskX", ms11mTaskPG())

	text, _ := ms11mLogText(s, "taskX")
	if !strings.Contains(text, "已在运行，跳过自动衔接") {
		t.Fatalf("legacy skip message missing: %s", text)
	}
	if strings.Contains(text, "链源不一致") {
		t.Fatalf("same source must not trigger the guard: %s", text)
	}
}

// Case ② foreign-source running → WARN naming both sources, no skip wording.
func TestMS11m_ForeignSourceRunningWarnsAndDoesNotStart(t *testing.T) {
	s, _, cfgFile := newCDCServer(t)
	rewriteCDCCfgSource(t, s, cfgFile, config.SourceConfig{Type: "mysql", Host: "myhost", Port: 3306})
	sup := newTestSupervisor(t, true)
	sup.SetFactory(func() (supervisedProcess, error) { return newFakeProc(9), nil })
	s.cdcSupervisor = sup
	if _, err := sup.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !waitForState(t, sup, StateRunning, 2*time.Second) {
		t.Fatal("never reached running")
	}

	s.startCDCChainAfterSuccess("taskX", ms11mTaskPG())

	text, hasWarn := ms11mLogText(s, "taskX")
	if !hasWarn || !strings.Contains(text, "链源不一致") {
		t.Fatalf("guard WARN missing: %s", text)
	}
	if !strings.Contains(text, "mysql@myhost:3306") || !strings.Contains(text, "postgres@pghost:5433") {
		t.Fatalf("both source identities must be named: %s", text)
	}
	if !strings.Contains(text, "slot 已保留 WAL") {
		t.Fatalf("loss-less hint missing: %s", text)
	}
	if strings.Contains(text, "已在运行，跳过自动衔接") {
		t.Fatalf("foreign source must not fall through to the legacy skip: %s", text)
	}
	if st := sup.Status(); st.State != StateRunning {
		t.Fatalf("supervisor state changed: %s", st.State)
	}
}

// Case ③ (v1.1) PG task meets a MySQL-form checkpoint while the CDC config
// itself is the foreign MySQL chain: no seed, no Start, and the checkpoint
// stays byte-identical (Binlog.File/Pos untouched, no Save). The second half
// drives seed directly with a same-source config (residual slotless file) —
// the seed's own guard must refuse to touch the file too.
func TestMS11m_MySQLFormCheckpointNotClobberedByPGChain(t *testing.T) {
	s, _, cfgFile := newCDCServer(t)
	raw := `{"lsn":0,"binlog":{"file":"binlog.000010","pos":13491}}`
	if err := os.WriteFile(chainCheckpointPathForTest(s), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	rewriteCDCCfgSource(t, s, cfgFile, config.SourceConfig{Type: "mysql", Host: "myhost", Port: 3306})
	sup := newTestSupervisor(t, true)
	var spawns int32
	sup.SetFactory(func() (supervisedProcess, error) {
		atomic.AddInt32(&spawns, 1)
		return newFakeProc(7), nil
	})
	s.cdcSupervisor = sup

	cfg := ms11mTaskPG()
	cfg.Migration.ChainStartLSN = "0/3D0000A0"
	s.startCDCChainAfterSuccess("taskX", cfg)

	text, hasWarn := ms11mLogText(s, "taskX")
	if !hasWarn || !strings.Contains(text, "链源不一致") {
		t.Fatalf("guard WARN missing: %s", text)
	}
	if spawns != 0 {
		t.Fatalf("Start must not be called, spawns=%d", spawns)
	}
	b, err := os.ReadFile(chainCheckpointPathForTest(s))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != raw {
		t.Fatalf("MySQL-form checkpoint must stay byte-identical, got %s", b)
	}

	// Residual shape: config back to the task's source, slotless file left
	// over from a binlog chain — the seed reached directly must keep it too.
	rewriteCDCCfgSource(t, s, cfgFile, config.SourceConfig{Type: "postgres", Host: "pghost", Port: 5433})
	if err := s.seedChainCheckpoint("taskY", cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b2, err := os.ReadFile(chainCheckpointPathForTest(s))
	if err != nil {
		t.Fatal(err)
	}
	if string(b2) != raw {
		t.Fatalf("seed must not rewrite the slotless checkpoint, got %s", b2)
	}
	text2, hasWarn2 := ms11mLogText(s, "taskY")
	if !hasWarn2 || !strings.Contains(text2, "MySQL 形态") {
		t.Fatalf("cross-source WARN missing: %s", text2)
	}
}

// Case ④ same-slot checkpoint semantics are unchanged: an existing
// same-slot position at/past the chain point still wins.
func TestMS11m_SameSlotCheckpointStillWins(t *testing.T) {
	s, _, _ := newCDCServer(t)
	lsn, err := pglogrepl.ParseLSN("0/3D0000A0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := cdc.NewCheckpointManager(chainCheckpointPathForTest(s))
	mgr.SetSlotName("pg2tidb_cdc")
	mgr.Update(lsn)
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Migration.ChainStartLSN = "0/1000000" // behind the existing point
	if err := s.seedChainCheckpoint("taskX", cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cp, err := cdc.NewCheckpointManager(chainCheckpointPathForTest(s)).Load()
	if err != nil || cp == nil {
		t.Fatalf("load: %v %v", cp, err)
	}
	if cp.LSN.String() != "0/3D0000A0" {
		t.Fatalf("same-slot checkpoint was clobbered: %s", cp.LSN.String())
	}
}

// Case ⑤ (the un-triggered hazard branch of the incident): foreign source
// with a STOPPED supervisor → no seed, no Start — the guard must fire before
// the checkpoint can be polluted or the wrong chain spawned.
func TestMS11m_ForeignSourceStoppedGuardsSeedAndStart(t *testing.T) {
	s, _, cfgFile := newCDCServer(t)
	rewriteCDCCfgSource(t, s, cfgFile, config.SourceConfig{Type: "mysql", Host: "myhost", Port: 3306})
	sup := newTestSupervisor(t, true) // stopped
	var spawns int32
	sup.SetFactory(func() (supervisedProcess, error) {
		atomic.AddInt32(&spawns, 1)
		return newFakeProc(7), nil
	})
	s.cdcSupervisor = sup

	cfg := ms11mTaskPG()
	cfg.Migration.ChainStartLSN = "0/3D0000A0"
	s.startCDCChainAfterSuccess("taskX", cfg)

	text, hasWarn := ms11mLogText(s, "taskX")
	if !hasWarn || !strings.Contains(text, "链源不一致") {
		t.Fatalf("guard WARN missing: %s", text)
	}
	if strings.Contains(text, "增量已自动衔接") {
		t.Fatalf("must not claim chaining happened: %s", text)
	}
	if spawns != 0 {
		t.Fatalf("Start must not be called on a foreign chain, spawns=%d", spawns)
	}
	if _, err := os.Stat(chainCheckpointPathForTest(s)); !os.IsNotExist(err) {
		t.Fatal("seed must not run for a foreign chain")
	}
}
