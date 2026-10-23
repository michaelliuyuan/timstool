package validator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

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

// wmFallbackKey is the per-table ctx override marker: present ⇒ this table
// degraded to full comparison (Plan A) and every query in the mode body must
// run WITHOUT the watermark predicate, regardless of the global config. It
// rides the existing ctx so no function signature changes and no Validator
// field is mutated (table goroutines run concurrently).
type wmFallbackKey struct{}

// ctxWithWMFallback marks ctx as "this table fell back to full comparison".
func ctxWithWMFallback(ctx context.Context) context.Context {
	return context.WithValue(ctx, wmFallbackKey{}, true)
}

// wmTableKey carries the table name through ctx so the mode bodies'
// wmFilterFor lookups can resolve per-table overrides (P1A) without any
// signature change. Same struct-key discipline as wmFallbackKey; the value
// is set once per table goroutine and never mutated afterwards.
type wmTableKey struct{ table string }

// ctxWithWMTable tags ctx with the table name for override resolution.
func ctxWithWMTable(ctx context.Context, table string) context.Context {
	return context.WithValue(ctx, wmTableKey{}, table)
}

// wmActive mirrors wmFilter's activation rule for an arbitrary filter group
// (used for both the global group and override entries): a group activates
// in the single-value form or the full-interval form; anything else reads
// as off.
func wmActive(wm *config.WatermarkFilter) bool {
	return wm != nil && wm.Column != "" && (wm.Value != "" || (wm.RangeStart != "" && wm.RangeEnd != ""))
}

// wmOverrideFor resolves the per-table override entry (P1A). hit ⇒ table has
// an override entry; wm is non-nil because only ACTIVE entries resolve (an
// inactive/half-configured entry is ignored — that table falls back to the
// global group, mirroring the API's activation rule; disclosed in the work
// order). The lookup reads the Validator's construction-time config every
// call: the config is frozen after NewValidator, so this IS the task-start
// read-only snapshot — no Validator field is mutated (concurrency red line)
// and table goroutines only ever read it.
func (v *Validator) wmOverrideFor(table string) (wm *config.WatermarkFilter, hit bool) {
	for i := range v.compareCfg().TableWatermarks {
		tw := &v.compareCfg().TableWatermarks[i]
		if tw.Table == table && wmActive(&tw.WatermarkFilter) {
			w := tw.WatermarkFilter // copy: callers must never see the slice element
			return &w, true
		}
	}
	return nil, false
}

// hasActiveWMOverrides reports whether ANY per-table override is active —
// the task-level watermark-mode switch (P3A: no global group + at least one
// override ⇒ still a watermark task for DSN/session gating).
func (v *Validator) hasActiveWMOverrides() bool {
	for i := range v.compareCfg().TableWatermarks {
		if wmActive(&v.compareCfg().TableWatermarks[i].WatermarkFilter) {
			return true
		}
	}
	return false
}

// wmFilterFor is the per-table-aware filter lookup for the mode bodies.
// Resolution order (work order D2a): per-table override entry (active form
// only) → fallback marker (Plan A: this table runs full comparison) → the
// global group. Override tables never carry the fallback marker (an
// override precheck failure is a hard FAIL, P2A), so the two ctx keys never
// conflict. Entry-point gating (precheck + scope note baseline) resolves the
// effective filter explicitly instead.
func (v *Validator) wmFilterFor(ctx context.Context) *config.WatermarkFilter {
	if t, ok := ctx.Value(wmTableKey{}).(string); ok {
		if ov, hit := v.wmOverrideFor(t); hit {
			return ov
		}
	}
	if _, ok := ctx.Value(wmFallbackKey{}).(bool); ok {
		return nil
	}
	return v.wmFilter()
}

// wmPrecheckGate decides a table's fate after the watermark column precheck
// (Plan A fallback semantics):
//   - nil error       → proceed WITH the watermark filter ("" / nil)
//   - sentinel hit    → FALL BACK to full comparison for this table; the
//     original precheck message is returned verbatim as the fallback reason
//   - ordinary error  → hard FAIL report (probe/DB failures stay fails)
//
// Override tables (P2A) never route through here — see wmPrecheckFailEx.
func wmPrecheckGate(table string, werr error) (fallback string, fail *reporter.TableReport) {
	if werr == nil {
		return "", nil
	}
	if errors.Is(werr, ErrWatermarkColumnSkip) {
		return werr.Error(), nil
	}
	tr := reporter.TableReport{TableName: table, Status: reporter.StatusFail, Error: werr.Error()}
	return "", &tr
}

// wmPrecheckFailEx is the override-table gate (P2A): an override is an
// EXPLICIT per-table configuration, so a precheck failure — column absent,
// type mismatch (sentinel) or an ordinary probe/DB error — is ALWAYS a hard
// FAIL with the misconfiguration surfaced, never a silent full-comparison
// fallback.
func wmPrecheckFailEx(table string, werr error) *reporter.TableReport {
	return &reporter.TableReport{
		TableName: table,
		Status:    reporter.StatusFail,
		Error:     wmExFailPrefix + werr.Error(),
	}
}

// wmExFailPrefix marks a wmPrecheckFailEx product — single source so the
// isWmExFail recognizer below can never drift from the producer.
const wmExFailPrefix = "例外水位列："

// isWmExFail reports whether tr is a wmPrecheckFailEx product (an override
// table's precheck FAIL). Such a row failed BEFORE any comparison ran, so its
// counts are zero; the report stamp site skips the watermark scope note for
// it (a「（源 0 行/目标 0 行）」note would read as "the filter matched
// nothing" instead of "never ran") — the attribution already lives in the
// Error field. Global-group FAIL rows keep their current note (out of scope
// here).
func isWmExFail(tr reporter.TableReport) bool {
	return tr.Status == reporter.StatusFail && strings.HasPrefix(tr.Error, wmExFailPrefix)
}

// wmFallbackNote renders the per-table fallback disclosure for reports.
func wmFallbackNote(reason string) string {
	return fmt.Sprintf("水位列不可用（%s），该表已退化为全量比对", reason)
}

// wmTableNote picks the per-table watermark disclosure: when the table fell
// back to full comparison the fallback note wins (a scope note would be
// misleading — the counts are full-table); an override table gets the
// override-tagged scope note (P1A/P4A: the report must show WHICH filter
// applied); otherwise the configured scope note; "" when no watermark is
// active and no fallback happened.
func wmTableNote(wm *config.WatermarkFilter, fallback string, srcRows, tgtRows int64, override bool) string {
	if fallback != "" {
		return wmFallbackNote(fallback)
	}
	if wm == nil {
		return ""
	}
	tag := ""
	if override {
		tag = "(例外)"
	}
	if wmRange(wm) {
		return fmt.Sprintf("水位过滤%s %s [%s, %s)（源 %d 行/目标 %d 行）",
			tag, wm.Column, wm.RangeStart, wm.RangeEnd, srcRows, tgtRows)
	}
	return fmt.Sprintf("水位过滤%s %s %s %s（源 %d 行/目标 %d 行）",
		tag, wm.Column, wmOp(wm), wm.Value, srcRows, tgtRows)
}

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

// wmSkipReport is REMOVED under Plan A: a sentinel hit no longer skips the
// table — wmPrecheckGate routes it to full-comparison fallback and the
// ordinary-error FAIL moved inline at the two call sites. See
// wmPrecheckGate / wmFallbackNote.

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
