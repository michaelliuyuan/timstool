package validator

// count_chunked_test.go anchors the BUG-CMP-COUNT-P1 chunked count engine
// (estimate-driven routing + keyset chunk COUNTs):
//
//	A1  estimate ≥ threshold → chunked path engages (probes + chunk COUNTs,
//	    no whole-table COUNT).
//	A2  estimate < threshold → legacy whole-table path byte-identical (one
//	    COUNT per side, exact legacy query text, no Suggestion).
//	A3  both estimates FAIL → conservative chunked routing (probes run).
//	A4  three windows + tail sum to the whole-table count (对拍), with
//	    per-chunk progress events (idx, total) fired in order.
//	A5  varchar keys flow through boundaries and re-bind as args.
//	A6  composite PK renders OR-chain predicates with continuous PG $N.
//	A7  watermark predicate + chunk range compose on BOTH sides with the
//	    same bound values (single-value and range forms).
//	A8  no real PK → whole-table fallback + 「无真主键无法分块」disclosure.
//	A9  a chunk COUNT that exhausts its retries fails the table with the
//	    unchanged "source count: " shape plus the range disclosure.
//	A10 count mismatch keeps the legacy "row count mismatch:" string and
//	    localizes the first 3 differing chunks in Suggestion.
//	A11 chunked_count_threshold: 0 disables the engine (zero regression:
//	    no estimate probes at all).
//	A12 negative threshold clamps to the 5M default (typo defense).
//	A13 chunk diffs that cancel out keep the PASS verdict but disclose.
//	A14 boundary-scan failure → "source count: chunk boundary scan:" shape.
//	A15 NULL PK value → whole-table fallback + disclosure.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// --- purpose-built fake driver for the chunked count engine ---

// chunkScript scripts ONE side of the chunked count engine. The dataset's
// KEY columns come first (in pkCols order); every probe window and every
// range COUNT is served by filtering that dataset, so chunk sums equal
// the whole-table count by construction (the 对拍 property under test).
type chunkScript struct {
	pkCols    []string // served via table_constraints (source key detection)
	uniqueDef string   // served via pg_indexes when pkCols is empty
	keyTypes  []string // per key column: int8 | text

	estRows   int64  // pg_stat_user_tables n_live_tup (source only)
	estErr    error  // estimate query failure (source only)
	tgtEst    string // SHOW TABLE STATUS Rows column (either side)
	tgtEstErr error  // SHOW TABLE STATUS query failure

	// A7 SHOW TABLE STATUS shape knobs (default = real 18-column shape):
	showCols17 bool // serve the 17-column 5.7 shape (no Data_free column)
	tgtEstNull bool // serve NULL in the Rows column
	noRowsCol  bool // serve a shape with NO "Rows" column at all
	showEmpty  bool // serve zero result rows

	data [][]driver.Value

	wmArgCount int // watermark args the queries carry (tests set this)
	targetHint int // chunk target the engine runs with (ordinal math)

	// countFailN fails the FIRST N chunk/whole COUNT queries with
	// countFailErr (retry machinery sees them all).
	countFailN   int
	countFailErr error

	// probeErr fails every keyset boundary probe (A14).
	probeErr error

	// shortChunk/shortN subtract shortN rows from the bounded chunk whose
	// ordinal is shortChunk (drives mismatch/cancel-out anchors per side).
	shortChunk int
	shortN     int64

	mu       sync.Mutex
	queries  []string
	argLog   [][]driver.Value
	countLog []string
	attempts int
}

func (s *chunkScript) log(q string, args []driver.NamedValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
	logged := make([]driver.Value, len(args))
	for i, a := range args {
		logged[i] = a.Value
	}
	s.argLog = append(s.argLog, logged)
}

func (s *chunkScript) respond(q string, args []driver.NamedValue) (driver.Rows, error) {
	s.log(q, args)
	switch {
	case strings.Contains(q, "pg_stat_user_tables"):
		if s.estErr != nil {
			return nil, s.estErr
		}
		return &scriptedRows{cols: []string{"n_live_tup"}, colTypes: []string{"int8"},
			rows: [][]driver.Value{{s.estRows}}}, nil
	case strings.Contains(q, "SHOW TABLE STATUS"):
		if s.tgtEstErr != nil {
			return nil, s.tgtEstErr
		}
		// Real 18-column shape (MySQL 8.x / TiDB measured on 103); Rows
		// stays at index 4 and Data_free sits at index 9.
		cols := []string{"Name", "Engine", "Version", "Row_format", "Rows", "Avg_row_length",
			"Data_length", "Max_data_length", "Index_length", "Data_free", "Auto_increment",
			"Create_time", "Update_time", "Check_time", "Collation", "Checksum", "Create_options", "Comment"}
		if s.showCols17 {
			cols = append(cols[:9:9], cols[10:]...) // 5.7-era shape: drop Data_free
		}
		if s.noRowsCol {
			filtered := cols[:0:0]
			for _, c := range cols {
				if c != "Rows" {
					filtered = append(filtered, c)
				}
			}
			cols = filtered
		}
		var data [][]driver.Value
		if !s.showEmpty {
			vals := make([]driver.Value, len(cols))
			for i := range vals {
				vals[i] = "x"
			}
			for i, c := range cols {
				switch c {
				case "Name":
					vals[i] = "t1"
				case "Rows":
					if s.tgtEstNull {
						vals[i] = nil
					} else {
						vals[i] = s.tgtEst
					}
				}
			}
			data = [][]driver.Value{vals}
		}
		return &scriptedRows{cols: cols, colTypes: []string{"text"}, rows: data}, nil
	case strings.Contains(q, "information_schema.statistics"):
		// mysql-source key detection: one row per PK column, PRIMARY +
		// non_unique=0 (dialect_mysql.go DetectTableKey).
		rows := make([][]driver.Value, 0, len(s.pkCols))
		for _, c := range s.pkCols {
			rows = append(rows, []driver.Value{"PRIMARY", c, int64(0)})
		}
		return &scriptedRows{cols: []string{"index_name", "column_name", "non_unique"},
			colTypes: []string{"text"}, rows: rows}, nil
	case strings.Contains(q, "table_constraints"):
		rows := make([][]driver.Value, 0, len(s.pkCols))
		for _, c := range s.pkCols {
			rows = append(rows, []driver.Value{c})
		}
		return &scriptedRows{cols: []string{"column_name"}, colTypes: []string{"text"}, rows: rows}, nil
	case strings.Contains(q, "pg_indexes"):
		var rows [][]driver.Value
		if s.uniqueDef != "" {
			rows = [][]driver.Value{{s.uniqueDef}}
		}
		return &scriptedRows{cols: []string{"indexdef"}, colTypes: []string{"text"}, rows: rows}, nil
	case strings.Contains(q, "COUNT(*)"):
		s.mu.Lock()
		s.attempts++
		n := s.attempts
		s.countLog = append(s.countLog, q)
		s.mu.Unlock()
		if n <= s.countFailN && s.countFailErr != nil {
			return nil, s.countFailErr
		}
		count := s.rangeCount(q, args)
		return &scriptedRows{cols: []string{"count"}, colTypes: []string{"int8"},
			rows: [][]driver.Value{{count}}}, nil
	case strings.Contains(q, "ORDER BY"):
		return s.respondProbe(q, args)
	}
	return nil, fmt.Errorf("unexpected query: %s", q)
}

// rangeCount serves a COUNT query: len(args) == wmArgCount is the legacy
// whole-table COUNT (the whole dataset); otherwise the (lower, upper]
// chunk bounds are recovered from the args using the engine's exact
// placeholder layout — single key: [lower?] [upper?] in order; composite
// n=2: lower chain [l1 l1 l2]? then upper chain [u1 u1 u2]?.
func (s *chunkScript) rangeCount(q string, args []driver.NamedValue) int64 {
	nk := len(s.pkCols)
	rest := make([]driver.Value, 0, len(args))
	for _, a := range args {
		rest = append(rest, a.Value)
	}
	if len(rest) <= s.wmArgCount {
		return int64(len(s.data)) // whole-table COUNT
	}
	rest = rest[s.wmArgCount:]

	hasLower := strings.Contains(q, " > ")
	var lower, upper []driver.Value
	if nk == 1 {
		// Arg-count driven (the wm single-value form also contains
		// " <= ", so text alone cannot identify the bounds).
		switch len(rest) {
		case 2:
			lower, upper = rest[0:1], rest[1:2]
		case 1:
			if hasLower {
				lower = rest[0:1]
			} else {
				upper = rest[0:1]
			}
		}
	} else {
		off := 0
		if len(rest) >= 3 && hasLower {
			lower = []driver.Value{rest[0], rest[2]}
			off = 3
		}
		if len(rest) >= off+3 {
			upper = []driver.Value{rest[off], rest[off+2]}
		}
	}

	count := int64(0)
	for _, row := range s.data {
		if lower != nil && !keyTupleGreater(row, idxRange(nk), lower) {
			continue
		}
		if upper != nil && !keyTupleLessEq(row, idxRange(nk), upper) {
			continue
		}
		count++
	}
	if s.chunkOrdinal(hasLower, lower, nk) == s.shortChunk {
		count -= s.shortN
		if count < 0 {
			count = 0
		}
	}
	return count
}

// chunkOrdinal recovers the running chunk ordinal: 0 for the first chunk
// (no lower bound), -2 for the tail (no upper bound — not shortable in
// these anchors), otherwise #dataset-keys ≤ lower divided by the window
// target (uniform windows).
func (s *chunkScript) chunkOrdinal(hasLower bool, lower []driver.Value, nk int) int {
	if !hasLower {
		return 0
	}
	// Tail = lower present but no upper: detectable by the caller's arg
	// layout (single bound) — approximated as -2 when only one bound was
	// parsed AND the query lacks " <= " on a KEY column. For the uniform
	// anchors here the bounded ordinal suffices; the tail sentinel is
	// conservative.
	le := 0
	for _, row := range s.data {
		if !keyTupleGreater(row, idxRange(nk), lower) {
			le++
		}
	}
	target := s.targetHint
	if target <= 0 {
		target = 1
	}
	return le / target
}

// idxRange is the identity key-column index list [0..n-1] (key columns
// are always the first dataset columns in this script).
func idxRange(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = maxInt // kept for potential ordinal math extensions

// keyTupleLessEq reports row[keyIdxs] <= tuple lexicographically
// (inclusive — the chunk upper bound).
func keyTupleLessEq(row []driver.Value, keyIdxs []int, tuple []driver.Value) bool {
	for i := range keyIdxs {
		c := compareDriverVals(row[keyIdxs[i]], tuple[i])
		if c != 0 {
			return c < 0
		}
	}
	return true
}

// respondProbe serves the key-only keyset window: SELECT keys [WHERE wm
// AND (seek)] ORDER BY keys LIMIT n — filtered by the seek tuple (the
// LAST nk args; PG form binds one arg per key column) and projected to
// the key columns.
func (s *chunkScript) respondProbe(q string, args []driver.NamedValue) (driver.Rows, error) {
	if s.probeErr != nil {
		return nil, s.probeErr
	}
	nk := len(s.pkCols)
	var limit int
	if m := limitOnlyRe.FindStringSubmatch(q); m != nil {
		fmt.Sscanf(m[1], "%d", &limit)
	}
	var seekTuple []driver.Value
	if len(args) > s.wmArgCount {
		tail := args[len(args)-nk:]
		seekTuple = make([]driver.Value, nk)
		for i, a := range tail {
			seekTuple[i] = a.Value
		}
	}
	win := make([][]driver.Value, 0, limit)
	for _, row := range s.data {
		if seekTuple != nil && !keyTupleGreater(row, idxRange(nk), seekTuple) {
			continue
		}
		win = append(win, row[:nk])
		if len(win) >= limit {
			break
		}
	}
	return &scriptedRows{cols: s.pkCols, colTypes: s.keyTypes, rows: win}, nil
}

func (s *chunkScript) open() *sql.DB {
	return sql.OpenDB(&chunkConnector{script: s})
}

func (s *chunkScript) sawQueries(substr string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, q := range s.queries {
		if strings.Contains(q, substr) {
			out = append(out, q)
		}
	}
	return out
}

func (s *chunkScript) countQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.countLog...)
}

func (s *chunkScript) argsFor(substr string) [][]driver.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]driver.Value
	for i, q := range s.queries {
		if strings.Contains(q, substr) {
			out = append(out, s.argLog[i])
		}
	}
	return out
}

func (s *chunkScript) attemptsNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

type chunkConn struct {
	script *chunkScript
}

func (c *chunkConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *chunkConn) Close() error                        { return nil }
func (c *chunkConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *chunkConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.script.respond(q, args)
}
func (c *chunkConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

type chunkConnector struct {
	script *chunkScript
}

func (c *chunkConnector) Connect(context.Context) (driver.Conn, error) {
	return &chunkConn{script: c.script}, nil
}
func (c *chunkConnector) Driver() driver.Driver { return fakeDriver{} }

// --- helpers ---

func newChunkValidator(cc config.CompareConfig) *Validator {
	v := NewValidator(config.Config{Compare: cc})
	v.countRetryBackoffFn = zeroBackoff
	return v
}

// chunkHarness wires a default int-key source/target pair: PK id, dataset
// 1..7, estimates 6M (routing engages), chunk target 2 → boundaries
// [2 4 6 7] and 5 chunks (-inf,2] (2,4] (4,6] (6,7] (7,+inf).
func chunkHarness(cc config.CompareConfig) (src, dst *chunkScript, v *Validator) {
	src = &chunkScript{
		pkCols:     []string{"id"},
		keyTypes:   []string{"int8"},
		estRows:    6_000_000,
		data:       intData(7),
		targetHint: 2,
	}
	dst = &chunkScript{pkCols: []string{"id"}, keyTypes: []string{"int8"},
		tgtEst: "6000000", data: intData(7), targetHint: 2}
	v = newChunkValidator(cc)
	return src, dst, v
}

func intData(n int) [][]driver.Value {
	out := make([][]driver.Value, n)
	for i := 0; i < n; i++ {
		out[i] = []driver.Value{int64(i + 1)}
	}
	return out
}

func runChunkedCount(t *testing.T, v *Validator, src, dst *chunkScript) reporter.TableReport {
	t.Helper()
	pgDB := src.open()
	defer pgDB.Close()
	dstDB := dst.open()
	defer dstDB.Close()
	tidbConn, err := dstDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("get tidb conn: %v", err)
	}
	defer tidbConn.Close()
	return v.validateRowCount(context.Background(), pgDB, tidbConn, "t1")
}

func chunkedCfg() config.CompareConfig {
	return config.CompareConfig{ChunkedCountThreshold: 5_000_000, ChunkedCountTarget: 2}
}

// A1: estimate ≥ threshold → chunked path engages — probes ran, chunk
// COUNTs ran on both sides, NO whole-table COUNT, sums equal the dataset.
func TestChunkedCountRoutingLargeTable(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass {
		t.Fatalf("status=%s error=%q", tr.Status, tr.Error)
	}
	if tr.SourceRows != 7 || tr.TargetRows != 7 || tr.DiffRows != 0 {
		t.Fatalf("totals src=%d dst=%d diff=%d, want 7/7/0", tr.SourceRows, tr.TargetRows, tr.DiffRows)
	}
	// Probe = key-only keyset window: ORDER BY + LIMIT (the PK-metadata
	// probe also carries an ORDER BY and must not count).
	probes := 0
	for _, q := range src.sawQueries("ORDER BY") {
		if strings.Contains(q, "LIMIT") {
			probes++
		}
	}
	if probes != 4 {
		t.Fatalf("boundary probes = %d, want 4 (windows [1,2] [3,4] [5,6] [7])", probes)
	}
	srcCounts, dstCounts := src.countQueries(), dst.countQueries()
	if len(srcCounts) != 5 || len(dstCounts) != 5 {
		t.Fatalf("chunk COUNTs src=%d dst=%d, want 5/5 (4 ranges + tail)", len(srcCounts), len(dstCounts))
	}
	for _, q := range src.countQueries() {
		if !strings.Contains(q, " WHERE ") {
			t.Fatalf("whole-table COUNT issued on the chunked path: %s", q)
		}
	}
	if !strings.Contains(tr.Suggestion, "chunked count: 5 chunks") {
		t.Fatalf("Suggestion %q must disclose the chunked path", tr.Suggestion)
	}
}

// A2: estimate < threshold → legacy path byte-identical — one COUNT per
// side with the exact legacy query text, no Suggestion, no probes.
func TestChunkedCountRoutingSmallTable(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.estRows = 100
	dst.tgtEst = "100"
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 7 || tr.TargetRows != 7 {
		t.Fatalf("small table must count whole: %+v", tr)
	}
	if len(src.sawQueries("ORDER BY")) != 0 {
		t.Fatal("no boundary probes may run below the threshold")
	}
	srcCounts, dstCounts := src.countQueries(), dst.countQueries()
	if len(srcCounts) != 1 || len(dstCounts) != 1 {
		t.Fatalf("COUNTs src=%d dst=%d, want exactly 1/1 (legacy)", len(srcCounts), len(dstCounts))
	}
	if want := `SELECT COUNT(*) FROM "public"."t1"`; srcCounts[0] != want {
		t.Fatalf("source COUNT = %q, want exact legacy %q", srcCounts[0], want)
	}
	if want := "SELECT COUNT(*) FROM `t1`"; dstCounts[0] != want {
		t.Fatalf("target COUNT = %q, want exact legacy %q", dstCounts[0], want)
	}
	if tr.Suggestion != "" {
		t.Fatalf("legacy path must not set a Suggestion, got %q", tr.Suggestion)
	}
}

// A3: both estimates fail → conservative chunked routing (probes run) and
// the Suggestion discloses it.
func TestChunkedCountRoutingBothEstimatesFail(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.estErr = fmt.Errorf("stats unavailable")
	dst.tgtEstErr = fmt.Errorf("show status failed")
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 7 {
		t.Fatalf("conservative chunked count must still pass: %+v", tr)
	}
	if probes := src.sawQueries("ORDER BY"); len(probes) == 0 {
		t.Fatal("both estimates failing must route conservatively to the chunked path")
	}
	if !strings.Contains(tr.Suggestion, "保守走分块") {
		t.Fatalf("Suggestion %q must disclose the conservative routing", tr.Suggestion)
	}
} // A4: three full windows + short window + tail — sums equal the dataset,
// per-chunk progress fires (idx, total) in order, and the tail chunk
// (lower bound = last key, no upper bound) actually runs.
func TestChunkedCountSumAndProgress(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	var events [][2]int
	v.OnChunkProgress(func(table string, idx, total int) {
		if table != "t1" {
			t.Errorf("progress table = %q", table)
		}
		events = append(events, [2]int{idx, total})
	})
	tr := runChunkedCount(t, v, src, dst)

	if tr.SourceRows != 7 || tr.TargetRows != 7 {
		t.Fatalf("chunk sums src=%d dst=%d, want 7/7", tr.SourceRows, tr.TargetRows)
	}
	want := [][2]int{{0, 5}, {1, 5}, {2, 5}, {3, 5}, {4, 5}}
	if len(events) != len(want) {
		t.Fatalf("progress events = %v, want %v", events, want)
	}
	for i, ev := range events {
		if ev != want[i] {
			t.Fatalf("progress events = %v, want %v", events, want)
		}
	}
	// Tail chunk: lower bound 7 (last boundary), no upper bound.
	tail := src.sawQueries("COUNT(*)")
	found := false
	for _, q := range tail {
		if strings.Contains(q, " > $1") && !strings.Contains(q, "<=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no tail chunk (lower-only) COUNT found: %v", tail)
	}
}

// A5: varchar keys — boundaries are strings, re-bound as string args.
func TestChunkedCountVarcharKeys(t *testing.T) {
	src, dst := &chunkScript{
		pkCols:   []string{"k"},
		keyTypes: []string{"text"},
		estRows:  6_000_000,
		tgtEst:   "",
		data:     [][]driver.Value{{"a"}, {"b"}, {"c"}, {"d"}},
	}, &chunkScript{pkCols: []string{"k"}, keyTypes: []string{"text"}, data: [][]driver.Value{{"a"}, {"b"}, {"c"}, {"d"}}}
	dst.tgtEst = "6000000"
	v := newChunkValidator(chunkedCfg())
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 4 || tr.TargetRows != 4 {
		t.Fatalf("varchar chunked count: %+v", tr)
	}
	// window 1 (target 2) = {a, b} → boundary "b" re-bound in chunk 1.
	args := src.argsFor("COUNT(*)")
	if len(args) < 2 {
		t.Fatalf("expected chunk counts with bound args, got %v", args)
	}
	sawBound := false
	for _, a := range args {
		for _, v := range a {
			if s, ok := v.(string); ok && s == "b" {
				sawBound = true
			}
		}
	}
	if !sawBound {
		t.Fatalf("boundary \"b\" never re-bound as an arg: %v", args)
	}
}

// A6: composite PK — OR-chain predicates on both bounds, PG $N numbered
// continuously (max placeholder == arg count).
func TestChunkedCountCompositePK(t *testing.T) {
	data := [][]driver.Value{{int64(1), "a"}, {int64(1), "b"}, {int64(2), "a"}, {int64(2), "b"}, {int64(3), "a"}}
	src := &chunkScript{pkCols: []string{"id", "name"}, keyTypes: []string{"int8", "text"},
		estRows: 6_000_000, data: data}
	dst := &chunkScript{pkCols: []string{"id", "name"}, keyTypes: []string{"int8", "text"},
		tgtEst: "6000000", data: data}
	v := newChunkValidator(chunkedCfg())
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 5 || tr.TargetRows != 5 {
		t.Fatalf("composite chunked count: %+v", tr)
	}
	var midChunk string
	for _, q := range src.countQueries() {
		if strings.Contains(q, " > ") && strings.Contains(q, " <= ") {
			midChunk = q
			break
		}
	}
	if midChunk == "" {
		t.Fatalf("no mid chunk (both bounds) query: %v", src.countQueries())
	}
	// OR chains spelled out on both bounds.
	if !strings.Contains(midChunk, " OR (") {
		t.Fatalf("composite bounds must render OR chains: %s", midChunk)
	}
	// PG $N continuous: count $n occurrences == bound args (3 lower + 3 upper).
	dollars := strings.Count(midChunk, "$")
	if dollars != 6 {
		t.Fatalf("mid chunk placeholder count = %d, want 6 (3 lower + 3 upper): %s", dollars, midChunk)
	}
}

// A7: watermark predicate + chunk range compose on BOTH sides with the
// same bound values — single-value form and range form.
func TestChunkedCountWatermarkComposition(t *testing.T) {
	for _, tc := range []struct {
		name string
		wm   *config.WatermarkFilter
		args int
	}{
		{"single", &config.WatermarkFilter{Column: "ts", Value: "2026-01-01"}, 1},
		{"range", &config.WatermarkFilter{Column: "ts", RangeStart: "2026-01-01", RangeEnd: "2026-02-01"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := chunkedCfg()
			cc.Watermark = tc.wm
			src, dst, v := chunkHarness(cc)
			src.wmArgCount, dst.wmArgCount = tc.args, tc.args
			tr := runChunkedCount(t, v, src, dst)

			if tr.Status != reporter.StatusPass || tr.SourceRows != 7 {
				t.Fatalf("wm chunked count: %+v", tr)
			}
			// A mid chunk carries wm fragment FIRST then the range, on
			// both sides, with identical VALUES (wm value(s) + key bounds).
			srcMid, dstMid := midCountQuery(src), midCountQuery(dst)
			if srcMid == "" || dstMid == "" {
				t.Fatalf("mid chunk missing: src=%v dst=%v", src.countQueries(), dst.countQueries())
			}
			if !strings.Contains(srcMid, `"ts"`) || !strings.Contains(dstMid, "`ts`") {
				t.Fatalf("watermark column must appear on both sides:\nsrc: %s\ndst: %s", srcMid, dstMid)
			}
			sa, da := argsOf(src, srcMid), argsOf(dst, dstMid)
			if len(sa) != len(da) || len(sa) != tc.args+2 {
				t.Fatalf("mid chunk args src=%v dst=%v, want %d+2 each", sa, da, tc.args)
			}
			for i := range sa {
				if sa[i] != da[i] {
					t.Fatalf("both sides must bind identical values: src=%v dst=%v", sa, da)
				}
			}
			// PG source numbers key placeholders AFTER the watermark's.
			if !strings.Contains(srcMid, fmt.Sprintf(" > $%d", tc.args+1)) ||
				!strings.Contains(srcMid, fmt.Sprintf(" <= $%d", tc.args+2)) {
				t.Fatalf("source placeholders must continue after the wm's: %s", srcMid)
			}
		})
	}
}

func midCountQuery(s *chunkScript) string {
	for _, q := range s.countQueries() {
		if strings.Contains(q, " > ") && strings.Contains(q, " <= ") {
			return q
		}
	}
	return ""
}

func argsOf(s *chunkScript, substr string) []driver.Value {
	for i, q := range s.queriesLocked() {
		if q == substr {
			return s.argLogLocked()[i]
		}
	}
	return nil
}

func (s *chunkScript) queriesLocked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries
}

func (s *chunkScript) argLogLocked() [][]driver.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.argLog
}

// A8: no real PK (unique index only) → whole-table fallback + disclosure.
func TestChunkedCountNoPKFallback(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.pkCols = nil
	src.uniqueDef = "CREATE UNIQUE INDEX uq ON t1(k)"
	src.keyTypes = nil
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 7 {
		t.Fatalf("no-PK fallback must still count whole: %+v", tr)
	}
	if len(src.countQueries()) != 1 {
		t.Fatalf("no-PK fallback = exactly one whole-table COUNT, got %v", src.countQueries())
	}
	if !strings.Contains(tr.Suggestion, "无真主键无法分块") {
		t.Fatalf("Suggestion %q must disclose the no-PK fallback", tr.Suggestion)
	}
}

// A9: chunk COUNT exhausting its retries fails the table with the legacy
// "source count: " shape plus the chunk range disclosure (and the retry
// ran exactly 1+2 times).
func TestChunkedCountChunkTimeoutShape(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.countFailN = 99
	src.countFailErr = context.DeadlineExceeded
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusFail {
		t.Fatalf("exhausted chunk retries must fail the table: %+v", tr)
	}
	if !strings.HasPrefix(tr.Error, "source count: chunk ") {
		t.Fatalf("error must keep the source-count shape + range: %q", tr.Error)
	}
	if !strings.Contains(tr.Error, "(-inf, 2]") {
		t.Fatalf("error must disclose the failing chunk range: %q", tr.Error)
	}
	if !strings.Contains(tr.Error, "query timeout") {
		t.Fatalf("timeout must be annotated: %q", tr.Error)
	}
	if n := src.attemptsNow(); n != 3 {
		t.Fatalf("retry attempts = %d, want 3 (initial + 2 retries)", n)
	}
}

// A10: count mismatch keeps the legacy mismatch string and localizes the
// first 3 differing chunks in Suggestion.
func TestChunkedCountDiffLocalization(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	dst.shortChunk, dst.shortN = 1, 1 // chunk (2,4] loses one row on target
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusFail {
		t.Fatalf("mismatch must fail: %+v", tr)
	}
	if want := "row count mismatch: source=7 target=6 diff=1"; tr.Error != want {
		t.Fatalf("error = %q, want the exact legacy %q", tr.Error, want)
	}
	if !strings.Contains(tr.Suggestion, "rows differ in (2, 4]: src=2 dst=1") {
		t.Fatalf("Suggestion %q must localize the differing chunk", tr.Suggestion)
	}
}

// A11: chunked_count_threshold: 0 disables the engine entirely — no
// estimate probes at all, straight legacy counts (zero regression).
func TestChunkedCountDisabled(t *testing.T) {
	cc := chunkedCfg()
	cc.ChunkedCountThreshold = 0
	src, dst, v := chunkHarness(cc)
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 7 || tr.Suggestion != "" {
		t.Fatalf("disabled engine = legacy report: %+v", tr)
	}
	if probes := src.sawQueries("pg_stat_user_tables"); len(probes) != 0 {
		t.Fatal("disabled engine must not even probe estimates")
	}
	if len(src.countQueries()) != 1 {
		t.Fatalf("disabled engine = one legacy COUNT, got %v", src.countQueries())
	}
}

// A12: negative threshold clamps to the 5M default (typo defense) — 6M
// still routes to the chunked path.
func TestChunkedCountNegativeThresholdClamp(t *testing.T) {
	cc := chunkedCfg()
	cc.ChunkedCountThreshold = -1
	src, dst, v := chunkHarness(cc)
	tr := runChunkedCount(t, v, src, dst)

	if len(src.sawQueries("ORDER BY")) == 0 {
		t.Fatal("negative threshold must clamp to the default and still route")
	}
	if tr.SourceRows != 7 {
		t.Fatalf("clamped routing totals: %+v", tr)
	}
}

// A13: chunk diffs that cancel out keep the PASS verdict (whole-table
// parity) but disclose the chunk-level evidence.
func TestChunkedCountCancelOutDisclosure(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.shortChunk, src.shortN = 0, 1 // (-inf,2]: src loses one
	dst.shortChunk, dst.shortN = 3, 1 // (6,7]: dst loses one
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass {
		t.Fatalf("net-zero diff keeps the legacy PASS verdict: %+v", tr)
	}
	if tr.SourceRows != 6 || tr.TargetRows != 6 || tr.DiffRows != 0 {
		t.Fatalf("totals: %+v", tr)
	}
	if !strings.Contains(tr.Suggestion, "cancel out") {
		t.Fatalf("Suggestion %q must disclose the canceled chunk diffs", tr.Suggestion)
	}
}

// A14: boundary-scan failure fails the table under the source-count shape.
func TestChunkedCountBoundaryScanError(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.probeErr = fmt.Errorf("relation gone")
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusFail {
		t.Fatalf("boundary scan failure must fail the table: %+v", tr)
	}
	if !strings.HasPrefix(tr.Error, "source count: chunk boundary scan:") {
		t.Fatalf("error shape: %q", tr.Error)
	}
	if !strings.Contains(tr.Error, "relation gone") {
		t.Fatalf("error must carry the cause: %q", tr.Error)
	}
}

// A15: NULL PK value during the boundary scan → whole-table fallback +
// disclosure (ranges cannot be carved safely).
func TestChunkedCountNullKeyFallback(t *testing.T) {
	src, dst, v := chunkHarness(chunkedCfg())
	src.data = append(src.data, []driver.Value{nil}) // NULL key in window 4
	dst.data = append(dst.data, []driver.Value{nil})
	tr := runChunkedCount(t, v, src, dst)

	if tr.Status != reporter.StatusPass || tr.SourceRows != 8 {
		t.Fatalf("NULL-key fallback must count whole (8 rows): %+v", tr)
	}
	if len(src.countQueries()) != 1 {
		t.Fatalf("NULL-key fallback = one whole-table COUNT, got %v", src.countQueries())
	}
	if !strings.Contains(tr.Suggestion, "主键列含 NULL") {
		t.Fatalf("Suggestion %q must disclose the NULL-key fallback", tr.Suggestion)
	}
}
