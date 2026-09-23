package ddlexport

import (
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
