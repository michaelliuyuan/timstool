package validator

// rowdiff_content_test.go anchors the B-line content probe (FEAT-ROWDIFF-V2
// ②): the {stem}.content.json artifact derived from the committed CSV name,
// per-category row presence (content_diff both sides, *_only single side),
// value/row truncation, per-entry probe errors (query failure AND row
// vanished mid-compare) without touching counts/CSV/verdict, the
// first-N-in-key-order budget stop, point-query shapes (key equality +
// watermark predicate, $N on PG / ? on TiDB), and the zero-diff red line.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// contentFixture drives the classic three-diff table (content 3, src_only 4,
// dst_only 6) and returns the report plus scripts.
func contentFixture(t *testing.T, dir string) (reporter.TableReport, *sideScript, *sideScript) {
	t.Helper()
	v := newDrillValidator(dir)
	src := &sideScript{count: 5, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 5,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(5), "a"}, [2]any{int64(6), "a"})}
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.RowDiffs == nil || tr.RowDiffs.File == "" {
		t.Fatalf("drill must commit a CSV: %+v", tr.RowDiffs)
	}
	return tr, src, dst
}

// loadContent unmarshals the task's content artifact.
func loadContent(t *testing.T, dir, csvFile string) []rowDiffContentEntry {
	t.Helper()
	path := filepath.Join(dir, strings.TrimSuffix(csvFile, ".csv")+".content.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("content artifact missing (%s): %v", path, err)
	}
	var entries []rowDiffContentEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatalf("content artifact not a JSON array: %v", err)
	}
	return entries
}

// Test K6a: rendering — name derived from the CSV file, entries in key
// order, both sides for content_diff, single side for *_only, values via
// the shared display normalization.
func TestRowDiffContentRender(t *testing.T) {
	dir := t.TempDir()
	tr, src, dst := contentFixture(t, dir)

	rd := tr.RowDiffs
	if rd.ContentDiff != 1 || rd.SrcOnly != 1 || rd.DstOnly != 1 {
		t.Fatalf("counts = %+v", rd)
	}
	entries := loadContent(t, dir, rd.File)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}

	e0 := entries[0]
	if e0.Key != "3" || e0.Category != rowDiffCatContent || e0.Error != "" {
		t.Fatalf("entry0 = %+v", e0)
	}
	if e0.SrcRow == nil || e0.DstRow == nil {
		t.Fatalf("content_diff must carry BOTH rows: %+v", e0)
	}
	if got := strings.Join(e0.SrcRow.Columns, ","); got != "id,name" {
		t.Fatalf("src columns = %q", got)
	}
	if e0.SrcRow.Values[0] != "3" || e0.SrcRow.Values[1] != "c" || e0.DstRow.Values[1] != "d" {
		t.Fatalf("src/dst values = %v / %v", e0.SrcRow.Values, e0.DstRow.Values)
	}
	if e0.SrcRow.Truncated || e0.DstRow.Truncated {
		t.Fatal("small rows must not truncate")
	}

	e1 := entries[1]
	if e1.Key != "4" || e1.Category != rowDiffCatSrcOnly || e1.SrcRow == nil || e1.DstRow != nil {
		t.Fatalf("src_only entry = %+v (dst_row must be absent)", e1)
	}
	if e1.SrcRow.Values[1] != "a" {
		t.Fatalf("src_only values = %v", e1.SrcRow.Values)
	}

	e2 := entries[2]
	if e2.Key != "6" || e2.Category != rowDiffCatDstOnly || e2.DstRow == nil || e2.SrcRow != nil {
		t.Fatalf("dst_only entry = %+v (src_row must be absent)", e2)
	}

	// point-query shapes: PG equality $1, TiDB equality ?, both bare LIMIT 1
	if pq := src.pointQueries(); len(pq) != 2 {
		t.Fatalf("src point queries = %d: %v", len(pq), pq)
	} else if pq[0] != `SELECT * FROM "public"."t1" WHERE "id" = $1 LIMIT 1` {
		t.Fatalf("src point query = %q", pq[0])
	}
	if pq := dst.pointQueries(); len(pq) != 2 {
		t.Fatalf("dst point queries = %d: %v", len(pq), pq)
	} else if pq[0] != "SELECT * FROM `t1` WHERE `id` = ? LIMIT 1" {
		t.Fatalf("dst point query = %q", pq[0])
	}
}

// Test K6b: the watermark predicate rides the point queries (same
// universe), PG placeholders continuing after the watermark's.
func TestRowDiffContentWatermarkUniverse(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)
	v.cfg.Compare.Watermark = &config.WatermarkFilter{Column: "wm", Value: "100"}

	src := &sideScript{count: 5, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 5,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(5), "a"}, [2]any{int64(6), "a"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.RowDiffs == nil || tr.RowDiffs.File == "" {
		t.Fatalf("drill must run under the watermark: %+v", tr.RowDiffs)
	}

	pq := src.pointQueries()
	if len(pq) == 0 {
		t.Fatal("no point queries recorded")
	}
	if want := `SELECT * FROM "public"."t1" WHERE "wm" <= $1 AND "id" = $2 LIMIT 1`; pq[0] != want {
		t.Fatalf("wm point query = %q; want %q", pq[0], want)
	}
	args := src.argsFor(`"id" = $2 LIMIT 1`)
	if len(args) == 0 || len(args[0]) != 2 || args[0][0] != "100" || args[0][1] != int64(3) {
		t.Fatalf("wm point args = %v, want [100 3]", args)
	}
}

// Test K6c: truncation — each value capped at 256 runes with the … mark;
// the row capped at rowDiffContentRowBytes with the truncated flag.
func TestRowDiffContentTruncation(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("值", 300) // 300 runes, multibyte
	v := newDrillValidator(dir)
	src := &sideScript{count: 2, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), long})}
	dst := &sideScript{count: 2,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "x"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	rd := tr.RowDiffs
	if rd.File == "" || rd.ContentDiff != 1 {
		t.Fatalf("drill counts = %+v", rd)
	}
	entries := loadContent(t, dir, rd.File)
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	val := entries[0].SrcRow.Values[1]
	if !strings.HasSuffix(val, "…") {
		t.Fatalf("value must end with the cut mark: …%d…", len(val))
	}
	if got := utf8.RuneCountInString(val); got != rowDiffContentValueRunes+1 {
		t.Fatalf("value runes = %d, want %d+mark", got, rowDiffContentValueRunes)
	}

	// row cap: budget only fits the first column
	oldRow := rowDiffContentRowBytes
	rowDiffContentRowBytes = len("id") + len("2") + 2 + 1
	defer func() { rowDiffContentRowBytes = oldRow }()
	dir2 := t.TempDir()
	v2 := newDrillValidator(dir2)
	tr2 := v2.validateChecksumChunked(context.Background(),
		(&sideScript{count: 2, pkCols: []string{"id"}, data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "b"})}).open(),
		(&sideScript{count: 2, data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "c"})}).open(),
		"t1", make(concSem, 4))
	if tr2.RowDiffs.File == "" {
		t.Fatalf("drill must commit: %+v", tr2.RowDiffs)
	}
	entries2 := loadContent(t, dir2, tr2.RowDiffs.File)
	if len(entries2) != 1 || entries2[0].SrcRow == nil {
		t.Fatalf("entries2 = %+v", entries2)
	}
	if !entries2[0].SrcRow.Truncated {
		t.Fatal("row past the byte cap must set truncated")
	}
	if len(entries2[0].SrcRow.Columns) != 1 {
		t.Fatalf("row cap must drop later columns, got %v", entries2[0].SrcRow.Columns)
	}
}

// Test K6d: per-entry probe errors — a failing point query and a vanished
// row both become entry-level error fields; the counts, the CSV and the
// verdict stay untouched.
func TestRowDiffContentProbeErrors(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)
	src := &sideScript{count: 5, pkCols: []string{"id"},
		probeErrKeys: map[string]error{"3": errFakeProbe},
		data:         rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	// key 4 exists in the merge data but VANISHES before the probe
	dst := &sideScript{count: 5, probeData: nil,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(5), "a"}, [2]any{int64(6), "a"})}
	src.probeData = rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}) // 3 errors (mapped), 4 gone

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	rd := tr.RowDiffs
	if rd.ContentDiff != 1 || rd.SrcOnly != 1 || rd.DstOnly != 1 {
		t.Fatalf("probe errors must not touch counts: %+v", rd)
	}

	entries := loadContent(t, dir, rd.File)
	if len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].Error == "" || !strings.Contains(entries[0].Error, "点查失败") {
		t.Fatalf("failing probe must carry the error: %+v", entries[0])
	}
	if entries[0].SrcRow != nil {
		t.Fatalf("failed src probe must not render a row: %+v", entries[0])
	}
	if entries[1].Error == "" || !strings.Contains(entries[1].Error, "行已不存在") {
		t.Fatalf("vanished row must carry the moved-note: %+v", entries[1])
	}
	if entries[2].Error != "" || entries[2].DstRow == nil {
		t.Fatalf("healthy entry must stay healthy: %+v", entries[2])
	}

	// the CSV detail is intact alongside the degraded content artifact
	b, err := os.ReadFile(filepath.Join(dir, rd.File))
	if err != nil {
		t.Fatalf("csv missing: %v", err)
	}
	for _, want := range []string{"content_diff,3", "src_only,4", "dst_only,6"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("csv missing %q", want)
		}
	}
}

// errFakeProbe is the scripted point-query failure.
var errFakeProbe = errors.New("fake probe failure")

// Test K6e: budget stop — only the first rowDiffContentBudget diff keys
// (key order) are probed.
func TestRowDiffContentBudgetStop(t *testing.T) {
	dir := t.TempDir()
	old := rowDiffContentBudget
	rowDiffContentBudget = 2
	defer func() { rowDiffContentBudget = old }()

	tr, _, _ := contentFixture(t, dir)
	entries := loadContent(t, dir, tr.RowDiffs.File)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want the budget 2", len(entries))
	}
	if entries[0].Key != "3" || entries[1].Key != "4" {
		t.Fatalf("budget keeps the FIRST keys in order, got %v", entries)
	}
}

// Test K6f: zero-diff / aborted drills write no content artifact.
func TestRowDiffContentZeroDiffNoFile(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "b"}, [2]any{int64(3), "c"}, [2]any{int64(4), "d"}, [2]any{int64(5), "e"})
	src := &sideScript{count: 5, pkCols: []string{"id"}, data: same}
	dst := &sideScript{count: 5, data: same,
		chunkOverride: rowsOf([2]any{int64(1), "X"}, [2]any{int64(2), "b"}, [2]any{int64(3), "c"}, [2]any{int64(4), "d"}, [2]any{int64(5), "e"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.Status != reporter.StatusFail {
		t.Fatal("chunk mismatch verdict must stand")
	}
	if tr.RowDiffs == nil || tr.RowDiffs.Note == "" {
		t.Fatalf("zero-diff disclosure missing: %+v", tr.RowDiffs)
	}
	if jsons, _ := filepath.Glob(filepath.Join(dir, "*.content.json")); len(jsons) != 0 {
		t.Fatalf("zero diffs must leave no content artifact, got %v", jsons)
	}
	if csvs, _ := filepath.Glob(filepath.Join(dir, "*.csv")); len(csvs) != 0 {
		t.Fatalf("zero diffs must leave no CSV either, got %v", csvs)
	}
}

// Test N-c (red team ④, leader final ruling): the dst side serves its id
// column as []byte during the scan (MySQL-family drivers' binary
// rendering) — the captured rawKey must be sanitized to string, the
// point query re-binds the STRING (never a driver buffer), and the probe
// still HITS. The K6 family scripts both sides as int64, so the
// []byte→string re-bind path was never truly anchored there.
func TestRowDiffContentByteSliceKeyPointQuery(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	src := &sideScript{count: 2, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "b"})}
	dst := &sideScript{count: 2,
		data: rowsOf([2]any{[]byte("1"), "a"}, [2]any{[]byte("2"), "c"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	rd := tr.RowDiffs
	if rd == nil || rd.ContentDiff != 1 || rd.SrcOnly != 0 || rd.DstOnly != 0 {
		t.Fatalf("byte-key drill counts = %+v, want content 1 only (key 2)", rd)
	}

	entries := loadContent(t, dir, rd.File)
	if len(entries) != 1 || entries[0].Key != "2" || entries[0].Error != "" {
		t.Fatalf("probe entry = %+v, want key 2 without error", entries)
	}
	e := entries[0]
	if e.SrcRow == nil || e.DstRow == nil {
		t.Fatalf("content_diff must carry BOTH rows: %+v", e)
	}
	if e.SrcRow.Values[0] != "2" || e.SrcRow.Values[1] != "b" {
		t.Fatalf("src row values = %v", e.SrcRow.Values)
	}
	if e.DstRow.Values[0] != "2" || e.DstRow.Values[1] != "c" {
		t.Fatalf("dst row values = %v (the []byte key must render as \"2\")", e.DstRow.Values)
	}

	// the dst point query bound the SANITIZED STRING, never the []byte
	// driver buffer; the src twin keeps its native int64 — one probe,
	// two driver shapes, same key.
	dstArgs := dst.argsFor("`id` = ? LIMIT 1")
	if len(dstArgs) != 1 || len(dstArgs[0]) != 1 {
		t.Fatalf("dst point query args = %v, want exactly one bind", dstArgs)
	}
	s, ok := dstArgs[0][0].(string)
	if !ok || s != "2" {
		t.Fatalf("dst probe must bind the sanitized string \"2\", got %T %v", dstArgs[0][0], dstArgs[0][0])
	}
	srcArgs := src.argsFor(`"id" = $1 LIMIT 1`)
	if len(srcArgs) != 1 || srcArgs[0][0] != int64(2) {
		t.Fatalf("src probe must keep the native int64, got %v", srcArgs)
	}
}
