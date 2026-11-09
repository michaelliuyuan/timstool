package validator

// rowhashspool_test.go — M1 spool regression anchors:
//  1. oracle equivalence: the spool's sorted (hash, count) stream is
//     item-for-item identical to the legacy in-memory map[string]int
//     (including forced spills and the cascade-merge path);
//  2. bounded memory: a distinct-heavy dataset spilling at a tiny
//     threshold keeps the live heap flat where the legacy map would
//     materialize everything;
//  3. cleanup: run files vanish on Close (both the Add-only abort path
//     and the Finish/iterate path);
//  4. fail-loud: disk errors surface as "row-hash spool" errors — both at
//     the spool level and through the full hash_group path;
//  5. end-to-end equivalence: the three no-PK strategies return the same
//     verdicts/messages as a legacy-map oracle reimplemented here (the
//     red-line "old vs new" differential; details are compared in the
//     deterministic ascending-hash order — the normalization of the
//     legacy Go-map-random detail order, byte-identical for ≤1 mismatch);
//  6. bounded mismatch rendering: the hash_group detail accumulator caps
//     inside the walk callback (阻断1) and the bucket detail list keeps
//     the legacy ≤5 shape with NO "and N more" suffix (阻断2);
//  7. mid-iteration scan failures (timeout) fail the table loud on all
//     three strategies and both sides — never a truncated comparison;
//  8. the no-PK path is query-frugal and connection-frugal, VERIFIED BY
//     COUNTING (not timing): 8 concurrent tables against an 8-conn pool
//     all pass, and the PG-side tally reads full=8 meta=0 peek=0 other=0
//     — one full-table query per table and zero auxiliary catalog
//     queries (the removed per-table space preflight regresses the tally
//     deterministically; mutation-verified in red-team r3 rework).

import (
	"context"
	"crypto/md5"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

func md5Hex(s string) string { return fmt.Sprintf("%x", md5.Sum([]byte(s))) }

// oraclePairs is the legacy aggregation: map counts, then sorted pairs.
func oraclePairs(hashes []string) []struct {
	h string
	c int
} {
	m := make(map[string]int, len(hashes))
	for _, h := range hashes {
		m[h]++
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]struct {
		h string
		c int
	}, 0, len(keys))
	for _, k := range keys {
		out = append(out, struct {
			h string
			c int
		}{k, m[k]})
	}
	return out
}

// genHashes builds a hash corpus: dup-heavy prefix + distinct tail, with
// boundary-shaped row inputs (multibyte, delimiter, empty, \N).
func genHashes(n int, seed int) []string {
	shapes := []string{"a", "", "\\N", "|", "中文值", "trim ", "\t"}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		if i%3 == 0 {
			out[i] = md5Hex(shapes[i%len(shapes)] + fmt.Sprintf("-%d", seed))
		} else {
			out[i] = md5Hex(fmt.Sprintf("row-%d-%d-%s", seed, i, shapes[i%len(shapes)]))
		}
	}
	return out
}

// TestRowHashSpoolOracleEquivalence walks the spool against the naive map
// across sizes that force 0, 1, several, and >fanIn runs (cascade merge).
func TestRowHashSpoolOracleEquivalence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		n          int
		maxEntries int
	}{
		{"tiny-no-spill", 10, 0},
		{"exact-limit", 1, 0},
		{"one-run-spill", 300, 100},
		{"many-runs", 5000, 100},
		{"cascade-over-fanin", 4000, 10}, // 400 runs > fanIn 32 → cascade
		// Review-probe anchors (TestReviewCascadeSingleRemainder twin): run
		// counts with a singleton tail group in the cascade pass — 65/2→33
		// runs (32+1) and 130/2→65 runs (32+32+1). The tail file must be
		// carried over, never deleted with the pass's consumed inputs.
		{"cascade-singleton-tail", 65, 2},
		{"cascade-doubleton-plus-one", 130, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := newRowHashSpool(dir, tc.maxEntries)
			hashes := genHashes(tc.n, 1)
			for _, h := range hashes {
				if err := s.Add(h); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}
			if got, wantSpill := s.Spilled(), (tc.maxEntries > 0 && tc.n > tc.maxEntries); got != wantSpill {
				t.Fatalf("Spilled() = %v, want %v", got, wantSpill)
			}
			it, err := s.Finish()
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			defer it.Close()
			oracle := oraclePairs(hashes)
			i := 0
			for {
				h, c, err := it.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if i >= len(oracle) {
					t.Fatalf("iterator yielded more pairs than oracle (%d)", len(oracle))
				}
				if h != oracle[i].h || c != oracle[i].c {
					t.Fatalf("pair %d = (%s,%d), oracle (%s,%d)", i, h, c, oracle[i].h, oracle[i].c)
				}
				i++
			}
			if i != len(oracle) {
				t.Fatalf("iterator yielded %d pairs, oracle %d", i, len(oracle))
			}
			if s.Rows() != uint64(tc.n) {
				t.Fatalf("Rows() = %d, want %d", s.Rows(), tc.n)
			}
			s.Close()
			files, _ := os.ReadDir(dir)
			if len(files) != 0 {
				t.Fatalf("run files not cleaned: %d left", len(files))
			}
		})
	}
}

// TestRowHashSpoolMemoryBounded proves disk-for-memory: 1M distinct hashes
// with a 1K-entry spill threshold keep the live heap flat (the legacy
// map[string]int would hold every hash alive, ~5-8x this bound).
func TestRowHashSpoolMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("memory probe skipped in -short")
	}
	dir := t.TempDir()
	s := newRowHashSpool(dir, 1000)

	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	const n = 1_000_000
	buf := make([]byte, 0, 64)
	for i := 0; i < n; i++ {
		buf = buf[:0]
		buf = append(buf, "m1-mem-"...)
		buf = append(buf, byte('0'+i%10))
		buf = append(buf, []byte(fmt.Sprintf("-%d", i))...)
		if err := s.Add(md5Hex(string(buf))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	grew := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	if grew < 0 {
		grew = 0
	}
	const bound = 32 << 20 // legacy map for 1M md5 keys ≈ 100-200MB live
	if grew > bound {
		t.Fatalf("live heap grew by %dMB (> %dMB) — spool is not bounding memory", grew>>20, bound>>20)
	}
	if !s.Spilled() {
		t.Fatal("1M distinct hashes must have spilled")
	}
	it, err := s.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	pairs := 0
	for {
		_, _, err := it.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		pairs++
	}
	it.Close()
	s.Close()
	if pairs != n {
		t.Fatalf("distinct pairs = %d, want %d", pairs, n)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("run files not cleaned after Close: %d left", len(files))
	}
}

// TestRowHashSpoolCloseWithoutFinishCleans covers the abort path: a table
// failing mid-scan still removes its flushed runs.
func TestRowHashSpoolCloseWithoutFinishCleans(t *testing.T) {
	dir := t.TempDir()
	s := newRowHashSpool(dir, 10)
	for _, h := range genHashes(500, 7) {
		if err := s.Add(h); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if !s.Spilled() {
		t.Fatal("expected spill")
	}
	s.Close()
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("abort path left %d files", len(files))
	}
}

// TestRowHashSpoolDiskFailureFailsLoud: an unusable spool dir must produce
// an explicit "row-hash spool" error, never a silent unbounded fallback.
func TestRowHashSpoolDiskFailureFailsLoud(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newRowHashSpool(notADir, 2)
	if err := s.Add(md5Hex("a")); err != nil {
		t.Fatalf("first Add (no flush yet): %v", err)
	}
	err := s.Add(md5Hex("b")) // crosses maxEntries → flush → CreateTemp fails
	if err == nil {
		t.Fatal("disk failure silently swallowed")
	}
	if !strings.Contains(err.Error(), "row-hash spool") {
		t.Fatalf("error must mention the spool, got: %v", err)
	}
	s.Close()
}

// ---------------------------------------------------------------------------
// Scripted-driver harness for the end-to-end three-path equivalence.
// ---------------------------------------------------------------------------

type m1Conn struct {
	pg   bool
	cols []string
	pgData,
	tidbData [][]driver.Value
	// poisonPG/poisonTiDB serve that side's rows then report a
	// mid-iteration deadline through sql.Rows.Err().
	poisonPG, poisonTiDB bool
	// counts, when non-nil, tallies every QueryContext by shape. This is
	// what gives the concurrency anchor teeth: the no-PK path must issue
	// exactly ONE full-table query per table and ZERO auxiliary catalog
	// queries on the PG side, so a re-introduced per-table pre-scan
	// (the removed space preflight) is caught by counting alone — no
	// pool or timing semantics required (this connector has none).
	counts *m1QCounts
}

func (c *m1Conn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *m1Conn) Close() error                        { return nil }
func (c *m1Conn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }

func (c *m1Conn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.counts != nil {
		c.counts.tally(q)
	}
	switch {
	case strings.Contains(q, "WHERE 1=0"):
		return &m1Rows{cols: c.cols}, nil
	case strings.Contains(q, "SELECT * FROM"):
		var data [][]driver.Value
		poison := false
		if c.pg {
			data, poison = c.pgData, c.poisonPG
		} else {
			data, poison = c.tidbData, c.poisonTiDB
		}
		if poison {
			return &m1ErrRows{m1Rows: m1Rows{cols: c.cols, data: data}, err: context.DeadlineExceeded}, nil
		}
		return &m1Rows{cols: c.cols, data: data}, nil
	}
	return &m1Rows{cols: []string{"x"}}, nil
}

// m1QCounts classifies queries the way a counting driver would. meta is
// checked FIRST so a catalog pre-scan ("SELECT ... FROM pg_catalog...")
// can never masquerade as a full-table scan; peek (WHERE 1=0) likewise
// outranks full. The removed spoolSpacePreflight queried pg_class once
// per table, i.e. it regresses this tally deterministically.
type m1QCounts struct{ full, meta, peek, other int64 }

func (m *m1QCounts) tally(q string) {
	var slot *int64
	switch {
	case strings.Contains(q, "pg_catalog") || strings.Contains(q, "information_schema"):
		slot = &m.meta
	case strings.Contains(q, "WHERE 1=0"):
		slot = &m.peek
	case strings.Contains(q, "SELECT"):
		slot = &m.full
	default:
		slot = &m.other
	}
	atomic.AddInt64(slot, 1)
}

func (m *m1QCounts) snapshot() (full, meta, peek, other int64) {
	return atomic.LoadInt64(&m.full),
		atomic.LoadInt64(&m.meta),
		atomic.LoadInt64(&m.peek),
		atomic.LoadInt64(&m.other)
}

type m1Rows struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *m1Rows) Columns() []string { return r.cols }
func (r *m1Rows) Close() error      { return nil }
func (r *m1Rows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

// m1ErrRows serves its rows then reports a mid-iteration deadline — the
// shape a real driver surfaces through sql.Rows.Err() after the Next()
// loop drains (mirrors iterErrRows in query_timeout_miditer_test.go).
type m1ErrRows struct {
	m1Rows
	err error
}

func (r *m1ErrRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return r.err
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

type m1Connector struct{ c *m1Conn }

func (f m1Connector) Connect(context.Context) (driver.Conn, error) { return f.c, nil }
func (f m1Connector) Driver() driver.Driver                        { return m1Driver{} }

type m1Driver struct{}

func (m1Driver) Open(string) (driver.Conn, error) { return nil, fmt.Errorf("unused") }

// rows2hashes normalizes scripted rows exactly like the scan loops do and
// hashes them with the production functions (no trim columns here — the
// scripted types carry no text type names).
func rows2hashes(rows [][]driver.Value) []string {
	pgCols := []colMapping{{pgIdx: 0, name: "a"}, {pgIdx: 1, name: "b"}}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		row := []string{normalizeValue(r[0]), normalizeValue(r[1])}
		out = append(out, computeRowHash(row, pgCols))
	}
	return out
}

// oracleHashGroup re-runs the LEGACY map-based verdict (message formats
// included; details sorted by hash to match the new deterministic order —
// the legacy detail order was Go-map random).
func oracleHashGroup(pgRows, tidbRows [][]driver.Value, label string) (string, string, string) {
	pgHashes := rows2hashes(pgRows)
	tidbHashes := rows2hashes(tidbRows)
	pgCounts := map[string]int{}
	for _, h := range pgHashes {
		pgCounts[h]++
	}
	tidbCounts := map[string]int{}
	for _, h := range tidbHashes {
		tidbCounts[h]++
	}
	set := map[string]bool{}
	for h := range pgCounts {
		set[h] = true
	}
	for h := range tidbCounts {
		set[h] = true
	}
	all := make([]string, 0, len(set))
	for h := range set {
		all = append(all, h)
	}
	sort.Strings(all)
	var details []string
	for _, h := range all {
		pc, tc := pgCounts[h], tidbCounts[h]
		if pc == tc {
			continue
		}
		switch {
		case tc == 0:
			details = append(details, fmt.Sprintf("%s has %d row(s) with hash %s not found in TiDB", label, pc, truncate(h, 16)))
		case pc == 0:
			details = append(details, fmt.Sprintf("TiDB has %d row(s) with hash %s not found in %s", tc, truncate(h, 16), label))
		default:
			details = append(details, fmt.Sprintf("hash %s: %s count=%d TiDB count=%d", truncate(h, 16), label, pc, tc))
		}
	}
	status, errMsg, sugg := "pass", "", ""
	if len(details) > 0 {
		status = "fail"
		if len(details) > 10 {
			details = details[:10]
		}
		errMsg = fmt.Sprintf("hash group mismatch: %s", strings.Join(details, "; "))
	}
	sugg = fmt.Sprintf("hash group validation: %d %s hashes vs %d TiDB rows, %d mismatches",
		len(pgCounts), label, len(tidbHashes), len(details))
	return status, errMsg, sugg
}

func oracleAggregate(pgRows, tidbRows [][]driver.Value, label string) (string, string, string) {
	pg := rows2hashes(pgRows)
	tidb := rows2hashes(tidbRows)
	sort.Strings(pg)
	sort.Strings(tidb)
	pa := fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(pg, ","))))
	ta := fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(tidb, ","))))
	status, errMsg := "pass", ""
	if pa != ta {
		status = "fail"
		errMsg = fmt.Sprintf("aggregate hash mismatch: src=%s tidb=%s", truncate(pa, 16), truncate(ta, 16))
	}
	sugg := fmt.Sprintf("aggregate hash validation: src_hash=%s tidb_hash=%s, %d %s rows vs %d TiDB rows",
		truncate(pa, 16), truncate(ta, 16), len(pg), label, len(tidb))
	return status, errMsg, sugg
}

// oracleBucket re-runs the LEGACY bucket verdict with its true output
// rules: buckets visited in bucket order; form 1 for count mismatches;
// form 2 (equal totals, differing multisets) shows ONE differing hash —
// legacy picked a map-random one, normalized here (and by the new code)
// to the LOWEST differing hash in the bucket; the detail list caps at 5
// with NO "and N more" suffix (that branch was unreachable dead code in
// the legacy implementation — len(details) ≤ 5 by construction — and
// must stay unreproduced; review 阻断2).
func oracleBucket(pgRows, tidbRows [][]driver.Value, bucketCount int, label string) (string, string, string) {
	pg := rows2hashes(pgRows)
	tidb := rows2hashes(tidbRows)
	pc := map[string]int{}
	for _, h := range pg {
		pc[h]++
	}
	tc := map[string]int{}
	for _, h := range tidb {
		tc[h]++
	}
	pgTotals := make([]int64, bucketCount)
	tidbTotals := make([]int64, bucketCount)
	for _, h := range pg {
		pgTotals[bucketOf(h, bucketCount)]++
	}
	for _, h := range tidb {
		tidbTotals[bucketOf(h, bucketCount)]++
	}
	set := map[string]bool{}
	for h := range pc {
		set[h] = true
	}
	for h := range tc {
		set[h] = true
	}
	all := make([]string, 0, len(set))
	for h := range set {
		all = append(all, h)
	}
	sort.Strings(all)

	var details []string
	mismatched := 0
	for i := 0; i < bucketCount; i++ {
		if pgTotals[i] != tidbTotals[i] {
			mismatched++
			if len(details) < 5 {
				details = append(details, fmt.Sprintf("bucket %d: %s=%d rows TiDB=%d rows", i, label, pgTotals[i], tidbTotals[i]))
			}
			continue
		}
		for _, h := range all { // ascending: first hit = lowest differing hash
			if bucketOf(h, bucketCount) == i && pc[h] != tc[h] {
				mismatched++
				if len(details) < 5 {
					details = append(details, fmt.Sprintf("bucket %d: hash %s count differs by %d", i, truncate(h, 16), pc[h]-tc[h]))
				}
				break
			}
		}
	}
	status, errMsg, sugg := "pass", "", ""
	if mismatched > 0 {
		status = "fail"
		errMsg = fmt.Sprintf("bucket mismatch (%d/%d buckets): %s", mismatched, bucketCount, strings.Join(details, "; "))
	}
	sugg = fmt.Sprintf("bucket validation: %d buckets, %d mismatched, %d %s rows", bucketCount, mismatched, len(pg), label)
	return status, errMsg, sugg
}

func runStrategy(t *testing.T, strategy string, pgRowsData, tidbRowsData [][]driver.Value, spoolRoot string, cmp config.CompareConfig, poison string) reporter.TableReport {
	t.Helper()
	conn := &m1Conn{pg: true, cols: []string{"a", "b"}, pgData: pgRowsData, tidbData: tidbRowsData, poisonPG: poison == "pg"}
	tgt := &m1Conn{pg: false, cols: []string{"a", "b"}, pgData: pgRowsData, tidbData: tidbRowsData, poisonTiDB: poison == "tidb"}
	pgDB := sql.OpenDB(m1Connector{conn})
	defer pgDB.Close()
	tidbDB := sql.OpenDB(m1Connector{tgt})
	defer tidbDB.Close()
	tctx := context.Background()
	tidbConn, err := tidbDB.Conn(tctx)
	if err != nil {
		t.Fatalf("tidb conn: %v", err)
	}
	defer tidbConn.Close()

	v := NewValidator(config.Config{Compare: cmp})
	if spoolRoot != "" {
		v.spoolRoot = spoolRoot
	}
	tr := reporter.TableReport{TableName: "t1", SourceRows: int64(len(pgRowsData))}
	switch strategy {
	case "hash_group":
		return v.validateSamplingWithHashGroup(tctx, pgDB, tidbConn, "t1", 1.0, tr, "public")
	case "aggregate":
		return v.validateNoPKWithAggregate(tctx, pgDB, tidbConn, "t1", tr, "public")
	case "bucket":
		return v.validateNoPKWithBucket(tctx, pgDB, tidbConn, "t1", tr, "public")
	}
	t.Fatalf("unknown strategy %q", strategy)
	return tr
}

// TestM1ThreePathsOracleEquivalence is the M1 red-line differential: for a
// matrix of diff-shaped datasets (equal, src-extra, tidb-extra, count
// swaps, NULLs, multibyte) each strategy's verdict must byte-match the
// legacy-map oracle (details in ascending-hash order — deterministic
// normalization of the legacy map-random order). Small datasets never
// spill → Suggestions match exactly; the big spilled dataset matches on
// prefix + spool disclosure.
func TestM1ThreePathsOracleEquivalence(t *testing.T) {
	base := func(n int) [][]driver.Value {
		out := make([][]driver.Value, n)
		for i := range out {
			out[i] = []driver.Value{int64(i), fmt.Sprintf("名前-%d", i)}
		}
		return out
	}

	// sameBucketPair finds two row values whose hashes share a bucket —
	// drives the bucket form-2 (equal totals, multiset diff).
	sameBucketPair := func() (string, string) {
		seen := map[int]string{}
		for i := 0; i < 20000; i++ {
			s := fmt.Sprintf("cand-%d", i)
			probe := []string{s, "\\N"}
			hh := computeRowHash(probe, []colMapping{{pgIdx: 0, name: "a"}, {pgIdx: 1, name: "b"}})
			b := bucketOf(hh, 100)
			if prev, ok := seen[b]; ok && prev != s {
				return prev, s
			}
			seen[b] = s
		}
		return "cand-0", "cand-1"
	}

	x, y := sameBucketPair()

	cases := []struct {
		name          string
		pg, tidb      [][]driver.Value
		wantSpillHint bool
	}{
		{"equal-small", base(50), base(50), false},
		{"src-extra", append(base(30), []driver.Value{int64(999), "extra"}), base(30), false},
		{"tidb-extra", base(30), append(base(30), []driver.Value{int64(998), "extra2"}, []driver.Value{int64(997), "extra3"}), false},
		{"null-and-mixed", [][]driver.Value{{nil, "中文"}, {"", nil}, {int64(1), "a"}, {nil, "\\N"}}, [][]driver.Value{{nil, "中文"}, {"", nil}, {int64(1), "a"}}, false},
		{"dup-counts", append(base(20), base(20)[3], base(20)[3], base(20)[3]), append(base(20), base(20)[3]), false},
		{"bucket-form2-swap", append(base(40), []driver.Value{x, "\\N"}), append(base(40), []driver.Value{y, "\\N"}), false},
		{"big-spilled", base(70_000), base(70_000), true},
	}
	for _, tc := range cases {
		for _, strategy := range []string{"hash_group", "aggregate", "bucket"} {
			t.Run(tc.name+"-"+strategy, func(t *testing.T) {
				tr := runStrategy(t, strategy, tc.pg, tc.tidb, t.TempDir(), config.CompareConfig{}, "")
				var wantStatus, wantErr, wantSugg string
				switch strategy {
				case "hash_group":
					wantStatus, wantErr, wantSugg = oracleHashGroup(tc.pg, tc.tidb, "PG")
				case "aggregate":
					wantStatus, wantErr, wantSugg = oracleAggregate(tc.pg, tc.tidb, "PG")
				case "bucket":
					wantStatus, wantErr, wantSugg = oracleBucket(tc.pg, tc.tidb, 100, "PG")
				}
				if string(tr.Status) != wantStatus {
					t.Fatalf("status = %s, oracle %s (err=%q)", tr.Status, wantStatus, tr.Error)
				}
				if tr.Error != wantErr {
					t.Fatalf("error mismatch:\n got: %q\nwant: %q", tr.Error, wantErr)
				}
				if !tc.wantSpillHint {
					if tr.Suggestion != wantSugg {
						t.Fatalf("suggestion mismatch:\n got: %q\nwant: %q", tr.Suggestion, wantSugg)
					}
					return
				}
				// 70k distinct rows cross the 64K-entry threshold on the
				// src side: verdict/messages unchanged, suggestion gains
				// the spool disclosure.
				if !strings.HasPrefix(tr.Suggestion, wantSugg) {
					t.Fatalf("spilled suggestion lost its prefix:\n got: %q\nwant prefix: %q", tr.Suggestion, wantSugg)
				}
				if !strings.Contains(tr.Suggestion, "row-hash spool") {
					t.Fatalf("spilled suggestion missing spool disclosure: %q", tr.Suggestion)
				}
			})
		}
	}
}

// TestM1HashGroupAllDiffSpilledDetailsBounded (review 阻断1 anchor): a
// fully-differing spilled table (70k+70k disjoint distinct rows → 140k
// mismatches) renders EXACTLY 10 details and reports the legacy
// post-truncation count — the walk-time accumulator no longer grows with
// the mismatch count.
func TestM1HashGroupAllDiffSpilledDetailsBounded(t *testing.T) {
	pg := make([][]driver.Value, 70_000)
	tidb := make([][]driver.Value, 70_000)
	for i := range pg {
		pg[i] = []driver.Value{int64(i), fmt.Sprintf("pg-%d", i)}
		tidb[i] = []driver.Value{int64(i), fmt.Sprintf("tidb-%d", i)}
	}
	tr := runStrategy(t, "hash_group", pg, tidb, t.TempDir(), config.CompareConfig{}, "")
	if tr.Status != reporter.StatusFail {
		t.Fatalf("status = %s, want fail (err=%q)", tr.Status, tr.Error)
	}
	if got := strings.Count(tr.Error, "; ") + 1; got != hashGroupMaxDetails {
		t.Fatalf("rendered details = %d, want %d (err=%q)", got, hashGroupMaxDetails, tr.Error)
	}
	if !strings.Contains(tr.Suggestion, fmt.Sprintf(", %d mismatches", hashGroupMaxDetails)) {
		t.Fatalf("suggestion must report the legacy capped count %d: %q", hashGroupMaxDetails, tr.Suggestion)
	}
	if !strings.Contains(tr.Suggestion, "row-hash spool") {
		t.Fatalf("suggestion must disclose the spill: %q", tr.Suggestion)
	}
	// Both sides walk ascending → the first 10 rendered details are
	// exactly the oracle's first 10: message equality holds.
	_, wantErr, _ := oracleHashGroup(pg, tidb, "PG")
	if tr.Error != wantErr {
		t.Fatalf("error mismatch:\n got: %q\nwant: %q", tr.Error, wantErr)
	}
}

// TestM1BucketDetailCapLegacyShape (review 阻断2 anchor): with EVERY bucket
// mismatched (>5), the message keeps the legacy shape — at most 5 bucket
// details and NO "and N more mismatched buckets" suffix — for both the
// count-mismatch (form 1) and equal-count/multiset (form 2) variants.
func TestM1BucketDetailCapLegacyShape(t *testing.T) {
	const bc = 8
	rowsForBucket := func(prefix string, b, n int) [][]driver.Value {
		var out [][]driver.Value
		for i := 0; len(out) < n && i < 5_000_000; i++ {
			s := fmt.Sprintf("%s-%d", prefix, i)
			probe := []string{s, "\\N"}
			h := computeRowHash(probe, []colMapping{{pgIdx: 0, name: "a"}, {pgIdx: 1, name: "b"}})
			if bucketOf(h, bc) == b {
				out = append(out, []driver.Value{s, "\\N"})
			}
		}
		if len(out) < n {
			panic("bucket search exhausted") // ~n*bc tries; unreachable in practice
		}
		return out
	}
	for _, form := range []string{"form1-count-mismatch", "form2-multiset"} {
		t.Run(form, func(t *testing.T) {
			var pg, tidb [][]driver.Value
			pgN, tidbN := 1, 1
			if form == "form1-count-mismatch" {
				pgN, tidbN = 2, 1
			}
			for b := 0; b < bc; b++ {
				pg = append(pg, rowsForBucket("pgb", b, pgN)...)
				tidb = append(tidb, rowsForBucket("tbb", b, tidbN)...)
			}
			tr := runStrategy(t, "bucket", pg, tidb, "", config.CompareConfig{NoPKBucketCount: bc}, "")
			if tr.Status != reporter.StatusFail {
				t.Fatalf("status = %s, want fail (err=%q)", tr.Status, tr.Error)
			}
			if strings.Contains(tr.Error, "more mismatched buckets") {
				t.Fatalf("legacy-dead suffix must NOT appear: %q", tr.Error)
			}
			if got := strings.Count(tr.Error, "; ") + 1; got != 5 {
				t.Fatalf("rendered bucket details = %d, want 5 (err=%q)", got, tr.Error)
			}
			if !strings.Contains(tr.Error, fmt.Sprintf("(%d/%d buckets)", bc, bc)) {
				t.Fatalf("message must lead with the full mismatched count: %q", tr.Error)
			}
			wantStatus, wantErr, _ := oracleBucket(pg, tidb, bc, "PG")
			if string(tr.Status) != wantStatus || tr.Error != wantErr {
				t.Fatalf("oracle mismatch:\n got: %s %q\nwant: %s %q", tr.Status, tr.Error, wantStatus, wantErr)
			}
		})
	}
}

// TestM1MidIterationTimeoutFailsLoud (review 高风险3 anchor): a scan that
// dies mid-iteration (driver deadline) must fail the table with the
// "query timeout" disclosure on all three strategies, both sides — with
// the spool as the only copy of the side's hashes, a silently truncated
// scan could otherwise fake a PASS.
func TestM1MidIterationTimeoutFailsLoud(t *testing.T) {
	prefixes := map[string]string{
		"hash_group": "hash group",
		"aggregate":  "aggregate hash",
		"bucket":     "bucket compare",
	}
	for _, strategy := range []string{"hash_group", "aggregate", "bucket"} {
		for _, side := range []string{"pg", "tidb"} {
			t.Run(strategy+"-"+side, func(t *testing.T) {
				data := genSpillRows(30)
				tr := runStrategy(t, strategy, data, data, t.TempDir(), config.CompareConfig{}, side)
				if tr.Status != reporter.StatusFail {
					t.Fatalf("mid-iteration deadline must fail the table, got %s (err=%q)", tr.Status, tr.Error)
				}
				if !strings.Contains(tr.Error, "query timeout") {
					t.Fatalf("err must disclose 'query timeout', got %q", tr.Error)
				}
				scan := "scan PG"
				if side == "tidb" {
					scan = "scan TiDB"
				}
				if !strings.Contains(tr.Error, scan) {
					t.Fatalf("err must attribute the failed scan (%s), got %q", scan, tr.Error)
				}
				if !strings.Contains(tr.Error, prefixes[strategy]) {
					t.Fatalf("err must carry the %q prefix, got %q", prefixes[strategy], tr.Error)
				}
			})
		}
	}
}

// TestM1NoPKConcurrencyEightTables (positive-review r2 blocker anchor,
// reworked per red-team r3 — counting, not timing): 8 tables validated
// CONCURRENTLY against one PG pool capped at 8 conns (the MaxConcBudget
// ceiling) must all pass, AND the query tally must prove frugality:
//
//	PG side (shared):   full=8  meta=0  peek=0  other=0
//	TiDB side (/table): full=1  meta=0  peek=1  other=0
//
// The fixture's connector returns the same conn instance with instant
// queries, so pool starvation is structurally invisible here (red-team
// r3 finding) — the COUNTS are the probative part: a re-introduced
// per-table space preflight issues a pg_catalog query per table and
// flips PG meta 0→8 even though nothing blocks or starves. The tally
// classification checks meta before full, so a catalog pre-scan cannot
// masquerade as a scan. Mutation-verified: injecting the old
// "SELECT ... FROM pg_catalog.pg_class" pre-scan into
// validateSamplingWithHashGroup fails this test on the meta count.
func TestM1NoPKConcurrencyEightTables(t *testing.T) {
	const tables = 8
	data := genSpillRows(50)

	pgCounts := &m1QCounts{}
	pgDB := sql.OpenDB(m1Connector{&m1Conn{pg: true, cols: []string{"a", "b"}, pgData: data, counts: pgCounts}})
	defer pgDB.Close()
	pgDB.SetMaxOpenConns(tables) // pool ceiling = budget ceiling (Run shape)

	v := NewValidator(config.Config{Compare: config.CompareConfig{
		Concurrency:  tables,
		QueryTimeout: "5s", // a starved table would surface as this deadline
	}})
	v.spoolRoot = t.TempDir()

	results := make([]reporter.TableReport, tables)
	tidbCounts := make([]m1QCounts, tables)
	var wg sync.WaitGroup
	for i := 0; i < tables; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tidbDB := sql.OpenDB(m1Connector{&m1Conn{cols: []string{"a", "b"}, tidbData: data, counts: &tidbCounts[i]}})
			defer tidbDB.Close()
			tidbConn, err := tidbDB.Conn(context.Background())
			if err != nil {
				results[i] = reporter.TableReport{Status: reporter.StatusFail, Error: err.Error()}
				return
			}
			defer tidbConn.Close()
			results[i] = v.validateSamplingWithHashGroup(context.Background(), pgDB, tidbConn,
				fmt.Sprintf("t%d", i), 1.0, reporter.TableReport{TableName: fmt.Sprintf("t%d", i)}, "public")
		}(i)
	}
	wg.Wait()

	for i, tr := range results {
		if tr.Status != reporter.StatusPass {
			t.Fatalf("table %d = %s (%q) — connection starvation/deadline poisoning regressed", i, tr.Status, tr.Error)
		}
		full, meta, peek, other := tidbCounts[i].snapshot()
		if full != 1 || meta != 0 || peek != 1 || other != 0 {
			t.Fatalf("table %d TiDB tally = full:%d meta:%d peek:%d other:%d — want full:1 peek:1 (one zero-row column probe then one scan) and nothing else", i, full, meta, peek, other)
		}
	}
	full, meta, peek, other := pgCounts.snapshot()
	if full != tables || meta != 0 || peek != 0 || other != 0 {
		t.Fatalf("PG tally = full:%d meta:%d peek:%d other:%d — want full:%d only: exactly one full scan per table and ZERO auxiliary queries (a reintroduced space preflight adds one pg_catalog query per table)", full, meta, peek, other, tables)
	}
}

// TestM1SpoolRootFailLoudThroughPath: a broken spool root fails the table
// explicitly with the spool in the message (never a silent RAM fallback).
func TestM1SpoolRootFailLoudThroughPath(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Direct spool-level failure surfaces through the hash_group path:
	// newSideSpool targets the file path; 70k distinct rows force the
	// default-limit flush, whose CreateTemp fails against a file path.
	v := NewValidator(config.Config{})
	v.spoolRoot = notADir
	conn := &m1Conn{pg: true, cols: []string{"a", "b"},
		pgData:   genSpillRows(70_000),
		tidbData: genSpillRows(70_000)}
	pgDB := sql.OpenDB(m1Connector{conn})
	defer pgDB.Close()
	tidbDB := sql.OpenDB(m1Connector{&m1Conn{cols: []string{"a", "b"}, tidbData: genSpillRows(70_000)}})
	defer tidbDB.Close()
	tctx := context.Background()
	tidbConn, err := tidbDB.Conn(tctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tidbConn.Close()
	out := v.validateSamplingWithHashGroup(tctx, pgDB, tidbConn, "t1", 1.0, reporter.TableReport{TableName: "t1"}, "public")
	if out.Status != reporter.StatusFail {
		t.Fatalf("broken spool must fail the table, got %s (%q)", out.Status, out.Error)
	}
	if !strings.Contains(out.Error, "row-hash spool") {
		t.Fatalf("error must mention spool, got %q", out.Error)
	}
}

// TestRunSpoolRootLifecycle: Run() prepares the caller-passed spool root
// and removes it when the run settles — error endings included.
func TestRunSpoolRootLifecycle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool", "task01")
	v := NewValidator(config.Config{})
	// Unroutable DSN: the run fails fast at connect; the deferred spool
	// cleanup must still fire.
	_, err := v.RunWithDSNs(context.Background(), "postgresql://127.0.0.1:1/nope", "root:@tcp(127.0.0.1:1)/nope", "public",
		config.CompareConfig{}, 0, common.ValidateOpts{Mode: "quick", SpoolRoot: root})
	if err == nil {
		t.Fatal("expected connect failure")
	}
	if _, serr := os.Stat(root); !os.IsNotExist(serr) {
		t.Fatalf("spool root survived a failed run: %v", serr)
	}
}

func genSpillRows(n int) [][]driver.Value {
	out := make([][]driver.Value, n)
	for i := range out {
		out[i] = []driver.Value{int64(i), fmt.Sprintf("v-%d", i)}
	}
	return out
}

// TestWalkHashCountersOracle locks the lockstep walker to the map-diff
// oracle across shapes (a-only, b-only, both, equal counts).
func TestWalkHashCountersOracle(t *testing.T) {
	a := genHashes(400, 3)
	b := genHashes(380, 4)
	b = append(b, a[:50]...) // shared prefix hashes

	sa := newRowHashSpool(t.TempDir(), 60)
	sb := newRowHashSpool(t.TempDir(), 60)
	for _, h := range a {
		sa.Add(h)
	}
	for _, h := range b {
		sb.Add(h)
	}
	ia, _ := sa.Finish()
	ib, _ := sb.Finish()
	defer ia.Close()
	defer ib.Close()
	defer sa.Close()
	defer sb.Close()

	ca, cb := map[string]int{}, map[string]int{}
	for _, h := range a {
		ca[h]++
	}
	for _, h := range b {
		cb[h]++
	}
	wantDiffs := map[string][2]int{}
	for h := range ca {
		if ca[h] != cb[h] {
			wantDiffs[h] = [2]int{ca[h], cb[h]}
		}
	}
	for h := range cb {
		if _, ok := ca[h]; !ok {
			wantDiffs[h] = [2]int{0, cb[h]}
		}
	}

	got := map[string][2]int{}
	if err := walkHashCounters(ia, ib, func(h string, x, y int) {
		if x != y {
			got[h] = [2]int{x, y}
		}
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != len(wantDiffs) {
		t.Fatalf("diff count %d, oracle %d", len(got), len(wantDiffs))
	}
	for h, d := range wantDiffs {
		if got[h] != d {
			t.Fatalf("hash %s: got %+v want %+v", h, got[h], d)
		}
	}
}
