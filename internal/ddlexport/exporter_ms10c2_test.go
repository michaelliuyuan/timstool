package ddlexport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"
)

// MS-10c2 anchors: README source-aware dispatch + MySQL TiDB conversion.

// TestReadmePGByteIdentity freezes the PG README byte-for-byte: "" legacy
// default and "postgres" must render the exact pre-MS-10c2 template (the
// dispatch must not perturb the PG path).
func TestReadmePGByteIdentity(t *testing.T) {
	frozen := `# DDL Export

Recommended apply order per schema (native PG DDL):
types.sql -> sequences.sql -> tables.sql -> indexes.sql -> functions.sql -> procedures.sql -> triggers.sql -> views.sql

Notes:
- tables.sql contains columns, PK and UNIQUE constraints, and COMMENT ON statements.
- Secondary indexes are in indexes.sql; PK / UNIQUE constraint backing indexes are inline in tables.sql.
- Sequences are exported before tables so serial DEFAULT nextval(...) references resolve.
- tidb-tables.sql (if present) is a TiDB-converted reference script, not native PG DDL.
- manifest.json lists exported object counts and any skipped objects (e.g. permission denied).

Schema: public
`
	for _, st := range []string{"", "postgres"} {
		if got := readmeFile("public", st); got != frozen {
			t.Errorf("readmeFile(%q) PG variant must be byte-identical to the frozen template;\ngot:\n%s", st, got)
		}
	}
}

// TestReadmeMySQLTemplate pins the MySQL README: apply order follows the
// SHOW CREATE walk and the dialect-map notes (DEFINER as-is, non-snapshot
// walk, functional key parts, session tz, 5.7 floor, passthrough ledger).
// The negative anchors scan TEMPLATE LITERALS ONLY — the schema name is
// caller data and is asserted separately (a database literally named with
// PG wording must not trip the template negative anchor).
func TestReadmeMySQLTemplate(t *testing.T) {
	got := readmeFile("db1", "mysql")
	wants := []string{
		"Recommended apply order per schema (native MySQL DDL):",
		"tables.sql -> views.sql -> functions.sql -> procedures.sql -> triggers.sql",
		"SHOW CREATE TABLE output verbatim",
		"DEFINER clauses in views/functions/procedures/triggers are exported as-is",
		"not a single consistent snapshot",
		"Functional key parts (MySQL 8.0.13+) are preserved",
		"session's time zone",
		"MySQL 5.7 is the minimum supported server",
		"per-type 1:1 passthrough ledger entries",
		"Schema: db1",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("MySQL README missing %q\ngot:\n%s", w, got)
		}
	}
	for _, f := range []string{"Postgre", "pg_", "nextval", "COMMENT ON"} {
		if strings.Contains(got, f) {
			t.Errorf("MySQL README must not contain PG wording %q\ngot:\n%s", f, got)
		}
	}
	// The negative anchor holds for caller data too: a database name is
	// user data, not template wording — it must pass through untouched.
	if got := readmeFile("pg_migration", "mysql"); !strings.Contains(got, "Schema: pg_migration") {
		t.Errorf("schema name is data, not template: %q must pass through", "pg_migration")
	}
}

// --- conversion stub: answers the three information_schema walks ---

type convDriver struct{ conn *convConn }

func (d convDriver) Open(string) (driver.Conn, error) { return d.conn, nil }

type convConn struct{ queries []string }

func (c *convConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("not implemented") }
func (c *convConn) Close() error                        { return nil }
func (c *convConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("not implemented") }

func (c *convConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	switch {
	case strings.Contains(q, "information_schema.STATISTICS"):
		return &convRows{cols: []string{"INDEX_NAME", "COLUMN_NAME", "EXPRESSION", "NON_UNIQUE", "SEQ_IN_INDEX"}, rows: [][]driver.Value{
			{"idx_fn", nil, "lower(`b`)", int64(1), int64(1)},
			{"PRIMARY", "id", nil, int64(0), int64(1)},
		}}, nil
	case strings.Contains(q, "information_schema.COLUMNS"):
		// Per-table answer keyed on the second placeholder (TABLE_NAME).
		tbl := ""
		if len(args) > 1 {
			tbl = fmt.Sprintf("%v", args[1].Value)
		}
		if tbl == "t0" {
			return &convRows{cols: []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_DEFAULT",
				"EXTRA", "COLUMN_COMMENT", "NUMERIC_PRECISION", "NUMERIC_SCALE", "COLUMN_KEY", "DATETIME_PRECISION"}, rows: [][]driver.Value{
				{"z", "int", "int(11)", "NO", nil, "", nil, int64(11), int64(0), "PRI", nil},
			}}, nil
		}
		return &convRows{cols: []string{"COLUMN_NAME", "DATA_TYPE", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_DEFAULT",
			"EXTRA", "COLUMN_COMMENT", "NUMERIC_PRECISION", "NUMERIC_SCALE", "COLUMN_KEY", "DATETIME_PRECISION"}, rows: [][]driver.Value{
			{"id", "bigint", "bigint", "NO", nil, "", nil, int64(20), int64(0), "PRI", nil},
			{"b", "varchar", "varchar(50)", "YES", nil, "", nil, int64(50), nil, nil, nil},
			{"loc", "geometry", "geometry", "YES", nil, "", nil, nil, nil, nil, nil},
		}}, nil
	case strings.Contains(q, "information_schema.TABLES"):
		// Deliberately NOT name-ordered: the exporter must sort before
		// rendering (deterministic replay, black-box note B).
		return &convRows{cols: []string{"TABLE_NAME"}, rows: [][]driver.Value{{"t1"}, {"t0"}}}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", q)
}

type convRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *convRows) Columns() []string { return r.cols }
func (r *convRows) Close() error      { return nil }
func (r *convRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

var convNames []string

func openConvDB(t *testing.T, conn *convConn) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("conv-%d", len(convNames))
	convNames = append(convNames, name)
	sql.Register(name, convDriver{conn: conn})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open conv stub: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMySQLTiDBTablesErrorPathNoFile pins the adversarial note-1 c-fix:
// when the CIR walk fails, the skip ledger entry is the whole record —
// no empty tidb-tables.sql file lands in the zip (an empty file reads as
// "zero tables converted").
func TestMySQLTiDBTablesErrorPathNoFile(t *testing.T) {
	e := NewExporter(openErrDB(t), Options{SourceType: "mysql"})
	ddl, n, err := e.mysqlTiDBTables(context.Background(), "db1")
	if err == nil {
		t.Fatalf("error path must surface err (ddl=%q n=%d)", ddl, n)
	}
	files, ferr := e.mysqlSchemaFiles(context.Background(), "db1")
	if ferr != nil {
		t.Fatalf("mysqlSchemaFiles: %v", ferr)
	}
	if _, ok := files["tidb-tables.sql"]; ok {
		t.Error("error path must not write an (empty) tidb-tables.sql entry")
	}
	if len(e.manifest.Skipped) == 0 {
		t.Error("error path must leave the skip ledger entry in the manifest")
	}
}

type errDriver struct{}

func (errDriver) Open(string) (driver.Conn, error) { return errConn{}, nil }

type errConn struct{}

func (errConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("not implemented") }
func (errConn) Close() error                        { return nil }
func (errConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("not implemented") }

func (errConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return nil, fmt.Errorf("stub: information_schema unavailable: %s", q)
}

var errNames []string

func openErrDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("conv-err-%d", len(errNames))
	errNames = append(errNames, name)
	sql.Register(name, errDriver{})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open err stub: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMySQLTiDBTablesConversion pins the MS-10c2 conversion contract:
// the CIR walk renders CREATE TABLE via the target renderer with the
// mysqlTypeMapper types, functional key parts replay their EXPRESSION
// original verbatim, and the geometry passthrough lands in the manifest
// skip ledger per type (no silent swallow).
func TestMySQLTiDBTablesConversion(t *testing.T) {
	conn := &convConn{}
	e := NewExporter(openConvDB(t, conn), Options{SourceType: "mysql"})
	ddl, n, err := e.mysqlTiDBTables(context.Background(), "db1")
	if err != nil {
		t.Fatalf("mysqlTiDBTables: %v", err)
	}
	if n != 2 {
		t.Fatalf("converted %d tables, want 2 (t1 + t0)", n)
	}
	wants := []string{
		"CREATE TABLE IF NOT EXISTS `t1`",
		"`id` BIGINT NOT NULL",
		"`b` VARCHAR(50)",
		// Live no-paren EXPRESSION shape replays wrap-canonicalized (pen-5).
		"INDEX `idx_fn` ((lower(`b`)))",
		// Second stub table also converted.
		"CREATE TABLE IF NOT EXISTS `t0`",
		"`z` INT NOT NULL",
	}
	for _, w := range wants {
		if !strings.Contains(ddl, w) {
			t.Errorf("tidb-tables.sql missing %q\ngot:\n%s", w, ddl)
		}
	}
	// Deterministic replay (note B): the stub yields tables NOT in name
	// order; the render must be name-sorted (t0 before t1) and two runs
	// over the same data render byte-identical output.
	if strings.Index(ddl, "`t0`") > strings.Index(ddl, "`t1`") {
		t.Errorf("tables must render in name order:\n%s", ddl)
	}
	e2 := NewExporter(openConvDB(t, &convConn{}), Options{SourceType: "mysql"})
	ddl2, _, err := e2.mysqlTiDBTables(context.Background(), "db1")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if ddl2 != ddl {
		t.Errorf("two runs over identical data must be byte-identical:\n1:\n%s\n2:\n%s", ddl, ddl2)
	}
	// Coverage ledger: geometry passed through 1:1 — one skip entry per
	// distinct type, nothing silently swallowed.
	found := false
	for _, sk := range e.manifest.Skipped {
		if sk.Type == "tidb-tables.sql" && sk.Object == "type:GEOMETRY" {
			found = true
		}
	}
	if !found {
		t.Errorf("geometry 1:1 passthrough must land in the manifest ledger, skipped=%v", e.manifest.Skipped)
	}
	// bigint/varchar map to distinct TiDB spellings (BIGINT vs bigint) —
	// they are conversions, not passthroughs, and must NOT be ledgered.
	for _, sk := range e.manifest.Skipped {
		if strings.Contains(sk.Object, "BIGINT") || strings.Contains(sk.Object, "VARCHAR") {
			t.Errorf("converted type must not be ledgered as passthrough: %+v", sk)
		}
	}
}
