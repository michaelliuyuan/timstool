package schema

import (
	"strings"
	"testing"
)

// F-05 anchor: a view whose body contains a semicolon must stay a single
// statement. executeDDL now consumes the builder's statement slice directly
// (no strings.Split(";") on the joined blob), so embedded semicolons can no
// longer truncate a statement.
func TestViewBodySemicolonStaysOneStatement(t *testing.T) {
	b := NewDDLBuilder()
	view := View{
		Name:       "v_notes",
		Definition: "SELECT id, 'a;b' AS note FROM t WHERE note <> 'x;y'",
	}
	// Same call shape as Migrate() in migrator.go.
	b.statements = append(b.statements, b.BuildViewDDL(view))

	stmts := b.Statements()
	if len(stmts) != 1 {
		t.Fatalf("view DDL must be exactly one statement, got %d", len(stmts))
	}
	if !strings.Contains(stmts[0], "'a;b'") || !strings.Contains(stmts[0], "'x;y'") {
		t.Errorf("view body literals must survive intact, got: %s", stmts[0])
	}
}

// F-05 anchor: the FK collection query must pair referencing and referenced
// columns positionally (unnest WITH ORDINALITY over conkey/confkey). The old
// kcu x ccu join on constraint_name alone produced a cross product for
// composite foreign keys.
func TestCollectForeignKeysQueryPairsByOrdinal(t *testing.T) {
	q := foreignKeysQuery()
	for _, needle := range []string{
		"WITH ORDINALITY",
		"con.conkey, con.confkey",
		"src_att.attnum = k.src_attnum",
		"ref_att.attnum = k.ref_attnum",
		"con.contype = 'f'",
	} {
		if !strings.Contains(q, needle) {
			t.Errorf("FK query missing positional pairing element %q", needle)
		}
	}
	if strings.Contains(q, "constraint_column_usage") {
		t.Error("FK query must not use the cross-product-prone ccu join")
	}
}

// F-07 anchor: pg_get_viewdef always emits PG casts (::text, ::character
// varying(N), ::timestamp without time zone) which TiDB rejects with 1064 —
// BuildViewDDL must strip them, while leaving casts inside string literals
// and non-cast colons untouched.
func TestBuildViewDDLStripsPGCasts(t *testing.T) {
	b := NewDDLBuilder()
	cases := []struct{ in, want string }{
		{"SELECT id, v::text FROM vsrc", "SELECT id, v FROM vsrc"},
		{"SELECT id::character varying(10) FROM t", "SELECT id FROM t"},
		{"SELECT ts::timestamp without time zone FROM t", "SELECT ts FROM t"},
		{"SELECT d::double precision, n::numeric(10,2) FROM t", "SELECT d, n FROM t"},
		{"SELECT 'x::y' AS lit FROM t", "SELECT 'x::y' AS lit FROM t"},
		{"SELECT id FROM t", "SELECT id FROM t"},
	}
	for _, c := range cases {
		got := b.BuildViewDDL(View{Name: "v", Definition: c.in})
		want := "CREATE OR REPLACE VIEW `v` AS " + c.want
		if got != want {
			t.Errorf("BuildViewDDL(%q)\n got %s\nwant %s", c.in, got, want)
		}
	}
}
