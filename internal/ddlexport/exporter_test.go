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

// TestSequencesQueryAnchors guards against catalog-column regressions such as
// the pg_sequences.schemaid bug (B-F01-1: PG16 SQLSTATE 42703 — the view only
// exposes schemaname, so the export must filter by schemaname and never
// reference schemaid or join pg_namespace on it).
func TestSequencesQueryAnchors(t *testing.T) {
	src, err := os.ReadFile("exporter.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "FROM pg_sequences")
	if i < 0 {
		t.Fatal("sequences query not found in exporter.go")
	}
	// Inspect the sequences query block (up to the closing backtick).
	end := strings.Index(s[i:], "`")
	if end < 0 {
		t.Fatal("unterminated query literal")
	}
	q := s[i : i+end]
	if !strings.Contains(q, "s.schemaname = $1") {
		t.Errorf("sequences query must filter by s.schemaname = $1, got:\n%s", q)
	}
	if strings.Contains(q, "schemaid") || strings.Contains(q, "pg_namespace") {
		t.Errorf("sequences query must not reference schemaid/pg_sequences schema joins, got:\n%s", q)
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
