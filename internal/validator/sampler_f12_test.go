package validator

import "testing"

// TestSampleOffset anchors the F-12 sampler defense: the offset span must
// never go non-positive into rand.Int63n (the production panic shape was
// sample_ratio=100 against a table under 1000 rows — sampleSize clamped to
// 1000 > SourceRows → negative argument → process-wide panic).
func TestSampleOffset(t *testing.T) {
	// Whole-window shapes: small table, ratio > 1 in effect.
	if got := sampleOffset(20, 1000); got != 0 {
		t.Fatalf("sampleOffset(20,1000) = %d, want 0 (no panic path)", got)
	}
	if got := sampleOffset(1, 1); got != 0 {
		t.Fatalf("sampleOffset(1,1) = %d, want 0", got)
	}
	if got := sampleOffset(0, 1); got != 0 {
		t.Fatalf("sampleOffset(0,1) = %d, want 0", got)
	}
	// Normal shape: offset stays inside [0, rows-sampleSize].
	for _, c := range [][2]int64{{1000, 100}, {10, 5}, {1000000, 1000}} {
		got := sampleOffset(c[0], int(c[1]))
		if got < 0 || got > c[0]-c[1] {
			t.Fatalf("sampleOffset(%d,%d) = %d, out of range", c[0], c[1], got)
		}
	}
}
