package validator

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
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
// column+value pair). A group activates in either form: the single-value
// form (Value non-empty) or the full half-open interval form (both RangeStart
// and RangeEnd non-empty); a half-configured interval stays off.
func (v *Validator) wmFilter() *config.WatermarkFilter {
	wm := v.compareCfg().Watermark
	if wm == nil || wm.Column == "" {
		return nil
	}
	if wm.Value != "" || (wm.RangeStart != "" && wm.RangeEnd != "") {
		return wm
	}
	return nil
}

// wmRange reports whether the filter uses the half-open interval form
// [RangeStart, RangeEnd) rather than the single-value form. Both endpoints
// must be set (wmFilter only activates a full interval; this guard keeps the
// query path safe for hand-built configs). Priority note for hand-written
// YAML (which bypasses the API's mutual-exclusion check): when both forms
// are present the full interval wins and Value is silently ignored; a half
// interval alongside Value falls back to the single-value form.
func wmRange(wm *config.WatermarkFilter) bool {
	return wm != nil && wm.RangeStart != "" && wm.RangeEnd != ""
}

// wmArgs returns the bound parameters matching the active filter's predicate
// placeholders: the single value for the single-value form, or
// [RangeStart, RangeEnd] for the interval form (2 placeholders). Values are
// ALWAYS bound as query parameters, never concatenated. When both forms are
// set the full interval wins (see wmRange) and Value is silently ignored.
func wmArgs(wm *config.WatermarkFilter) []any {
	if wmRange(wm) {
		return []any{wm.RangeStart, wm.RangeEnd}
	}
	return []any{wm.Value}
}

// wmOp returns the effective comparison operator ("<" only when explicitly
// configured; anything else — including "" — is the inclusive "<=").
func wmOp(wm *config.WatermarkFilter) string {
	if wm.Op == "<" {
		return "<"
	}
	return "<="
}

// ErrWatermarkColumnSkip is the package-level sentinel marking a watermark
// column precheck failure that is per-table UNUSABLE (column absent, or its
// type outside the comparable whitelist) rather than an infrastructure
// error. errors.Is-detectable: the compare flow converts sentinel hits into
// a per-table StatusSkip report (runtime skip), while probe/DB failures stay
// ordinary errors ⇒ StatusFail.
var ErrWatermarkColumnSkip = errors.New("watermark column unavailable")

// wmSkipError carries the ORIGINAL precheck message verbatim (the wording is
// UI-anchored) while staying errors.Is-detectable as ErrWatermarkColumnSkip
// via the Is hook — no text is prefixed or altered.
type wmSkipError struct{ msg string }

func (e wmSkipError) Error() string        { return e.msg }
func (e wmSkipError) Is(target error) bool { return target == ErrWatermarkColumnSkip }

// wmSkipf wraps fmt.Errorf-shaped messages as a sentinel hit.
func wmSkipf(format string, args ...interface{}) error {
	return wmSkipError{fmt.Sprintf(format, args...)}
}

// wmSkipReport builds the TableReport for a failed watermark column precheck:
// a sentinel hit (column missing / type not comparable) ⇒ StatusSkip with the
// original reason disclosed in Error and the skip explained in Suggestion;
// any other error stays a hard StatusFail.
func wmSkipReport(table string, werr error) reporter.TableReport {
	if errors.Is(werr, ErrWatermarkColumnSkip) {
		return reporter.TableReport{
			TableName:  table,
			Status:     reporter.StatusSkip,
			Error:      werr.Error(),
			Suggestion: "水位列在该表不可用，已运行期跳过（可更换水位列、手输列名或调整表选择）",
		}
	}
	return reporter.TableReport{TableName: table, Status: reporter.StatusFail, Error: werr.Error()}
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
