package ddlexport

import (
	"os"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source/mysql"
)

// TestMySQLDatabaseFilter pins the SHOW DATABASES system-set filter: the
// system databases must never appear in the schema list. MS-10b2 item 8:
// the set now lives in the shared exported source/mysql.SystemDatabases
// (single source of truth; assess consumes the same copy).
func TestMySQLDatabaseFilter(t *testing.T) {
	for _, sys := range []string{"mysql", "information_schema", "performance_schema", "sys", "metrics_schema"} {
		if !mysql.SystemDatabases[sys] {
			t.Errorf("system database %q missing from the filter set", sys)
		}
	}
	if mysql.SystemDatabases["migration_test"] {
		t.Error("user database must not be filtered")
	}
	// TiDB (and Windows MySQL) report the system databases upper-case.
	for _, sys := range []string{"INFORMATION_SCHEMA", "PERFORMANCE_SCHEMA", "METRICS_SCHEMA", "Mysql"} {
		if !mysql.SystemDatabases[strings.ToLower(sys)] {
			t.Errorf("case-insensitive filter miss: %q", sys)
		}
	}
}

// TestMySQLQueryAnchors pins the information_schema listing queries and
// the SHOW CREATE statement shapes (server-documented column/view names)
// plus the DDL column index per SHOW family, mirroring the PG-side
// TestCatalogQueryAnchors regression guard.
func TestMySQLQueryAnchors(t *testing.T) {
	src, err := os.ReadFile("exporter_mysql.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, tc := range []struct {
		name    string
		marker  string
		require []string
		forbid  []string
	}{
		{"databases", "SHOW DATABASES",
			[]string{}, nil},
		{"tables", "SELECT TABLE_NAME FROM information_schema.TABLES",
			[]string{"TABLE_SCHEMA = ?", "TABLE_TYPE = 'BASE TABLE'", "ORDER BY TABLE_NAME"},
			[]string{"VIEW"}},
		{"views", "SELECT TABLE_NAME FROM information_schema.VIEWS",
			[]string{"TABLE_SCHEMA = ?", "ORDER BY TABLE_NAME"}, nil},
		{"functions", "SELECT ROUTINE_NAME FROM information_schema.ROUTINES",
			[]string{"ROUTINE_TYPE = 'FUNCTION'", "ORDER BY ROUTINE_NAME"}, nil},
		// the marker itself pins ROUTINE_TYPE='PROCEDURE' (the shared
		// ROUTINES literal would otherwise resolve to the functions copy).
		{"procedures", "WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE'",
			[]string{"ORDER BY ROUTINE_NAME"}, nil},
		{"triggers", "SELECT TRIGGER_NAME FROM information_schema.TRIGGERS",
			[]string{"TRIGGER_SCHEMA = ?", "ORDER BY TRIGGER_NAME"}, nil},
	} {
		i := strings.Index(s, tc.marker)
		if i < 0 {
			t.Fatalf("%s: marker %q not found", tc.name, tc.marker)
		}
		end := strings.Index(s[i:], "`")
		block := s[i : i+end]
		for _, r := range tc.require {
			if !strings.Contains(block, r) {
				t.Errorf("%s: query missing %q", tc.name, r)
			}
		}
		for _, f := range tc.forbid {
			if strings.Contains(block, f) {
				t.Errorf("%s: query must not contain %q", tc.name, f)
			}
		}
	}
	// SHOW DATABASES output must pass the case-insensitive system-set
	// filter (the loop body lives outside the query literal).
	if !strings.Contains(s, "mysql.SystemDatabases[strings.ToLower(s)]") {
		t.Error("SHOW DATABASES walk must filter via the shared mysql.SystemDatabases (case-insensitive)")
	}
	// SHOW CREATE DDL column pins: table/view at index 1,
	// function/procedure/trigger at index 2.
	for _, pin := range []struct{ stmt, col string }{
		{"SHOW CREATE TABLE %s.%s", "1"},
		{"SHOW CREATE VIEW %s.%s", "1"},
		{"SHOW CREATE FUNCTION %s.%s", "2"},
		{"SHOW CREATE PROCEDURE %s.%s", "2"},
		{"SHOW CREATE TRIGGER %s.%s", "2"},
	} {
		i := strings.Index(s, `"`+pin.stmt+`"`)
		if i < 0 {
			t.Fatalf("statement %q not found", pin.stmt)
		}
		// the ddlCol argument follows the format string
		tail := s[i : i+120]
		if !strings.Contains(tail, ", "+pin.col+")") {
			t.Errorf("statement %q: DDL column pin %s not adjacent", pin.stmt, pin.col)
		}
	}
}

// TestMySQLDegradeAnchors pins the degrade contract (MS10C-DIALECT-MAP):
// indexes/sequences/types write note files (zip-visible, zero counts);
// since MS-10c2 the TiDB conversion is PRODUCED for a MySQL source via the
// source adapter's CIR path (the old "PG catalog only" skip is gone), and
// 1:1 passthrough column types land in the manifest skip ledger per type.
func TestMySQLDegradeAnchors(t *testing.T) {
	src, err := os.ReadFile("exporter_mysql.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{
		`files["indexes.sql"] = "-- MySQL: indexes have no standalone namespace`,
		`files["sequences.sql"] = "-- MySQL: no sequence objects`,
		`files["types.sql"] = "-- MySQL: no user-defined type objects`,
		"mysql.NewSchemaReaderForDB(e.db, schemaName)",
		"target.RenderCreateTable(tbl)",
		"no TiDB conversion applied) — coverage ledger: MS10C-DIALECT-MAP.md",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("degrade anchor missing: %q", want)
		}
	}
	// The flipped contract must not regress to the MS-10c skip.
	if strings.Contains(s, "tidb-tables.sql requires the PG catalog path") {
		t.Error("MySQL TiDB conversion must not fall back to the PG-catalog skip (MS-10c2)")
	}
}

// TestMySQLSchemaDefaultKept pins the NewExporter schema default: the
// "public" fallback is PG-only — a MySQL run keeps the caller's list
// untouched (a MySQL "schema" is a database).
func TestMySQLSchemaDefaultKept(t *testing.T) {
	e := NewExporter(nil, Options{SourceType: "mysql"})
	if len(e.opts.Schemas) != 0 {
		t.Fatalf("mysql default schemas = %v, want empty (no public fallback)", e.opts.Schemas)
	}
	p := NewExporter(nil, Options{})
	if len(p.opts.Schemas) != 1 || p.opts.Schemas[0] != "public" {
		t.Fatalf("pg default schemas = %v, want [public]", p.opts.Schemas)
	}
}

// TestQiBEscapesBackticks mirrors the PG-side qi escaping anchor.
func TestQiBEscapesBackticks(t *testing.T) {
	if got := qiB("we`ird"); got != "`we``ird`" {
		t.Errorf("qiB escaping failed: %s", got)
	}
}
