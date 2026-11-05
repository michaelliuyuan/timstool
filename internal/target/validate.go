package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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
	Name             string `json:"name"`
	SourceRows       int64  `json:"source_rows"`
	TargetRows       int64  `json:"target_rows"`
	Passed           bool   `json:"passed"`
	SampleChecked    int    `json:"sample_checked"`
	SampleMismatches int    `json:"sample_mismatches"`
}

// queryTimeout bounds every validation query (count + value-sample scans,
// #t94 P0-2): a hung query fails the current table's validation entry
// instead of stalling the whole migration validation forever. A package
// default because target.ValidateMigration has no compare-config plumbing
// (its signature is frozen — orchestrator binds it via cirValidateMigration).
var queryTimeout = 5 * time.Minute

// validateParallel bounds the per-table concurrency of ValidateMigration
// (#t95 P0-5): the CIR validate phase used to walk every table serially, so
// a 300+-table migration sat in validate with no tables_done movement and
// looked stuck. Both source and TiDB pools are capped at 8 conns, so the
// default of 4 keeps per-table concurrency safely under pool capacity.
var validateParallel = 4

// tableValidatedHook is an optional per-table completion hook installed by
// the orchestrator before a CIR validate run: fired EXACTLY once per table
// after its count (+sample) verdict is final — success or a fatal error —
// so checkpoint progress (tables_done) climbs during the validate phase
// instead of jumping at the very end (#t95 P0-5). nil = no hook. Must be
// safe for concurrent calls (table goroutines fire it in parallel).
var tableValidatedHook func(TableValidation, error)

// SetTableValidatedHook installs (fn) or clears (nil) the per-table
// completion hook consumed by ValidateMigration.
func SetTableValidatedHook(fn func(TableValidation, error)) {
	tableValidatedHook = fn
}

// TableValidatedHook returns the currently installed per-table hook (nil
// when none) — the orchestrator CIR tests assert the wiring is live during
// validate and cleared afterwards.
func TableValidatedHook() func(TableValidation, error) {
	return tableValidatedHook
}

// ValidateMigration checks row-count parity AND value-level consistency (sampled
// rows compared by normalized string values) for each CIR table — the core data
// consistency check (#t82). Source-agnostic: reads from any *sql.DB (the source
// adapter's DB) + the TiDB target. sampleSize controls how many rows per table
// are value-compared (0 → default 10).
func ValidateMigration(ctx context.Context, sourceDB, tidbDB *sql.DB, schema *source.Schema, sampleSize int) (*ValidationReport, error) {
	// #t95 P0-5: per-table validation runs on a fixed concurrency pool
	// (validateParallel, default 4) instead of serially — a 300+-table CIR
	// migration used to sit in validate with no tables_done movement. Each
	// table is ONE work unit. Results are collected into their original
	// schema positions, so the report order stays deterministic regardless
	// of completion order. ctx cancellation makes queued goroutines return
	// without touching the DB.
	results := make([]TableValidation, len(schema.Tables))
	var wg sync.WaitGroup
	sem := make(chan struct{}, validateParallel)
	var (
		failedMu sync.Mutex
		failed   int
		errMu    sync.Mutex
		firstErr error
	)

	for i, t := range schema.Tables {
		wg.Add(1)
		go func(i int, t source.Table) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errMu.Lock()
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				errMu.Unlock()
				return
			}
			defer func() { <-sem }()

			tv, terr := validateTable(ctx, sourceDB, tidbDB, t.Name, sampleSize)
			if terr != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = terr
				}
				errMu.Unlock()
			} else {
				results[i] = tv
				if !tv.Passed {
					failedMu.Lock()
					failed++
					failedMu.Unlock()
				}
			}
			// #t95 P0-5: per-table completion hook (checkpoint progress) —
			// fired even for a failed table so the UI marks it red promptly.
			if tableValidatedHook != nil {
				tableValidatedHook(tv, terr)
			}
		}(i, t)
	}
	wg.Wait()

	if firstErr != nil {
		// The report contract is unchanged: ANY table-level failure aborts
		// the whole validation (nil report + error), exactly like the serial
		// implementation.
		return nil, firstErr
	}

	rpt := &ValidationReport{
		Tables:       results,
		TotalTables:  len(results),
		FailedTables: failed,
	}
	rpt.AllPassed = rpt.FailedTables == 0
	return rpt, nil
}

// validateTable runs the count (+optional sample) comparison for ONE table.
// The error contract mirrors the serial implementation: a count failure
// aborts the table with the same "validate: count ..." message.
func validateTable(ctx context.Context, sourceDB, tidbDB *sql.DB, name string, sampleSize int) (TableValidation, error) {
	tv := TableValidation{Name: name}
	srcN, err := countRows(ctx, sourceDB, name)
	if err != nil {
		return tv, fmt.Errorf("validate: count source %q: %w", name, err)
	}
	tgtN, err := countRows(ctx, tidbDB, name)
	if err != nil {
		return tv, fmt.Errorf("validate: count target %q: %w", name, err)
	}
	tv.SourceRows = srcN
	tv.TargetRows = tgtN
	tv.Passed = srcN == tgtN

	// Value-level sample comparison only when explicitly requested (sampleSize > 0).
	// Row-count parity alone is the default (ValidateRowCounts passes 0).
	if sampleSize > 0 && srcN > 0 && srcN == tgtN {
		checked, mismatches, err := compareSample(ctx, sourceDB, tidbDB, name, sampleSize)
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
	return tv, nil
}

// ValidateRowCounts is a convenience wrapper for row-count-only validation.
func ValidateRowCounts(ctx context.Context, sourceDB, tidbDB *sql.DB, schema *source.Schema) (*ValidationReport, error) {
	return ValidateMigration(ctx, sourceDB, tidbDB, schema, 0)
}

func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	// P0-2: query-level deadline — a hung COUNT fails only this table.
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var n int64
	if err := db.QueryRowContext(qctx, "SELECT COUNT(*) FROM "+QuoteIdent(table)).Scan(&n); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, fmt.Errorf("query timeout (%s): %w", queryTimeout, err)
		}
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
	checked, mismatches := compareNormalized(srcRows, tgtRows)
	return checked, mismatches, nil
}

// compareNormalized compares two normalized row sets positionally and returns
// (rowsChecked, mismatches). Pure — extracted from compareSample so a negative
// test (injected value diff → detected) can run without a live DB (#t82 gate ①).
func compareNormalized(srcRows, tgtRows [][]string) (checked, mismatches int) {
	for i := 0; i < len(srcRows) && i < len(tgtRows); i++ {
		if !normalizedRowsEqual(srcRows[i], tgtRows[i]) {
			mismatches++
		}
	}
	return len(srcRows), mismatches
}

func queryNormalizedRows(ctx context.Context, db *sql.DB, table string, limit int) ([][]string, error) {
	// P0-2: query-level deadline on the value-sample scan.
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	q := fmt.Sprintf("SELECT * FROM %s ORDER BY 1 LIMIT %d", QuoteIdent(table), limit)
	rows, err := db.QueryContext(qctx, q)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("query timeout (%s): %w", queryTimeout, err)
		}
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.ColumnTypes()
	if len(cols) == 0 {
		return nil, fmt.Errorf("no columns")
	}
	// Snapshot the column DB type names once (e.g. TIMESTAMP vs DATETIME) so
	// normalizeForCompare can branch time semantics per column (#t82 gap A).
	dbTypes := make([]string, len(cols))
	for i, c := range cols {
		dbTypes[i] = c.DatabaseTypeName()
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
			row[i] = normalizeForCompare(v, dbTypes[i])
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
// cross-DB comparison. dbType is the column's database type name (from
// rows.ColumnTypes().DatabaseTypeName), used to branch time semantics.
//
// Time-type branch (架构师 spec, #t82 gap A):
//   - TIMESTAMP has instant semantics → .UTC() so the same instant read in
//     different session/loc zones compares equal (the c_ts case).
//   - DATETIME/DATE/TIME/YEAR are wall-clock / TZ-naive → formatted as-is.
//     .UTC() here would shift by the local offset and fabricate a mismatch
//     that doesn't exist (the c_dt trap). Correctness gate: c_ts + c_dt both pass.
//
// nil → "\N" (NULL marker); []byte → string; numeric → string. Intentionally
// simpler than the full PG validator's normalizeValue (PG arrays/UUID/JSON) —
// MySQL→TiDB (both MySQL-family) type representations are largely aligned.
func normalizeForCompare(v interface{}, dbType string) string {
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
		if strings.EqualFold(dbType, "TIMESTAMP") {
			x = x.UTC() // instant semantics — compare the same moment across zones
		}
		return x.Format("2006-01-02 15:04:05.999999")
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
