package validator

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// postgresDialect implements CompareDialect. Every method body is a MECHANICAL
// relocation of the pre-MS-03 inline PG logic; the line-number map lives in
// docs/MS03-DIALECT-MAP.md (baseline d08e661). No concurrency, no fallback
// policy, no guard logic lives here - those stay in the main flow.
type postgresDialect struct{}

// QuoteIdent relocates quotePG (validator.go :1173-1175).
func (postgresDialect) QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Qualify relocates the two-part "schema"."table" reference shape
// (checksum.go :213-218, validator.go :401-404).
func (d postgresDialect) Qualify(schema, table string) string {
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
}

// EstimateRows relocates the pg_stat_user_tables estimate (quick.go :26-30).
// An error means "estimate unavailable" - the fallback decision belongs to
// the main flow.
func (postgresDialect) EstimateRows(ctx context.Context, q dialectQueryer, schema, table string) (int64, error) {
	var pgCount sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(n_live_tup, 0)
		FROM pg_stat_user_tables
		WHERE schemaname = $1 AND relname = $2
	`, schema, table).Scan(&pgCount)
	return pgCount.Int64, err
}

// CountExact relocates the PG exact-count shape (quick.go :35-36,
// validator.go :404).
func (d postgresDialect) CountExact(ctx context.Context, q dialectQueryer, schema, table string) (int64, error) {
	var count sql.NullInt64
	err := q.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", d.Qualify(schema, table))).Scan(&count)
	return count.Int64, err
}

// WmPredicateFragment relocates wmWherePG (wmfilter.go :56-58) - the $1
// placeholder follows the PG driver; wmOp semantics stay in the main flow.
func (d postgresDialect) WmPredicateFragment(wm *config.WatermarkFilter) string {
	return fmt.Sprintf("%s %s $1", d.QuoteIdent(wm.Column), wmOp(wm))
}

// BuildSelect relocates the ordered+paged fetch shapes (checksum.go :213-218).
func (d postgresDialect) BuildSelect(schema, table, wmFragment, orderBy string, limit, offset int64) string {
	if wmFragment != "" {
		return fmt.Sprintf("SELECT * FROM %s WHERE %s ORDER BY %s LIMIT %d OFFSET %d",
			d.Qualify(schema, table), wmFragment, quoteOrderByCols(orderBy, d.QuoteIdent), limit, offset)
	}
	return fmt.Sprintf("SELECT * FROM %s ORDER BY %s LIMIT %d OFFSET %d",
		d.Qualify(schema, table), quoteOrderByCols(orderBy, d.QuoteIdent), limit, offset)
}

// BuildSelectAll relocates the full-table fetch shapes (nopk.go :170-173).
func (d postgresDialect) BuildSelectAll(schema, table, wmFragment string) string {
	if wmFragment != "" {
		return fmt.Sprintf("SELECT * FROM %s WHERE %s", d.Qualify(schema, table), wmFragment)
	}
	return fmt.Sprintf("SELECT * FROM %s", d.Qualify(schema, table))
}

// ValidateWatermarkColumn relocates checkWatermarkColumn's information_schema
// probe (wmfilter.go :68-84); the type-whitelist judgment (wmAllowedColumnTypes)
// stays in the main flow.
func (postgresDialect) ValidateWatermarkColumn(ctx context.Context, q dialectQueryer, schema, table string, wm *config.WatermarkFilter) error {
	var dataType string
	err := q.QueryRowContext(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
		schema, table, wm.Column).Scan(&dataType)
	if err == sql.ErrNoRows {
		return fmt.Errorf("水位列 %s 不存在于表 %s（水位过滤仅支持列存在的表参与比对）", wm.Column, table)
	}
	if err != nil {
		return fmt.Errorf("校验水位列: %w", err)
	}
	if !wmAllowedColumnTypes[dataType] {
		return fmt.Errorf("水位列 %s 类型 %s 不在可比白名单（timestamptz/timestamp/date/int/bigint）", wm.Column, dataType)
	}
	return nil
}

// DetectTableKey relocates the PK / unique-index metadata probe
// (nopk.go detectTableKey :37-100, including parseIndexColumns :102-128).
func (postgresDialect) DetectTableKey(ctx context.Context, q dialectQueryer, schema, table string) (*TableKeyInfo, error) {
	info := &TableKeyInfo{}

	pkRows, err := q.QueryContext(ctx, `
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON tc.constraint_name = kcu.constraint_name
			AND tc.table_schema = kcu.table_schema
		WHERE tc.table_schema = $1
			AND tc.table_name = $2
			AND tc.constraint_type = 'PRIMARY KEY'
		ORDER BY kcu.ordinal_position
	`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("query primary key: %w", err)
	}
	defer pkRows.Close()

	for pkRows.Next() {
		var col string
		if err := pkRows.Scan(&col); err != nil {
			return nil, fmt.Errorf("scan pk column: %w", err)
		}
		info.PKColumns = append(info.PKColumns, col)
	}
	info.HasPK = len(info.PKColumns) > 0

	if info.HasPK {
		return info, nil
	}

	uidxRows, err := q.QueryContext(ctx, `
		SELECT indexdef
		FROM pg_indexes
		WHERE schemaname = $1
			AND tablename = $2
			AND indexdef LIKE '%UNIQUE%'
			AND indexdef NOT LIKE '%pkey%'
	`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("query unique indexes: %w", err)
	}
	defer uidxRows.Close()

	for uidxRows.Next() {
		var def string
		if err := uidxRows.Scan(&def); err != nil {
			return nil, fmt.Errorf("scan unique index def: %w", err)
		}
		cols := parseIndexColumns(def)
		if len(cols) > 0 {
			info.HasUniqueIndex = true
			info.UniqueColumns = cols
			break
		}
	}

	return info, nil
}

// AdjustDSN relocates appendPGDSNUTC (wmfilter.go :97-103) - P-INC-TZ
// touchpoint: segment 1 relocates this behavior BYTE-identically; the fix
// lands only in segment 2 as an independent commit.
func (postgresDialect) AdjustDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "options=" + url.QueryEscape("-c TimeZone=UTC")
}
