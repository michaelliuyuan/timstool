package webapi

import (
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
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

// Adversarial R2 P1: the MySQL cursor must render in MySQL-native shape
// (space separator, no T/Z) so string compares against MIN()/index scans
// and the saturation equality check stay format-consistent.
func TestMySQLCursorValue(t *testing.T) {
	d := mysqlWatermarkDialect{}
	// UTC instant with microseconds: space separator, trailing zeros
	// trimmed, NO T and NO Z suffix.
	ts := time.Date(2026, 10, 5, 2, 50, 6, 123456000, time.UTC)
	if got := d.CursorValue(ts); got != "2026-10-05 02:50:06.123456" {
		t.Fatalf("CursorValue(time) = %q, want native MySQL form", got)
	}
	// Whole seconds: no dangling dot.
	ts = time.Date(2026, 10, 5, 2, 50, 6, 0, time.UTC)
	if got := d.CursorValue(ts); got != "2026-10-05 02:50:06" {
		t.Fatalf("CursorValue(whole-second time) = %q, want no dot fragment", got)
	}
	// A +08 wall-clock instant must render as its UTC reading (session pin).
	ts = time.Date(2026, 10, 5, 10, 50, 6, 0, time.FixedZone("CST", 8*3600))
	if got := d.CursorValue(ts); got != "2026-10-05 02:50:06" {
		t.Fatalf("CursorValue(+08 time) = %q, want UTC reading", got)
	}
	// DATE columns carry no time fragment -> the zero-time shape.
	ts = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if got := d.CursorValue(ts); got != "2026-10-05 00:00:00" {
		t.Fatalf("CursorValue(date-shaped time) = %q, want 00:00:00 short form", got)
	}
	// Non-time values keep the shared rendering (int/bigint cursors).
	if got := d.CursorValue(int64(42)); got != "42" {
		t.Fatalf("CursorValue(int64) = %q, want 42", got)
	}
	if got := d.CursorValue(nil); got != "" {
		t.Fatalf("CursorValue(nil) = %q, want empty", got)
	}
	// PG side: byte-identical to the historical incValueToString form.
	pg := pgWatermarkDialect{}
	ts = time.Date(2026, 10, 5, 2, 50, 6, 123456000, time.UTC)
	if pg.CursorValue(ts) != incValueToString(ts) {
		t.Fatal("pgWatermarkDialect.CursorValue must equal incValueToString byte-for-byte")
	}
}

// Adversarial R2 P0: a UI-shaped MySQL datasource carries schema="" - the
// watermark flows must resolve it to the connection database (validator
// sourceSchema parity), never query an empty schema.
func TestIncSchemaMySQLFallback(t *testing.T) { // UI shape: mysql ref, schema empty -> database.
	sc := config.SourceConfig{Type: "mysql", Host: "h", Port: 3306, Database: "appdb"}
	if got := incSchema(sc); got != "appdb" {
		t.Fatalf("incSchema(mysql empty) = %q, want appdb (database fallback)", got)
	}
	// Explicit schema wins (raw API can still pin one).
	sc.Schema = "pinned"
	if got := incSchema(sc); got != "pinned" {
		t.Fatalf("incSchema(mysql explicit) = %q, want pinned", got)
	}
	// PG empty stays empty at this helper - dataSourceToSourceConfig has
	// already applied its "public" fallback before incSchema is consulted.
	pg := config.SourceConfig{Type: "postgres", Database: "pgdb"}
	if got := incSchema(pg); got != "" {
		t.Fatalf("incSchema(pg empty) = %q, want empty (public fallback lives upstream)", got)
	}
}

// c-fix P2-1 (seq683): every STATISTICS probe must filter IS_VISIBLE='YES'
// (invisible indexes must not mark the column indexed / disclose a key).
// SQL-shape pin over the shared fragment and the two const'd probe shapes;
// the keys probe embeds the fragment dynamically (IN list), pinned via the
// fragment itself. Live invisible-index behavior rides the test-eng
// black-box list (production 8.0.26); IS_VISIBLE is 8.0+, the 5.7 floor
// ruling lives in the tail-batch pool.
func TestMySQLStatisticsVisibilityFilter(t *testing.T) {
	if !strings.Contains(mysqlQueryColumnsSQL, mysqlIdxVisibleFrag) {
		t.Fatal("mysqlQueryColumnsSQL lost the IS_VISIBLE filter")
	}
	if !strings.Contains(mysqlSuggestCatalogSQL, mysqlIdxVisibleFrag) {
		t.Fatal("mysqlSuggestCatalogSQL lost the IS_VISIBLE filter")
	}
	if mysqlIdxVisibleFrag != `AND s.IS_VISIBLE = 'YES'` {
		t.Fatalf("mysqlIdxVisibleFrag drifted: %q", mysqlIdxVisibleFrag)
	}
}
