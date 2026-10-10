package checkpoint

// MS-11p anchor: the honest-flip semantics at the stamp layer — a failed
// table that genuinely completes flips green with a NEW FinishedAt and its
// stale failure note cleared (never green with a lingering error), while the
// sticky guard upstream keeps FIRST completions immutable.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMarkTableCompletedFlipsFailedClearsErrorNewStamp(t *testing.T) {
	mgr, err := NewManager(filepath.Join(t.TempDir(), "cp"))
	if err != nil {
		t.Fatal(err)
	}
	mgr.GetOrCreateTable("ta", 0)
	_ = mgr.MarkTableRunning("ta")
	_ = mgr.MarkTableFailed("ta", "prior run export aborted")
	failed, _ := mgr.GetTable("ta")
	failedFin := failed.FinishedAt

	if err := mgr.MarkTableCompleted("ta", 7); err != nil {
		t.Fatal(err)
	}
	got, _ := mgr.GetTable("ta")
	if got.State != StateCompleted {
		t.Fatalf("state = %s, want completed (honest flip)", got.State)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want stale failure cleared", got.Error)
	}
	if !got.FinishedAt.After(failedFin) {
		t.Fatalf("FinishedAt = %v, want NEW stamp after %v", got.FinishedAt, failedFin)
	}
	if got.RowsDone != 7 {
		t.Fatalf("rows = %d, want 7", got.RowsDone)
	}
}

// Guard rail: the sticky-completion semantics stay intact — a second
// completion mark on an already completed table never rewrites the stamp.
func TestMarkTableCompletedStickyStillHolds(t *testing.T) {
	mgr, err := NewManager(filepath.Join(t.TempDir(), "cp"))
	if err != nil {
		t.Fatal(err)
	}
	mgr.GetOrCreateTable("tb", 0)
	_ = mgr.MarkTableRunning("tb")
	_ = mgr.MarkTableCompleted("tb", 3)
	first, _ := mgr.GetTable("tb")
	firstFin := first.FinishedAt

	time.Sleep(2 * time.Millisecond)
	_ = mgr.MarkTableCompleted("tb", 5)
	got, _ := mgr.GetTable("tb")
	if got.FinishedAt != firstFin {
		t.Fatalf("sticky guard broken: %v -> %v", firstFin, got.FinishedAt)
	}
	if got.RowsDone != 5 || got.RowsTotal < 5 {
		t.Fatalf("rows did not converge: done=%d total=%d", got.RowsDone, got.RowsTotal)
	}
	if strings.TrimSpace(got.Error) != "" {
		t.Fatalf("error = %q, want empty", got.Error)
	}
}
