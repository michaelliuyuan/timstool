package validator

import (
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
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

// A3: filter activation — nil, column-less, or value-less groups are OFF
// (zero regression for CLI / migration pipeline paths).
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
