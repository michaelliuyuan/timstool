package validator

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"unicode"

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
// The half-open interval form renders `col >= $1 AND col < $2` (both bounds
// bound as parameters, never concatenated).
func (d postgresDialect) WmPredicateFragment(wm *config.WatermarkFilter) string {
	if wmRange(wm) {
		return fmt.Sprintf("%s >= $1 AND %s < $2", d.QuoteIdent(wm.Column), d.QuoteIdent(wm.Column))
	}
	return fmt.Sprintf("%s %s $1", d.QuoteIdent(wm.Column), wmOp(wm))
}

// Row-fetch shapes are NOT dialect methods (leader seq 214 dead-surface
// pruning): call sites compose the SELECT templates in the main flow via
// fmt.Sprintf + QuoteIdent/WmPredicateFragment (checksum.go :213-218,
// validator.go :508-516/:889-893/:967-970/:1029-1032/:1085-1088, nopk.go
// :170-173/:507-510/:626-631).

// ValidateWatermarkColumn relocates checkWatermarkColumn's information_schema
// probe (wmfilter.go :68-84); the type-whitelist judgment (wmAllowedColumnTypes)
// stays in the main flow. The column-absent and type-not-comparable branches
// return wmSkipf sentinel errors (same text, errors.Is ⇒ ErrWatermarkColumnSkip)
// so the compare flow can skip those tables at runtime instead of failing them.
func (postgresDialect) ValidateWatermarkColumn(ctx context.Context, q dialectQueryer, schema, table string, wm *config.WatermarkFilter) error {
	var dataType string
	err := q.QueryRowContext(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
		schema, table, wm.Column).Scan(&dataType)
	if err == sql.ErrNoRows {
		return wmSkipf("水位列 %s 不存在于表 %s（水位过滤仅支持列存在的表参与比对）", wm.Column, table)
	}
	if err != nil {
		return fmt.Errorf("校验水位列: %w", err)
	}
	if !wmAllowedColumnTypes[dataType] {
		return wmSkipf("水位列 %s 类型 %s 不在可比白名单（timestamptz/timestamp/date/int/bigint）", wm.Column, dataType)
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
		// #t95 B1 (dialect root-cause for the M1-b cost): a partial unique
		// index (WHERE predicate) or an expression index (lower(email),
		// (a-b), casts) does NOT give table-level uniqueness on PLAIN
		// columns — using it as the keyset seek key would silently drop
		// rows outside the predicate / mis-align the cursor. Reject both so
		// the table falls into the no-PK path (hash_group + guardrail); the
		// M1-b covered<count guardrail stays as correctness double-insurance.
		if !usableTableUniqueIndex(def) {
			continue
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

// usableTableUniqueIndex reports whether a pg_get_indexdef output is a
// TABLE-LEVEL unique constraint on PLAIN columns that a keyset seek can
// safely advance on (#t95 B1). Rejected:
//
//   - partial indexes (a "WHERE" predicate): uniqueness holds only within
//     the predicate's row subset — a strict-greater seek over the whole
//     table silently drops rows outside it;
//   - expression indexes (lower(email), (a-b), casts): uniqueness holds on
//     the EXPRESSION, not the raw column(s) parseIndexColumns would name —
//     the cursor would mis-align.
func usableTableUniqueIndex(def string) bool {
	if strings.Contains(strings.ToUpper(def), " WHERE ") {
		return false
	}
	list := indexColumnList(def)
	if list == "" {
		return false
	}
	// A nested paren inside the top-level column list marks an expression
	// index ((a-b), lower(email), ...) — the list must be plain identifiers.
	if strings.ContainsAny(list, "()") {
		return false
	}
	cols := parseIndexColumns(def)
	if len(cols) == 0 {
		return false
	}
	for _, c := range cols {
		if !plainIndexColumn(c) {
			return false
		}
	}
	return true
}

// indexColumnList extracts the TOP-LEVEL column list of a CREATE
// [UNIQUE] INDEX ... ON <table> ... (cols) statement — the first
// parenthesized group after the " ON " clause, matched to its closing paren
// (so nested expression parens stay inside the list). Empty when the shape
// is unexpected.
func indexColumnList(def string) string {
	idx := strings.Index(strings.ToUpper(def), " ON ")
	if idx < 0 {
		return ""
	}
	rest := def[idx+4:]
	open := strings.Index(rest, "(")
	if open < 0 {
		return ""
	}
	depth := 0
	end := -1
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
	}
	if end < 0 {
		return ""
	}
	return rest[open+1 : end]
}

// plainIndexColumn matches a bare identifier — the shape parseIndexColumns
// already reduced to (quotes/ASC/DESC/NULLS stripped). Anything else (a
// cast remnant like "a::text", an operator, etc.) is not a seekable key.
func plainIndexColumn(col string) bool {
	if col == "" {
		return false
	}
	for _, r := range col {
		if !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
	}
	return true
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

// ListTables relocates validator.go getTables' information_schema probe
// (MS-08: the $1 placeholder is pgx-owned). Behavior is byte-identical to
// the pre-relocation query.
func (postgresDialect) ListTables(ctx context.Context, q dialectQueryer, schema string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return tables, nil
}

// parseIndexColumns extracts column names from a CREATE UNIQUE INDEX
// statement (relocated verbatim from nopk.go :102-128; anchor test
// TestParseIndexColumns keeps targeting it).
func parseIndexColumns(indexDef string) []string {
	// Find the last parenthesized group: ... ON table (col1, col2, ...)
	idx := strings.LastIndex(indexDef, "(")
	if idx < 0 {
		return nil
	}
	inner := indexDef[idx+1:]
	end := strings.Index(inner, ")")
	if end < 0 {
		return nil
	}
	inner = inner[:end]

	parts := strings.Split(inner, ",")
	var cols []string
	for _, p := range parts {
		col := strings.TrimSpace(p)
		// Remove optional ASC/DESC/NULLS options
		col = strings.Split(col, " ")[0]
		col = strings.Trim(col, "\"")
		if col != "" {
			cols = append(cols, col)
		}
	}
	return cols
}
