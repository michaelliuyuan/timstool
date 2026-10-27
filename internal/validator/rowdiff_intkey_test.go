package validator

// rowdiff_intkey_test.go anchors the numeric-aware canonical key order
// (leader ruling 3, seq=43): integer-family key columns (driver-reported
// types, negotiated src&&dst) align NUMERICALLY so integer PK tables drill
// 9→10→11 instead of aborting on the bytewise "10" < "9"; string/decimal
// keys keep the V1 behavior; a cross-end integer-ness drift aborts honestly.
//
//	N1  int keys 1..12 in injected windows of 5 (crossing 9→10→11 inside
//	    one window AND across window boundaries): zero abort, three-class
//	    counts exact, CSV reconciliation, boundaries seek-bound as native
//	    int64, the B-line content.json written.
//	N2  varchar keys "01" vs "1" NEVER pair — the numeric awareness does
//	    not leak into string keys.
//	N3  cross-end integer-ness mismatch (src int8, dst varchar) aborts
//	    with rowDiffKeyKindNote before any classification; detection
//	    rides the first window (no extra metadata query).
//	N4  decimal keys keep the V1 bytewise abort (numeric order 9.5→10.5
//	    is a canonical decrease — recorded for v2).
//	N5  numeric disorder still aborts — the upgraded guard is STRICTER
//	    than the V1 bytewise guard ([10, 9] is byte-ascending but a true
//	    numeric decrease; V1 would misalign, the upgrade aborts).

import (
	"database/sql/driver"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// Test N1: integer keys 1..12, rowDiffWindow=5. src misses 4 (dst_only),
// dst misses 10 (src_only), content differs at 3/9/11 — src window 2
// carries the 8→9→10→11 crossing INSIDE one window, dst window 2 carries
// 9→11, and both sides' third windows seek past 11. The pre-fix bytewise
// canonical order would abort at 9→10 ("10" < "9").
func TestRowDiffIntKeyNumericOrderPaging(t *testing.T) {
	dir := t.TempDir()
	withRowDiffWindow(t, 5, func() {
		srcData := make([][]driver.Value, 0, 11)
		dstData := make([][]driver.Value, 0, 11)
		for i := int64(1); i <= 12; i++ {
			srcName, dstName := driver.Value("a"), driver.Value("a")
			switch i {
			case 3, 9, 11: // content_diff, spanning the 9→10→11 crossing
				srcName, dstName = "c", "d"
			}
			if i != 4 { // 4 exists only on dst → dst_only
				srcData = append(srcData, []driver.Value{i, srcName})
			}
			if i != 10 { // 10 exists only on src → src_only (ON the crossing)
				dstData = append(dstData, []driver.Value{i, dstName})
			}
		}
		src := &sideScript{count: 11, pkCols: []string{"id"}, data: srcData}
		dst := &sideScript{count: 11, data: dstData}

		tr := drillQueries(t, dir, src, dst)

		rd := tr.RowDiffs
		if rd.Note != "" {
			t.Fatalf("integer keys must drill across 9→10→11 without abort: %+v", rd)
		}
		if rd.ContentDiff != 3 || rd.SrcOnly != 1 || rd.DstOnly != 1 {
			t.Fatalf("int-key counts = %+v, want content 3 / src 1 / dst 1", rd)
		}
		if tr.DiffRows != 5 {
			t.Fatalf("DiffRows = %d, want 5", tr.DiffRows)
		}
		if len(rd.SampleKeys) != 5 || rd.SampleKeys[0] != "3" {
			t.Fatalf("sample keys = %v, want 5 samples led by 3", rd.SampleKeys)
		}

		// 11 rows per side in windows of 5 → exactly 3 windows each,
		// every post-first window a fresh seek, never OFFSET.
		for _, s := range []*sideScript{src, dst} {
			wq := s.drillWindowQueries()
			if len(wq) != 3 {
				t.Fatalf("expected exactly 3 drill windows, got %d: %v", len(wq), wq)
			}
			for _, q := range wq {
				if strings.Contains(q, "OFFSET") {
					t.Fatalf("keyset paging must never emit OFFSET: %s", q)
				}
			}
		}

		// Boundaries seek-bound as NATIVE int64 (src: 6 then 11; dst: 5
		// then 11 — the 11 boundary sits exactly on the crossing).
		boundaries := func(s *sideScript) map[int64]bool {
			out := map[int64]bool{}
			for _, args := range s.argsFor(" > ") {
				for _, v := range args {
					i, ok := v.(int64)
					if !ok {
						t.Fatalf("seek must bind native int64, got %T %v", v, v)
					}
					out[i] = true
				}
			}
			return out
		}
		if got := boundaries(src); len(got) != 2 || !got[6] || !got[11] {
			t.Fatalf("src seek boundaries = %v, want {6 11}", got)
		}
		if got := boundaries(dst); len(got) != 2 || !got[5] || !got[11] {
			t.Fatalf("dst seek boundaries = %v, want {5 11}", got)
		}

		// CSV reconciliation: exactly the five classified keys, no dups.
		b, err := os.ReadFile(filepath.Join(dir, rd.File))
		if err != nil {
			t.Fatalf("detail artifact missing: %v", err)
		}
		text := string(b)
		for _, want := range []string{"content_diff,3", "content_diff,9", "content_diff,11", "src_only,10", "dst_only,4"} {
			if !strings.Contains(text, want) {
				t.Fatalf("diff %q missing in:\n%s", want, text)
			}
		}
		if got := strings.Count(text, "content_diff") + strings.Count(text, "src_only") + strings.Count(text, "dst_only"); got != 5 {
			t.Fatalf("CSV detail rows = %d, want 5:\n%s", got, text)
		}

		// B-line rides along on integer keys: the content.json exists
		// next to the CSV (format anchored separately in K6).
		if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(rd.File, ".csv")+".content.json")); err != nil {
			t.Fatalf("content.json must accompany the int-key drill: %v", err)
		}
	})
}

// Test N2: varchar keys — "01" and "1" are DIFFERENT keys forever; the
// numeric awareness only applies to columns BOTH sides report as integer
// family, never to string keys (numeric equality 01==1 must not pair).
func TestRowDiffIntKeyVarcharIsolation(t *testing.T) {
	dir := t.TempDir()
	src := &sideScript{count: 1, pkCols: []string{"id"},
		dataColTypes: []string{"varchar", "text"},
		data:         rowsOf([2]any{"01", "a"})}
	dst := &sideScript{count: 1,
		dataColTypes: []string{"varchar", "text"},
		data:         rowsOf([2]any{"1", "a"})}

	tr := drillQueries(t, dir, src, dst)

	rd := tr.RowDiffs
	if rd.Note != "" || rd.ContentDiff != 0 || rd.SrcOnly != 1 || rd.DstOnly != 1 {
		t.Fatalf(`varchar "01" vs "1" must classify as one src_only + one dst_only, never pair: %+v`, rd)
	}
	b, err := os.ReadFile(filepath.Join(dir, rd.File))
	if err != nil {
		t.Fatalf("detail artifact missing: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "src_only,01") || !strings.Contains(text, "dst_only,1") {
		t.Fatalf("CSV must carry both only-rows:\n%s", text)
	}
}

// Test N3: cross-end integer-ness drift (src id int8, dst id varchar)
// aborts with rowDiffKeyKindNote BEFORE any classification — pairing under
// a half-numeric order would fabricate diffs. Detection rides the two
// first windows' ColumnTypes: no extra metadata query, no seek window.
func TestRowDiffIntKeyCrossEndKindDriftAborts(t *testing.T) {
	dir := t.TempDir()
	src := &sideScript{count: 3, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "b"}, [2]any{int64(3), "c"})}
	dst := &sideScript{count: 3,
		dataColTypes: []string{"varchar", "text"},
		data:         rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "b"}, [2]any{int64(3), "d"})}

	tr := drillQueries(t, dir, src, dst)

	if tr.Status != reporter.StatusFail {
		t.Fatal("chunk mismatch verdict must stand (FAIL)")
	}
	rd := tr.RowDiffs
	if rd == nil || rd.Note != rowDiffKeyKindNote {
		t.Fatalf("cross-end integer-ness drift must abort with the kind note, got %+v", rd)
	}
	if rd.File != "" {
		t.Fatalf("aborted drill must leave no artifact, got %q", rd.File)
	}
	if csvs, _ := filepath.Glob(filepath.Join(dir, "*.csv")); len(csvs) != 0 {
		t.Fatalf("no detail CSV expected, got %v", csvs)
	}
	if js, _ := filepath.Glob(filepath.Join(dir, "*.content.json")); len(js) != 0 {
		t.Fatalf("no content.json expected, got %v", js)
	}
	// Detection rides the first windows (both sides fetched once); the
	// abort precedes every seek window.
	for _, s := range []*sideScript{src, dst} {
		if got := len(s.drillWindowQueries()); got != 1 {
			t.Fatalf("drift abort must fire on the first window, saw %d: %v", got, s.drillWindowQueries())
		}
		if got := len(s.sawQueries(" > ")); got != 0 {
			t.Fatalf("no seek window may follow the drift abort, saw %v", s.sawQueries(" > "))
		}
	}
}

// Test N4: decimal keys keep the V1 bytewise canonical order — numeric
// order 9.5→10.5 is a canonical DECREASE ("10.5" < "9.5"), the guard
// aborts exactly as before (variable decimal renderings stay out of the
// integer family; a numeric decimal alignment is a v2 item, not this one).
func TestRowDiffIntKeyDecimalKeepsV1Abort(t *testing.T) {
	dir := t.TempDir()
	src := &sideScript{count: 2, pkCols: []string{"id"},
		dataColTypes: []string{"numeric", "text"},
		data:         rowsOf([2]any{"9.5", "a"}, [2]any{"10.5", "a"})}
	dst := &sideScript{count: 2,
		dataColTypes: []string{"numeric", "text"},
		data:         rowsOf([2]any{"9.5", "a"}, [2]any{"10.5", "b"})}

	tr := drillQueries(t, dir, src, dst)

	rd := tr.RowDiffs
	if rd == nil || !strings.Contains(rd.Note, "键序与对齐序不一致") {
		t.Fatalf("decimal keys must keep the V1 sortedness abort, got %+v", rd)
	}
	if rd.File != "" {
		t.Fatalf("aborted drill must leave no artifact, got %q", rd.File)
	}
}

// Test N5: the numeric-aware guard stays a guard — a TRUE numeric disorder
// still aborts. [10, 9] is byte-ASCENDING ("10" < "9" bytewise — the V1
// guard would wave it through and misalign) but numerically descending:
// the upgraded guard aborts it. Stricter, never permissive.
func TestRowDiffIntKeyTrueDisorderStillAborts(t *testing.T) {
	dir := t.TempDir()
	src := &sideScript{count: 2, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(10), "a"}, [2]any{int64(9), "a"})}
	dst := &sideScript{count: 1, data: rowsOf([2]any{int64(9), "b"})}

	tr := drillQueries(t, dir, src, dst)

	rd := tr.RowDiffs
	if rd == nil || !strings.Contains(rd.Note, "键序与对齐序不一致") {
		t.Fatalf("true numeric disorder must still abort, got %+v", rd)
	}
	if rd.File != "" {
		t.Fatalf("aborted drill must leave no artifact, got %q", rd.File)
	}
}
