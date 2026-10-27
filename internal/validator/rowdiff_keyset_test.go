package validator

// rowdiff_keyset_test.go anchors the v2 keyset pagination (FEAT-ROWDIFF-V2
// ①) and the mega-table guardrail (③):
//
//	K1  keyset SQL shape — single/composite key × (no wm / single wm /
//	    range wm) × (PG source $N numbering / MySQL source ? / TiDB target
//	    ?); first window carries no seek predicate; PG seek placeholders
//	    number AFTER the watermark's ($3 起 under a range watermark).
//	K2  seek binds NATIVE driver values — int64 stays int64, never the
//	    normalized string "2".
//	K3  paging equivalence — 123 rows in windows of 50 (injected), the key
//	    set led by the 2/10/11 numeric-order group (native varchar keys):
//	    no skip, no dup, counts exact, every window a fresh seek.
//	K5  guardrail two states — mega trigger caps the detail at
//	    rowDiffMegaDetailCap with the 超大表护栏 disclosure; non-trigger
//	    keeps the full cap; counts exact either way.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// withRowDiffWindow runs body with rowDiffWindow injected (multi-window
// drills without materializing 50k rows).
func withRowDiffWindow(t *testing.T, w int, body func()) {
	t.Helper()
	old := rowDiffWindow
	rowDiffWindow = w
	defer func() { rowDiffWindow = old }()
	body()
}

// drillQueries drives one drilled compare (count-mismatch face) and
// returns the table report.
func drillQueries(t *testing.T, dir string, src, dst *sideScript) reporter.TableReport {
	t.Helper()
	v := newDrillValidator(dir)
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.RowDiffs == nil {
		t.Fatalf("drill must run: %+v", tr)
	}
	return tr
}

// drillQueriesWM drives a drilled compare with an explicit watermark group.
func drillQueriesWM(t *testing.T, dir string, src, dst *sideScript, wm *config.WatermarkFilter) reporter.TableReport {
	t.Helper()
	v := newDrillValidator(dir)
	v.cfg.Compare.Watermark = wm
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.RowDiffs == nil {
		t.Fatalf("drill must run: %+v / %+v", tr, tr.RowDiffs)
	}
	return tr
}

// Test K1a: single-key keyset SQL shapes on the PG source and the TiDB
// target, across all three watermark forms — first window WITHOUT a seek
// predicate, later windows with the single-key seek, PG placeholders
// numbered after the watermark's.
func TestRowDiffKeysetSQLShapeSingleKey(t *testing.T) {
	base := func() (*sideScript, *sideScript) {
		src := &sideScript{count: 5, pkCols: []string{"id"},
			data: rowsOf(
				[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"},
				[2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
		dst := &sideScript{count: 4,
			data: rowsOf(
				[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(4), "a"})}
		return src, dst
	}

	withRowDiffWindow(t, 2, func() {
		t.Run("no watermark", func(t *testing.T) {
			dir := t.TempDir()
			src, dst := base()
			drillQueries(t, dir, src, dst)

			wq := src.drillWindowQueries()
			if len(wq) < 3 {
				t.Fatalf("expected >=3 source windows, got %d: %v", len(wq), wq)
			}
			first := wq[0]
			if strings.Contains(first, "WHERE") || strings.Contains(first, "OFFSET") {
				t.Fatalf("first window must have no seek predicate and no OFFSET: %s", first)
			}
			if want := `SELECT * FROM "public"."t1" ORDER BY "id" LIMIT 2`; first != want {
				t.Fatalf("first window = %q; want %q", first, want)
			}
			second := wq[1]
			if want := `SELECT * FROM "public"."t1" WHERE ("id" > $1) ORDER BY "id" LIMIT 2`; second != want {
				t.Fatalf("second window = %q; want %q", second, want)
			}
			// TiDB target twin: positional ? placeholder, backticks
			tq := dst.drillWindowQueries()
			if len(tq) < 3 {
				t.Fatalf("expected >=3 target windows, got %d", len(tq))
			}
			if want := "SELECT * FROM `t1` WHERE (`id` > ?) ORDER BY `id` LIMIT 2"; tq[1] != want {
				t.Fatalf("target second window = %q; want %q", tq[1], want)
			}
			for _, q := range append(wq, tq...) {
				if strings.Contains(q, "OFFSET") {
					t.Fatalf("keyset paging must never emit OFFSET: %s", q)
				}
			}
		})

		t.Run("single-value watermark", func(t *testing.T) {
			dir := t.TempDir()
			src, dst := base()
			_ = drillQueriesWM(t, dir, src, dst, &config.WatermarkFilter{Column: "wm", Value: "100"})
			wq := src.drillWindowQueries()
			if len(wq) < 3 {
				t.Fatalf("expected >=3 windows, got %d", len(wq))
			}
			if !strings.Contains(wq[0], `"wm" <= $1`) || strings.Contains(wq[0], " > ") {
				t.Fatalf("first window: watermark only, no seek: %s", wq[0])
			}
			if want := `SELECT * FROM "public"."t1" WHERE "wm" <= $1 AND ("id" > $2) ORDER BY "id" LIMIT 2`; wq[1] != want {
				t.Fatalf("wm single second window = %q; want %q", wq[1], want)
			}
			// TiDB: watermark first, then the positional seek
			tq := dst.drillWindowQueries()
			if want := "SELECT * FROM `t1` WHERE `wm` <= ? AND (`id` > ?) ORDER BY `id` LIMIT 2"; tq[1] != want {
				t.Fatalf("wm single target second window = %q; want %q", tq[1], want)
			}
		})

		t.Run("range watermark", func(t *testing.T) {
			dir := t.TempDir()
			src, dst := base()
			_ = drillQueriesWM(t, dir, src, dst, &config.WatermarkFilter{
				Column: "wm", RangeStart: "50", RangeEnd: "90"})
			wq := src.drillWindowQueries()
			if len(wq) < 3 {
				t.Fatalf("expected >=3 windows, got %d", len(wq))
			}
			if want := `SELECT * FROM "public"."t1" WHERE "wm" >= $1 AND "wm" < $2 AND ("id" > $3) ORDER BY "id" LIMIT 2`; wq[1] != want {
				t.Fatalf("wm range second window = %q; want %q (seek must start at $3)", wq[1], want)
			}
			tq := dst.drillWindowQueries()
			if want := "SELECT * FROM `t1` WHERE `wm` >= ? AND `wm` < ? AND (`id` > ?) ORDER BY `id` LIMIT 2"; tq[1] != want {
				t.Fatalf("wm range target second window = %q; want %q", tq[1], want)
			}
		})
	})
}

// Test K1b: composite-key OR chains — PG reuses one placeholder per key
// column ((k1 > $1 OR (k1 = $1 AND k2 > $2))), MySQL/TiDB repeat the value
// per positional occurrence.
func TestRowDiffKeysetSQLShapeCompositeKey(t *testing.T) {
	withRowDiffWindow(t, 2, func() {
		dir := t.TempDir()
		src := &sideScript{count: 3, pkCols: []string{"id", "name"},
			data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"})}
		dst := &sideScript{count: 2,
			data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(3), "d"})}
		tr := drillQueries(t, dir, src, dst)

		if tr.RowDiffs.ContentDiff != 0 || tr.RowDiffs.SrcOnly != 2 || tr.RowDiffs.DstOnly != 1 {
			t.Fatalf("composite drill counts = %+v, want content 0 / src 2 / dst 1", tr.RowDiffs)
		}

		wq := src.drillWindowQueries()
		if len(wq) < 2 {
			t.Fatalf("expected >=2 source windows, got %v", wq)
		}
		if want := `SELECT * FROM "public"."t1" WHERE ("id" > $1 OR ("id" = $1 AND "name" > $2)) ORDER BY "id", "name" LIMIT 2`; wq[1] != want {
			t.Fatalf("composite PG second window = %q; want %q", wq[1], want)
		}
		pgArgs := src.argsFor(wq[1])
		if len(pgArgs) != 1 || len(pgArgs[0]) != 2 {
			t.Fatalf("composite PG seek must bind exactly the 2 key values once each, got %v", pgArgs)
		}

		tq := dst.drillWindowQueries()
		if len(tq) < 2 {
			t.Fatalf("expected >=2 target windows, got %v", tq)
		}
		if want := "SELECT * FROM `t1` WHERE (`id` > ? OR (`id` = ? AND `name` > ?)) ORDER BY `id`, `name` LIMIT 2"; tq[1] != want {
			t.Fatalf("composite target second window = %q; want %q", tq[1], want)
		}
		tArgs := dst.argsFor(tq[1])
		if len(tArgs) != 1 || len(tArgs[0]) != 3 {
			t.Fatalf("composite positional seek binds 3 occurrences (v1, v1, v2), got %v", tArgs)
		}
	})
}

// Test K1c: MySQL SOURCE dialect — ? placeholders, ` quoting, the
// information_schema.statistics key probe.
func TestRowDiffKeysetSQLShapeMySQLSource(t *testing.T) {
	withRowDiffWindow(t, 2, func() {
		dir := t.TempDir()
		v := newDrillValidator(dir)
		v.srcDialect = mysqlDialect{}

		src := &sideScript{count: 5, pkCols: []string{"id"},
			data: rowsOf(
				[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"},
				[2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
		dst := &sideScript{count: 4,
			data: rowsOf(
				[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(4), "a"})}

		tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
		if tr.RowDiffs == nil {
			t.Fatalf("drill must run: %+v", tr)
		}

		wq := src.drillWindowQueries()
		if len(wq) < 3 {
			t.Fatalf("expected >=3 source windows, got %v", wq)
		}
		if want := "SELECT * FROM `public`.`t1` WHERE (`id` > ?) ORDER BY `id` LIMIT 2"; wq[1] != want {
			t.Fatalf("mysql source second window = %q; want %q", wq[1], want)
		}
		if strings.Contains(wq[1], "$") {
			t.Fatalf("mysql source must never see $N placeholders: %s", wq[1])
		}
	})
}

// Test K2: the seek binds NATIVE driver values — the window boundary key
// 2 (then 4) arrives as int64, never the normalized string.
func TestRowDiffKeysetBindsNativeTypes(t *testing.T) {
	withRowDiffWindow(t, 2, func() {
		dir := t.TempDir()
		src := &sideScript{count: 5, pkCols: []string{"id"},
			data: rowsOf(
				[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"},
				[2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
		dst := &sideScript{count: 4,
			data: rowsOf(
				[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(4), "a"})}
		drillQueries(t, dir, src, dst)

		seekArgs := src.argsFor(" > ")
		if len(seekArgs) == 0 {
			t.Fatal("no seek windows recorded")
		}
		for _, args := range seekArgs {
			for _, v := range args {
				switch tv := v.(type) {
				case int64:
					if tv != 2 && tv != 4 {
						t.Fatalf("unexpected int64 seek bound: %v", tv)
					}
				case string:
					t.Fatalf("seek must bind the native int64, not the string %q", tv)
				default:
					t.Fatalf("unexpected seek bound type %T: %v", v, v)
				}
			}
		}
	})
}

// Test K3: paging equivalence on a 123-row table with injected 50-row
// windows. The key set leads with the 2/10/11 numeric-order group as
// native STRINGS (varchar keys: DB string order == canonical order, so the
// unchanged sortedness guard stays quiet; zero-padded k-keys fill out the
// body). Window boundaries land after the 50th and 100th rows; one diff
// sits exactly ON a boundary (k050) and one just past it (k051). Keyset
// paging must yield the identical diff set with no skip and no dup —
// counts exact, four CSV rows, boundaries seek-bound as native strings.
func TestRowDiffKeysetPagingEquivalence(t *testing.T) {
	dir := t.TempDir()
	withRowDiffWindow(t, 50, func() {
		// key universe in ascending string order:
		// "10" < "11" < "2" < "k001" < … < "k120" < "z9"
		keys := []string{"10", "11", "2"}
		for i := 1; i <= 120; i++ {
			keys = append(keys, fmt.Sprintf("k%03d", i))
		}
		keyIdx := func(k string) int {
			for i, existing := range keys {
				if existing == k {
					return i
				}
			}
			return -1
		}

		srcData := make([][]driver.Value, 0, len(keys))
		dstData := make([][]driver.Value, 0, len(keys))
		for _, k := range keys {
			srcData = append(srcData, []driver.Value{k, "a"})
			if k != "k051" { // src_only just past the first boundary
				dstData = append(dstData, []driver.Value{k, "a"})
			}
		}
		dstData[keyIdx("10")][1] = "x"                       // content_diff inside the 2/10/11 group
		dstData[keyIdx("k050")][1] = "y"                     // content_diff exactly ON the boundary
		dstData = append(dstData, []driver.Value{"z9", "a"}) // dst_only, sorts last

		src := &sideScript{count: int64(len(srcData)), pkCols: []string{"id"}, data: srcData}
		dst := &sideScript{count: int64(len(dstData)), data: dstData}

		tr := drillQueries(t, dir, src, dst)

		rd := tr.RowDiffs
		if rd.ContentDiff != 2 || rd.SrcOnly != 1 || rd.DstOnly != 1 {
			t.Fatalf("paging lost rows: counts = %+v, want content 2 / src 1 / dst 1", rd)
		}
		if tr.DiffRows != 4 {
			t.Fatalf("DiffRows = %d, want 4", tr.DiffRows)
		}

		// every drill window paged by seek; never OFFSET
		for _, q := range append(src.drillWindowQueries(), dst.drillWindowQueries()...) {
			if strings.Contains(q, "OFFSET") {
				t.Fatalf("keyset paging must never emit OFFSET: %s", q)
			}
		}
		// boundaries after the 50th row (k047) and the 100th (k097),
		// bound as NATIVE strings
		boundaries := map[string]bool{}
		for _, args := range append(src.argsFor(" > "), dst.argsFor(" > ")...) {
			for _, v := range args {
				if s, ok := v.(string); ok {
					boundaries[s] = true
				}
			}
		}
		if !boundaries["k047"] || !boundaries["k097"] {
			t.Fatalf("window boundaries k047/k097 must be seek-bound natively, saw %v", boundaries)
		}

		// the CSV diff set is exactly the four keys, no dups
		b, err := os.ReadFile(filepath.Join(dir, rd.File))
		if err != nil {
			t.Fatalf("detail artifact missing: %v", err)
		}
		text := string(b)
		for _, want := range []string{"content_diff,10", "content_diff,k050", "src_only,k051", "dst_only,z9"} {
			if !strings.Contains(text, want) {
				t.Fatalf("diff %q missing (skip?) in:\n%s", want, text)
			}
		}
		if got := strings.Count(text, "content_diff") + strings.Count(text, "src_only") + strings.Count(text, "dst_only"); got != 4 {
			t.Fatalf("CSV detail rows = %d (dup?), want 4:\n%s", got, text)
		}
	})
}

// Test K5: mega-table guardrail, both states. Triggered: the effective
// detail cap drops to rowDiffMegaDetailCap, the truncation disclosure
// names the guardrail, counts stay exact. Not triggered: full cap.
func TestRowDiffMegaGuardrail(t *testing.T) {
	oldMega := rowDiffMegaRows
	oldMegaCap := rowDiffMegaDetailCap
	defer func() {
		rowDiffMegaRows = oldMega
		rowDiffMegaDetailCap = oldMegaCap
	}()

	src := func() *sideScript {
		return &sideScript{count: 5, pkCols: []string{"id"},
			data: rowsOf(
				[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"},
				[2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	}

	t.Run("triggered caps detail and discloses the guardrail", func(t *testing.T) {
		dir := t.TempDir()
		rowDiffMegaRows = 4      // max(5,0) ≥ 4 → mega
		rowDiffMegaDetailCap = 2 // effective cap below the 5 diffs
		tr := drillQueries(t, dir, src(), &sideScript{count: 0})

		rd := tr.RowDiffs
		if rd.SrcOnly != 5 {
			t.Fatalf("counts must stay exact under the guardrail: %+v", rd)
		}
		if !rd.Truncated {
			t.Fatal("mega guardrail must mark Truncated")
		}
		b, err := os.ReadFile(filepath.Join(dir, rd.File))
		if err != nil {
			t.Fatalf("detail artifact missing: %v", err)
		}
		if got := strings.Count(string(b), "src_only"); got != 2 {
			t.Fatalf("mega detail rows = %d, want the lowered cap 2", got)
		}
		if !strings.Contains(tr.Suggestion, "超大表护栏：明细截断至") || !strings.Contains(tr.Suggestion, "计数仍精确") {
			t.Fatalf("guardrail disclosure missing: %q", tr.Suggestion)
		}
	})

	t.Run("not triggered keeps the full cap", func(t *testing.T) {
		dir := t.TempDir()
		rowDiffMegaRows = 6 // max(5,0) < 6 → not mega
		rowDiffMegaDetailCap = 2
		tr := drillQueries(t, dir, src(), &sideScript{count: 0})

		rd := tr.RowDiffs
		if rd.Truncated {
			t.Fatal("non-mega table must not hit the mega cap")
		}
		b, err := os.ReadFile(filepath.Join(dir, rd.File))
		if err != nil {
			t.Fatalf("detail artifact missing: %v", err)
		}
		if got := strings.Count(string(b), "src_only"); got != 5 {
			t.Fatalf("non-mega detail rows = %d, want all 5", got)
		}
		if strings.Contains(tr.Suggestion, "超大表护栏") {
			t.Fatalf("guardrail disclosure must stay off: %q", tr.Suggestion)
		}
	})
}

// Test K4v2 (upgraded per leader ruling 1): the NULL-key abort must also
// fire when the NULL row sits BEYOND the first window at the NULLS-LAST
// end — the exact blind spot where a keyset seek alone would silently
// drop it (seek is UNKNOWN on NULL). The pre-paging NULL pre-scan aborts
// before any window is fetched; PK-keyed tables skip the pre-scan at zero
// cost (NOT NULL by PK semantics).
func TestRowDiffNullKeyPreScanBeyondFirstWindow(t *testing.T) {
	withRowDiffWindow(t, 2, func() {
		dir := t.TempDir()
		v := newDrillValidator(dir)
		uk := "CREATE UNIQUE INDEX uk_name ON t1 (name)"
		src := &sideScript{count: 3, uniqueIdxDef: uk,
			// NULL row LAST (NULLS LAST order), beyond the 2-row first
			// window — first-window hits mask nothing here.
			data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"}, [2]any{int64(3), nil})}
		dst := &sideScript{count: 3, uniqueIdxDef: uk,
			data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"}, [2]any{int64(3), "cc"})}

		tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

		if tr.Status != reporter.StatusFail {
			t.Fatal("chunk mismatch verdict must stand (FAIL)")
		}
		rd := tr.RowDiffs
		if rd == nil || !strings.Contains(rd.Note, "NULL") {
			t.Fatalf("NULL pre-scan must abort with the NULL note: %+v", rd)
		}
		if rd.File != "" {
			t.Fatalf("aborted drill must leave no artifact, got %q", rd.File)
		}
		if csvs, _ := filepath.Glob(filepath.Join(dir, "*.csv")); len(csvs) != 0 {
			t.Fatalf("no detail CSV expected, got %v", csvs)
		}
		// the src hit aborts BEFORE any drill window (short-circuit: the
		// dst side is not probed after the abort) — the seek never gets a
		// chance to silently drop the NULL row
		if got := len(src.sawQueries("IS NULL")); got != 1 {
			t.Fatalf("expected exactly 1 NULL pre-scan on src, got %d: %v", got, src.sawQueries("IS NULL"))
		}
		if got := len(dst.sawQueries("IS NULL")); got != 0 {
			t.Fatalf("abort on src hit must short-circuit before dst, saw %d", got)
		}
		for _, s := range []*sideScript{src, dst} {
			if got := len(s.sawQueries(" > ")); got != 0 {
				t.Fatalf("pre-scan abort must precede every seek window, saw %d: %v", got, s.sawQueries(" > "))
			}
		}
		if !strings.Contains(src.sawQueries("IS NULL")[0], `"name" IS NULL`) {
			t.Fatalf("pre-scan must probe the key column: %s", src.sawQueries("IS NULL")[0])
		}
	})
}

// Test K4v2 (clean table, both sides): with NO NULL keys anywhere the
// pre-scan runs exactly once per side (恰 2 单元) and the drill proceeds.
func TestRowDiffNullKeyPreScanCleanTableBothSides(t *testing.T) {
	dir := t.TempDir()
	withRowDiffWindow(t, 2, func() {
		uk := "CREATE UNIQUE INDEX uk_name ON t1 (name)"
		src := &sideScript{count: 3, uniqueIdxDef: uk,
			data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"}, [2]any{int64(3), "cc"})}
		dst := &sideScript{count: 3, uniqueIdxDef: uk,
			data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"}, [2]any{int64(3), "dd"})}

		tr := drillQueries(t, dir, src, dst)

		rd := tr.RowDiffs
		// key=name: cc vs dd are DISTINCT keys → one src_only + one dst_only
		if rd.ContentDiff != 0 || rd.SrcOnly != 1 || rd.DstOnly != 1 || rd.Note != "" {
			t.Fatalf("clean unique-index drill must complete: %+v", rd)
		}
		for _, s := range []*sideScript{src, dst} {
			if got := len(s.sawQueries("IS NULL")); got != 1 {
				t.Fatalf("expected exactly 1 NULL pre-scan per side (恰 2 单元), got %d", got)
			}
		}
	})
}

// Test K4v2 (wm universe): the NULL pre-scan carries the SAME watermark
// predicate as the drill windows.
func TestRowDiffNullKeyPreScanWatermarkUniverse(t *testing.T) {
	withRowDiffWindow(t, 2, func() {
		dir := t.TempDir()
		v := newDrillValidator(dir)
		v.cfg.Compare.Watermark = &config.WatermarkFilter{Column: "wm", Value: "100"}
		uk := "CREATE UNIQUE INDEX uk_name ON t1 (name)"
		src := &sideScript{count: 2, uniqueIdxDef: uk,
			data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), nil})}
		dst := &sideScript{count: 2, uniqueIdxDef: uk,
			data: rowsOf([2]any{int64(1), "aa"}, [2]any{int64(2), "bb"})}

		tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
		rd := tr.RowDiffs
		if rd == nil || !strings.Contains(rd.Note, "NULL") {
			t.Fatalf("NULL pre-scan must abort: %+v", rd)
		}
		pq := src.sawQueries("IS NULL")
		if len(pq) != 1 || !strings.Contains(pq[0], `"wm" <= $1 AND`) {
			t.Fatalf("pre-scan must carry the watermark predicate, got %v", pq)
		}
		args := src.argsFor("IS NULL")
		if len(args) != 1 || len(args[0]) != 1 || args[0][0] != "100" {
			t.Fatalf("pre-scan binds exactly the wm value, got %v", args)
		}
	})
}

// Test K4v2 (PK path zero cost): PK-keyed tables never issue a NULL
// pre-scan (PK columns are NOT NULL by PK semantics on both engines).
func TestRowDiffNullKeyPreScanSkippedOnPK(t *testing.T) {
	dir := t.TempDir()
	src := &sideScript{count: 5, pkCols: []string{"id"},
		data: rowsOf(
			[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"},
			[2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 5,
		data: rowsOf(
			[2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(5), "a"}, [2]any{int64(6), "a"})}
	tr := drillQueries(t, dir, src, dst)

	if tr.RowDiffs.ContentDiff != 1 || tr.RowDiffs.SrcOnly != 1 || tr.RowDiffs.DstOnly != 1 {
		t.Fatalf("PK drill counts = %+v", tr.RowDiffs)
	}
	for _, s := range []*sideScript{src, dst} {
		if got := len(s.sawQueries("IS NULL")); got != 0 {
			t.Fatalf("PK path must skip the NULL pre-scan, saw %d: %v", got, s.sawQueries("IS NULL"))
		}
	}
}
