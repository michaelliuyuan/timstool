package validator

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// #t3 watermark-compare anchors: predicate SQL shapes (parameterized,
// quoted identifiers, both operators, both dialects), filter activation
// (nil/half-configured = off), the identifier allow-list, the PG UTC DSN
// injection, and the column-type allow-list. Live end-to-end behavior
// (timestamptz UTC symmetry, </<= boundary rows, NULL rows) is covered by
// isolation testing against a live PG+TiDB pair.

func wmTestFilter(op string) *config.WatermarkFilter {
	return &config.WatermarkFilter{Column: "updated_at", Value: "2026-10-01 00:00:00", Op: op, BaseMode: "checksum"}
}

// wmTestRangeFilter builds a half-open interval [RangeStart, RangeEnd) filter
// (the range form is mutually exclusive with Value).
func wmTestRangeFilter() *config.WatermarkFilter {
	return &config.WatermarkFilter{Column: "updated_at", RangeStart: "2026-09-01 00:00:00", RangeEnd: "2026-10-01 00:00:00", BaseMode: "checksum"}
}

// A1: PG predicate shape — quoted column, parameterized $1, both operators.
func TestWmWherePG(t *testing.T) {
	if got := (postgresDialect{}).WmPredicateFragment(wmTestFilter("")); got != `"updated_at" <= $1` {
		t.Fatalf("default op predicate wrong: %s", got)
	}
	if got := (postgresDialect{}).WmPredicateFragment(wmTestFilter("<")); got != `"updated_at" < $1` {
		t.Fatalf("strict op predicate wrong: %s", got)
	}
	if strings.Contains((postgresDialect{}).WmPredicateFragment(wmTestFilter("")), "2026-") {
		t.Fatalf("value must NEVER be concatenated into the predicate")
	}
}

// A2: MySQL predicate shape — backtick column, driver "?" placeholder.
func TestWmWhereMySQL(t *testing.T) {
	if got := (tidbDialect{}).WmPredicateFragment(wmTestFilter("<")); got != "`updated_at` < ?" {
		t.Fatalf("mysql predicate wrong: %s", got)
	}
	if strings.Contains((tidbDialect{}).WmPredicateFragment(wmTestFilter("")), "2026-") {
		t.Fatalf("value must NEVER be concatenated into the predicate")
	}
}

// A2b: range predicate shapes — every dialect renders the half-open interval
// [start, end) with BOTH bounds parameterized (PG $1/$2, MySQL/TiDB ?/?),
// and the single-value fragment is byte-identical to the pre-range shape.
func TestWmWhereRange(t *testing.T) {
	pg := (postgresDialect{}).WmPredicateFragment(wmTestRangeFilter())
	if pg != `"updated_at" >= $1 AND "updated_at" < $2` {
		t.Fatalf("pg range predicate wrong: %s", pg)
	}
	if strings.Contains(pg, "2026-") || strings.Contains(pg, "09-") {
		t.Fatalf("range bounds must NEVER be concatenated: %s", pg)
	}
	for _, got := range []string{
		(mysqlDialect{}).WmPredicateFragment(wmTestRangeFilter()),
		(tidbDialect{}).WmPredicateFragment(wmTestRangeFilter()),
	} {
		if got != "`updated_at` >= ? AND `updated_at` < ?" {
			t.Fatalf("mysql/tidb range predicate wrong: %s", got)
		}
	}
	// Single-value fragment unchanged (zero regression).
	if got := (postgresDialect{}).WmPredicateFragment(wmTestFilter("")); got != `"updated_at" <= $1` {
		t.Fatalf("pg single-value predicate regressed: %s", got)
	}
	if got := (tidbDialect{}).WmPredicateFragment(wmTestFilter("")); got != "`updated_at` <= ?" {
		t.Fatalf("mysql single-value predicate regressed: %s", got)
	}
}

// A2c: wmArgs — the bound parameters match the predicate's placeholders
// (single-value form = [Value]; interval form = [RangeStart, RangeEnd]).
func TestWmArgs(t *testing.T) {
	if got := wmArgs(wmTestFilter("")); len(got) != 1 || got[0] != "2026-10-01 00:00:00" {
		t.Fatalf("single-value args wrong: %#v", got)
	}
	if got := wmArgs(wmTestRangeFilter()); len(got) != 2 || got[0] != "2026-09-01 00:00:00" || got[1] != "2026-10-01 00:00:00" {
		t.Fatalf("range args wrong: %#v", got)
	}
	if got := wmArgs(&config.WatermarkFilter{Column: "c"}); len(got) != 1 || got[0] != "" {
		t.Fatalf("incomplete group args wrong: %#v", got)
	}
}

// A3: filter activation — nil, column-less, or value-less groups are OFF
// (zero regression for CLI / migration pipeline paths). A full interval
// [RangeStart, RangeEnd) activates; a half-configured interval stays off.
func TestWmFilterActivation(t *testing.T) {
	v := NewValidator(config.Config{})
	if v.wmFilter() != nil {
		t.Fatalf("zero config must yield nil filter")
	}
	v2 := NewValidator(config.Config{Compare: config.CompareConfig{Watermark: &config.WatermarkFilter{Column: "updated_at"}}})
	if v2.wmFilter() != nil {
		t.Fatalf("value-less group must be treated as off")
	}
	v3 := NewValidator(config.Config{Compare: config.CompareConfig{Watermark: wmTestFilter("")}})
	if v3.wmFilter() == nil {
		t.Fatalf("complete group must activate")
	}
	v4 := NewValidator(config.Config{Compare: config.CompareConfig{Watermark: wmTestRangeFilter()}})
	if v4.wmFilter() == nil {
		t.Fatalf("full interval group must activate")
	}
	v5 := NewValidator(config.Config{Compare: config.CompareConfig{Watermark: &config.WatermarkFilter{Column: "updated_at", RangeStart: "2026-09-01 00:00:00"}}})
	if v5.wmFilter() != nil {
		t.Fatalf("half-configured interval (missing range_end) must be treated as off")
	}
	v6 := NewValidator(config.Config{Compare: config.CompareConfig{Watermark: &config.WatermarkFilter{Column: "updated_at", RangeEnd: "2026-10-01 00:00:00"}}})
	if v6.wmFilter() != nil {
		t.Fatalf("half-configured interval (missing range_start) must be treated as off")
	}
}

// A4: identifier allow-list — injection shapes rejected before any SQL.
func TestWmIdentAllowList(t *testing.T) {
	for _, bad := range []string{`c"; DROP TABLE x`, "c--", "c extra", "`c`", "'c'", "1c", ""} {
		if wmIdentRe.MatchString(bad) {
			t.Errorf("identifier %q must be rejected", bad)
		}
	}
	for _, ok := range []string{"c", "updated_at", "_t", "A1b2"} {
		if !wmIdentRe.MatchString(ok) {
			t.Errorf("identifier %q must be accepted", ok)
		}
	}
}

// A5: PG DSN gains exactly one pool-level UTC option; existing query
// strings get "&" instead of a second "?".
func TestAppendPGDSNUTC(t *testing.T) {
	got := (postgresDialect{}).AdjustDSN("postgresql://u:p@h:5432/db?sslmode=disable")
	if !strings.Contains(got, "&options=") {
		t.Fatalf("existing query must append with &: %s", got)
	}
	got2 := (postgresDialect{}).AdjustDSN("postgresql://u:p@h:5432/db")
	if !strings.Contains(got2, "?options=") {
		t.Fatalf("bare DSN must open a query: %s", got2)
	}
	if !strings.Contains(got2, "TimeZone%3DUTC") {
		t.Fatalf("UTC option must be escaped into the DSN: %s", got2)
	}
}

// A6: comparable-type allow-list mirrors the incremental module's set —
// text/numeric-decimal/unknown types are NOT watermark-comparable.
func TestWmAllowedColumnTypes(t *testing.T) {
	for _, ok := range []string{"timestamp with time zone", "timestamp without time zone", "date", "integer", "bigint"} {
		if !wmAllowedColumnTypes[ok] {
			t.Errorf("type %q must be allowed", ok)
		}
	}
	for _, bad := range []string{"text", "numeric", "varchar", "boolean", "real", ""} {
		if wmAllowedColumnTypes[bad] {
			t.Errorf("type %q must be rejected", bad)
		}
	}
}

// A7: sentinel wrap — the column-absent / type-not-comparable precheck
// failures are errors.Is-detectable as ErrWatermarkColumnSkip while the
// ORIGINAL message text stays verbatim (UI-anchored wording).
func TestWmSkipSentinel(t *testing.T) {
	err := wmSkipf("水位列 %s 不存在于表 %s（水位过滤仅支持列存在的表参与比对）", "updated_at", "orders")
	if !errors.Is(err, ErrWatermarkColumnSkip) {
		t.Fatalf("wmSkipf must satisfy errors.Is(ErrWatermarkColumnSkip): %v", err)
	}
	want := "水位列 updated_at 不存在于表 orders（水位过滤仅支持列存在的表参与比对）"
	if err.Error() != want {
		t.Fatalf("text altered: %q, want %q", err.Error(), want)
	}
	// An ordinary error must NOT satisfy the sentinel.
	if errors.Is(fmt.Errorf("校验水位列: boom"), ErrWatermarkColumnSkip) {
		t.Fatal("ordinary error must not satisfy the sentinel")
	}
}

// A8: Plan A gate — sentinel ⇒ fallback (reason verbatim, no report);
// ordinary error ⇒ hard FAIL; nil error ⇒ proceed with watermark.
func TestWmPrecheckGateMapping(t *testing.T) {
	sentinel := wmSkipf("水位列 %s 不存在于表 %s（水位过滤仅支持列存在的表参与比对）", "updated_at", "orders")
	fb, failRep := wmPrecheckGate("orders", sentinel)
	if failRep != nil {
		t.Fatalf("sentinel must not produce a fail report: %+v", *failRep)
	}
	if fb != sentinel.Error() {
		t.Fatalf("fallback reason must be the verbatim precheck message: %q", fb)
	}

	_, failRep = wmPrecheckGate("orders", fmt.Errorf("校验水位列: connection refused"))
	if failRep == nil {
		t.Fatal("ordinary error must stay a hard fail")
	}
	if failRep.Status != reporter.StatusFail {
		t.Fatalf("ordinary error status = %q, want fail", failRep.Status)
	}
	if failRep.Error != "校验水位列: connection refused" {
		t.Fatalf("fail error text altered: %q", failRep.Error)
	}

	fb, failRep = wmPrecheckGate("orders", nil)
	if fb != "" || failRep != nil {
		t.Fatalf("nil error must pass through clean: %q %+v", fb, failRep)
	}
}

// A8b: fallback note wording — the original precheck message rides verbatim
// inside the disclosure (UI-anchored wording preserved).
func TestWmFallbackNote(t *testing.T) {
	reason := "水位列 updated_at 类型 text 不在可比白名单（timestamptz/timestamp/date/int/bigint）"
	note := wmFallbackNote(reason)
	if !strings.Contains(note, reason) {
		t.Fatalf("note must embed the original reason verbatim: %q", note)
	}
	if !strings.Contains(note, "退化为全量比对") {
		t.Fatalf("note must disclose the full-comparison fallback: %q", note)
	}
}
