package data

import (
	"strings"
	"testing"
)

// F-05 anchor: offset-mode chunk queries must read exactly chunkSize rows so
// the cursor stride never skips rows; the final chunk reads to the end.
func TestOffsetChunkQueryLimitMatchesStride(t *testing.T) {
	const chunkSize = int64(500000)

	mid := offsetChunkQuery("*", "public", "events", chunkSize, 2*chunkSize, false)
	if !strings.Contains(mid, "LIMIT 500000 OFFSET 1000000") {
		t.Errorf("mid chunk must LIMIT by the chunk stride, got: %s", mid)
	}

	last := offsetChunkQuery("*", "public", "events", chunkSize, 4*chunkSize, true)
	if !strings.Contains(last, "LIMIT ALL") {
		t.Errorf("last chunk must read to the end (LIMIT ALL), got: %s", last)
	}
	if !strings.Contains(last, "OFFSET 2000000") {
		t.Errorf("last chunk keeps its offset, got: %s", last)
	}
}

// F-05 anchor: JSON columns must pass through the streaming path untouched;
// only real PG array literals are rewritten to JSON arrays.
func TestConvertSQLValueLeavesJSONUntouched(t *testing.T) {
	cases := []string{`{"a":1}`, `{"k":"v;drop"}`, `{"nested":{"x":[1,2]}}`}
	for _, c := range cases {
		if got := convertSQLValue(c); got != c {
			t.Errorf("convertSQLValue(%s) rewrote JSON to %v", c, got)
		}
		if got := convertSQLValue([]byte(c)); got != c {
			t.Errorf("convertSQLValue([]byte %s) rewrote JSON to %v", c, got)
		}
		if got := convertStringValue(c); got != c {
			t.Errorf("convertStringValue(%s) rewrote JSON to %s", c, got)
		}
	}

	if got := convertSQLValue("{1,2,3}"); got != "[1,2,3]" {
		t.Errorf("PG int array should become JSON array, got %v", got)
	}
	if got := convertSQLValue(`{"x","y"}`); got != `["x","y"]` {
		t.Errorf("PG text array should become JSON array, got %v", got)
	}
}

// F-05 anchor: isPGArrayLiteral is the shared guard — true for PG arrays,
// false for JSON objects.
func TestIsPGArrayLiteral(t *testing.T) {
	if !isPGArrayLiteral("{1,2}") {
		t.Error("PG array literal expected true")
	}
	if isPGArrayLiteral(`{"a":1}`) {
		t.Error("JSON object must not be treated as PG array")
	}
	if isPGArrayLiteral("[1,2]") {
		t.Error("JSON array is not a PG array")
	}
	if isPGArrayLiteral("") {
		t.Error("empty string is not a PG array")
	}
}
