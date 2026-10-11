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
			// Mixed parts: plain identifier + F-15 expression shapes
			// (paren fixture form).
			{Name: "idx_a", Parts: []source.IndexPart{
				{Value: "a"}, {Value: f15ExprArith, IsExpression: true},
			}},
			// Deployed-server live shape: EXPRESSION WITHOUT outer parens
			// (black-box seq848) — the renderer must ensure the wrap.
			{Name: "idx_fn", Unique: true, Parts: []source.IndexPart{
				{Value: "lower(`a-b`)", IsExpression: true},
			}},
		},
	}
	got := RenderCreateTable(tbl)
	wants := []string{
		"INDEX `idx_a` (`a`, (`a` + 1))",
		"UNIQUE INDEX `idx_fn` ((lower(`a-b`)))",
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

// TestRenderIndexPartQuotedNonBareIdentifiers pins the P2 c-fix: legal
// reference identifiers that are NOT bare forms — spaces, hyphens, digit
// prefixes — must be backtick-quoted like any plain column (the earlier
// character-class allowlist verbatim-emitted them as broken DDL). Only
// the parenthesized EXPRESSION shape verbatim-replays.
func TestRenderIndexPartQuotedNonBareIdentifiers(t *testing.T) {
	cases := []struct {
		part source.IndexPart
		want string
	}{
		// Column-flagged parts quote REGARDLESS of string shape — the
		// pathological "(x)" family dissolves (flag路线, pen-5).
		{source.IndexPart{Value: "my col"}, "`my col`"},
		{source.IndexPart{Value: "a-b"}, "`a-b`"},
		{source.IndexPart{Value: "123abc"}, "`123abc`"},
		{source.IndexPart{Value: "(abc"}, "`(abc`"},
		{source.IndexPart{Value: "(x)"}, "`(x)`"},
		{source.IndexPart{Value: "col"}, "`col`"},
		// Expression-flagged: parens ensured exactly once (both shapes —
		// wrap de-dup is the surviving string-test defense layer).
		{source.IndexPart{Value: "lower(`a-b`)", IsExpression: true}, "(lower(`a-b`))"},
		{source.IndexPart{Value: "(lower(`b`))", IsExpression: true}, "(lower(`b`))"},
	}
	for _, tc := range cases {
		var b strings.Builder
		renderIndexPart(&b, tc.part)
		if got := b.String(); got != tc.want {
			t.Errorf("renderIndexPart(%+v) = %q, want %q", tc.part, got, tc.want)
		}
	}
}

// TestRenderIndexPartPGParity pins the PG-identity face: index parts from
// the PG collector are plain column names, so every part renders quoted —
// the exact bytes the pre-MS-10c2 writeIdentList produced.
func TestRenderIndexPartPGParity(t *testing.T) {
	got := RenderCreateTable(source.Table{
		Name:    "tp",
		Columns: []source.Column{{Name: "id", TiDBType: "BIGINT"}},
		PK:      []string{"id"},
		Indexes: []source.Index{{Name: "ix", Parts: []source.IndexPart{{Value: "user_name"}, {Value: "data"}}}},
	})
	if !strings.Contains(got, "INDEX `ix` (`user_name`, `data`)") {
		t.Errorf("PG-collector index parts must render fully quoted (writeIdentList parity):\n%s", got)
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
				{Name: "idx_fn", Parts: []source.IndexPart{{Value: f15ExprLower, IsExpression: true}}},
				// Live no-paren shape replays wrap-canonicalized (pen-5).
				{Name: "idx_live", Parts: []source.IndexPart{{Value: "lower(`a-b`)", IsExpression: true}}},
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
	// Live shape: the no-paren EXPRESSION original replays with exactly
	// one ensured paren pair (SHOW CREATE canonical, TiDB-buildable).
	if !strings.Contains(executed, "INDEX `idx_live` ((lower(`a-b`)))") {
		t.Errorf("no-paren live shape must replay wrap-canonicalized:\n%s", executed)
	}
}
