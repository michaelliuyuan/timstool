package orchestrator

// MS-11p anchors: the dumpling export fast-path is now user-controllable and
// honest. (1) The configured path is wired into binary resolution. (2) With
// use_dumpling=true (failure semantics A) an unresolvable path, a silently
// substituted path, or a failed dump FAILS the task — never a silent swap to
// stream. (3) With the switch off the original auto-discover + fallback
// behavior is byte-identical. (4) Per-table accounting (pool ①②): chained
// Running pre-stamps, evidence-based terminal stamps — a table with no CSV
// from this dump is failed and stays failed, a genuinely re-exported table
// flips green with a NEW stamp.

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
	"github.com/michaelliuyuan/timstool/internal/dumpling"
	"github.com/michaelliuyuan/timstool/internal/source"
)

type fakeCIRSource3Tables struct{ closed bool }

func (f *fakeCIRSource3Tables) Name() string                      { return "mysql" }
func (f *fakeCIRSource3Tables) Connect(ctx context.Context) error { return nil }
func (f *fakeCIRSource3Tables) Close() error                      { f.closed = true; return nil }
func (f *fakeCIRSource3Tables) SchemaReader() source.SchemaReader { return f }
func (f *fakeCIRSource3Tables) DataReader() source.DataReader     { return nil }
func (f *fakeCIRSource3Tables) TypeMapper() source.TypeMapper     { return nil }
func (f *fakeCIRSource3Tables) IncrementalCapture() (source.IncrementalCapture, error) {
	return nil, source.ErrNotImplemented
}
func (f *fakeCIRSource3Tables) Dialect() source.Dialect { return nil }

func (f *fakeCIRSource3Tables) DB() *sql.DB {
	db, _ := sql.Open("mysql", "user:pass@tcp(127.0.0.1:1)/db")
	return db
}

func (f *fakeCIRSource3Tables) ReadSchema(ctx context.Context, opts source.Filter) (*source.Schema, error) {
	mk := func(name string) source.Table {
		return source.Table{
			Schema: "db", Name: name,
			Columns: []source.Column{{Name: "id", SourceType: "bigint", TiDBType: "BIGINT"}},
			PK:      []string{"id"},
		}
	}
	return &source.Schema{Catalog: "db", Tables: []source.Table{mk("ta"), mk("tb"), mk("tc")}}, nil
}

// fakeDump writes real CSV evidence into cfg.OutputDir for the named tables
// (rows = 1/2/3 per table), so the evidence/row-count accounting runs its
// real code path.
func fakeDump(tables ...string) func(context.Context, dumpling.DumpConfig) error {
	return func(ctx context.Context, cfg dumpling.DumpConfig) error {
		rows := map[string]int{"ta": 1, "tb": 2, "tc": 3}
		for _, name := range tables {
			content := strings.Repeat("1\n", rows[name])
			if err := os.WriteFile(filepath.Join(cfg.OutputDir, "db."+name+".000000000000.csv"), []byte(content), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
}

// ms11pOrch builds an orchestrator whose source reports ta/tb/tc and whose
// seams are installed with the dump path forced to the fake binary.
func ms11pOrch(t *testing.T, dump func(context.Context, dumpling.DumpConfig) error) (*Orchestrator, *cirCounters, *checkpoint.Manager) {
	t.Helper()
	var c cirCounters
	restore := installCIRSeams(&c)
	t.Cleanup(restore)

	prevOpen := cirOpenSource
	fs := &fakeCIRSource3Tables{}
	cirOpenSource = func(kind string, cfg source.SourceConfig) (source.Source, error) { return fs, nil }
	t.Cleanup(func() { cirOpenSource = prevOpen })

	prevFind := cirFindDumpling
	cirFindDumpling = func(cfg string) string { return "/fake/tidb-dumpling" }
	t.Cleanup(func() { cirFindDumpling = prevFind })

	prevDump := cirDumpDumpling
	cirDumpDumpling = dump
	t.Cleanup(func() { cirDumpDumpling = prevDump })

	o := cirTestOrch()
	o.cfg.Migration.UseDumpling = true
	mgr, err := checkpoint.NewManager(filepath.Join(t.TempDir(), "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	o.cpMgr = mgr
	return o, &c, mgr
}

// TestRunSourceCIRDumplingWiringPassesConfiguredPath: the user's configured
// dumpling path reaches binary resolution even with the switch off — the
// wiring is always-on, only the failure semantics differ.
func TestRunSourceCIRDumplingWiringPassesConfiguredPath(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	got := ""
	prevFind := cirFindDumpling
	cirFindDumpling = func(cfg string) string { got = cfg; return "" }
	defer func() { cirFindDumpling = prevFind }()

	o := cirTestOrch()
	o.cfg.Migration.DumplingPath = "/configured/tidb-dumpling"
	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	if got != "/configured/tidb-dumpling" {
		t.Fatalf("cirFindDumpling arg = %q, want the configured path (wiring)", got)
	}
	if c.load != 1 {
		t.Fatalf("stream load = %d, want 1 (discovery miss → stream)", c.load)
	}
}

// TestRunSourceCIRDumplingExplicitResolutionFailsTaskFails: semantics A —
// switch on + nothing resolvable → the task FAILS and the stream path stays
// cold (no silent fallback).
func TestRunSourceCIRDumplingExplicitResolutionFailsTaskFails(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	prevFind := cirFindDumpling
	cirFindDumpling = func(cfg string) string { return "" }
	defer func() { cirFindDumpling = prevFind }()

	o := cirTestOrch()
	o.cfg.Migration.UseDumpling = true
	_, err := o.runSourceCIR(context.Background(), PipelineConfig{})
	if err == nil || !strings.Contains(err.Error(), "use_dumpling=true") {
		t.Fatalf("err = %v, want explicit-resolution failure naming use_dumpling", err)
	}
	if c.load != 0 || c.lightning != 0 {
		t.Fatalf("seams fired after explicit failure: load=%d lightning=%d, want 0/0 (no fallback)", c.load, c.lightning)
	}
}

// TestRunSourceCIRDumplingExplicitPathSubstitutionRefused: semantics A — a
// configured path that cannot be used must not be silently replaced by a
// discovery hit (a typo fails loudly, a different dumpling never runs).
func TestRunSourceCIRDumplingExplicitPathSubstitutionRefused(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	prevFind := cirFindDumpling
	cirFindDumpling = func(cfg string) string { return "/usr/local/bin/tidb-dumpling" }
	defer func() { cirFindDumpling = prevFind }()

	o := cirTestOrch()
	o.cfg.Migration.UseDumpling = true
	o.cfg.Migration.DumplingPath = "/typo/tidb-dumpling"
	_, err := o.runSourceCIR(context.Background(), PipelineConfig{})
	if err == nil || !strings.Contains(err.Error(), "拒绝静默替换") {
		t.Fatalf("err = %v, want substitution refusal", err)
	}
	if c.load != 0 || c.lightning != 0 {
		t.Fatalf("seams fired: load=%d lightning=%d, want 0/0", c.load, c.lightning)
	}
}

// TestRunSourceCIRDumplingExplicitDumpFailsTaskFailsNoFallback: semantics A
// negative anchor — dump failure under the explicit switch fails the task
// (stream cold) and stamps the running table failed with the abort error.
func TestRunSourceCIRDumplingExplicitDumpFailsTaskFailsNoFallback(t *testing.T) {
	o, c, mgr := ms11pOrch(t, func(ctx context.Context, cfg dumpling.DumpConfig) error {
		return errors.New("dump boom")
	})
	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err == nil ||
		!strings.Contains(err.Error(), "不回退 stream") {
		t.Fatalf("err = %v, want dump failure propagated without fallback", err)
	}
	if c.load != 0 || c.lightning != 0 {
		t.Fatalf("seams fired: load=%d lightning=%d, want 0/0 (semantics A: no stream swap)", c.load, c.lightning)
	}
	ta, ok := mgr.GetTable("ta")
	if !ok {
		t.Fatal("ta missing from checkpoint")
	}
	if ta.State != checkpoint.StateFailed || !strings.Contains(ta.Error, "export aborted: dumpling export failed") {
		t.Fatalf("ta = %s/%q, want failed with abort error", ta.State, ta.Error)
	}
	// The never-started successors keep their honest pending state.
	if tb, _ := mgr.GetTable("tb"); tb.State != checkpoint.StatePending {
		t.Fatalf("tb = %s, want pending (never started)", tb.State)
	}
}

// TestRunSourceCIRDumplingSwitchOffFallsBackToStream: regression anchor —
// switch off keeps today's behavior byte-for-byte: a failed dump falls back
// to the stream path and the task still succeeds.
func TestRunSourceCIRDumplingSwitchOffFallsBackToStream(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	prevFind := cirFindDumpling
	cirFindDumpling = func(cfg string) string { return "/fake/tidb-dumpling" }
	defer func() { cirFindDumpling = prevFind }()

	prevDump := cirDumpDumpling
	cirDumpDumpling = func(ctx context.Context, cfg dumpling.DumpConfig) error { return errors.New("dump boom") }
	defer func() { cirDumpDumpling = prevDump }()

	o := cirTestOrch()
	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err != nil {
		t.Fatalf("runSourceCIR: %v (switch off must fall back to stream)", err)
	}
	if c.load != 1 || c.lightning != 0 {
		t.Fatalf("load=%d lightning=%d, want 1/0 (stream fallback)", c.load, c.lightning)
	}
}

// TestRunSourceCIRDumplingPerTableStampsChained: pool ① shape — every table
// carries its own start/finish stamps, chained like the stream path
// (successor start never precedes predecessor finish).
func TestRunSourceCIRDumplingPerTableStampsChained(t *testing.T) {
	o, c, mgr := ms11pOrch(t, fakeDump("ta", "tb", "tc"))
	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	if c.lightning != 1 {
		t.Fatalf("lightning = %d, want 1 (dumpling fast-path)", c.lightning)
	}
	ta, _ := mgr.GetTable("ta")
	tb, _ := mgr.GetTable("tb")
	tc, _ := mgr.GetTable("tc")
	for _, tc_ := range []*checkpoint.TableCheckpoint{ta, tb, tc} {
		if tc_.State != checkpoint.StateCompleted {
			t.Fatalf("%s state = %s, want completed", tc_.TableName, tc_.State)
		}
		if tc_.StartedAt.IsZero() || tc_.FinishedAt.IsZero() {
			t.Fatalf("%s missing stamps: %v -> %v", tc_.TableName, tc_.StartedAt, tc_.FinishedAt)
		}
		if tc_.StartedAt.After(tc_.FinishedAt) {
			t.Fatalf("%s duration inverted: %v -> %v", tc_.TableName, tc_.StartedAt, tc_.FinishedAt)
		}
	}
	// Chained order (monotonic ≥, never before — same tolerance as the
	// stream-path MS-11n anchor).
	if tb.StartedAt.Before(ta.FinishedAt) || tc.StartedAt.Before(tb.FinishedAt) {
		t.Fatalf("chain broken: ta.fin=%v tb.start=%v tb.fin=%v tc.start=%v", ta.FinishedAt, tb.StartedAt, tb.FinishedAt, tc.StartedAt)
	}
	// Real row counts from the dumped CSVs.
	if ta.RowsDone != 1 || tb.RowsDone != 2 || tc.RowsDone != 3 {
		t.Fatalf("rows: ta=%d tb=%d tc=%d, want 1/2/3", ta.RowsDone, tb.RowsDone, tc.RowsDone)
	}
}

// TestRunSourceCIRDumplingNoCSVEvidenceStaysFailed: pool ② shape — a table
// whose CSV this dump never produced is stamped failed and stays failed;
// NO unconditional all-green.
func TestRunSourceCIRDumplingNoCSVEvidenceStaysFailed(t *testing.T) {
	o, _, mgr := ms11pOrch(t, fakeDump("ta", "tc")) // tb deliberately skipped
	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err != nil {
		t.Fatalf("runSourceCIR: %v (missing CSV is table-level honesty, task proceeds)", err)
	}
	ta, _ := mgr.GetTable("ta")
	tb, _ := mgr.GetTable("tb")
	tc, _ := mgr.GetTable("tc")
	if ta.State != checkpoint.StateCompleted || tc.State != checkpoint.StateCompleted {
		t.Fatalf("exported tables: ta=%s tc=%s, want completed", ta.State, tc.State)
	}
	if tb.State != checkpoint.StateFailed || !strings.Contains(tb.Error, "no CSV") {
		t.Fatalf("tb = %s/%q, want failed with evidence error", tb.State, tb.Error)
	}
}

// TestRunSourceCIRDumplingHonestFlipWithNewStamp: pool ① re-run shape — a
// previously failed table that THIS dump really exports flips green with a
// NEW terminal stamp and its stale error cleared.
func TestRunSourceCIRDumplingHonestFlipWithNewStamp(t *testing.T) {
	o, _, mgr := ms11pOrch(t, fakeDump("ta", "tb", "tc"))
	// Seed a prior-run failure for ta.
	mgr.GetOrCreateTable("ta", 0)
	_ = mgr.MarkTableRunning("ta")
	_ = mgr.MarkTableFailed("ta", "prior run export aborted")
	old, _ := mgr.GetTable("ta")
	oldFin := old.FinishedAt

	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	ta, _ := mgr.GetTable("ta")
	if ta.State != checkpoint.StateCompleted {
		t.Fatalf("ta = %s, want honest green flip", ta.State)
	}
	if ta.Error != "" {
		t.Fatalf("ta error = %q, want stale failure cleared", ta.Error)
	}
	if !ta.FinishedAt.After(oldFin) {
		t.Fatalf("ta FinishedAt = %v, want NEW stamp after prior %v (新章异刻)", ta.FinishedAt, oldFin)
	}
}
