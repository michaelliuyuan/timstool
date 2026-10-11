package orchestrator

// MS-10d pen 1 anchors: the source-CIR path honors the same PipelineConfig
// skip switches as the PG path (a switch the user never set must never show
// "skipped" — the silent-lie bug class), plus the v1 minimal precheck probe.

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "github.com/go-sql-driver/mysql" // lazy *sql.DB handle for the target seam

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/source"
	"github.com/michaelliuyuan/timstool/internal/target"
)

// fakeCIRSource is a source.Source that reports one table and touches
// nothing else. It implements DB() with a lazy handle (the validate wiring
// requires the dbConn probe; every consumer seam is replaced).
type fakeCIRSource struct{ closed bool }

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
	return &source.Schema{
		Catalog: "db",
		Tables: []source.Table{{
			Schema: "db", Name: "t1",
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
