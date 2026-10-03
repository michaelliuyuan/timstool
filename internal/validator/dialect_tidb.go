package validator

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// tidbDialect implements TargetDialect. Every method body is a MECHANICAL
// relocation of the pre-MS-03 inline TiDB logic; the line-number map lives in
// docs/MS03-DIALECT-MAP.md (baseline d08e661). No concurrency, no fallback
// policy, no guard logic lives here - those stay in the main flow.
type tidbDialect struct{}

// QuoteIdent relocates quoteMySQL (validator.go :1177-1179).
func (tidbDialect) QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// Qualify relocates the single-part `table` reference shape (the target
// database is a connection property; checksum.go :298-303).
func (d tidbDialect) Qualify(schema, table string) string {
	return d.QuoteIdent(table)
}

// EstimateRows relocates the SHOW TABLE STATUS estimate (quick.go :44-73,
// escapeSQLLike :90-96). An error means "estimate unavailable" (query error
// only, matching quick.go :54-64); a Rows value that fails to parse yields
// 0+nil, preserving quick.go :67-72 semantics - the fallback decision belongs
// to the main flow.
func (d tidbDialect) EstimateRows(ctx context.Context, q dialectQueryer, schema, table string) (int64, error) {
	var tidbCount sql.NullInt64
	var tidbName, tidbEngine, tidbVersion sql.NullString
	var tidbRowFormat, tidbRows, tidbAvgRowLen, tidbDataLen, tidbMaxDataLen, tidbIndexLen, tidbAutoInc, tidbCreateTime, tidbUpdateTime, tidbCheckTime, tidbCollation, tidbChecksum, tidbCreateOpts, tidbComment sql.NullString
	err := q.QueryRowContext(ctx,
		fmt.Sprintf("SHOW TABLE STATUS LIKE '%s'", escapeSQLLike(table))).Scan(
		&tidbName, &tidbEngine, &tidbVersion, &tidbRowFormat, &tidbRows,
		&tidbAvgRowLen, &tidbDataLen, &tidbMaxDataLen, &tidbIndexLen,
		&tidbAutoInc, &tidbCreateTime, &tidbUpdateTime, &tidbCheckTime,
		&tidbCollation, &tidbChecksum, &tidbCreateOpts, &tidbComment)
	if err != nil {
		return 0, err
	}
	if tidbRows.Valid {
		var val int64
		if _, err := fmt.Sscanf(tidbRows.String, "%d", &val); err == nil {
			tidbCount = sql.NullInt64{Int64: val, Valid: true}
		}
	}
	return tidbCount.Int64, nil
}

// CountExact relocates the TiDB exact-count shape (quick.go :58-59,
// validator.go :419).
func (d tidbDialect) CountExact(ctx context.Context, q dialectQueryer, schema, table string) (int64, error) {
	var count sql.NullInt64
	err := q.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", d.QuoteIdent(table))).Scan(&count)
	return count.Int64, err
}

// WmPredicateFragment relocates wmWhereMySQL (wmfilter.go :61-63) - the
// driver "?" placeholder; wmOp semantics stay in the main flow.
func (d tidbDialect) WmPredicateFragment(wm *config.WatermarkFilter) string {
	return fmt.Sprintf("%s %s ?", d.QuoteIdent(wm.Column), wmOp(wm))
}

// BuildSelect relocates the ordered+paged fetch shapes (checksum.go :298-303).
func (d tidbDialect) BuildSelect(schema, table, wmFragment, orderBy string, limit, offset int64) string {
	if wmFragment != "" {
		return fmt.Sprintf("SELECT * FROM %s WHERE %s ORDER BY %s LIMIT %d OFFSET %d",
			d.QuoteIdent(table), wmFragment, quoteOrderByCols(orderBy, d.QuoteIdent), limit, offset)
	}
	return fmt.Sprintf("SELECT * FROM %s ORDER BY %s LIMIT %d OFFSET %d",
		d.QuoteIdent(table), quoteOrderByCols(orderBy, d.QuoteIdent), limit, offset)
}

// BuildSelectAll relocates the full-table fetch shapes (nopk.go :626-631).
func (d tidbDialect) BuildSelectAll(schema, table, wmFragment string) string {
	if wmFragment != "" {
		return fmt.Sprintf("SELECT * FROM %s WHERE %s", d.QuoteIdent(table), wmFragment)
	}
	return fmt.Sprintf("SELECT * FROM %s", d.QuoteIdent(table))
}

// SessionInit relocates getTiDBConn's per-connection session setup
// (validator.go getTiDBConn: "SET time_zone = '+00:00'") - P-INC-TZ
// touchpoint: segment 1 relocates this behavior BYTE-identically (including
// the latent -8h state); the fix lands only in segment 2 as an independent
// commit.
func (tidbDialect) SessionInit(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		return fmt.Errorf("set TiDB timezone: %w", err)
	}
	return nil
}

// MatchHashColumns relocates the nopk bucket path's target column mapping
// (nopk.go :646-662): lowercase name match + approximate-float/json skip.
// Row hashing itself stays in the main flow with the paired functions
// computeRowHash (PG rows, nopk.go :617) / computeTiDBRowHash (TiDB rows,
// nopk.go :679) - the pairing must NOT be unified (ruling seq 204-1).
func (tidbDialect) MatchHashColumns(sourceColNames []string, targetCols []*sql.ColumnType) []tidbColMapping {
	tidbColNameToIdx := make(map[string]int)
	for i, c := range targetCols {
		tidbColNameToIdx[strings.ToLower(c.Name())] = i
	}

	var tidbHashCols []tidbColMapping
	for _, name := range sourceColNames {
		idx, ok := tidbColNameToIdx[strings.ToLower(name)]
		if !ok {
			continue
		}
		dt := strings.ToLower(targetCols[idx].DatabaseTypeName())
		if isApproximateFloatType(dt) || strings.Contains(dt, "json") {
			continue
		}
		tidbHashCols = append(tidbHashCols, tidbColMapping{tidbIdx: idx, name: name})
	}
	return tidbHashCols
}
