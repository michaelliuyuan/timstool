package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// MS-10c2 anchors: functional key parts (MySQL 8.0.13+
// information_schema.STATISTICS EXPRESSION text, F-15 same fixture shapes)
// must render VERBATIM — no backtick re-wrap, no paren stripping — and the
// rendered statement must replay through the real ApplyDDL call chain
// byte-identically (replay-equivalence rule), not just as a string compare.

const (
	f15ExprLower = "(lower(`b`))"
	f15ExprArith = "(`a` + 1)"
)

func TestRenderCreateTableFunctionalKeyParts(t *testing.T) {
	tbl := source.Table{
		Name: "t1",
		Columns: []source.Column{
			{Name: "a", TiDBType: "BIGINT", Nullable: false},
			{Name: "b", TiDBType: "VARCHAR(50)", Nullable: true},
		},
		PK: []string{"a"},
		Indexes: []source.Index{
			// Mixed parts: plain identifier + F-15 expression shapes.
			{Name: "idx_a", Columns: []string{"a", f15ExprArith}},
			{Name: "idx_fn", Columns: []string{f15ExprLower}, Unique: true},
		},
	}
	got := RenderCreateTable(tbl)
	wants := []string{
		"INDEX `idx_a` (`a`, (`a` + 1))",
		"UNIQUE INDEX `idx_fn` ((lower(`b`)))",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("functional key part render missing %q\ngot:\n%s", w, got)
		}
	}
	// The expression must NOT be re-wrapped as an identifier (the pre-fix
	// writeIdentList behavior produced ``(`a` + 1)`` — broken DDL).
	for _, bad := range []string{"``(", "``lower"} {
		if strings.Contains(got, bad) {
			t.Errorf("expression must not be backtick-wrapped, found %q\ngot:\n%s", bad, got)
		}
	}
}

// --- replay stub: captures the DDL ApplyDDL executes (ExecContext) ---

type replayDriver struct{ conn *replayConn }

func (d replayDriver) Open(string) (driver.Conn, error) { return d.conn, nil }

type replayConn struct{ executed []string }

func (c *replayConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("not implemented") }
func (c *replayConn) Close() error                        { return nil }
func (c *replayConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("not implemented") }
func (c *replayConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.executed = append(c.executed, q)
	return nil, nil
}

var replayNames []string

func openReplayDB(t *testing.T, conn *replayConn) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("replay-%d", len(replayNames))
	replayNames = append(replayNames, name)
	sql.Register(name, replayDriver{conn: conn})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open replay stub: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestApplyDDLReplaysFunctionalKeyPartsVerbatim runs the REAL ApplyDDL
// path (RenderCreateTable inside it) against a capturing stub and asserts
// the executed DDL carries the information_schema EXPRESSION original
// byte-for-byte: replay-equivalence, not just string shape.
func TestApplyDDLReplaysFunctionalKeyPartsVerbatim(t *testing.T) {
	sch := &source.Schema{
		Catalog: "db1",
		Tables: []source.Table{{
			Name: "t1",
			Columns: []source.Column{
				{Name: "a", TiDBType: "BIGINT", Nullable: false},
				{Name: "b", TiDBType: "VARCHAR(50)", Nullable: true},
			},
			Indexes: []source.Index{
				{Name: "idx_fn", Columns: []string{f15ExprLower}},
			},
		}},
	}
	conn := &replayConn{}
	db := openReplayDB(t, conn)
	if err := ApplyDDL(context.Background(), db, sch); err != nil {
		t.Fatalf("ApplyDDL: %v", err)
	}
	if len(conn.executed) != 1 {
		t.Fatalf("executed %d statements, want 1", len(conn.executed))
	}
	executed := conn.executed[0]
	i := strings.Index(executed, f15ExprLower)
	if i < 0 {
		t.Fatalf("executed DDL missing the EXPRESSION original %q:\n%s", f15ExprLower, executed)
	}
	// No re-wrap directly adjacent (identifier quoting would add backticks).
	if i > 0 && executed[i-1] == '`' {
		t.Errorf("expression was backtick-wrapped before the original:\n%s", executed)
	}
	if i+len(f15ExprLower) < len(executed) && executed[i+len(f15ExprLower)] == '`' {
		t.Errorf("expression was backtick-wrapped after the original:\n%s", executed)
	}
}
