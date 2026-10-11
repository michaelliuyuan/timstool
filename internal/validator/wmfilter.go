package validator

import (
	"regexp"

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

// MS-03: the predicate builders (wmWherePG/wmWhereMySQL), the column-check
// probe (checkWatermarkColumn) and appendPGDSNUTC were relocated verbatim
// into the dialect implementations (dialect_postgres.go / dialect_tidb.go);
// the guards above (allow-lists, activation, op semantics) stay here in the
// main flow by ruling (docs/MS03-DIALECT-MAP.md).

// schemaOrDefault returns the effective source schema ("public" when unset).
func schemaOrDefault(s string) string {
	if s == "" {
		return "public"
	}
	return s
}
