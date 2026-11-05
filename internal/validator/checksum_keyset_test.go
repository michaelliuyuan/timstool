package validator

// checksum_keyset_test.go anchors the P0-1 keyset-cursor chunk pagination
// (BUG-COMPARE-STUCK: deep OFFSET chunking was O(n²) on TiDB):
//
//	C1  chunk SQL shape — window 0 carries no seek, later windows carry the
//	    strict-greater seek (single-key `pk > $last`, PG $N / TiDB ?), and
//	    NOTHING across the whole scan emits OFFSET.
//	C2  cursor coverage continuity — the cursor pages to the table's tail
//	    (a diff in the LAST window is still detected: no early stop).
//	C3  composite-key OR-chain seek shape + native bound values.
//	C4  watermark predicate AND'd onto the seek (WM first, seek second).
//	C5  nullable unique-index key with NULL rows: the coverage guardrail
//	    re-runs the table as hash_group with the NULL disclosure (no silent
//	    undercount — the seek is UNKNOWN on NULL).

import (
	"context"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// TestChecksumChunkKeysetSingleKey (C1): 5 rows / chunk size 2 → exactly 3
// windows; window 0 has no seek, windows 1-2 carry `WHERE ("id" > $1)` /
// `WHERE (\`id\` > ?)` with the NATIVE last-key (int64) bound; no OFFSET
// anywhere on either side.
func TestChecksumChunkKeysetSingleKey(t *testing.T) {
	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "a"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})
	src := &sideScript{count: 5, pkCols: []string{"id"}, data: same}
	dst := &sideScript{count: 5, data: same}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.Status != reporter.StatusPass {
		t.Fatalf("identical keyset chunks must pass, got status=%s err=%q", tr.Status, tr.Error)
	}

	// table passed → no drill windows; the keyset windows ARE the chunks.
	wq := src.drillWindowQueries()
	if len(wq) != 3 {
		t.Fatalf("expected 3 chunk windows for 5 rows / size 2, got %d: %v", len(wq), wq)
	}
	if want := `SELECT * FROM "public"."t1" ORDER BY "id" LIMIT 2`; wq[0] != want {
		t.Fatalf("window 0 must have no seek predicate: %q want %q", wq[0], want)
	}
	if want := `SELECT * FROM "public"."t1" WHERE ("id" > $1) ORDER BY "id" LIMIT 2`; wq[1] != want {
		t.Fatalf("window 1 = %q, want %q", wq[1], want)
	}
	if wq[2] != wq[1] {
		t.Fatalf("window 2 must reuse the seek shape, got %q", wq[2])
	}

	// the seek bounds are the windows' last native keys: 2 then 4.
	seeks := src.argsFor(" > ")
	if len(seeks) != 2 {
		t.Fatalf("expected 2 seek windows, got %d: %v", len(seeks), seeks)
	}
	for i, want := range []int64{2, 4} {
		if len(seeks[i]) != 1 {
			t.Fatalf("seek %d args = %v, want exactly 1", i, seeks[i])
		}
		if v, ok := seeks[i][0].(int64); !ok || v != want {
			t.Fatalf("seek %d must bind native int64 %d, got %#v", i, want, seeks[i][0])
		}
	}

	// TiDB twin: positional ? and backticks.
	tq := dst.drillWindowQueries()
	if len(tq) != 3 {
		t.Fatalf("expected 3 target windows, got %d: %v", len(tq), tq)
	}
	if want := "SELECT * FROM `t1` WHERE (`id` > ?) ORDER BY `id` LIMIT 2"; tq[1] != want {
		t.Fatalf("target window 1 = %q, want %q", tq[1], want)
	}

	for _, q := range append(wq, tq...) {
		if strings.Contains(q, "OFFSET") {
			t.Fatalf("keyset chunking must never emit OFFSET: %s", q)
		}
	}
}

// TestChecksumChunkKeysetCoversTail (C2): the cursor must page to the very
// last row — a content diff in the FINAL window is still detected. An early
// stop (e.g. a scan that trusted the count-derived window bound instead of
// advancing the cursor) would PASS silently.
func TestChecksumChunkKeysetCoversTail(t *testing.T) {
	src := &sideScript{count: 6, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "a"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"}, [2]any{int64(6), "a"})}
	dst := &sideScript{count: 6,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "a"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"}, [2]any{int64(6), "z"})}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail {
		t.Fatalf("tail diff must fail the table (cursor must cover the last window), got %s", tr.Status)
	}
	if !strings.Contains(tr.Error, "chunk 2 (rows 4-6)") {
		t.Fatalf("mismatch must be attributed to the last chunk, got %q", tr.Error)
	}
	// the scan advanced through all 3 windows (seeks 2 and 4), never OFFSET.
	for _, q := range append(src.drillWindowQueries(), dst.drillWindowQueries()...) {
		if strings.Contains(q, "OFFSET") {
			t.Fatalf("keyset chunking must never emit OFFSET: %s", q)
		}
	}
}

// TestChecksumChunkKeysetCompositeKey (C3): composite (id, name) seeks reuse
// the rowDiffSeek OR chain; PG reuses one placeholder per key column.
func TestChecksumChunkKeysetCompositeKey(t *testing.T) {
	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "b"}, [2]any{int64(3), "c"})
	src := &sideScript{count: 3, pkCols: []string{"id", "name"}, data: same}
	dst := &sideScript{count: 3, data: same}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.Status != reporter.StatusPass {
		t.Fatalf("composite identical chunks must pass, got %s", tr.Status)
	}

	wq := src.drillWindowQueries()
	if len(wq) != 2 {
		t.Fatalf("expected 2 windows for 3 rows / size 2, got %d: %v", len(wq), wq)
	}
	if want := `SELECT * FROM "public"."t1" WHERE ("id" > $1 OR ("id" = $1 AND "name" > $2)) ORDER BY "id", "name" LIMIT 2`; wq[1] != want {
		t.Fatalf("composite PG seek = %q, want %q", wq[1], want)
	}
	seeks := src.argsFor(" > ")
	if len(seeks) != 1 || len(seeks[0]) != 2 {
		t.Fatalf("composite PG seek must bind exactly the 2 key values once each, got %v", seeks)
	}
	if id, ok := seeks[0][0].(int64); !ok || id != 2 {
		t.Fatalf("composite seek id must be native int64 2, got %#v", seeks[0][0])
	}
	if name, ok := seeks[0][1].(string); !ok || name != "b" {
		t.Fatalf("composite seek name must be native string b, got %#v", seeks[0][1])
	}

	// TiDB positional twin repeats the value per OR-chain occurrence.
	tq := dst.drillWindowQueries()
	if want := "SELECT * FROM `t1` WHERE (`id` > ? OR (`id` = ? AND `name` > ?)) ORDER BY `id`, `name` LIMIT 2"; tq[1] != want {
		t.Fatalf("composite TiDB seek = %q, want %q", tq[1], want)
	}
	tArgs := dst.argsFor(" > ")
	if len(tArgs) != 1 || len(tArgs[0]) != 3 {
		t.Fatalf("composite positional seek binds 3 occurrences (v1, v1, v2), got %v", tArgs)
	}
}

// TestChecksumChunkKeysetWMSingle (C4): the watermark predicate rides the
// chunk windows AND'd BEFORE the seek (WM first, seek second), matching the
// row-diff drill's composition; PG numbers the seek placeholder after the
// watermark's.
func TestChecksumChunkKeysetWMSingle(t *testing.T) {
	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "a"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})
	src := &sideScript{count: 5, pkCols: []string{"id"}, data: same}
	dst := &sideScript{count: 5, data: same}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	v.cfg.Compare.Watermark = &config.WatermarkFilter{Column: "wm", Value: "100"}
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.Status != reporter.StatusPass {
		t.Fatalf("wm chunked compare must pass, got %s", tr.Status)
	}

	wq := src.drillWindowQueries()
	if want := `SELECT * FROM "public"."t1" WHERE "wm" <= $1 AND ("id" > $2) ORDER BY "id" LIMIT 2`; wq[1] != want {
		t.Fatalf("wm+seek PG window = %q, want %q", wq[1], want)
	}
	seeks := src.argsFor(" > ")
	if len(seeks) != 2 || len(seeks[0]) != 2 {
		t.Fatalf("wm+seek must bind [wm, seek] per window, got %v", seeks)
	}
	if v, ok := seeks[0][0].(string); !ok || v != "100" {
		t.Fatalf("first arg must be the watermark value, got %#v", seeks[0][0])
	}
	if v, ok := seeks[0][1].(int64); !ok || v != 2 {
		t.Fatalf("second arg must be the native seek, got %#v", seeks[0][1])
	}

	tq := dst.drillWindowQueries()
	if want := "SELECT * FROM `t1` WHERE `wm` <= ? AND (`id` > ?) ORDER BY `id` LIMIT 2"; tq[1] != want {
		t.Fatalf("wm+seek TiDB window = %q, want %q", tq[1], want)
	}
}

// TestChecksumChunkKeysetWMRange (C4 range form): a two-endpoint watermark
// AND's both predicates before the seek; PG seek numbering starts at $3.
func TestChecksumChunkKeysetWMRange(t *testing.T) {
	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "a"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})
	src := &sideScript{count: 5, pkCols: []string{"id"}, data: same}
	dst := &sideScript{count: 5, data: same}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	v.cfg.Compare.Watermark = &config.WatermarkFilter{Column: "wm", RangeStart: "50", RangeEnd: "90"}
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.Status != reporter.StatusPass {
		t.Fatalf("wm-range chunked compare must pass, got %s", tr.Status)
	}

	wq := src.drillWindowQueries()
	if want := `SELECT * FROM "public"."t1" WHERE "wm" >= $1 AND "wm" < $2 AND ("id" > $3) ORDER BY "id" LIMIT 2`; wq[1] != want {
		t.Fatalf("wm-range+seek PG window = %q, want %q", wq[1], want)
	}
	seeks := src.argsFor(" > ")
	if len(seeks) != 2 || len(seeks[0]) != 3 {
		t.Fatalf("wm-range+seek must bind [start, end, seek] per window, got %v", seeks)
	}
	if v, ok := seeks[0][0].(string); !ok || v != "50" {
		t.Fatalf("first arg must be range start, got %#v", seeks[0][0])
	}
	if v, ok := seeks[0][2].(int64); !ok || v != 2 {
		t.Fatalf("third arg must be the native seek, got %#v", seeks[0][2])
	}
}

// TestChecksumChunkKeysetNullKeyFallback (C5): a nullable unique-index key
// with NULL rows would silently drop every post-NULL window (seek is
// UNKNOWN on NULL) — the coverage guardrail (covered < count) detects it,
// the NULL pre-scan attributes it, and the table re-runs as hash_group with
// the NULL disclosure. No silent PASS, no fabricated alignment.
func TestChecksumChunkKeysetNullKeyFallback(t *testing.T) {
	uk := "CREATE UNIQUE INDEX uk_name ON t1 (name)"
	src := &sideScript{count: 3, uniqueIdxDef: uk,
		data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"}, [2]any{int64(3), nil})}
	dst := &sideScript{count: 3, uniqueIdxDef: uk,
		data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"}, [2]any{int64(3), "cc"})}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail {
		t.Fatalf("NULL-key table must not silently pass, got %s err=%q", tr.Status, tr.Error)
	}
	rd := tr.RowDiffs
	if rd == nil || !strings.Contains(rd.Note, "NULL") {
		t.Fatalf("hash_group re-run must carry the NULL disclosure, got %+v", rd)
	}
}
