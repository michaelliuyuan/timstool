package checkpoint

import (
	"path/filepath"
	"testing"
)

// TestSchemaStateIndependentOfDataResume anchors the schema-progress design:
// the schema phase marks SchemaState only — the data-resume cursor (State),
// row counters and resume decisions must be untouched, so pause/resume of the
// data phase behaves exactly as before schema reporting existed.
func TestSchemaStateIndependentOfDataResume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)

	// Pre-existing data entry (resumed task): completed with real rows.
	if tc := m.GetOrCreateTable("users", 5000); tc == nil {
		t.Fatal("GetOrCreateTable users")
	}
	if err := m.MarkTableCompleted("users", 5000); err != nil {
		t.Fatal(err)
	}

	// Schema registration over the same names + one new table.
	if err := m.RegisterSchemaTables([]string{"users", "orders"}); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSchemaTableCompleted("users"); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSchemaTableCompleted("orders"); err != nil {
		t.Fatal(err)
	}

	users, ok := m.GetTable("users")
	if !ok {
		t.Fatal("users missing")
	}
	if users.State != StateCompleted || users.RowsDone != 5000 || users.RowsTotal != 5000 {
		t.Errorf("data cursor changed by schema marks: %+v", users)
	}
	if users.SchemaState != StateCompleted {
		t.Errorf("schema state = %v, want completed", users.SchemaState)
	}

	// Fresh schema entry carries no data state/denominators.
	orders, _ := m.GetTable("orders")
	if orders.State != StatePending || orders.RowsTotal != 0 || orders.RowsDone != 0 {
		t.Errorf("schema registration polluted data fields: %+v", orders)
	}

	// Resume semantics unchanged: pending data tables still listed, completed
	// still skipped.
	if m.IsTableCompleted("users") != true || m.IsTableCompleted("orders") != false {
		t.Error("IsTableCompleted semantics changed")
	}
	pending := m.GetPendingTables()
	if len(pending) != 1 || pending[0] != "orders" {
		t.Errorf("pending tables = %v, want [orders]", pending)
	}
}

func TestSchemaStateFailedAndReset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, _ := NewManager(dir)

	if err := m.RegisterSchemaTables([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSchemaTableFailed("a", "boom"); err != nil {
		t.Fatal(err)
	}
	a, _ := m.GetTable("a")
	if a.SchemaState != StateFailed {
		t.Errorf("schema state = %v, want failed", a.SchemaState)
	}

	m.ResetAllTables()
	a, _ = m.GetTable("a")
	if a.SchemaState != "" || a.State != StatePending {
		t.Errorf("ResetAllTables did not clear schema state: %+v", a)
	}
}
