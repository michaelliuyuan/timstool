package checkpoint

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPhaseStateMachine anchors the P1 lifecycle: InitPhases seeds pending
// + skipped, StartPhase flips running, FinishPhase records completed /
// failed / warn, SetSubPhase tracks the current sub-step, and GetPhases
// returns a copy (mutating it must not leak into the manager).
func TestPhaseStateMachine(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := m.InitPhases(map[string]bool{
		"precheck": false,
		"schema":   false,
		"data":     true, // user skipped
		"validate": false,
	}); err != nil {
		t.Fatal(err)
	}

	phases := m.GetPhases()
	if phases["precheck"].Status != StatePending {
		t.Errorf("precheck = %s, want pending", phases["precheck"].Status)
	}
	if phases["data"].Status != StateSkipped {
		t.Errorf("data = %s, want skipped", phases["data"].Status)
	}

	// precheck runs clean.
	if err := m.StartPhase("precheck"); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishPhase("precheck", nil, false); err != nil {
		t.Fatal(err)
	}

	// schema runs with a tolerated error → completed-with-warn.
	if err := m.StartPhase("schema"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSubPhase("schema", "schema-execute"); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishPhase("schema", nil, true); err != nil {
		t.Fatal(err)
	}

	// validate fails.
	if err := m.StartPhase("validate"); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishPhase("validate", errors.New("boom"), false); err != nil {
		t.Fatal(err)
	}

	phases = m.GetPhases()
	if phases["precheck"].Status != StateCompleted || phases["precheck"].Warn {
		t.Errorf("precheck = %+v, want completed no-warn", phases["precheck"])
	}
	if phases["schema"].Status != StateCompleted || !phases["schema"].Warn {
		t.Errorf("schema = %+v, want completed with warn", phases["schema"])
	}
	if phases["schema"].SubPhase != "schema-execute" {
		t.Errorf("schema sub_phase = %q, want schema-execute", phases["schema"].SubPhase)
	}
	if phases["validate"].Status != StateFailed || phases["validate"].Error != "boom" {
		t.Errorf("validate = %+v, want failed/boom", phases["validate"])
	}
	if phases["data"].Status != StateSkipped {
		t.Errorf("data = %s, want skipped", phases["data"].Status)
	}
	for _, name := range []string{"precheck", "schema", "validate"} {
		if phases[name].FinishedAt.IsZero() || phases[name].StartedAt.IsZero() {
			t.Errorf("%s timestamps not set: %+v", name, phases[name])
		}
		if phases[name].FinishedAt.Before(phases[name].StartedAt) {
			t.Errorf("%s finished before started: %+v", name, phases[name])
		}
	}

	// Copy semantics: mutating the returned map must not affect the manager.
	phases["data"].Status = StateCompleted
	if m.GetPhases()["data"].Status != StateSkipped {
		t.Error("GetPhases must return a copy")
	}

	// Durations are positive (allow clock granularity).
	if d := phases["precheck"].FinishedAt.Sub(phases["precheck"].StartedAt); d < 0 || d > 5*time.Second {
		t.Errorf("precheck duration = %v, implausible", d)
	}
}

// TestPhasesLegacyCheckpointCompat: checkpoints written before the phases
// field existed (no "phases" key) must load cleanly with a nil/empty map —
// consumers detect this and fall back to ordinal inference.
func TestPhasesLegacyCheckpointCompat(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
  "version": "1.0",
  "created_at": "2026-09-01T00:00:00Z",
  "updated_at": "2026-09-01T00:00:00Z",
  "phase": "data",
  "tables": {"t1": {"table_name": "t1", "state": "pending", "rows_done": 0, "rows_total": 10, "started_at": "2026-09-01T00:00:00Z", "updated_at": "2026-09-01T00:00:00Z"}},
  "imported_tables": 0,
  "import_mode": "lightning"
}`
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := NewReadOnlyManager(dir)
	if err != nil {
		t.Fatalf("NewReadOnlyManager: %v", err)
	}
	if got := m.GetPhases(); len(got) != 0 {
		t.Errorf("legacy checkpoint phases = %v, want empty (fallback path)", got)
	}
	// Round-trip: a legacy JSON blob must also unmarshal into Checkpoint
	// without error (schema evolution safe).
	var cp Checkpoint
	if err := json.Unmarshal([]byte(legacy), &cp); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if cp.Phases != nil {
		t.Errorf("legacy phases = %v, want nil", cp.Phases)
	}
}
