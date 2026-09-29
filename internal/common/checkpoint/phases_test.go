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

// TestFinishPhaseWithReloadDualInstance anchors the runData stomp bug:
// the data migrator writes through its OWN checkpoint.Manager (manager A)
// while the orchestrator holds a second manager (B) whose in-memory copy
// went stale when the data phase started. When B finishes the "data"
// phase, it must NOT overwrite A's persisted data-plane progress.
// FinishPhaseWithReload reloads from disk first and preserves it; the
// control case shows plain FinishPhase does stomp (that is why runData
// must use the reload variant).
func TestFinishPhaseWithReloadDualInstance(t *testing.T) {
	seedAndFinish := func(finish func(m *Manager, name string, err error, warn bool) error) *Manager {
		dir := t.TempDir()
		// Manager B = the ORCHESTRATOR's instance, created at pipeline
		// start: it seeds the phases and records the schema outcome, then
		// never reloads — stale from the schema era onward.
		b, err := NewManager(dir)
		if err != nil {
			t.Fatalf("NewManager(B): %v", err)
		}
		if err := b.InitPhases(map[string]bool{"precheck": false, "schema": false, "data": false, "validate": false}); err != nil {
			t.Fatal(err)
		}
		if err := b.StartPhase("schema"); err != nil {
			t.Fatal(err)
		}
		if err := b.FinishPhase("schema", nil, false); err != nil {
			t.Fatal(err)
		}

		// Manager A = the DATA MIGRATOR's own instance, created at data
		// phase start (loads schema-era state), then writes the data
		// plane: table progress, imported counts, import mode, sub-phase.
		a, err := NewManager(dir)
		if err != nil {
			t.Fatalf("NewManager(A): %v", err)
		}
		for _, tbl := range []string{"orders", "customers", "products"} {
			a.GetOrCreateTable(tbl, 100)
			if err := a.MarkTableCompleted(tbl, 100); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.SetImportedTables(3); err != nil {
			t.Fatal(err)
		}
		if err := a.SetImportMode(ImportModeLightning); err != nil {
			t.Fatal(err)
		}
		if err := a.SetSubPhase("data", "data-import"); err != nil {
			t.Fatal(err)
		}

		// Orchestrator's B finishes the data phase from its stale copy.
		if err := finish(b, "data", nil, false); err != nil {
			t.Fatal(err)
		}

		// Verify through a fresh reader of the on-disk file.
		r, err := NewManager(dir)
		if err != nil {
			t.Fatalf("NewManager(reader): %v", err)
		}
		return r
	}

	// Fixed path: reload variant preserves A's data-plane progress.
	r := seedAndFinish(func(m *Manager, name string, err error, warn bool) error {
		return m.FinishPhaseWithReload(name, err, warn)
	})
	tables := r.GetAllTables()
	if len(tables) != 3 {
		t.Errorf("tables after reload-finish = %d, want 3 (stomped?)", len(tables))
	}
	for _, tbl := range []string{"orders", "customers", "products"} {
		tc, ok := tables[tbl]
		if !ok {
			t.Fatalf("table %s missing after reload-finish", tbl)
		}
		if tc.State != StateCompleted {
			t.Errorf("%s state = %s, want completed", tbl, tc.State)
		}
		if tc.RowsDone != 100 || tc.RowsTotal != 100 {
			t.Errorf("%s rows = %d/%d, want 100/100", tbl, tc.RowsDone, tc.RowsTotal)
		}
	}
	if got := r.GetImportedTables(); got != 3 {
		t.Errorf("imported_tables = %d, want 3", got)
	}
	if got := r.GetImportMode(); got != ImportModeLightning {
		t.Errorf("import_mode = %q, want %q", got, ImportModeLightning)
	}
	phases := r.GetPhases()
	if phases["data"].Status != StateCompleted {
		t.Errorf("data phase = %s, want completed", phases["data"].Status)
	}
	if phases["data"].SubPhase != "data-import" {
		t.Errorf("data sub-phase = %q, want data-import", phases["data"].SubPhase)
	}
	if phases["schema"].Status != StateCompleted {
		t.Errorf("schema phase = %s, want completed (must survive reload)", phases["schema"].Status)
	}

	// Control: plain FinishPhase (old behavior) stomps the data plane —
	// pins the exact difference the reload variant exists to fix.
	r2 := seedAndFinish(func(m *Manager, name string, err error, warn bool) error {
		return m.FinishPhase(name, err, warn)
	})
	tables2 := r2.GetAllTables()
	if len(tables2) != 0 {
		t.Errorf("control: tables = %d, want 0 (expected stomp with plain FinishPhase — if this now passes, the anchor's premise changed)", len(tables2))
	}
	if got := r2.GetImportedTables(); got != 0 {
		t.Errorf("control: imported_tables = %d, want 0 (stomped)", got)
	}
}
