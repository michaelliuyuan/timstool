package target

import (
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// RenderCSVRow mirrors the PG path's Lightning TSV format (TAB-separated, \N
// NULL, backslash-escape) so the same Lightning config reads it.
func TestRenderCSVRow_TSVFormat(t *testing.T) {
	cols := []source.Column{
		{Name: "id"}, {Name: "name"}, {Name: "amt"}, {Name: "flag"}, {Name: "ts"}, {Name: "note"},
	}
	ts := time.Date(2026, 6, 21, 17, 5, 0, 0, time.UTC)
	row := source.Row{
		"id":   {Val: int64(7)},
		"name": {Val: "a\tb"}, // TAB in value → escaped \t
		"amt":  {Val: 12.5},
		"flag": {Val: true},
		"ts":   {Val: ts},
		"note": {Val: nil}, // NULL → \N
	}
	got := RenderCSVRow(cols, row)
	if strings.Count(got, "\t") != 5 {
		t.Errorf("want 5 TAB separators (6 fields), got %d:\n%s", strings.Count(got, "\t"), got)
	}
	if !strings.HasPrefix(got, "7\t") {
		t.Errorf("int field:\n%s", got)
	}
	if !strings.Contains(got, "a\\tb") {
		t.Errorf("TAB in value must escape to \\t:\n%s", got)
	}
	if !strings.Contains(got, "\t12.5\t") {
		t.Errorf("float field:\n%s", got)
	}
	if !strings.Contains(got, "\t1\t") { // bool true → 1
		t.Errorf("bool field:\n%s", got)
	}
	if !strings.Contains(got, "2026-06-21 17:05:00") {
		t.Errorf("datetime field:\n%s", got)
	}
	if !strings.HasSuffix(got, "\t"+`\N`+"\n") {
		t.Errorf("trailing NULL field must be \\N:\n%s", got)
	}
}

func TestRenderCSVRow_NullAndBackslash(t *testing.T) {
	cols := []source.Column{{Name: "a"}, {Name: "b"}}
	row := source.Row{
		"a": {Val: nil},           // NULL → \N
		"b": {Val: `back\slash`},  // backslash → escaped \\
	}
	got := RenderCSVRow(cols, row)
	if !strings.HasPrefix(got, `\N`+"\t") {
		t.Errorf("NULL field must be \\N:\n%s", got)
	}
	if !strings.Contains(got, `back\\slash`) {
		t.Errorf("backslash must be escaped:\n%s", got)
	}
}
