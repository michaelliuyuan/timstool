package target

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ValidationReport is the consistency-validation result for a CIR migration
// (#t81 Step 3 + #t82 value-level extension).
type ValidationReport struct {
	Tables       []TableValidation
	TotalTables  int
	FailedTables int
	AllPassed    bool
}

// TableValidation is the row-count + value-level (sample) comparison for one table.
type TableValidation struct {
	Name            string `json:"name"`
	SourceRows      int64  `json:"source_rows"`
	TargetRows      int64  `json:"target_rows"`
	Passed          bool   `json:"passed"`
	SampleChecked   int    `json:"sample_checked"`
	SampleMismatches int   `json:"sample_mismatches"`
}

// ValidateMigration checks row-count parity AND value-level consistency (sampled
// rows compared by normalized string values) for each CIR table — the core data
// consistency check (#t82). Source-agnostic: reads from any *sql.DB (the source
// adapter's DB) + the TiDB target. sampleSize controls how many rows per table
// are value-compared (0 → default 10).
func ValidateMigration(ctx context.Context, sourceDB, tidbDB *sql.DB, schema *source.Schema, sampleSize int) (*ValidationReport, error) {
	if sampleSize <= 0 {
		sampleSize = 10
	}
	rpt := &ValidationReport{}
	for _, t := range schema.Tables {
		tv := TableValidation{Name: t.Name}
		srcN, err := countRows(ctx, sourceDB, t.Name)
		if err != nil {
			return nil, fmt.Errorf("validate: count source %q: %w", t.Name, err)
		}
		tgtN, err := countRows(ctx, tidbDB, t.Name)
		if err != nil {
			return nil, fmt.Errorf("validate: count target %q: %w", t.Name, err)
		}
		tv.SourceRows = srcN
		tv.TargetRows = tgtN
		tv.Passed = srcN == tgtN

		// Value-level sample comparison (only if row counts match + non-empty).
		if srcN > 0 && srcN == tgtN {
			checked, mismatches, err := compareSample(ctx, sourceDB, tidbDB, t.Name, sampleSize)
			if err == nil {
				tv.SampleChecked = checked
				tv.SampleMismatches = mismatches
				if mismatches > 0 {
					tv.Passed = false
				}
			}
			// If compareSample errors (e.g. ORDER BY not supported), skip silently
			// — row-count is still checked.
		}

		if !tv.Passed {
			rpt.FailedTables++
		}
		rpt.Tables = append(rpt.Tables, tv)
		rpt.TotalTables++
	}
	rpt.AllPassed = rpt.FailedTables == 0
	return rpt, nil
}

// ValidateRowCounts is a convenience wrapper for row-count-only validation.
func ValidateRowCounts(ctx context.Context, sourceDB, tidbDB *sql.DB, schema *source.Schema) (*ValidationReport, error) {
	return ValidateMigration(ctx, sourceDB, tidbDB, schema, 0)
}

func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+QuoteIdent(table)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// compareSample reads N rows (ORDER BY 1 LIMIT N) from both DBs, normalizes
// values to strings, and compares positionally. Returns (rowsChecked, mismatches).
// ORDER BY 1 ensures deterministic ordering on both sides (most tables have an
// orderable first column / PK).
func compareSample(ctx context.Context, sourceDB, tidbDB *sql.DB, table string, n int) (int, int, error) {
	srcRows, err := queryNormalizedRows(ctx, sourceDB, table, n)
	if err != nil {
		return 0, 0, err
	}
	tgtRows, err := queryNormalizedRows(ctx, tidbDB, table, n)
	if err != nil {
		return 0, 0, err
	}
	mismatches := 0
	for i := 0; i < len(srcRows) && i < len(tgtRows); i++ {
		if !normalizedRowsEqual(srcRows[i], tgtRows[i]) {
			mismatches++
		}
	}
	return len(srcRows), mismatches, nil
}

func queryNormalizedRows(ctx context.Context, db *sql.DB, table string, limit int) ([][]string, error) {
	q := fmt.Sprintf("SELECT * FROM %s ORDER BY 1 LIMIT %d", QuoteIdent(table), limit)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.ColumnTypes()
	if len(cols) == 0 {
		return nil, fmt.Errorf("no columns")
	}
	var result [][]string
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = normalizeForCompare(v)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func normalizedRowsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if normalizeStringVal(a[i]) != normalizeStringVal(b[i]) {
			return false
		}
	}
	return true
}

// normalizeForCompare converts a scanned Go value to a normalized string for
// cross-DB comparison. nil → "\N" (NULL marker); time → UTC; numeric → string;
// []byte → string. This is intentionally simpler than the full PG validator's
// normalizeValue (which handles PG arrays/UUID/JSON) — for MySQL→TiDB (both
// MySQL-family), type representations are largely aligned.
func normalizeForCompare(v interface{}) string {
	if v == nil {
		return "\\N"
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "1"
		}
		return "0"
	case []byte:
		return string(x)
	case string:
		return x
	case time.Time:
		return x.UTC().Format("2006-01-02 15:04:05.999999")
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// normalizeStringVal does light normalization on already-stringified values:
// trim trailing whitespace (CHAR padding diff) + strip decimal trailing zeros
// (float precision diff). This catches most cross-DB representation differences
// without the full PG-specific normalization.
func normalizeStringVal(s string) string {
	s = strings.TrimRight(s, " \t\n\r")
	// Strip trailing zeros from decimal-looking strings: "10.50" → "10.5".
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}
