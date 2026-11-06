package webapi

// compare_chunked_knobs_test.go anchors the BUG-CMP-COUNT-P1 A6 wiring:
// the standalone compare task's compare override inherits ONLY the two
// chunked-count knobs from the live config.yaml snapshot, so the UI path
// gets the engine defaults (or the operator's explicit opt-out) instead of
// the zero value — which would silently disable chunked counting on the
// exact path the bug was reported from.
//
//	W1  explicit 0 in the file → 0 reaches the UI task (runtime opt-out).
//	W2  absent keys → config.Load's DefaultConfig prefill (5M/1M).
//	W3  custom values pass through verbatim.
//	W4  snapshot unavailable (not wired / read failure) → explicit
//	    DefaultConfig fallback, never the zero value.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"go.uber.org/zap"
)

func inheritHarness(t *testing.T, yamlBody string) func() (*config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return func() (*config.Config, error) { return config.Load(path) }
}

func TestInheritChunkedCountKnobs(t *testing.T) {
	t.Run("W1 explicit zero disables on the UI path", func(t *testing.T) {
		snap := inheritHarness(t, "compare:\n  chunked_count_threshold: 0\n")
		cc := config.CompareConfig{}
		inheritChunkedCountKnobs(&cc, snap, zap.NewNop())
		if cc.ChunkedCountThreshold != 0 {
			t.Fatalf("explicit 0 must reach the UI task, got %d", cc.ChunkedCountThreshold)
		}
		if cc.ChunkedCountTarget != 1000000 {
			t.Fatalf("target keeps its default, got %d", cc.ChunkedCountTarget)
		}
	})
	t.Run("W2 absent keys inherit the Load defaults", func(t *testing.T) {
		snap := inheritHarness(t, "source:\n  host: db\n")
		cc := config.CompareConfig{}
		inheritChunkedCountKnobs(&cc, snap, zap.NewNop())
		if cc.ChunkedCountThreshold != 5000000 || cc.ChunkedCountTarget != 1000000 {
			t.Fatalf("absent keys = %d/%d, want 5000000/1000000",
				cc.ChunkedCountThreshold, cc.ChunkedCountTarget)
		}
	})
	t.Run("W3 custom values pass through", func(t *testing.T) {
		snap := inheritHarness(t, "compare:\n  chunked_count_threshold: 12345\n  chunked_count_target: 200000\n")
		cc := config.CompareConfig{}
		inheritChunkedCountKnobs(&cc, snap, zap.NewNop())
		if cc.ChunkedCountThreshold != 12345 || cc.ChunkedCountTarget != 200000 {
			t.Fatalf("custom = %d/%d, want 12345/200000",
				cc.ChunkedCountThreshold, cc.ChunkedCountTarget)
		}
	})
	t.Run("W4 snapshot unavailable falls back to defaults", func(t *testing.T) {
		snap := func() (*config.Config, error) { return nil, os.ErrNotExist }
		cc := config.CompareConfig{}
		inheritChunkedCountKnobs(&cc, snap, zap.NewNop())
		if cc.ChunkedCountThreshold != 5000000 || cc.ChunkedCountTarget != 1000000 {
			t.Fatalf("fallback = %d/%d, want the explicit 5000000/1000000 (never zero)",
				cc.ChunkedCountThreshold, cc.ChunkedCountTarget)
		}
	})
}
