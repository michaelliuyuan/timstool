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
	"database/sql/driver"
	"strings"
	"testing"
	"time"

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

// TestChecksumChunkCoverageDiscrepancyNoNullReRuns (M1, NOT-NULL
// predicate-column variant): a coverage shortfall WITHOUT a NULL hit is NOT
// benign "data movement". A PG PARTIAL unique index whose WHERE predicate is
// mis-parsed by parseIndexColumns as the key column (last paren group →
// predicate column, here NOT-NULL `status`) makes the strict-greater seek
// silently drop duplicate boundary values on BOTH sides while the window
// hashes still align — covered < count with a diff inside the dropped rows
// would PASS silently. The guardrail must re-run the table as hash_group
// (full multiset comparison) and surface the diff.
func TestChecksumChunkCoverageDiscrepancyNoNullReRuns(t *testing.T) {
	// PARENTHESIZED predicate: parseIndexColumns grabs the LAST paren group
	// "(status = 1)" → UniqueColumns=[status] — a NOT-NULL column whose
	// values legitimately repeat outside the partial predicate. This is the
	// TRUE silent form (no NULLs → pre-fix guardrail found nothing).
	uk := "CREATE UNIQUE INDEX uk_email ON t1 USING btree (email) WHERE (status = 1)"
	// Duplicate status value across the chunkSize=2 boundary: window 0
	// fetches [1,1],[2,1] (lastKey 1), window 1 seeks status > 1 and DROPS
	// row 3 (status 1) on BOTH sides — windows still match, covered 4 <
	// count 5. The dropped row's value differs between sides; only a full
	// hash_group re-run can see it.
	src := &sideScript{count: 5, uniqueIdxDef: uk, dataCols: []string{"id", "email", "status", "val"}, dataColTypes: []string{"int8", "text", "int8", "text"},
		data: [][]driver.Value{
			{int64(1), "a@x", int64(1), "x"}, {int64(2), "b@x", int64(1), "x"}, {int64(3), "c@x", int64(1), "x"}, {int64(4), "d@x", int64(2), "x"}, {int64(5), "e@x", int64(3), "x"},
		}}
	dst := &sideScript{count: 5, uniqueIdxDef: uk, dataCols: []string{"id", "email", "status", "val"}, dataColTypes: []string{"int8", "text", "int8", "text"},
		data: [][]driver.Value{
			{int64(1), "a@x", int64(1), "x"}, {int64(2), "b@x", int64(1), "x"}, {int64(3), "c@x", int64(1), "DIFF"}, {int64(4), "d@x", int64(2), "x"}, {int64(5), "e@x", int64(3), "x"},
		}}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail {
		t.Fatalf("coverage-discrepancy table with a diff in dropped rows must FAIL, got %s err=%q", tr.Status, tr.Error)
	}
	if !strings.Contains(tr.Error, "hash group") {
		t.Fatalf("the re-run verdict must be the hash_group full comparison, got %q", tr.Error)
	}
	// No NULL involved: the re-run must NOT carry the NULL disclosure.
	if rd := tr.RowDiffs; rd != nil && rd.Note != "" {
		t.Fatalf("non-NULL coverage shortfall must not get the NULL disclosure, got %+v", rd)
	}
	// hash_group re-run evidence: a full-table SELECT * (no LIMIT) must have
	// been issued on both sides — the window shapes all carry LIMIT.
	if !src.sawFullScan() {
		t.Fatal("M1-b fallback must re-run the source table as a full-table scan (hash_group evidence)")
	}
	if !dst.sawFullScan() {
		t.Fatal("M1-b fallback must re-run the target table as a full-table scan (hash_group evidence)")
	}
}

// TestChecksumChunkCoverageDiscrepancyExprIndex (M1, expression-index
// auxiliary variant): `UNIQUE (lower(email))` mis-parses to [email] — the
// detected key is the raw column while the real uniqueness is on the
// expression. Raw email values are still bytewise-distinct (case variants
// sort apart), so the seek pages them all and the honest window comparison
// must still catch a real diff — the detection confusion must never yield a
// silent PASS.
func TestChecksumChunkCoverageDiscrepancyExprIndex(t *testing.T) {
	uk := "CREATE UNIQUE INDEX uk_email ON t1 USING btree (lower(email))"
	src := &sideScript{count: 5, uniqueIdxDef: uk, dataCols: []string{"id", "email", "val"}, dataColTypes: []string{"int8", "text", "text"},
		data: [][]driver.Value{
			{int64(1), "aa", "x"}, {int64(2), "AA", "x"}, {int64(3), "ab", "x"}, {int64(4), "bb", "x"}, {int64(5), "cc", "x"},
		}}
	dst := &sideScript{count: 5, uniqueIdxDef: uk, dataCols: []string{"id", "email", "val"}, dataColTypes: []string{"int8", "text", "text"},
		data: [][]driver.Value{
			{int64(1), "aa", "x"}, {int64(2), "AA", "x"}, {int64(3), "ab", "DIFF"}, {int64(4), "bb", "x"}, {int64(5), "cc", "x"},
		}}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail {
		t.Fatalf("expression-index table with a real diff must FAIL (never silent pass), got %s err=%q", tr.Status, tr.Error)
	}
}

// TestChecksumChunkPartialIndexNullPredicate (M1, NULL-predicate variant):
// `WHERE (deleted_at IS NULL)` mis-parses to [deleted_at] — a NULLABLE
// predicate column. This variant is caught by the pre-existing NULL path
// (NULL pre-scan hits → hash_group + NULL disclosure); it locks that path so
// the two M1 forms stay distinguishable.
func TestChecksumChunkPartialIndexNullPredicate(t *testing.T) {
	uk := "CREATE UNIQUE INDEX uk_email ON t1 USING btree (email) WHERE (deleted_at IS NULL)"
	// deleted_at holds NULL rows → the seek drops them (covered < count);
	// the NULL pre-scan attributes the shortfall.
	src := &sideScript{count: 3, uniqueIdxDef: uk, dataCols: []string{"id", "email", "deleted_at"}, dataColTypes: []string{"int8", "text", "text"},
		data: [][]driver.Value{
			{int64(1), "a@x", nil}, {int64(2), "b@x", "2024-01-01"}, {int64(3), "c@x", nil},
		}}
	dst := &sideScript{count: 3, uniqueIdxDef: uk, dataCols: []string{"id", "email", "deleted_at"}, dataColTypes: []string{"int8", "text", "text"},
		data: [][]driver.Value{
			{int64(1), "a@x", nil}, {int64(2), "b@x", "2024-01-01"}, {int64(3), "c@x", nil},
		}}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	// Identical table with NULL key rows must PASS via the honest hash_group
	// re-run (minor4 semantics) — never fail on the NULL alone.
	if tr.Status != reporter.StatusPass {
		t.Fatalf("identical NULL-keyed partial-index table must pass via hash_group re-run, got %s err=%q", tr.Status, tr.Error)
	}
	if !src.sawFullScan() {
		t.Fatal("NULL-predicate variant must re-run as a full-table scan (hash_group evidence)")
	}
}

// TestChecksumChunkCoveredEqualsCountNullPass (minor4): a small table fully
// covered in window 0 with a NULL key row — covered == count, so the
// in-flight NULL check fires but the guardrail must still re-run as
// hash_group and PASS (identical) instead of failing on the NULL alone.
func TestChecksumChunkCoveredEqualsCountNullPass(t *testing.T) {
	uk := "CREATE UNIQUE INDEX uk_name ON t1 (name)"
	same := rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), nil})
	src := &sideScript{count: 2, uniqueIdxDef: uk, data: same}
	dst := &sideScript{count: 2, uniqueIdxDef: uk, data: same}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusPass {
		t.Fatalf("identical small NULL-keyed table must pass via hash_group re-run, got %s err=%q", tr.Status, tr.Error)
	}
	if !src.sawFullScan() {
		t.Fatal("covered==count + NULL must still re-run as a full-table scan (hash_group evidence)")
	}
}

// TestChecksumGuardrailNullScanBounded (M2): the guardrail's NULL pre-scan
// (and the hash_group re-run connection) runs under the query-timeout ctx —
// a sick DB whose chunk scan ended early must fail the table, never hang
// the run on the guardrail's own probe.
func TestChecksumGuardrailNullScanBounded(t *testing.T) {
	uk := "CREATE UNIQUE INDEX uk_name ON t1 (name)"
	// count 3 / chunkSize 2: window 0 fetches 2 rows, window 1 seeks and
	// returns 0 (the NULL row is excluded) → covered 2 < count 3 → guardrail
	// fires and runs the NULL pre-scan.
	src := &sideScript{count: 3, uniqueIdxDef: uk,
		data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"}, [2]any{int64(3), nil})}
	dst := &sideScript{count: 3, uniqueIdxDef: uk,
		data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"}, [2]any{int64(3), "cc"})}

	v := NewValidator(config.Config{Compare: config.CompareConfig{
		ChecksumChunkSize: 2,
		QueryTimeout:      "100ms",
	}})

	start := time.Now()
	// The src NULL pre-scan query blocks until the guardrail's own ctx
	// deadline fires — the run must return bounded, never hang.
	tr := v.validateChecksumChunked(context.Background(), src.openHangNullScan(), dst.open(), "t1", make(concSem, 4))
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("guardrail NULL pre-scan hung (%.1fs): the query-timeout ctx did not bound it", elapsed.Seconds())
	}
	if tr.Status != reporter.StatusFail {
		t.Fatalf("a coverage-discrepancy table must fail, got %s err=%q", tr.Status, tr.Error)
	}
	if got := len(src.sawQueries("IS NULL")); got == 0 {
		t.Fatal("guardrail must have attempted the NULL pre-scan on src")
	}
}
