package target

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ValidationReport is the row-count parity result for a CIR migration (#t81
// Step 3 — data consistency verification).
type ValidationReport struct {
	Tables       []TableValidation
	TotalTables  int
	FailedTables int
	AllPassed    bool
}

// TableValidation is the row-count comparison for one table.
type TableValidation struct {
	Name       string `json:"name"`
	SourceRows int64  `json:"source_rows"`
	TargetRows int64  `json:"target_rows"`
	Passed     bool   `json:"passed"`
}

// ValidateRowCounts checks row-count parity (source vs target) for each CIR
// table — the core "did all data arrive" consistency check. Source-agnostic:
// just COUNT(*) on both DBs. Deeper value-level validation (checksum/sample) is
// a follow-up (the existing validator's strategies are PG-specific).
func ValidateRowCounts(ctx context.Context, sourceDB, tidbDB *sql.DB, schema *source.Schema) (*ValidationReport, error) {
	rpt := &ValidationReport{}
	for _, t := range schema.Tables {
		srcN, err := countRows(ctx, sourceDB, t.Name)
		if err != nil {
			return nil, fmt.Errorf("validate: count source %q: %w", t.Name, err)
		}
		tgtN, err := countRows(ctx, tidbDB, t.Name)
		if err != nil {
			return nil, fmt.Errorf("validate: count target %q: %w", t.Name, err)
		}
		tv := TableValidation{Name: t.Name, SourceRows: srcN, TargetRows: tgtN, Passed: srcN == tgtN}
		if !tv.Passed {
			rpt.FailedTables++
		}
		rpt.Tables = append(rpt.Tables, tv)
		rpt.TotalTables++
	}
	rpt.AllPassed = rpt.FailedTables == 0
	return rpt, nil
}

func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+QuoteIdent(table)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
