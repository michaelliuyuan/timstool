package orchestrator

// MS-11o A1 anchor: the CIR stream path's chained export pre-stamps the
// next table Running as each table completes. When the export chain aborts
// mid-table, the pre-stamped Running table must land failed with the abort
// error — never stay Running forever (which the migration report renders as
// skip, masking the failure). Tables that completed before the break keep
// their honest green.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/source"
)

func TestRunSourceCIRStreamAbortStampsRunningFailed(t *testing.T) {
	var c cirCounters
	restore := installCIRSeams(&c)
	defer restore()

	prevOpen := cirOpenSource
	fs := &fakeCIRSource2Tables{}
	cirOpenSource = func(kind string, cfg source.SourceConfig) (source.Source, error) { return fs, nil }
	defer func() { cirOpenSource = prevOpen }()

	// ta completes; the completion callback pre-stamps tb Running; then the
	// export chain dies mid-tb (the exact stale-Running shape from the
	// MS-11n observation pool).
	prevLoad := cirLoadData
	cirLoadData = func(ctx context.Context, src source.Source, s *source.Schema, tc config.TargetConfig, dir string, cb func(string, int64)) error {
		c.load++
		cb("ta", 10)
		time.Sleep(5 * time.Millisecond)
		return errors.New("connection reset mid-export")
	}
	defer func() { cirLoadData = prevLoad }()

	o := cirTestOrch()
	mgr, err := checkpoint.NewManager(filepath.Join(t.TempDir(), "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	o.cpMgr = mgr

	if _, err := o.runSourceCIR(context.Background(), PipelineConfig{}); err == nil || !strings.Contains(err.Error(), "load data") {
		t.Fatalf("err = %v, want propagated load-data error", err)
	}

	// Zero Running residue: the aborted table must be terminal.
	completed, failed, pending, running := mgr.Summary()
	if running != 0 {
		t.Fatalf("running residue = %d, want 0 (completed=%d failed=%d pending=%d)", running, completed, failed, pending)
	}

	ta, ok := mgr.GetTable("ta")
	if !ok {
		t.Fatal("ta missing from checkpoint")
	}
	if ta.State != checkpoint.StateCompleted {
		t.Errorf("ta state = %s, want completed (honest green before the break is preserved)", ta.State)
	}

	tb, ok := mgr.GetTable("tb")
	if !ok {
		t.Fatal("tb missing from checkpoint (must exist — pre-stamped Running before the abort)")
	}
	if tb.State != checkpoint.StateFailed {
		t.Errorf("tb state = %s, want failed (report must show fail, not skip)", tb.State)
	}
	if tb.Error == "" || !strings.Contains(tb.Error, "export aborted") {
		t.Errorf("tb error = %q, want abort semantics with the underlying error", tb.Error)
	}
	if tb.FinishedAt.IsZero() {
		t.Error("tb FinishedAt must be stamped (terminal state carries both stamps)")
	}
	if completed != 1 || failed != 1 {
		t.Errorf("summary completed=%d failed=%d, want 1/1", completed, failed)
	}
}
