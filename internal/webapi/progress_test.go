package webapi

import (
	"math"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestWeightedProgress(t *testing.T) {
	if !almostEqual(weightedProgress(1, 0, 0), 0.05) {
		t.Errorf("schema only = %v, want 0.05", weightedProgress(1, 0, 0))
	}
	if !almostEqual(weightedProgress(1, 1, 0), 0.525) {
		t.Errorf("export done = %v, want 0.525", weightedProgress(1, 1, 0))
	}
	if !almostEqual(weightedProgress(1, 1, 1), 1.0) {
		t.Errorf("all done = %v, want 1.0", weightedProgress(1, 1, 1))
	}
	if !almostEqual(weightedProgress(0, 0, 0), 0) {
		t.Errorf("nothing done = %v, want 0", weightedProgress(0, 0, 0))
	}
	if got := weightedProgress(1, 1, 1.5); got != 1.0 {
		t.Errorf("overflow clamp = %v, want 1.0", got)
	}
}

func TestComputeTaskProgress(t *testing.T) {
	cases := []struct {
		name          string
		phase         string
		tablesDone    int
		tablesTotal   int
		rowsDone      int64
		rowsTotal     int64
		importedTable int
		importMode    string
		want          float64
	}{
		// schema 阶段：粗粒度，接近 0~5%。
		{"schema half tables", "schema", 5, 10, 0, 0, 0, "", 0.5},
		{"schema none", "schema", 0, 10, 0, 0, 0, "", 0},
		// 导出中：≤52.5%。
		{"export half rows", "data-export", 0, 10, 500, 1000, 0, "", 0.05 + 0.475*0.5},
		{"export done", "data-export", 10, 10, 1000, 1000, 0, "", 0.525},
		{"export zero rows denom", "data-export", 0, 10, 0, 0, 0, "", 0.05},
		// lightning 导入：importFrac 只看表数，imported=0 时恒 0（绝不套 rows 比）。
		{"lightning imported=0 full rows", "data-import", 10, 10, 1000, 1000, 0, checkpoint.ImportModeLightning, 0.525},
		{"lightning imported=0 no rows", "data-import", 0, 10, 0, 1000, 0, checkpoint.ImportModeLightning, 0.05},
		{"lightning imported=3 of 10", "data-import", 10, 10, 1000, 1000, 3, checkpoint.ImportModeLightning, 0.525 + 0.475*0.3},
		{"lightning imported=10 of 10", "data-import", 10, 10, 1000, 1000, 10, checkpoint.ImportModeLightning, 1.0},
		{"lightning zero tables denom", "data-import", 0, 0, 0, 0, 3, checkpoint.ImportModeLightning, 0.05},
		// stream：exportFrac 视为 1，importFrac = rows 比。
		{"stream half rows", "data-import", 0, 10, 500, 1000, 0, checkpoint.ImportModeStream, 0.05 + 0.475 + 0.475*0.5},
		{"stream done", "data-import", 10, 10, 1000, 1000, 0, checkpoint.ImportModeStream, 1.0},
		{"stream zero denom", "data-import", 0, 10, 0, 0, 0, checkpoint.ImportModeStream, 0.05 + 0.475},
		// 空 mode（历史 checkpoint）：imported>0 按 lightning，否则按 stream。
		{"legacy imported>0 acts lightning", "data-import", 10, 10, 1000, 1000, 5, "", 0.525 + 0.475*0.5},
		{"legacy imported=0 acts stream", "data-import", 0, 10, 500, 1000, 0, "", 0.05 + 0.475 + 0.475*0.5},
		// validate 及之后：100%。
		{"validate", "validate", 10, 10, 1000, 1000, 10, checkpoint.ImportModeLightning, 1.0},
		{"completed", "completed", 10, 10, 1000, 1000, 10, "", 1.0},
		// data / data-migration：schema 完成，导入未开始。
		{"data phase", "data", 0, 10, 0, 1000, 0, "", 0.05},
		{"data-migration phase", "data-migration", 0, 10, 250, 1000, 0, "", 0.05 + 0.475*0.25},
	}
	for _, c := range cases {
		got := computeTaskProgress(c.phase, c.tablesDone, c.tablesTotal, c.rowsDone, c.rowsTotal, c.importedTable, c.importMode)
		if !almostEqual(got, c.want) {
			t.Errorf("%s: computeTaskProgress = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestComputeTaskProgressLightningMonotonicInImported(t *testing.T) {
	// Export completion lands at 52.5%; lightning import with imported=0
	// must stay exactly there (no rows-ratio leakage, no jump to 100%).
	exportDone := computeTaskProgress("data-export", 10, 10, 1000, 1000, 0, "")
	if !almostEqual(exportDone, 0.525) {
		t.Fatalf("export done = %v, want 0.525", exportDone)
	}
	atZero := computeTaskProgress("data-import", 10, 10, 1000, 1000, 0, checkpoint.ImportModeLightning)
	if !almostEqual(atZero, 0.525) {
		t.Fatalf("lightning imported=0 = %v, want 0.525 (must not jump)", atZero)
	}
	prev := atZero
	for imported := 1; imported <= 10; imported++ {
		cur := computeTaskProgress("data-import", 10, 10, 1000, 1000, imported, checkpoint.ImportModeLightning)
		if cur < prev-1e-9 {
			t.Fatalf("progress dropped: imported=%d cur=%v prev=%v", imported, cur, prev)
		}
		prev = cur
	}
	if !almostEqual(prev, 1.0) {
		t.Errorf("final = %v, want 1.0", prev)
	}
}

// TestStreamFallbackEntryStartsAtHalfway: the lightning→stream fallback
// enters importViaSQL with RowsDone still holding the export final values
// (rowsDone==rowsTotal). migrator R1 zeroes RowsDone for ALL tables at that
// entry, so the first stream-mode poll sees rowsFrac=0 and reports
// 0.525 — never 1.0. (Directly exercising importViaSQL needs a live DB, so
// this asserts the formula semantics R1 relies on.)
func TestStreamFallbackEntryStartsAtHalfway(t *testing.T) {
	// Old export terminal state, before the reset: rows ratio is 1.0.
	lethal := computeTaskProgress("data-import", 10, 10, 1000, 1000, 0, checkpoint.ImportModeStream)
	if !almostEqual(lethal, 1.0) {
		t.Fatalf("pre-R1 stream entry with rowsFrac=1.0 = %v, want 1.0 (demonstrates the bug)", lethal)
	}
	// After R1 the entry point guarantees rowsFrac=0 on the first poll.
	got := computeTaskProgress("data-import", 10, 10, 0, 1000, 0, checkpoint.ImportModeStream)
	if !almostEqual(got, 0.525) {
		t.Fatalf("stream entry after R1 (rowsFrac=0) = %v, want 0.525", got)
	}
}

// TestR1bModeFlipAfterRowsReset documents the R1-b ordering guarantee: the
// forbidden intermediate checkpoint state "mode=stream + rowsDone==rowsTotal"
// computes to 1.0, so migrator persists the RowsDone reset BEFORE flipping
// ImportMode to stream. During the reset window the poller still sees
// mode=lightning + imported=0 → 0.525; after the flip rowsFrac is already 0
// → 0.525. The combination below is therefore unreachable in the real
// sequence (kept here to pin down why the ordering matters).
func TestR1bModeFlipAfterRowsReset(t *testing.T) {
	forbidden := computeTaskProgress("data-import", 10, 10, 1000, 1000, 0, checkpoint.ImportModeStream)
	if !almostEqual(forbidden, 1.0) {
		t.Fatalf("forbidden intermediate (stream + rowsFrac=1.0) = %v, want 1.0 (why R1-b ordering exists)", forbidden)
	}
	// The two states actually observable around the reset window:
	duringReset := computeTaskProgress("data-import", 10, 10, 1000, 1000, 0, checkpoint.ImportModeLightning)
	if !almostEqual(duringReset, 0.525) {
		t.Fatalf("during reset (lightning + imported=0) = %v, want 0.525", duringReset)
	}
	afterFlip := computeTaskProgress("data-import", 10, 10, 0, 1000, 0, checkpoint.ImportModeStream)
	if !almostEqual(afterFlip, 0.525) {
		t.Fatalf("after mode flip (stream + rowsFrac=0) = %v, want 0.525", afterFlip)
	}
}

// TestLightningToStreamFallbackTimeline walks the exact checkpoint sequence
// of a fallback run and asserts progress never hits 1.0 early and climbs
// monotonically after the (accepted) honest 0.6→0.525 dip at the switch.
func TestLightningToStreamFallbackTimeline(t *testing.T) {
	seq := []struct {
		name       string
		phase      string
		tablesDone int
		rowsDone   int64
		rowsTotal  int64
		imported   int
		mode       string
		want       float64
	}{
		{"export done", "data-export", 10, 1000, 1000, 0, "", 0.525},
		{"lightning underway (2/10 tables)", "data-import", 10, 1000, 1000, 2, checkpoint.ImportModeLightning, 0.525 + 0.475*0.2},
		{"fallback: stream entry, R1 zeroed rows", "data-import", 10, 0, 1000, 0, checkpoint.ImportModeStream, 0.525},
		{"stream half rows", "data-import", 10, 500, 1000, 0, checkpoint.ImportModeStream, 0.525 + 0.475*0.5},
		{"stream all rows", "data-import", 10, 1000, 1000, 0, checkpoint.ImportModeStream, 1.0},
	}
	prev := 0.0
	for i, s := range seq {
		got := computeTaskProgress(s.phase, s.tablesDone, 10, s.rowsDone, s.rowsTotal, s.imported, s.mode)
		if !almostEqual(got, s.want) {
			t.Fatalf("step %d (%s): progress = %v, want %v", i, s.name, got, s.want)
		}
		if got >= 1.0 && i < len(seq)-1 {
			t.Fatalf("step %d (%s): progress hit 1.0 before the run finished", i, s.name)
		}
		// The single accepted non-monotonic step is the fallback switch
		// itself (step 2, honest 0.62→0.525 dip); everything after the
		// switch must climb.
		if i > 2 && got < prev-1e-9 {
			t.Fatalf("step %d (%s): progress dropped after fallback switch: %v < %v", i, s.name, got, prev)
		}
		prev = got
	}
}

// TestImportModeSetBeforeImportPhase documents the R2 ordering guarantee:
// the migrator's Run records ImportMode=lightning before SetPhase(
// "data-import"), and importViaSQL/importViaLightning re-set the mode on
// entry as the fallback override. So a checkpoint with phase=data-import
// never observes mode=="" from a fresh run — that combination only exists
// for historical checkpoints written before the field existed (handled in
// computeTaskProgress). Run/migrator sequencing is not directly unit-
// testable without a live DB, hence the assertion on the compatibility
// fallback rather than the ordering itself.
func TestImportModeSetBeforeImportPhase(t *testing.T) {
	// Historical checkpoint (mode=="" && imported==0) still gets a sane
	// stream-formula value rather than an indeterminate one.
	got := computeTaskProgress("data-import", 5, 10, 200, 1000, 0, "")
	if !almostEqual(got, 0.525+0.475*0.2) {
		t.Fatalf("legacy empty-mode = %v, want %v", got, 0.525+0.475*0.2)
	}
}
