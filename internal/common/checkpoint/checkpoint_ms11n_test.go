package checkpoint

// MS-11n anchors: ① sticky first-completion FinishedAt in MarkTableCompleted
// (a later re-mark only converges row counters, mirroring
// MarkSchemaTableCompleted), ② UpdateTable's pure-field contract (no implicit
// state/timestamp side effects — the Lightning-end refresh loop relies on it),
// and b) the Lightning-end convergence keeping each table's FinishedAt
// distinct and first-true.

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMarkTableCompletedStickyFinishedAt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)
	m.GetOrCreateTable("t1", 100)
	if err := m.MarkTableRunning("t1"); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkTableCompleted("t1", 100); err != nil {
		t.Fatal(err)
	}
	tc, _ := m.GetTable("t1")
	firstFin := tc.FinishedAt
	if firstFin.IsZero() {
		t.Fatal("first completion must stamp FinishedAt")
	}

	time.Sleep(5 * time.Millisecond)
	if err := m.MarkTableCompleted("t1", 120); err != nil {
		t.Fatal(err)
	}
	tc, _ = m.GetTable("t1")
	if !tc.FinishedAt.Equal(firstFin) {
		t.Errorf("re-mark rewrote FinishedAt: first=%v now=%v (sticky guard must preserve the first completion)", firstFin, tc.FinishedAt)
	}
	if tc.State != StateCompleted {
		t.Errorf("state = %s, want completed", tc.State)
	}
	if tc.RowsTotal != 120 || tc.RowsDone != 120 {
		t.Errorf("rows must still converge on re-mark: RowsTotal=%d RowsDone=%d, want 120/120", tc.RowsTotal, tc.RowsDone)
	}
}

func TestUpdateTableLeavesStateAndStampsUntouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)
	m.GetOrCreateTable("t1", 300)
	if err := m.MarkTableRunning("t1"); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkTableCompleted("t1", 250); err != nil {
		t.Fatal(err)
	}
	before, _ := m.GetTable("t1")
	snapState, snapStart, snapFin := before.State, before.StartedAt, before.FinishedAt

	if err := m.UpdateTable("t1", func(tc *TableCheckpoint) {
		tc.RowsDone = tc.RowsTotal
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := m.GetTable("t1")
	if after.State != snapState || after.State != StateCompleted {
		t.Errorf("UpdateTable changed state: %s -> %s", snapState, after.State)
	}
	if !after.StartedAt.Equal(snapStart) {
		t.Errorf("UpdateTable changed StartedAt: %v -> %v", snapStart, after.StartedAt)
	}
	if !after.FinishedAt.Equal(snapFin) {
		t.Errorf("UpdateTable changed FinishedAt: %v -> %v", snapFin, after.FinishedAt)
	}
	if after.RowsDone != 300 {
		t.Errorf("RowsDone = %d, want 300 (fn must be applied)", after.RowsDone)
	}
}

// TestMarkTableCompletedWithoutRunningLeavesStartEmpty: a completion stamped
// without a prior running mark (the CIR dumpling shape — monolithic dump, no
// per-table start point) must leave StartedAt zero, so the report renders an
// honest "—" rather than a fabricated duration.
func TestMarkTableCompletedWithoutRunningLeavesStartEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)
	m.GetOrCreateTable("t1", 7)
	if err := m.MarkTableCompleted("t1", 7); err != nil {
		t.Fatal(err)
	}
	tc, _ := m.GetTable("t1")
	if !tc.StartedAt.IsZero() {
		t.Errorf("StartedAt must stay zero when no start was stamped, got %v", tc.StartedAt)
	}
	if tc.FinishedAt.IsZero() {
		t.Error("completion stamp must still land")
	}
	if tc.State != StateCompleted || tc.RowsDone != 7 {
		t.Errorf("state=%s rows=%d, want completed/7", tc.State, tc.RowsDone)
	}
}

func TestLightningEndConvergenceKeepsDistinctFinishedAt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)
	for _, name := range []string{"ta", "tb"} {
		m.GetOrCreateTable(name, 50)
		_ = m.MarkTableRunning(name)
	}
	if err := m.MarkTableCompleted("ta", 50); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := m.MarkTableCompleted("tb", 50); err != nil {
		t.Fatal(err)
	}
	snap := map[string]time.Time{}
	for _, name := range []string{"ta", "tb"} {
		tc, _ := m.GetTable(name)
		snap[name] = tc.FinishedAt
	}
	if !snap["ta"].Before(snap["tb"]) {
		t.Fatalf("setup requires distinct FinishedAt, got ta=%v tb=%v", snap["ta"], snap["tb"])
	}

	// The Lightning-end refresh loop's exact convergence ops (data/migrator.go):
	// settle RowsDone=RowsTotal on completed tables only — no completion
	// re-stamp, so every table keeps its own first-true finish time.
	for _, name := range []string{"ta", "tb"} {
		m.GetOrCreateTable(name, 0)
		if err := m.UpdateTable(name, func(tc *TableCheckpoint) {
			if tc.State == StateCompleted {
				tc.RowsDone = tc.RowsTotal
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ta", "tb"} {
		tc, _ := m.GetTable(name)
		if !tc.FinishedAt.Equal(snap[name]) {
			t.Errorf("%s: FinishedAt moved %v -> %v after Lightning refresh", name, snap[name], tc.FinishedAt)
		}
		if tc.State != StateCompleted || tc.RowsDone != tc.RowsTotal {
			t.Errorf("%s: state=%s rows=%d/%d, want completed with RowsDone=RowsTotal", name, tc.State, tc.RowsDone, tc.RowsTotal)
		}
	}
	ta, _ := m.GetTable("ta")
	tb, _ := m.GetTable("tb")
	if !ta.FinishedAt.Before(tb.FinishedAt) {
		t.Errorf("per-table FinishedAt must stay distinct after refresh: ta=%v tb=%v", ta.FinishedAt, tb.FinishedAt)
	}

	// Even a stray full re-mark must not flatten the stamps (① double insurance).
	if err := m.MarkTableCompleted("ta", 50); err != nil {
		t.Fatal(err)
	}
	tc, _ := m.GetTable("ta")
	if !tc.FinishedAt.Equal(snap["ta"]) {
		t.Errorf("stray re-mark rewrote FinishedAt: %v -> %v", snap["ta"], tc.FinishedAt)
	}
}
