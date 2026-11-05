package orchestrator

// MS-10d pen 1 anchors: the source-CIR path honors the same PipelineConfig
// skip switches as the PG path (a switch the user never set must never show
// "skipped" — the silent-lie bug class), plus the v1 minimal precheck probe.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // lazy *sql.DB handle for the target seam

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/source"
	"github.com/michaelliuyuan/timstool/internal/target"
)

// fakeCIRSource is a source.Source that reports one table and touches
// nothing else. It implements DB() with a lazy handle (the validate wiring
// requires the dbConn probe; every consumer seam is replaced). table names
// the single reported table; empty → "t1" (concurrency anchors pass distinct
// names so each run's checkpoint stays distinguishable).
type fakeCIRSource struct {
	closed bool
	table  string
}

func (f *fakeCIRSource) Name() string                      { return "mysql" }
func (f *fakeCIRSource) Connect(ctx context.Context) error { return nil }
func (f *fakeCIRSource) Close() error                      { f.closed = true; return nil }
func (f *fakeCIRSource) SchemaReader() source.SchemaReader { return f }
func (f *fakeCIRSource) DataReader() source.DataReader     { return nil }
func (f *fakeCIRSource) TypeMapper() source.TypeMapper     { return nil }
func (f *fakeCIRSource) IncrementalCapture() (source.IncrementalCapture, error) {
	return nil, source.ErrNotImplemented
}
func (f *fakeCIRSource) Dialect() source.Dialect { return nil }

// DB satisfies the orchestrator's dbConn probe (validate wiring + precheck
// probe reachability). Returns a lazy handle — every consumer seam in these
// tests is replaced, so it is never dialed.
func (f *fakeCIRSource) DB() *sql.DB {
	db, _ := sql.Open("mysql", "user:pass@tcp(127.0.0.1:1)/db")
	return db
}

func (f *fakeCIRSource) ReadSchema(ctx context.Context, opts source.Filter) (*source.Schema, error) {
	name := f.table
	if name == "" {
		name = "t1"
	}
	return &source.Schema{
		Catalog: "db",
		Tables: []source.Table{{
			Schema: "db", Name: name,
			Columns: []source.Column{{Name: "id", SourceType: "bigint", TiDBType: "BIGINT"}},
			PK:      []string{"id"},
		}},
	}, nil
}

// cirCounters records which seams actually fired.
type cirCounters struct {
	apply, load, lightning, drop, truncate, validate, precheck int
}

func installCIRSeams(c *cirCounters) (restore func()) {
	prevOpen, prevTarget := cirOpenSource, cirOpenTargetDB
	prevApply, prevLoad, prevLightning := cirApplyDDL, cirLoadData, cirRunLightning
	prevDrop, prevTruncate := cirDropTables, cirTruncateTables
	prevValidate, prevFind, prevPre := cirValidateMigration, cirFindDumpling, cirPrecheck

	fs := &fakeCIRSource{}
	cirOpenSource = func(kind string, cfg source.SourceConfig) (source.Source, error) { return fs, nil }
	cirOpenTargetDB = func(cfg config.Config) (*sql.DB, error) {
		// Lazy handle: never dialed (every consumer is a replaced seam);
		// a nil *sql.DB would panic on the deferred Close.
		return sql.Open("mysql", "user:pass@tcp(127.0.0.1:1)/db")
	}
	cirApplyDDL = func(ctx context.Context, db *sql.DB, s *source.Schema) error { c.apply++; return nil }
	cirLoadData = func(ctx context.Context, src source.Source, s *source.Schema, t config.TargetConfig, dir string, cb func(string, int64)) error {
		c.load++
		cb("t1", 1)
		return nil
	}
	cirRunLightning = func(ctx context.Context, dir string, t config.TargetConfig) error { c.lightning++; return nil }
	cirDropTables = func(ctx context.Context, db *sql.DB, s *source.Schema) error { c.drop++; return nil }
	cirTruncateTables = func(ctx context.Context, db *sql.DB, s *source.Schema) error { c.truncate++; return nil }
	cirValidateMigration = func(ctx context.Context, sdb, tdb *sql.DB, s *source.Schema, n int) (*target.ValidationReport, error) {
		c.validate++
		return &target.ValidationReport{TotalTables: 1, AllPassed: true}, nil
	}
	cirFindDumpling = func(cfg string) string { return "" } // force stream path
	cirPrecheck = func(ctx context.Context, src source.Source) error { c.precheck++; return nil }

	return func() {
		cirOpenSource, cirOpenTargetDB = prevOpen, prevTarget
		cirApplyDDL, cirLoadData, cirRunLightning = prevApply, prevLoad, prevLightning
		cirDropTables, cirTruncateTables = prevDrop, prevTruncate
		cirValidateMigration, cirFindDumpling, cirPrecheck = prevValidate, prevFind, prevPre
	}
}

func cirTestOrch() *Orchestrator {
	return &Orchestrator{cfg: config.Config{
		Source: config.SourceConfig{Type: "mysql", Host: "h", Database: "db"},
		Target: config.TargetConfig{Host: "t"},
	}}
}

// TestRunSourceCIRSkipSwitchesHonored: all four skips set → NO phase seam
// fires (precheck probe, ApplyDDL, load/lightning, validate all zero) and
// the result slice is empty — mirroring the PG path's "a result row only
// for phases actually executed" semantics.
func TestRunSourceCIRSkipSwitchesHonored(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	o := cirTestOrch()
	results, err := o.runSourceCIR(context.Background(), PipelineConfig{
		SkipPrecheck: true, SkipSchema: true, SkipData: true, SkipValidate: true,
	})
	if err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %v, want empty when every phase is skipped", results)
	}
	if c.precheck != 0 || c.apply != 0 || c.load != 0 || c.lightning != 0 || c.validate != 0 || c.drop != 0 || c.truncate != 0 {
		t.Fatalf("skipped phases still fired seams: %+v", c)
	}
}

// TestRunSourceCIRRunsAllPhases: no skips → precheck probe + ApplyDDL +
// stream load (dumpling stubbed absent) + validate all fire exactly once,
// and the result slice carries all four phases.
func TestRunSourceCIRRunsAllPhases(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	o := cirTestOrch()
	results, err := o.runSourceCIR(context.Background(), PipelineConfig{})
	if err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	if c.precheck != 1 {
		t.Fatalf("precheck probe calls = %d, want 1", c.precheck)
	}
	if c.apply != 1 {
		t.Fatalf("ApplyDDL calls = %d, want 1", c.apply)
	}
	if c.load != 1 || c.lightning != 0 {
		t.Fatalf("stream load = %d lightning = %d, want 1/0 (dumpling absent)", c.load, c.lightning)
	}
	if c.validate != 1 {
		t.Fatalf("validate calls = %d, want 1", c.validate)
	}
	seen := map[Phase]bool{}
	for _, r := range results {
		seen[r.Phase] = true
		if !r.Success {
			t.Fatalf("phase %q not Success", r.Phase)
		}
	}
	for _, ph := range []Phase{PhasePrecheck, PhaseSchema, PhaseData, PhaseValidate} {
		if !seen[ph] {
			t.Fatalf("phase %q missing from results %v", ph, results)
		}
	}
}

// TestRunSourceCIRSelectiveSkips: skip only schema+validate → ApplyDDL and
// validate seams stay cold while precheck + data still run; the result
// slice names exactly the two executed phases.
func TestRunSourceCIRSelectiveSkips(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	o := cirTestOrch()
	results, err := o.runSourceCIR(context.Background(), PipelineConfig{SkipSchema: true, SkipValidate: true})
	if err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	if c.apply != 0 || c.validate != 0 || c.drop != 0 || c.truncate != 0 {
		t.Fatalf("skipped schema/validate still fired: %+v", c)
	}
	if c.precheck != 1 || c.load != 1 {
		t.Fatalf("precheck=%d load=%d, want 1/1 (not skipped)", c.precheck, c.load)
	}
	if len(results) != 2 || results[0].Phase != PhasePrecheck || results[1].Phase != PhaseData {
		t.Fatalf("results = %v, want exactly [precheck data]", results)
	}
}

// TestRunSourceCIRPrecheckFailureAborts: a failing probe aborts the run
// with the probe error — a red precheck must never silently continue into
// schema/data (mirrors the PG path's precheck abort semantics).
func TestRunSourceCIRPrecheckFailureAborts(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()
	prev := cirPrecheck
	cirPrecheck = func(ctx context.Context, src source.Source) error { return errors.New("probe: unreachable") }
	defer func() { cirPrecheck = prev }()

	o := cirTestOrch()
	_, err := o.runSourceCIR(context.Background(), PipelineConfig{})
	if err == nil || err.Error() != "probe: unreachable" {
		t.Fatalf("err = %v, want probe error propagated", err)
	}
	if c.apply != 0 || c.load != 0 {
		t.Fatalf("phases ran after precheck failure: %+v", c)
	}
}

// TestRunSourceCIRValidateFailureSetsError: a failing validation must set
// the result row's Error (with a table-count message) alongside
// Success=false — parity with the PG path's runValidation (:697/:712);
// Success=false with Error=nil is the "failure with no message" bug class.
func TestRunSourceCIRValidateFailureSetsError(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()
	prev := cirValidateMigration
	cirValidateMigration = func(ctx context.Context, sdb, tdb *sql.DB, s *source.Schema, n int) (*target.ValidationReport, error) {
		c.validate++
		return &target.ValidationReport{TotalTables: 2, FailedTables: 1, AllPassed: false}, nil
	}
	defer func() { cirValidateMigration = prev }()

	o := cirTestOrch()
	results, err := o.runSourceCIR(context.Background(), PipelineConfig{})
	if err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %v, want all four phases", results)
	}
	last := results[len(results)-1]
	if last.Phase != PhaseValidate || last.Success {
		t.Fatalf("last result = %+v, want validate/Success=false", last)
	}
	if last.Error == nil || last.Error.Error() != "source-cir: validation failed: 1/2 tables failed" {
		t.Fatalf("Error = %v, want table-count message (PG-path parity)", last.Error)
	}
}

// TestRunSourceCIRRoutingPreconditions pins the dispatch preconditions the
// zero-regression guarantee rests on: mysql routes source-CIR, the legacy
// empty-type default stays postgres (PG pipeline), so PG never enters
// runSourceCIR.
func TestRunSourceCIRRoutingPreconditions(t *testing.T) {
	if got := cirTestOrch().cfg.Source.SourceType(); got != "mysql" {
		t.Fatalf("SourceType = %q, want mysql", got)
	}
	pg := config.Config{Source: config.SourceConfig{}}
	if got := (&Orchestrator{cfg: pg}).cfg.Source.SourceType(); got != "postgres" {
		t.Fatalf("empty SourceType = %q, want postgres (legacy default)", got)
	}
}

// TestRunSourceCIRValidateWiresPerTableHook (#t95 P0-5): during the CIR
// validate phase the orchestrator installs the per-table validated hook
// (so ValidateMigration's parallel tables mark tables_done on the fly) and
// clears it once the validate seam returns. With a cpMgr the hook drives
// completed/failed checkpoint marks — a completed table reports its real
// rows, a failed table reports its error.
func TestRunSourceCIRValidateWiresPerTableHook(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	dir := t.TempDir()
	mgr, err := checkpoint.NewManager(filepath.Join(dir, "cp"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	prev := cirValidateMigration
	cirValidateMigration = func(ctx context.Context, sdb, tdb *sql.DB, s *source.Schema, n int) (*target.ValidationReport, error) {
		c.validate++
		if target.TableValidatedHook() == nil {
			t.Errorf("per-table validated hook not installed during CIR validate")
			return &target.ValidationReport{TotalTables: 2, AllPassed: true}, nil
		}
		// Drive the hook the way the real ValidateMigration does: one
		// completed table, one failed table.
		target.TableValidatedHook()(target.TableValidation{Name: "t1", SourceRows: 7, Passed: true}, nil)
		target.TableValidatedHook()(target.TableValidation{Name: "t2"}, errors.New("simulated validate failure"))
		return &target.ValidationReport{TotalTables: 2, FailedTables: 1, AllPassed: false}, nil
	}
	defer func() { cirValidateMigration = prev }()

	o := &Orchestrator{cfg: config.Config{
		Source: config.SourceConfig{Type: "mysql", Host: "h", Database: "db"},
		Target: config.TargetConfig{Host: "t"},
	}, cpMgr: mgr}

	results, err := o.runSourceCIR(context.Background(), PipelineConfig{})
	if err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %v, want all four phases", results)
	}
	last := results[len(results)-1]
	if last.Phase != PhaseValidate || last.Success {
		t.Fatalf("last result = %+v, want validate/Success=false", last)
	}
	if target.TableValidatedHook() != nil {
		t.Fatalf("per-table validated hook not cleared after runSourceCIR")
	}

	tc1 := mgr.GetAllTables()["t1"]
	if tc1 == nil || tc1.State != checkpoint.StateCompleted || tc1.RowsDone != 7 {
		t.Fatalf("t1 checkpoint = %+v, want completed/7 rows", tc1)
	}
	tc2 := mgr.GetAllTables()["t2"]
	if tc2 == nil || tc2.State != checkpoint.StateFailed || !strings.Contains(tc2.Error, "simulated validate failure") {
		t.Fatalf("t2 checkpoint = %+v, want failed with simulated error", tc2)
	}
}

// TestRunSourceCIRConcurrentValidateHooksIsolated (#t95 R1, red-team): two
// CIR migrations running concurrently must never cross-write each other's
// checkpoint through the package-level per-table validated hook. Each run
// drives the validate seam with its own single table (atomic-distinct
// names) and a sleep that holds the hook window open — pre-fix, run A's
// callbacks hit B's hook (landing in B's cpMgr) and A's clear silences B.
// Post-fix the install→validate→clear segment is serialized, so each run's
// table is completed (rows=7) in exactly its own cpMgr — disjoint, no
// cross-pollution.
func TestRunSourceCIRConcurrentValidateHooksIsolated(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	prev := cirValidateMigration
	cirValidateMigration = func(ctx context.Context, sdb, tdb *sql.DB, s *source.Schema, n int) (*target.ValidationReport, error) {
		// Hold the hook window open so the two runs genuinely overlap
		// (widen the pre-fix race into a deterministic cross-write).
		time.Sleep(30 * time.Millisecond)
		if hook := target.TableValidatedHook(); hook != nil {
			for _, tb := range s.Tables {
				hook(target.TableValidation{Name: tb.Name, SourceRows: 7, Passed: true}, nil)
			}
		}
		return &target.ValidationReport{TotalTables: len(s.Tables), AllPassed: true}, nil
	}
	defer func() { cirValidateMigration = prev }()

	var seq int32
	prevOpen := cirOpenSource
	cirOpenSource = func(kind string, cfg source.SourceConfig) (source.Source, error) {
		n := atomic.AddInt32(&seq, 1)
		return &fakeCIRSource{table: fmt.Sprintf("tbl%d", n)}, nil
	}
	defer func() { cirOpenSource = prevOpen }()

	dirs := []string{filepath.Join(t.TempDir(), "cp"), filepath.Join(t.TempDir(), "cp")}
	var wg sync.WaitGroup
	mgrs := make([]*checkpoint.Manager, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mgr, err := checkpoint.NewManager(dirs[i])
			if err != nil {
				errs[i] = fmt.Errorf("NewManager: %w", err)
				return
			}
			o := &Orchestrator{cfg: config.Config{
				Source: config.SourceConfig{Type: "mysql", Host: "h", Database: "db"},
				Target: config.TargetConfig{Host: "t"},
			}, cpMgr: mgr}
			_, err = o.runSourceCIR(context.Background(), PipelineConfig{
				SkipPrecheck: true, SkipSchema: true, SkipData: true,
			})
			mgrs[i], errs[i] = mgr, err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	namesByRun := make([]map[string]bool, 2)
	for i, mgr := range mgrs {
		namesByRun[i] = map[string]bool{}
		for name, tc := range mgr.GetAllTables() {
			namesByRun[i][name] = true
			if tc == nil || tc.State != checkpoint.StateCompleted || tc.RowsDone != 7 {
				t.Fatalf("run %d table %q checkpoint = %+v, want completed/7 rows", i, name, tc)
			}
		}
	}
	for i, names := range namesByRun {
		if len(names) != 1 {
			t.Fatalf("run %d tables = %v, want exactly its own table (cross-write or lost progress)", i, names)
		}
	}
	for n := range namesByRun[0] {
		if namesByRun[1][n] {
			t.Fatalf("table %q landed in both runs' checkpoints — hook cross-write (R1)", n)
		}
	}
}
