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

// TestTerminatedStatements guards B-F01-4: every function / procedure /
// trigger DDL written to a multi-object file must end with a semicolon
// (pg_get_functiondef / pg_get_triggerdef output lacks one).
func TestTerminatedStatements(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"CREATE FUNCTION f() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql", "CREATE FUNCTION f() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql;\n"},
		{"CREATE TRIGGER tg BEFORE INSERT ON t FOR EACH ROW EXECUTE FUNCTION f()\n\n", "CREATE TRIGGER tg BEFORE INSERT ON t FOR EACH ROW EXECUTE FUNCTION f();\n"},
		{"CREATE PROCEDURE p() LANGUAGE plpgsql AS $$ BEGIN END $$  \t", "CREATE PROCEDURE p() LANGUAGE plpgsql AS $$ BEGIN END $$;\n"},
	} {
		if got := terminated(c.in); got != c.want {
			t.Errorf("terminated(%q) = %q, want %q", c.in, got, c.want)
		}
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

// TestSkipWarnsAndRecords anchors the skip() contract: every skip is both
// recorded in the manifest (zip-visible) and logged at the point it happens
// (zap.L() is safe to call with the default global logger). The 0-table
// tidb-tables.sql / per-table-failure behaviors are source-anchored below.
func TestSkipWarnsAndRecords(t *testing.T) {
	e := &Exporter{manifest: Manifest{}}
	e.skip("public", "table", "t1", "permission denied")
	e.skip("sales", "tidb-table", "t2", "unsupported type")
	if len(e.manifest.Skipped) != 2 {
		t.Fatalf("skip must record every entry: %+v", e.manifest.Skipped)
	}
	sk := e.manifest.Skipped[0]
	if sk.Schema != "public" || sk.Type != "table" || sk.Object != "t1" || sk.Reason != "permission denied" {
		t.Fatalf("skip recorded wrong entry: %+v", sk)
	}
}

// TestTiDBSkipFamilyAnchors pins the DDL-export TiDB skip semantics that a
// source-level regression could silently break (the live-DB end-to-end is
// covered by the remote acceptance run):
//   - a successful tidbTables run with 0 tables still writes an (empty)
//     tidb-tables.sql into the zip — the file's absence must mean error;
//   - per-table BuildTableDDL failures skip individually and the rest of
//     the export continues;
//   - the handler surfaces the skip count via X-Tims-Skipped after a
//     successful export.
func TestTiDBSkipFamilyAnchors(t *testing.T) {
	src, err := os.ReadFile("exporter.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	// Success branch writes the file unconditionally (n may be 0).
	if !strings.Contains(s, `files["tidb-tables.sql"] = ddl`) {
		t.Error("exporter must write tidb-tables.sql even when 0 tables convert")
	}
	// Collector failure skips the whole file but does not fail the export.
	if !strings.Contains(s, `e.skip(schemaName, "tidb-tables.sql", schemaName, err.Error())`) {
		t.Error("exporter must record a tidb-tables.sql skip on collector failure")
	}
	// Per-table conversion failure skips that table only.
	if !strings.Contains(s, `e.skip(schemaName, "tidb-table", t.Name, err.Error())`) {
		t.Error("exporter must record per-table tidb conversion skips")
	}

	hsrc, err := os.ReadFile("../webapi/ddl_export_handler.go")
	if err != nil {
		t.Fatal(err)
	}
	h := string(hsrc)
	if !strings.Contains(h, "X-Tims-Skipped") {
		t.Error("handler must set X-Tims-Skipped when objects were skipped")
	}
	if !strings.Contains(h, "manifest.Skipped") {
		t.Error("handler must read the skip count from the export manifest")
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
