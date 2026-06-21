package target

import (
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/source"
)

func TestRenderCSVRow_Types(t *testing.T) {
	cols := []source.Column{
		{Name: "id", TiDBType: "BIGINT"},
		{Name: "name", TiDBType: "VARCHAR(50)"},
		{Name: "amount", TiDBType: "DECIMAL(10,2)"},
		{Name: "flag", TiDBType: "TINYINT(1)"},
		{Name: "created", TiDBType: "DATETIME"},
		{Name: "data", TiDBType: "BLOB"},
		{Name: "note", TiDBType: "TEXT"},
	}
	ts := time.Date(2026, 6, 21, 17, 5, 0, 0, time.UTC)
	row := source.Row{
		"id":      {TiDBType: "BIGINT", Val: int64(7)},
		"name":    {TiDBType: "VARCHAR(50)", Val: "hi, there"}, // comma -> quoted
		"amount":  {TiDBType: "DECIMAL(10,2)", Val: 12.5},
		"flag":    {TiDBType: "TINYINT(1)", Val: true},
		"created": {TiDBType: "DATETIME", Val: ts},
		"data":    {TiDBType: "BLOB", Val: []byte{0xAB, 0xCD}},
		"note":    {TiDBType: "TEXT", Val: nil}, // NULL
	}
	got := RenderCSVRow(cols, row)
	// Fields: 7 | "hi, there" | 12.5 | 1 | 2026-06-21 17:05:00 | \xabcd | \N
	if !strings.HasPrefix(got, "7,") {
		t.Errorf("int field: %q", got)
	}
	if !strings.Contains(got, `"hi, there"`) {
		t.Errorf("string with comma must be quoted: %q", got)
	}
	if !strings.Contains(got, ",12.5,") {
		t.Errorf("decimal field: %q", got)
	}
	if !strings.Contains(got, ",1,") { // bool true -> 1
		t.Errorf("bool field: %q", got)
	}
	if !strings.Contains(got, "2026-06-21 17:05:00") {
		t.Errorf("datetime field: %q", got)
	}
	if !strings.Contains(got, `\xabcd`) {
		t.Errorf("blob hex field: %q", got)
	}
	if !strings.HasSuffix(got, `,`+`\N`+"\n") {
		t.Errorf("trailing NULL field must be \\N: %q", got)
	}
}

func TestRenderCSVRow_NullAndEmptyAndLiteralN(t *testing.T) {
	cols := []source.Column{
		{Name: "a"}, {Name: "b"}, {Name: "c"},
	}
	row := source.Row{
		"a": {Val: nil},           // NULL -> \N
		"b": {Val: ""},            // empty string -> "" (distinct from NULL)
		"c": {Val: `\N`},          // literal "\N" -> quoted to avoid NULL ambiguity
	}
	got := RenderCSVRow(cols, row)
	if !strings.HasPrefix(got, `\N,`) {
		t.Errorf("NULL must be \\N: %q", got)
	}
	if !strings.Contains(got, `,"",`) {
		t.Errorf("empty string must be \"\": %q", got)
	}
	if !strings.Contains(got, `"\N"`) {
		t.Errorf("literal \\N must be quoted: %q", got)
	}
}
