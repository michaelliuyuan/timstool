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
func (mysqlStubConn) Begin() (driver.Tx, error)           { return mysqlStubTx{}, nil }

// ExecContext serves the MS-10b2 session pin (SET SESSION
// group_concat_max_len) issued inside the snapshot tx.
func (mysqlStubConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	mysqlQueries = append(mysqlQueries, q)
	return mysqlStubResult{}, nil
}

type mysqlStubTx struct{}

func (mysqlStubTx) Commit() error   { return nil }
func (mysqlStubTx) Rollback() error { return nil }

type mysqlStubResult struct{}

func (mysqlStubResult) LastInsertId() (int64, error) { return 0, nil }
func (mysqlStubResult) RowsAffected() (int64, error) { return 0, nil }

var errNotImpl = fmt.Errorf("not implemented")

// mysqlStubTableRows lets a test grow the canned TABLES result set so the
// large-catalog guard (MS-10b2 item 6) can be exercised for real.
var mysqlStubTableRows = [][]driver.Value{{"db1", "t1"}}

func (mysqlStubConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	mysqlQueries = append(mysqlQueries, q)
	switch {
	case strings.Contains(q, "information_schema.TABLES"):
		return &stubRows{
			cols: []string{"TABLE_SCHEMA", "TABLE_NAME"},
			rows: mysqlStubTableRows,
		}, nil
	case strings.Contains(q, "information_schema.COLUMNS"):
		return &stubRows{
			cols: []string{"s", "t", "c", "dt", "ml", "np", "ns", "nu", "def", "pk", "pos", "ai"},
			rows: [][]driver.Value{{"db1", "t1", "id", "int", int64(0), int64(10), int64(0), false, "", true, int64(1), true}},
		}, nil
	case strings.Contains(q, "information_schema.STATISTICS"):
		return &stubRows{
			cols: []string{"t", "n", "ty", "u", "p", "def", "part", "expr"},
			rows: [][]driver.Value{
				{"t1", "idx_a", "btree", true, false, "idx_a (a)", int64(0), int64(0)},
				// MS-10b P1-2: a functional key part has COLUMN_NAME NULL
				// (EXPRESSION non-NULL, 8.0.13+); the aggregate must flag
				// IsExpression so the checker can raise it.
				{"t1", "idx_fn", "btree", false, false, "idx_fn (LOWER(a))", int64(0), int64(1)},
			},
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
		"COLUMN_NAME IS NULL",
		"GROUP_CONCAT",
		"information_schema.VIEWS",
		"information_schema.ROUTINES",
		"information_schema.TRIGGERS",
		// c-fix item 3: the session GROUP_CONCAT cap pin must ride along
		// so wide composite index definitions never truncate.
		"group_concat_max_len = 1048576",
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
	if len(res.Indexes) != 2 || res.Indexes[0].IndexType != "btree" || res.Indexes[0].IsPartial || res.Indexes[0].IsExpression {
		t.Errorf("indexes = %+v", res.Indexes)
	}
	// P1-2 anchor: the functional-key-part row (COLUMN_NAME NULL -> the
	// MAX(COLUMN_NAME IS NULL) aggregate reads 1) must carry IsExpression
	// so the index checker raises the MySQL->TiDB incompatibility.
	if !res.Indexes[1].IsExpression {
		t.Errorf("functional index idx_fn must have IsExpression=true, got %+v", res.Indexes[1])
	}
	if len(res.Triggers) != 1 || res.Triggers[0].Timing != "AFTER" || res.Triggers[0].EventType != "INSERT" {
		t.Errorf("triggers = %+v", res.Triggers)
	}
	if len(res.Functions) != 1 || res.Functions[0].Language != "sql" {
		t.Errorf("functions = %+v (Language degraded to sql)", res.Functions)
	}
}

// Schema fail-loud anchor (MS-10b2 item 5): the PG-ism "public" default is
// gone — an empty schema must error out of ScanAll instead of silently
// scanning a nonexistent "public" database; the handler owns the
// database-as-schema normalization.
func TestMySQLScannerSchemaRequired(t *testing.T) {
	db := openMySQLStub(t)
	s := newMySQLScanner(db, "")
	if _, err := s.ScanAll(context.Background()); err == nil {
		t.Error("empty schema must fail loud in ScanAll")
	} else if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error text = %q, want schema-required wording", err.Error())
	}
	if got := newMySQLScanner(db, "db1").schema; got != "db1" {
		t.Errorf("explicit schema = %q, want db1", got)
	}
}

// Snapshot-tx + session-pin affinity anchor (MS-10b2 items 6+9, ruling
// seq843-⑥): the GROUP_CONCAT pin rides the same single transaction as
// every catalog query (one tx object, not just one session), and the
// pool's MaxOpenConns is pinned to 1 for the scan and restored after.
func TestMySQLScannerSnapshotTxAffinity(t *testing.T) {
	db := openMySQLStub(t)
	db.SetMaxOpenConns(3)
	mysqlQueries = nil
	s := newMySQLScanner(db, "db1")
	if _, err := s.ScanAll(context.Background()); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	joined := strings.Join(mysqlQueries, "\n---\n")
	if !strings.Contains(joined, "group_concat_max_len = 1048576") {
		t.Error("session pin missing from the recorded query stream")
	}
	// The pin must be the FIRST statement (inside the tx, before any
	// catalog query touches the session).
	if mysqlQueries[0] != "SET SESSION group_concat_max_len = 1048576" {
		t.Errorf("pin must precede catalog queries, stream starts with %q", mysqlQueries[0])
	}
	// Pool restore: the pre-scan limit must survive the scan.
	if got := db.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("MaxOpenConns restore = %d, want 3", got)
	}
}

// Large-catalog guard anchor (MS-10b2 item 6, ddlexport same contract):
// a scan whose total object count exceeds maxScanObjects fails loud with
// the guard wording instead of exhausting memory.
func TestMySQLScannerObjectGuard(t *testing.T) {
	if maxScanObjects != 20000 {
		t.Errorf("maxScanObjects = %d, want 20000 (ddlexport defaultMaxObjects parity)", maxScanObjects)
	}
	prev := mysqlStubTableRows
	mysqlStubTableRows = make([][]driver.Value, maxScanObjects+1)
	for i := range mysqlStubTableRows {
		mysqlStubTableRows[i] = []driver.Value{"db1", fmt.Sprintf("t%d", i)}
	}
	defer func() { mysqlStubTableRows = prev }()

	db := openMySQLStub(t)
	s := newMySQLScanner(db, "db1")
	_, err := s.ScanAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "object guard") {
		t.Errorf("over-limit scan must fail loud with the object guard, got %v", err)
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
