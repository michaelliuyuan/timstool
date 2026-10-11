package checkpoint

import (
	"path/filepath"
	"testing"
)

// The progress denominator (RowsTotal) must be raised whenever rowsDone
// exceeds it, so aggregated progress never exceeds 100% when a table exports
// more rows than its registered estimate.

func TestMarkTableCompletedRaisesDenominator(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)
	m.GetOrCreateTable("t1", 100)

	if err := m.MarkTableCompleted("t1", 120); err != nil {
		t.Fatal(err)
	}
	tc, _ := m.GetTable("t1")
	if tc.RowsTotal != 120 {
		t.Errorf("expected RowsTotal raised to 120, got %d", tc.RowsTotal)
	}
	if tc.RowsDone != 120 {
		t.Errorf("expected RowsDone 120, got %d", tc.RowsDone)
	}
	if tc.Progress() > 1.0 {
		t.Errorf("progress must not exceed 1.0, got %f", tc.Progress())
	}
}

func TestMarkTableCompletedKeepsLargerDenominator(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)
	m.GetOrCreateTable("t1", 200)

	if err := m.MarkTableCompleted("t1", 150); err != nil {
		t.Fatal(err)
	}
	tc, _ := m.GetTable("t1")
	if tc.RowsTotal != 200 {
		t.Errorf("expected RowsTotal to stay 200, got %d", tc.RowsTotal)
	}
	if tc.RowsDone != 150 {
		t.Errorf("expected RowsDone 150, got %d", tc.RowsDone)
	}
}

func TestUpdateTableProgressRaisesDenominator(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)
	m.GetOrCreateTable("t1", 50)

	if err := m.UpdateTableProgress("t1", 80, 1024); err != nil {
		t.Fatal(err)
	}
	tc, _ := m.GetTable("t1")
	if tc.RowsTotal != 80 {
		t.Errorf("expected RowsTotal raised to 80, got %d", tc.RowsTotal)
	}
	if tc.BytesDone != 1024 {
		t.Errorf("expected BytesDone 1024, got %d", tc.BytesDone)
	}
	if tc.Progress() > 1.0 {
		t.Errorf("progress must not exceed 1.0, got %f", tc.Progress())
	}
}
