package cdc

import (
	"strings"
	"testing"
)

// TestMakeDDLIdempotent covers the at-least-once DDL replay guard (#t59 §4.2):
// CREATE/DROP become IF NOT EXISTS / IF EXISTS; ALTER and already-guarded
// statements are left alone.
func TestMakeDDLIdempotent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"create adds IF NOT EXISTS", "CREATE TABLE t (id int)", "CREATE TABLE IF NOT EXISTS t (id int)"},
		{"create already guarded", "CREATE TABLE IF NOT EXISTS t (id int)", "CREATE TABLE IF NOT EXISTS t (id int)"},
		{"drop adds IF EXISTS", "DROP TABLE t", "DROP TABLE IF EXISTS t"},
		{"drop already guarded", "DROP TABLE IF EXISTS t", "DROP TABLE IF EXISTS t"},
		{"alter unchanged (non-idempotent; checkpoint protects)", "ALTER TABLE t ADD COLUMN c int", "ALTER TABLE t ADD COLUMN c int"},
		{"lowercase keyword handled", "create table t (id int)", "CREATE TABLE IF NOT EXISTS t (id int)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := makeDDLIdempotent(c.in); got != c.want {
				t.Errorf("makeDDLIdempotent(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestDDLTransform_TypeMappingAndIdempotency checks the full TABLE transform:
// PG→TiDB type mapping still applies AND the IF NOT EXISTS guard is added.
func TestDDLTransform_TypeMappingAndIdempotency(t *testing.T) {
	dt := NewDDLTransformer()
	got := dt.Transform("CREATE TABLE t (id SERIAL PRIMARY KEY, data JSONB, uid UUID)", "TABLE")
	for _, sub := range []string{"CREATE TABLE IF NOT EXISTS", "BIGINT AUTO_INCREMENT", "JSON", "CHAR(36)"} {
		if !strings.Contains(got, sub) {
			t.Errorf("Transform: output %q missing %q", got, sub)
		}
	}
	if strings.Contains(got, "SERIAL") {
		t.Errorf("Transform: SERIAL not mapped, got %q", got)
	}
}

// TestIsToolBookkeepingObject anchors the self-DDL replay filter (P1
// production incident 2026-09-27): the tool's own migration ALTER on
// pg2tidb_ddl_log was captured by its own event trigger and replayed to TiDB,
// where `status TEXT NOT NULL DEFAULT` fails with 1101 (non-degradable) and
// halts the runner in a revive-halt crash loop. All pg2tidb_* objects —
// schema-qualified, quoted, upper-cased — must be filtered; user tables must
// not be.
func TestIsToolBookkeepingObject(t *testing.T) {
	for _, name := range []string{
		"pg2tidb_ddl_log",
		"public.pg2tidb_ddl_log",
		`"public".pg2tidb_ddl_log`,
		"PG2TIDB_DDL_LOG",
		"public.PG2TIDB_DDL_Trigger",
		"pg2tidb_ddl_capture",
		" pg2tidb_ddl_log ",
	} {
		if !isToolBookkeepingObject(name) {
			t.Errorf("isToolBookkeepingObject(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"users",
		"public.users",
		`"public"."users"`,
		"order_items",
		"xpg2tidb_foo", // prefix must anchor at the name segment, not a substring
		"",
	} {
		if isToolBookkeepingObject(name) {
			t.Errorf("isToolBookkeepingObject(%q) = true, want false", name)
		}
	}
}

// TestDDLLogSQLDualLegal anchors the P1 hotfix: the tool's own CREATE/ALTER
// migration DDL is captured by its own event trigger and replayed to TiDB,
// so no TEXT column may carry a DEFAULT (TiDB err 1101, non-degradable halt).
func TestDDLLogSQLDualLegal(t *testing.T) {
	for name, sql := range map[string]string{"create": ddlLogCreateSQL, "migrate": ddlLogMigrateSQL} {
		up := strings.ToUpper(sql)
		if strings.Contains(up, "TEXT NOT NULL DEFAULT") || strings.Contains(up, "TEXT DEFAULT") {
			t.Errorf("%s SQL has a DEFAULT on a TEXT column (TiDB 1101): %s", name, sql)
		}
		if strings.Contains(up, "STATUS TEXT,") == false && name == "migrate" {
			t.Errorf("migrate SQL lost the bare `status TEXT` column: %s", sql)
		}
	}
}

// TestShouldApplyDDL_SkipsToolBookkeeping anchors the stored-row unlock path:
// pre-fix ddl_log rows (object pg2tidb_ddl_log) whose ddl_command still
// carries the TiDB-illegal `TEXT NOT NULL DEFAULT` shape must be SKIPPED
// (not applied, not fatal) so the id cursor advances and the chain unblocks.
func TestShouldApplyDDL_SkipsToolBookkeeping(t *testing.T) {
	old := DDLEntry{
		Schema:     "public",
		ObjectName: "public.pg2tidb_ddl_log",
		ObjectType: "table",
		DDL:        "ALTER TABLE public.pg2tidb_ddl_log ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'applied'",
	}
	if shouldApplyDDL(old) {
		t.Fatal("tool bookkeeping DDL must not be applied to the target")
	}
	user := DDLEntry{ObjectName: "public.users", ObjectType: "table", DDL: "ALTER TABLE public.users ADD COLUMN c int"}
	if !shouldApplyDDL(user) {
		t.Fatal("user table DDL must still be applied")
	}
}

// TestCheckpoint_LastDDLID round-trips the DDL id through the checkpoint
// manager (at-least-once resume, #t59).
func TestCheckpoint_LastDDLID(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cp.json"
	cm := NewCheckpointManager(path)
	cm.SetSlotName("s")
	cm.SetLastDDLID(42)
	if got := cm.GetLastDDLID(); got != 42 {
		t.Fatalf("GetLastDDLID before save = %d, want 42", got)
	}
	if err := cm.Save(); err != nil {
		t.Fatal(err)
	}

	cm2 := NewCheckpointManager(path)
	if _, err := cm2.Load(); err != nil {
		t.Fatal(err)
	}
	if got := cm2.GetLastDDLID(); got != 42 {
		t.Errorf("GetLastDDLID after reload = %d, want 42 (must persist)", got)
	}
}

// TestShouldApplyDDL covers #t61: PG ddl_command_end fires once per sub-object,
// all carrying the parent statement. Only the entry whose object type matches
// the statement's primary kind is applied; piggybacked sub-objects (sequence,
// a table's PK index) are skipped. A standalone CREATE INDEX is still applied.
func TestShouldApplyDDL(t *testing.T) {
	createTable := "CREATE TABLE cdc_ddl_e2e (id SERIAL PRIMARY KEY, j JSONB)"
	cases := []struct {
		name string
		e    DDLEntry
		want bool
	}{
		{"table row of CREATE TABLE", DDLEntry{DDL: createTable, ObjectType: "table"}, true},
		{"sequence sub-object of CREATE TABLE", DDLEntry{DDL: createTable, ObjectType: "sequence"}, false},
		{"PK-index sub-object of CREATE TABLE", DDLEntry{DDL: createTable, ObjectType: "index"}, false},
		{"standalone CREATE INDEX", DDLEntry{DDL: "CREATE INDEX idx ON t (c)", ObjectType: "index"}, true},
		{"standalone CREATE UNIQUE INDEX", DDLEntry{DDL: "CREATE UNIQUE INDEX uq ON t (c)", ObjectType: "index"}, true},
		{"standalone DROP INDEX", DDLEntry{DDL: "DROP INDEX idx", ObjectType: "index"}, true},
		{"ALTER TABLE ADD COLUMN", DDLEntry{DDL: "ALTER TABLE t ADD COLUMN c INT", ObjectType: "table"}, true},
		{"DROP TABLE", DDLEntry{DDL: "DROP TABLE t", ObjectType: "table"}, true},
		{"standalone CREATE SEQUENCE", DDLEntry{DDL: "CREATE SEQUENCE s", ObjectType: "sequence"}, false},
		{"object-type case-insensitive", DDLEntry{DDL: createTable, ObjectType: "TABLE"}, true},
	}
	for _, c := range cases {
		if got := shouldApplyDDL(c.e); got != c.want {
			t.Errorf("%s: shouldApplyDDL = %v, want %v (ddl=%q type=%q)",
				c.name, got, c.want, c.e.DDL, c.e.ObjectType)
		}
	}
}

// TestTransformTableDDL_SchemaStrip: PG schema qualifiers (schema.table) are
// stripped from CREATE/ALTER/DROP so the table lands in the connected target
// database (TiDB has no schemas). #t61.
func TestTransformTableDDL_SchemaStrip(t *testing.T) {
	dt := NewDDLTransformer()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"CREATE strips public.", "CREATE TABLE public.foo (id SERIAL)", "CREATE TABLE IF NOT EXISTS foo (id BIGINT AUTO_INCREMENT)"},
		{"ALTER strips schema.", "ALTER TABLE myschema.foo ADD COLUMN c INT", "ALTER TABLE foo ADD COLUMN c INT"},
		{"DROP strips public.", "DROP TABLE public.foo", "DROP TABLE IF EXISTS foo"},
		{"unqualified CREATE unchanged", "CREATE TABLE foo (id SERIAL)", "CREATE TABLE IF NOT EXISTS foo (id BIGINT AUTO_INCREMENT)"},
	}
	for _, c := range cases {
		got := dt.Transform(c.in, "table")
		if got != c.want {
			t.Errorf("%s:\n  got:  %s\n  want: %s", c.name, got, c.want)
		}
	}
}
