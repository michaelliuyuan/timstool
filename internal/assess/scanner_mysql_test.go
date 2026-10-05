package assess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"
)

// MS-10b anchors: the MySQL scanner dialect. A recording stub driver serves
// canned information_schema result sets and captures every query so the SQL
// shapes (placeholder style, IS_VISIBLE probe, PK EXISTS subquery, degraded
// empty sets) stay pinned.

type mysqlStubDriver struct{ name string }

func (d mysqlStubDriver) Open(string) (driver.Conn, error) { return mysqlStubConn{}, nil }

type mysqlStubConn struct{}

var mysqlQueries []string

func (mysqlStubConn) Prepare(string) (driver.Stmt, error) { return nil, errNotImpl }
func (mysqlStubConn) Close() error                        { return nil }
func (mysqlStubConn) Begin() (driver.Tx, error)           { return nil, errNotImpl }

var errNotImpl = fmt.Errorf("not implemented")

func (mysqlStubConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	mysqlQueries = append(mysqlQueries, q)
	switch {
	case strings.Contains(q, "information_schema.TABLES"):
		return &stubRows{
			cols: []string{"TABLE_SCHEMA", "TABLE_NAME"},
			rows: [][]driver.Value{{"db1", "t1"}},
		}, nil
	case strings.Contains(q, "information_schema.COLUMNS"):
		return &stubRows{
			cols: []string{"s", "t", "c", "dt", "ml", "np", "ns", "nu", "def", "pk", "pos"},
			rows: [][]driver.Value{{"db1", "t1", "id", "int", int64(0), int64(10), int64(0), false, "", true, int64(1)}},
		}, nil
	case strings.Contains(q, "information_schema.STATISTICS"):
		return &stubRows{
			cols: []string{"t", "n", "ty", "u", "p", "def", "part", "expr"},
			rows: [][]driver.Value{{"t1", "idx_a", "btree", true, false, "idx_a (a)", int64(0), int64(0)}},
		}, nil
	case strings.Contains(q, "information_schema.VIEWS"):
		return &stubRows{
			cols: []string{"s", "n", "def", "ddl"},
			rows: [][]driver.Value{{"db1", "v1", "SELECT 1", ""}},
		}, nil
	case strings.Contains(q, "information_schema.ROUTINES"):
		return &stubRows{
			cols: []string{"s", "n", "ret", "lang", "src", "proc", "ddl"},
			rows: [][]driver.Value{{"db1", "f1", "integer", "sql", "RETURN 1", false, ""}},
		}, nil
	case strings.Contains(q, "information_schema.TRIGGERS"):
		return &stubRows{
			cols: []string{"t", "n", "ev", "tm", "st", "ddl"},
			rows: [][]driver.Value{{"t1", "trg1", "INSERT", "AFTER", "INSERT INTO log VALUES (NEW.id)", ""}},
		}, nil
	}
	return &stubRows{cols: []string{"x"}}, nil
}

func openMySQLStub(t *testing.T) *sql.DB {
	t.Helper()
	mysqlStubSeq++
	name := fmt.Sprintf("mysql-stub-%d", mysqlStubSeq)
	mysqlStubNames = append(mysqlStubNames, name)
	sql.Register(name, mysqlStubDriver{})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open stub: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var mysqlStubNames []string

var mysqlStubSeq int

// NewScannerFor must dispatch on the routed driver kind: "mysql" lands on
// the information_schema scanner, everything else (including the legacy ""
// callers pre-MS-10b) keeps the PG catalog scanner byte-identical.
func TestNewScannerForDispatch(t *testing.T) {
	db := openMySQLStub(t)
	if _, ok := NewScannerFor("mysql", db, "db1").(*mysqlScanner); !ok {
		t.Error(`NewScannerFor("mysql") must return *mysqlScanner`)
	}
	if _, ok := NewScannerFor("", db, "public").(*Scanner); !ok {
		t.Error(`NewScannerFor("") must keep the legacy PG scanner`)
	}
	if _, ok := NewScannerFor("pgx", db, "public").(*Scanner); !ok {
		t.Error(`NewScannerFor("pgx") must keep the PG scanner`)
	}
}

// SQL shape anchors: placeholder style is '?' (MySQL wire), the STATISTICS
// aggregate carries the MS-10a IS_VISIBLE filter, and the column scan finds
// PK membership via an EXISTS probe on INDEX_NAME='PRIMARY'.
func TestMySQLScannerSQLShapes(t *testing.T) {
	db := openMySQLStub(t)
	s := newMySQLScanner(db, "db1")
	if _, err := s.ScanAll(context.Background()); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	joined := strings.Join(mysqlQueries, "\n---\n")

	for _, frag := range []string{
		"information_schema.TABLES",
		"TABLE_SCHEMA = ?",
		"TABLE_TYPE = 'BASE TABLE'",
		"information_schema.COLUMNS",
		"INDEX_NAME = 'PRIMARY'",
		"information_schema.STATISTICS",
		"IS_VISIBLE = 'YES'",
		"GROUP_CONCAT",
		"information_schema.VIEWS",
		"information_schema.ROUTINES",
		"information_schema.TRIGGERS",
	} {
		if !strings.Contains(joined, frag) {
			t.Errorf("mysql scanner SQL missing fragment %q", frag)
		}
	}
	if strings.Contains(joined, "$1") {
		t.Error("mysql scanner SQL must use '?' placeholders, found '$1'")
	}
	// No PG-catalog leakage on the MySQL path.
	for _, pgFrag := range []string{"pg_indexes", "pg_proc", "pg_trigger", "pg_class"} {
		if strings.Contains(joined, pgFrag) {
			t.Errorf("mysql scanner SQL must not touch PG catalogs, found %q", pgFrag)
		}
	}
}

// Degraded-set anchor: MySQL has no schema-level enums/extensions/sequences,
// so those slices stay empty (checkers score an empty dimension compatible).
func TestMySQLScannerDegradedEmptySets(t *testing.T) {
	db := openMySQLStub(t)
	s := newMySQLScanner(db, "db1")
	res, err := s.ScanAll(context.Background())
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if res.Enums != nil || res.Extensions != nil || res.Sequences != nil {
		t.Errorf("enums/extensions/sequences must be nil-degraded, got %+v/%+v/%+v",
			res.Enums, res.Extensions, res.Sequences)
	}
	if len(res.Tables) != 1 || res.Tables[0].Name != "t1" {
		t.Errorf("tables = %+v", res.Tables)
	}
	if len(res.Columns) != 1 || !res.Columns[0].IsPrimary {
		t.Errorf("columns = %+v (PK probe must mark id)", res.Columns)
	}
	if len(res.Indexes) != 1 || res.Indexes[0].IndexType != "btree" || res.Indexes[0].IsPartial || res.Indexes[0].IsExpression {
		t.Errorf("indexes = %+v", res.Indexes)
	}
	if len(res.Triggers) != 1 || res.Triggers[0].Timing != "AFTER" || res.Triggers[0].EventType != "INSERT" {
		t.Errorf("triggers = %+v", res.Triggers)
	}
	if len(res.Functions) != 1 || res.Functions[0].Language != "sql" {
		t.Errorf("functions = %+v (Language degraded to sql)", res.Functions)
	}
}

// Schema fallback anchor: the scanner itself keeps the conservative
// "public" default (same shape as the PG scanner); the handler-level
// database-as-schema fallback is the caller's job (MS-10a incSchema shape).
func TestMySQLScannerSchemaFallback(t *testing.T) {
	db := openMySQLStub(t)
	if got := newMySQLScanner(db, "").schema; got != "public" {
		t.Errorf("empty schema fallback = %q, want public", got)
	}
	if got := newMySQLScanner(db, "db1").schema; got != "db1" {
		t.Errorf("explicit schema = %q, want db1", got)
	}
}

// rows.Err() propagation anchor: a failing rows iteration must surface as
// scanError with the stage prefix.
func TestMySQLScannerScanErrorWraps(t *testing.T) {
	var e error = wrapScanErr("scan tables", io.EOF)
	if !strings.Contains(e.Error(), "scan tables: ") {
		t.Errorf("scanError text = %q", e.Error())
	}
	if _, ok := e.(*scanError); !ok {
		t.Error("wrapScanErr must return *scanError")
	}
}
