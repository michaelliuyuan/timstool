package validator

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// #t3 watermark compare: an OPTIONAL filter dimension overlaid on the three
// compare algorithms (quick/sample/checksum). nil filter = byte-identical
// legacy behavior; a non-nil filter appends the same predicate to every
// source/target query so both sides compare the same row universe.

// wmIdentRe is the column allow-list (same conservative ASCII rule as the
// incremental module's incIdentifierRe — identifiers outside it are rejected
// instead of quoted, so a column name can never smuggle SQL).
var wmIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// wmAllowedColumnTypes lists the information_schema.data_type values a
// watermark column may have (mirrors the incremental module's set).
var wmAllowedColumnTypes = map[string]bool{
	"timestamp with time zone":    true,
	"timestamp without time zone": true,
	"date":                        true,
	"integer":                     true,
	"bigint":                      true,
}

// wmFilter returns the active watermark filter, or nil when absent/incomplete
// (a half-configured group is treated as off, matching the API's required
// column+value pair).
func (v *Validator) wmFilter() *config.WatermarkFilter {
	wm := v.compareCfg().Watermark
	if wm == nil || wm.Column == "" || wm.Value == "" {
		return nil
	}
	return wm
}

// wmOp returns the effective comparison operator ("<" only when explicitly
// configured; anything else — including "" — is the inclusive "<=").
func wmOp(wm *config.WatermarkFilter) string {
	if wm.Op == "<" {
		return "<"
	}
	return "<="
}

// wmWherePG builds the parameterized PG predicate. The watermark value is
// the ONLY parameter of every query it joins, so the placeholder is always $1.
func wmWherePG(wm *config.WatermarkFilter) string {
	return fmt.Sprintf("%s %s $1", quotePG(wm.Column), wmOp(wm))
}

// wmWhereMySQL builds the parameterized TiDB predicate (driver "?" style).
func wmWhereMySQL(wm *config.WatermarkFilter) string {
	return fmt.Sprintf("%s %s ?", quoteMySQL(wm.Column), wmOp(wm))
}

// checkWatermarkColumn verifies per table that the watermark column exists
// and its type is comparable — a table that fails this check fails alone
// with an explicit error (per-table failure semantics, same as incremental).
func checkWatermarkColumn(ctx context.Context, pgDB *sql.DB, schema, table string, wm *config.WatermarkFilter) error {
	var dataType string
	err := pgDB.QueryRowContext(ctx, `
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

// schemaOrDefault returns the effective source schema ("public" when unset).
func schemaOrDefault(s string) string {
	if s == "" {
		return "public"
	}
	return s
}

// appendPGDSNUTC appends a pool-level `options=-c TimeZone=UTC` to a PG DSN
// so timestamptz watermark literals are interpreted as UTC on every pooled
// connection (no per-conn SET race), symmetric with the TiDB UTC session.
func appendPGDSNUTC(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "options=" + url.QueryEscape("-c TimeZone=UTC")
}
