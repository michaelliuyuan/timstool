package webapi

import (
	"strings"
	"testing"
)

// MS-10a MySQL dialect shape anchors: the rendering forms (backticks, ?
// placeholders, LIMIT ?), the eligibility catalog, the system-schema set,
// the seam selection, and the scorer's dual-catalog read. Byte-level pins
// mirror TestIncSelectSQL / TestIncDrainAndJumpSQL on the PG side.

func TestMySQLWatermarkSelectSQL(t *testing.T) {
	d := mysqlWatermarkDialect{}
	got := d.BuildSelectSQL("app", "users", []string{"id", "update_time", "name"}, "update_time", false)
	want := "SELECT `id`, `update_time`, `name` FROM `app`.`users` WHERE `update_time` >= ? ORDER BY `update_time` LIMIT ?"
	if got != want {
		t.Fatalf("non-strict select =\n%s\nwant\n%s", got, want)
	}
	// Strict mode must use ">" (same boundary semantics as the PG side).
	got = d.BuildSelectSQL("app", "users", []string{"id"}, "wm", true)
	if !strings.Contains(got, "`wm` > ?") {
		t.Fatalf("strict select missing > : %s", got)
	}
	// Backtick escaping: a backtick inside an identifier must double, never
	// terminate the quoted fragment raw.
	got = d.BuildSelectSQL("s`x", "t`y", []string{"c`z"}, "w`m", false)
	if strings.Contains(got, "`s`x`") || !strings.Contains(got, "`s``x`") {
		t.Fatalf("backtick escaping failed: %s", got)
	}
	// The watermark value is ALWAYS a bound parameter - never interpolated.
	if c := strings.Count(want, "?"); c != 2 {
		t.Fatalf("select must bind exactly wm+limit (2 ?): %s", want)
	}
}

func TestMySQLWatermarkDrainAndJumpSQL(t *testing.T) {
	d := mysqlWatermarkDialect{}
	got := d.BuildDrainSQL("app", "users", []string{"id", "update_time"}, "update_time")
	want := "SELECT `id`, `update_time` FROM `app`.`users` WHERE `update_time` = ?"
	if got != want {
		t.Fatalf("drain SQL =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "LIMIT") {
		t.Fatalf("drain scan must be unbounded (streamed): %s", got)
	}
	jump := d.BuildNextWatermarkSQL("app", "users", "update_time")
	wantJump := "SELECT MIN(`update_time`) FROM `app`.`users` WHERE `update_time` > ?"
	if jump != wantJump {
		t.Fatalf("jump SQL =\n%s\nwant\n%s", jump, wantJump)
	}
}

func TestMySQLWatermarkEligible(t *testing.T) {
	for _, ok := range []string{"timestamp", "datetime", "date", "int", "bigint"} {
		if !mysqlWatermarkTypes[ok] {
			t.Errorf("type %q must be whitelisted", ok)
		}
		if !(mysqlWatermarkDialect{}).WatermarkEligible(ok) {
			t.Errorf("WatermarkEligible(%q) = false", ok)
		}
	}
	// MySQL renders int columns as "int", not "integer"; and the PG names
	// must NOT leak into the MySQL catalog (and vice versa).
	for _, bad := range []string{"integer", "text", "varchar", "decimal", "json", "blob", ""} {
		if mysqlWatermarkTypes[bad] {
			t.Errorf("type %q must NOT be whitelisted", bad)
		}
	}
	if incWatermarkTypes["timestamp"] || incWatermarkTypes["datetime"] || incWatermarkTypes["int"] {
		t.Error("PG catalog must not gain the MySQL type names (single-source discipline)")
	}
}

func TestMySQLSystemSchemas(t *testing.T) {
	sys := mysqlWatermarkDialect{}.SystemSchemas()
	for _, s := range []string{"mysql", "sys", "performance_schema", "information_schema"} {
		if !sys[s] {
			t.Errorf("system schema %q missing", s)
		}
	}
	for _, s := range []string{"pg_catalog", "pg_toast", "public", "app"} {
		if sys[s] {
			t.Errorf("schema %q must not be a MySQL system schema", s)
		}
	}
}

// The seam: kind-based dispatch with the PG default preserved.
func TestIncDialectFor(t *testing.T) {
	if _, ok := incDialectFor("mysql").(mysqlWatermarkDialect); !ok {
		t.Fatal(`incDialectFor("mysql") must be mysqlWatermarkDialect`)
	}
	if _, ok := incDialectFor("postgres").(pgWatermarkDialect); !ok {
		t.Fatal(`incDialectFor("postgres") must be pgWatermarkDialect`)
	}
	if _, ok := incDialectFor("").(pgWatermarkDialect); !ok {
		t.Fatal(`incDialectFor("") must default to pgWatermarkDialect (unreachable behind the capability guard)`)
	}
	// The package-level seam stays the PG instance (anchor pinning).
	if _, ok := incSourceDialect.(pgWatermarkDialect); !ok {
		t.Fatal("incSourceDialect must stay the PG default (white-box anchors pin PG shapes)")
	}
}

// The suggest scorer reads BOTH catalogs (single scorer, two dialects).
func TestWMTypeWeightMySQLNames(t *testing.T) {
	for dt, wantW := range map[string]float64{"timestamp": 0.9, "datetime": 0.9, "date": 0.7, "bigint": 0.5, "int": 0.4} {
		if w, _ := wmTypeWeight(dt); w != wantW {
			t.Errorf("wmTypeWeight(%q) = %v, want %v", dt, w, wantW)
		}
	}
	if w, _ := wmTypeWeight("integer"); w != 0.4 {
		t.Errorf(`wmTypeWeight("integer") = %v, want 0.4 (PG name must keep its weight)`, w)
	}
	if w, _ := wmTypeWeight("varchar"); w != 0 {
		t.Errorf(`wmTypeWeight("varchar") = %v, want 0`, w)
	}
}
