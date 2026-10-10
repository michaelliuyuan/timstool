package orchestrator

// MS-11p anchors (task book v1.1): the dumpling export fast-path is
// user-controllable and honest. (1) The configured path is wired into binary
// resolution. (2) With use_dumpling=true (failure semantics A) an
// unresolvable path, a silently substituted path, or a failed dump FAILS the
// task — never a silent swap to stream. (3) With the switch off the original
// auto-discover + fallback behavior is byte-identical. Per-table stamp
// semantics stay STATUS QUO (pool ①② rides a later batch per the v1.1
// ruling — the pre-stamp/evidence face built under v1.0 was reverted).

import (
	"context"
	"database/sql"
	"errors"
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

// fakeDumpOK is a successful dump seam (no CSV evidence needed: the
// accounting loop runs its real CountExportedRows over the empty tempDir and
// stamps the status-quo zero-row completions).
func fakeDumpOK(ctx context.Context, cfg dumpling.DumpConfig) error { return nil }

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
	mgr, err := checkpoint.NewManager(t.TempDir())
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
// and the stream path stays cold. Status-quo stamp face: this branch never
// pre-stamps tables Running, so the checkpoint simply carries no table rows
// from it (task-level failure is the honest terminal state).
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
	if ta, ok := mgr.GetTable("ta"); ok && ta.State == checkpoint.StateRunning {
		t.Fatalf("ta stuck Running: %+v (this branch never pre-stamps)", ta)
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

// TestRunSourceCIRDumplingHappyPathStatusQuoStamps: the fast path still runs
// end-to-end (dump → lightning) and the accounting loop keeps its STATUS-QUO
// shape — per-table completions with zero StartedAt (the MS-11n honest "—"
// face; pool ①② deliberately untouched per v1.1).
func TestRunSourceCIRDumplingHappyPathStatusQuoStamps(t *testing.T) {
	o, c, mgr := ms11pOrch(t, fakeDumpOK)
	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	if c.lightning != 1 {
		t.Fatalf("lightning = %d, want 1 (dumpling fast-path)", c.lightning)
	}
	for _, name := range []string{"ta", "tb", "tc"} {
		tc, ok := mgr.GetTable(name)
		if !ok {
			t.Fatalf("%s missing from checkpoint", name)
		}
		if tc.State != checkpoint.StateCompleted {
			t.Fatalf("%s state = %s, want completed (status-quo accounting)", name, tc.State)
		}
		if !tc.StartedAt.IsZero() {
			t.Fatalf("%s StartedAt = %v, want zero (status-quo: no pre-stamps, honest —)", name, tc.StartedAt)
		}
	}
}
