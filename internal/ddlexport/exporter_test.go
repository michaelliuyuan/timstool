package ddlexport

import (
	"os"
	"strings"
	"testing"
)

func TestParsePGArray(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"{a,b,c}", []string{"a", "b", "c"}},
		{"{}", []string{}},
		{`{"with,comma","with""quote",plain}`, []string{"with,comma", `with"quote`, "plain"}},
		{"{active,disabled}", []string{"active", "disabled"}},
		{"not-an-array", nil},
	}
	for _, c := range cases {
		got := ParsePGArray(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ParsePGArray(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParsePGArray(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestMarshalManifest(t *testing.T) {
	m := Manifest{
		Counts: ObjectCounts{
			"public": {"tables.sql": 3, "indexes.sql": 2},
			"sales":  {"views.sql": 1},
		},
		Skipped: []SkippedObject{{Schema: "public", Type: "table", Object: "t", Reason: "permission denied"}},
	}
	out := marshalManifest(m)
	for _, want := range []string{`"public"`, `"tables.sql": 3`, `"sales"`, `"permission denied"`, `"total_objects"`} {
		if !strings.Contains(out, want) {
			t.Errorf("manifest missing %s:\n%s", want, out)
		}
	}
}

func TestQiEscapesDoubleQuotes(t *testing.T) {
	if got := qi(`we"ird`); got != `"we""ird"` {
		t.Errorf("qi escaping failed: %s", got)
	}
}

// TestCatalogQueryAnchors pins every catalog query in exporter.go to the
// PostgreSQL 16 documented column/view names (regression guard for the
// pg_sequences.schemaid / minimum_value class of bugs).
func TestCatalogQueryAnchors(t *testing.T) {
	src, err := os.ReadFile("exporter.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	block := func(marker string) string {
		i := strings.Index(s, marker)
		if i < 0 {
			t.Fatalf("marker %q not found in exporter.go", marker)
		}
		end := strings.Index(s[i:], "`")
		if end < 0 {
			t.Fatalf("unterminated query literal after %q", marker)
		}
		return s[i : i+end]
	}
	for _, tc := range []struct {
		name    string
		marker  string
		require []string
		forbid  []string
	}{
		{"schemas", "SELECT schema_name FROM information_schema.schemata",
			[]string{"schema_name NOT IN ('pg_catalog','information_schema')"}, nil},
		{"views", "SELECT c.relname, c.relkind, pg_get_viewdef",
			[]string{"c.relkind IN ('v','m')", "n.oid = c.relnamespace"}, nil},
		{"sequences", "SELECT s.sequencename, s.data_type",
			[]string{"s.min_value", "s.max_value", "s.increment_by", "s.schemaname = $1"},
			[]string{"schemaid", "minimum_value", "maximum_value", "s.increment,", "pg_namespace"}},
		{"functions", "WHERE n.nspname = $1 AND p.prokind = 'f'",
			[]string{"p.prokind = 'f'"}, nil},
		{"procedures", "WHERE n.nspname = $1 AND p.prokind = 'p'",
			[]string{"p.prokind = 'p'"}, nil},
		{"triggers", "SELECT t.tgname, pg_get_triggerdef",
			[]string{"NOT t.tgisinternal"}, nil},
		{"indexes", "SELECT t.relname, i.relname, pg_get_indexdef",
			[]string{"NOT x.indisprimary", "con.conindid = i.oid", "con.oid IS NULL"}, nil},
		{"tables-list", "SELECT c.relname, obj_description(c.oid, 'pg_class')",
			[]string{"c.relkind = 'r'", "n.oid = c.relnamespace"}, nil},
		{"tables-columns", "SELECT a.attname, format_type(a.atttypid, a.atttypmod)",
			[]string{"NOT a.attisdropped", "a.attidentity", "a.attgenerated", "pg_get_expr(ad.adbin, ad.adrelid)"}, nil},
		{"tables-constraints", "SELECT con.contype, con.conname, pg_get_constraintdef(con.oid)",
			[]string{"c.oid = con.conrelid", "n.nspname = $1 AND c.relname = $2", "con.contype IN ('p','u')"},
			[]string{"($1 || '.' || $2)::regclass", "format('%I.%I', $1, $2)::regclass"}},
		{"foreign-keys", "SELECT rt.relname, con.conname, pg_get_constraintdef(con.oid)",
			[]string{"con.contype = 'f'"}, []string{"conrelid::regclass::text"}},
		{"enum-types", "SELECT t.typname, array_agg(e.enumlabel",
			[]string{"t.typtype = 'e'", "e.enumtypid = t.oid", "e.enumsortorder"}, nil},
		{"composite-types", "SELECT t.typname, a.attname, format_type(a.atttypid, a.atttypmod)",
			[]string{"t.typtype = 'c'", "c.relkind = 'c'"}, nil},
		{"domains", "SELECT t.typname, format_type(t.typbasetype, t.typtypmod)",
			[]string{"t.typtype = 'd'", "t.typnotnull", "t.typdefault"}, nil},
		{"domain-checks", "SELECT dt.typname, con.conname, pg_get_constraintdef(con.oid)",
			[]string{"con.contype = 'c'", "con.contypid <> 0"}, nil},
	} {
		q := block(tc.marker)
		for _, r := range tc.require {
			if !strings.Contains(q, r) {
				t.Errorf("%s: query missing %q:\n%s", tc.name, r, q)
			}
		}
		for _, f := range tc.forbid {
			if strings.Contains(q, f) {
				t.Errorf("%s: query must not contain %q:\n%s", tc.name, f, q)
			}
		}
	}
	if !strings.Contains(s, "pg_get_functiondef(p.oid)") {
		t.Error("source must render functions/procedures via pg_get_functiondef(p.oid)")
	}
}

// TestManifestForPerSchema verifies the per-schema self-contained manifest
// (B-F01-2): counts and skipped entries of other schemas must not leak in.
func TestManifestForPerSchema(t *testing.T) {
	e := &Exporter{manifest: Manifest{
		Counts: ObjectCounts{"public": {"tables.sql": 3}, "sales": {"views.sql": 1}},
		Skipped: []SkippedObject{
			{Schema: "public", Type: "table", Object: "t1", Reason: "denied"},
			{Schema: "sales", Type: "view", Object: "v1", Reason: "denied"},
		},
	}}
	m := e.manifestFor("public")
	if m.Total != 3 || m.Counts["sales"] != nil {
		t.Errorf("manifestFor leaked other schemas: %+v", m)
	}
	if len(m.Skipped) != 1 || m.Skipped[0].Schema != "public" {
		t.Errorf("manifestFor skipped filter failed: %+v", m.Skipped)
	}
}
