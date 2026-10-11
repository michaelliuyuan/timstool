package validator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// TestSrcLabelDispatch pins the dynamic source label (source-side report
// labels must stop hard-coding PG): the legacy/empty assembly stays "PG"
// byte-identically, the mysql assembly reads "MySQL".
func TestSrcLabelDispatch(t *testing.T) {
	if got := NewValidator(config.Config{}).srcLabel(); got != "PG" {
		t.Fatalf("empty-type srcLabel = %q, want PG", got)
	}
	pg := config.Config{Source: config.SourceConfig{Type: "postgres", Host: "h", Database: "d"}}
	if got := NewValidator(pg).srcLabel(); got != "PG" {
		t.Fatalf("postgres srcLabel = %q, want PG", got)
	}
	my := config.Config{Source: config.SourceConfig{Type: "mysql", Host: "h", Database: "d"}}
	if got := NewValidator(my).srcLabel(); got != "MySQL" {
		t.Fatalf("mysql srcLabel = %q, want MySQL", got)
	}
}

// TestDiagnoseRowDiffSourceLabel pins the two ruled shapes: the type tag is
// <SRC>(TYPE) and the hex key is src_hex_after, never a hard-coded PG label
// when the source is MySQL.
func TestDiagnoseRowDiffSourceLabel(t *testing.T) {
	srcRow := []string{"2026-09-28 14:23:47"}
	tidbRow := []string{"2026-09-28 06:23:47"}
	srcCols := []colMapping{{name: "update_time", pgIdx: 0}}
	tidbCols := []tidbColMapping{{name: "update_time", tidbIdx: 0}}
	diag := diagnoseRowDiff("MySQL", srcRow, tidbRow, srcCols, tidbCols, nil, nil, nil)
	if !strings.Contains(diag, " MySQL(") {
		t.Fatalf("type tag must carry the MySQL source label: %s", diag)
	}
	if strings.Contains(diag, "PG(") || strings.Contains(diag, "pg_hex_after") {
		t.Fatalf("hard-coded PG label leaked into mysql-source diag: %s", diag)
	}
	if !strings.Contains(diag, "src_hex_after=") {
		t.Fatalf("hex key must be src_hex_after: %s", diag)
	}
	pg := diagnoseRowDiff("PG", srcRow, tidbRow, srcCols, tidbCols, nil, nil, nil)
	if !strings.Contains(pg, " PG(") {
		t.Fatalf("PG assembly keeps the PG label: %s", pg)
	}
}

// TestNoHardcodedPGReportLabels freezes the fix: no non-test source file in
// this package may hard-code a source-side PG label in a user-facing report
// string again (mismatch details, type tags, hex keys, errors, suggestions).
// New intentional PG mentions must go through Validator.srcLabel().
func TestNoHardcodedPGReportLabels(t *testing.T) {
	patterns := []string{
		`not found in PG`, `in PG but`, `PG(`, `pg_hex_after`, `pg=`,
		`PG=`, `PG column types`, `query PG`, `scan PG row`,
		`PG error`, `PG count`, `PG rows`, `PG hashes`, `PG has`,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// Only inspect string literals (report text), not identifiers.
			for _, lit := range stringLiterals(line) {
				for _, p := range patterns {
					if strings.Contains(lit, p) {
						t.Errorf("%s: hard-coded source label %q in user-facing string: %s", f, p, trimmed)
					}
				}
			}
		}
	}
}

// stringLiterals extracts the double-quoted segments of a single source line
// (good enough for the report-format lines this anchor guards).
func stringLiterals(line string) []string {
	var out []string
	start := -1
	for i := 0; i < len(line); i++ {
		if line[i] == '"' {
			if start >= 0 {
				out = append(out, line[start+1:i])
				start = -1
			} else {
				start = i
			}
		}
	}
	return out
}
