package target

import (
	"testing"
	"time"
)

// TestNormalizeForCompareTimestampUTC is the c_ts gate (#t82 gap A): a TIMESTAMP
// (instant semantics) read in two driver locations must normalize to the same
// string — .UTC() collapses the zone difference. A different instant must NOT
// collapse (guard against over-normalization).
func TestNormalizeForCompareTimestampUTC(t *testing.T) {
	eastern := time.Date(2026, 7, 5, 12, 0, 0, 0, time.FixedZone("CDT", -5*3600)) // == 17:00:00Z
	utc := time.Date(2026, 7, 5, 17, 0, 0, 0, time.UTC)

	gotE := normalizeForCompare(eastern, "TIMESTAMP")
	gotU := normalizeForCompare(utc, "TIMESTAMP")
	want := "2026-07-05 17:00:00"
	if gotE != want {
		t.Fatalf("c_ts: eastern not UTC-normalized: got %q want %q", gotE, want)
	}
	if gotU != want {
		t.Fatalf("c_ts: utc drift: got %q want %q", gotU, want)
	}
	if gotE != gotU {
		t.Fatalf("c_ts: same instant in different zones must compare equal: %q != %q", gotE, gotU)
	}
	// A genuinely different instant must not be equalized.
	other := normalizeForCompare(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC), "TIMESTAMP")
	if other == gotU {
		t.Fatalf("c_ts: different instant falsely collapsed to %q", gotU)
	}
}

// TestNormalizeForCompareDatetimeLiteral is the c_dt gate (#t82 gap A): DATETIME
// is wall-clock / TZ-naive, so the same literal read in two driver locations must
// compare EQUAL as-is. A global .UTC() would shift the eastern scan to
// "2026-07-05 17:00:00" and fabricate a mismatch — this test fails under that
// (rejected) implementation.
func TestNormalizeForCompareDatetimeLiteral(t *testing.T) {
	eastern := time.Date(2026, 7, 5, 12, 0, 0, 0, time.FixedZone("CDT", -5*3600))
	utc := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	gotE := normalizeForCompare(eastern, "DATETIME")
	gotU := normalizeForCompare(utc, "DATETIME")
	want := "2026-07-05 12:00:00" // literal preserved — no zone shift
	if gotE != want {
		t.Fatalf("c_dt: eastern was shifted (must stay literal): got %q want %q", gotE, want)
	}
	if gotU != want {
		t.Fatalf("c_dt: utc drift: got %q want %q", gotU, want)
	}
	if gotE != gotU {
		t.Fatalf("c_dt: same literal in different zones must compare equal: %q != %q", gotE, gotU)
	}
}

// TestNormalizeForCompareDateLiteral: DATE/TIME/YEAR share the literal (non-shift)
// path with DATETIME.
func TestNormalizeForCompareDateLiteral(t *testing.T) {
	v := time.Date(2026, 7, 5, 0, 0, 0, 0, time.FixedZone("CDT", -5*3600))
	for _, dbType := range []string{"DATE", "TIME", "YEAR", "DATETIME"} {
		if got := normalizeForCompare(v, dbType); got != "2026-07-05 00:00:00" {
			t.Fatalf("%s must be literal (no shift): got %q", dbType, got)
		}
	}
	// Unknown / empty dbType with a time value → literal (safe default: do not
	// shift, since shifting a non-TIMESTAMP would fabricate a mismatch).
	if got := normalizeForCompare(v, ""); got != "2026-07-05 00:00:00" {
		t.Fatalf("unknown dbType must default to literal: got %q", got)
	}
}

// TestNormalizeForCompareTypes covers the non-time branches (dbType unused for
// these) so behavior stays stable.
func TestNormalizeForCompareTypes(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"nil", nil, "\\N"},
		{"bool_true", true, "1"},
		{"bool_false", false, "0"},
		{"bytes", []byte("abc"), "abc"},
		{"string", "xyz", "xyz"},
		{"float64", 3.5, "3.5"},
		{"int64", int64(42), "42"},
		{"int", 7, "7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeForCompare(c.in, ""); got != c.want {
				t.Fatalf("normalizeForCompare(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalizeStringVal locks the light cross-DB normalization: trailing
// whitespace trim (CHAR padding) + decimal trailing-zero strip (float precision).
func TestNormalizeStringVal(t *testing.T) {
	cases := []struct{ in, want string }{
		{"10.50", "10.5"},
		{"10.00", "10"},
		{"abc   ", "abc"},     // CHAR trailing padding trimmed
		{"10.500  ", "10.5"},  // trailing trim + decimal strip together
		{"  10.5", "  10.5"},  // leading whitespace NOT trimmed (by design)
		{"text", "text"},
	}
	for _, c := range cases {
		if got := normalizeStringVal(c.in); got != c.want {
			t.Fatalf("normalizeStringVal(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCompareNormalizedNegative is the #t82 gate-① acceptance (validation
// acceptance §4.3 负向): an injected value difference MUST be detected, without a
// live DB (compareNormalized is pure). This is the unit-level negative case
// standing in until the integration harness (gap B) lands.
func TestCompareNormalizedNegative(t *testing.T) {
	src := [][]string{{"1", "a"}, {"2", "b"}}

	// Identical → no mismatches.
	if _, m := compareNormalized(src, src); m != 0 {
		t.Fatalf("identical sets: want 0 mismatches, got %d", m)
	}

	// Inject one value diff ("b"→"X") → must be detected exactly once.
	tgt := [][]string{{"1", "a"}, {"2", "X"}}
	checked, m := compareNormalized(src, tgt)
	if m != 1 {
		t.Fatalf("injected diff: want 1 mismatch, got %d", m)
	}
	if checked != 2 {
		t.Fatalf("checked: want 2, got %d", checked)
	}

	// A row present only on one side is a count parity issue (detected by the
	// row-count check), not a value mismatch — checked reflects the comparable
	// prefix length so a missing row can't mask value diffs.
	checked, _ = compareNormalized(src, append(src, []string{"3", "c"}))
	if checked != 2 {
		t.Fatalf("prefix checked: want 2, got %d", checked)
	}
}
