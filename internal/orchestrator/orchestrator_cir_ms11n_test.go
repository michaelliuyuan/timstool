package orchestrator

// MS-11n ③ anchors: the CIR stream path stamps per-table starts. The stream
// exporter (target.LoadData) walks tables sequentially, so the first table's
// start lands before export begins and table k's start lands when table k-1
// completes — queue wait never inflates a table's own duration, and every
// completed table carries both stamps so the report shows real durations.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/source"
)

// fakeCIRSource2Tables mirrors fakeCIRSource but reports two tables. Standalone
// (not embedded): fakeCIRSource acts as its own SchemaReader, so an embedded
// override would still route ReadSchema to the inner one-table fake.
type fakeCIRSource2Tables struct{ closed bool }

func (f *fakeCIRSource2Tables) Name() string                      { return "mysql" }
func (f *fakeCIRSource2Tables) Connect(ctx context.Context) error { return nil }
func (f *fakeCIRSource2Tables) Close() error                      { f.closed = true; return nil }
func (f *fakeCIRSource2Tables) SchemaReader() source.SchemaReader { return f }
func (f *fakeCIRSource2Tables) DataReader() source.DataReader     { return nil }
func (f *fakeCIRSource2Tables) TypeMapper() source.TypeMapper     { return nil }
func (f *fakeCIRSource2Tables) IncrementalCapture() (source.IncrementalCapture, error) {
	return nil, source.ErrNotImplemented
}
func (f *fakeCIRSource2Tables) Dialect() source.Dialect { return nil }

func (f *fakeCIRSource2Tables) DB() *sql.DB {
	db, _ := sql.Open("mysql", "user:pass@tcp(127.0.0.1:1)/db")
	return db
}

func (f *fakeCIRSource2Tables) ReadSchema(ctx context.Context, opts source.Filter) (*source.Schema, error) {
	mk := func(name string) source.Table {
		return source.Table{
			Schema: "db", Name: name,
			Columns: []source.Column{{Name: "id", SourceType: "bigint", TiDBType: "BIGINT"}},
			PK:      []string{"id"},
		}
	}
	return &source.Schema{Catalog: "db", Tables: []source.Table{mk("ta"), mk("tb")}}, nil
}

func TestRunSourceCIRStreamStampsPerTableStart(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	prevOpen := cirOpenSource
	fs := &fakeCIRSource2Tables{}
	cirOpenSource = func(kind string, cfg source.SourceConfig) (source.Source, error) { return fs, nil }
	defer func() { cirOpenSource = prevOpen }()

	// Sequential two-table walk with sleeps so clock granularity can never
	// collapse the stamps (mirrors LoadData's one-table-at-a-time loop).
	prevLoad := cirLoadData
	cirLoadData = func(ctx context.Context, src source.Source, s *source.Schema, tc config.TargetConfig, dir string, cb func(string, int64)) error {
		c.load++
		time.Sleep(5 * time.Millisecond)
		cb("ta", 10)
		time.Sleep(5 * time.Millisecond)
		cb("tb", 20)
		return nil
	}
	defer func() { cirLoadData = prevLoad }()

	o := cirTestOrch()
	mgr, err := checkpoint.NewManager(filepath.Join(t.TempDir(), "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	o.cpMgr = mgr

	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err != nil {
		t.Fatalf("runSourceCIR: %v", err)
	}

	ta, ok := mgr.GetTable("ta")
	if !ok {
		t.Fatal("ta missing from checkpoint")
	}
	tb, ok := mgr.GetTable("tb")
	if !ok {
		t.Fatal("tb missing from checkpoint")
	}
	if ta.StartedAt.IsZero() || ta.FinishedAt.IsZero() {
		t.Fatalf("ta must carry both stamps: started=%v finished=%v", ta.StartedAt, ta.FinishedAt)
	}
	if tb.StartedAt.IsZero() || tb.FinishedAt.IsZero() {
		t.Fatalf("tb must carry both stamps: started=%v finished=%v", tb.StartedAt, tb.FinishedAt)
	}
	if !ta.StartedAt.Before(ta.FinishedAt) {
		t.Errorf("ta duration inverted: %v -> %v", ta.StartedAt, ta.FinishedAt)
	}
	// tb starts exactly when ta completes (sequential exporter) — never before.
	if tb.StartedAt.Before(ta.FinishedAt) {
		t.Errorf("tb started before ta finished (queue wait leaked into tb's duration): tb.start=%v ta.fin=%v", tb.StartedAt, ta.FinishedAt)
	}
	if !tb.StartedAt.Before(tb.FinishedAt) {
		t.Errorf("tb duration inverted: %v -> %v", tb.StartedAt, tb.FinishedAt)
	}
	if ta.State != checkpoint.StateCompleted || tb.State != checkpoint.StateCompleted {
		t.Errorf("states: ta=%s tb=%s, want completed", ta.State, tb.State)
	}
	if ta.RowsTotal != 10 || tb.RowsTotal != 20 {
		t.Errorf("rows: ta=%d/%d tb=%d/%d, want totals 10/20", ta.RowsTotal, ta.RowsDone, tb.RowsTotal, tb.RowsDone)
	}
}

// TestRunSourceCIRDumplingLeavesStartEmptyNote: the dumpling branch has no
// per-table start point (monolithic dump — dumpling.Dump is not seamed, so
// this shape is anchored at the checkpoint layer instead: see
// TestMarkTableCompletedWithoutRunningLeavesStartEmpty in
// internal/common/checkpoint/checkpoint_ms11n_test.go). The orchestrator's
// dumpling loop deliberately calls only GetOrCreateTable+MarkTableCompleted,
// so StartedAt stays zero and the report shows an honest "—" (reporter's
// dash anchor).
