package webapi

// Placeholder-sharding anchors (PG-1390 "too many placeholders"): a fake
// database/sql driver records every ExecContext (SQL + arg count) so the
// shard boundaries are asserted at the real database/sql boundary, not on a
// re-implemented model.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// shardFakeDB records ExecContext calls and can fail a chosen call
// (1-based) to exercise mid-shard aborts. QueryContext serves the preset
// drain rows first, then a single empty MIN() row for the jump lookup.
type shardFakeDB struct {
	mu         sync.Mutex
	execArgs   []int
	execSQL    []string
	failOn     int // 1-based Exec index to fail; 0 = never
	queryCalls int
	queryCols  []string
	queryRows  [][]driver.Value
}

func (f *shardFakeDB) execCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execArgs)
}

type shardConn struct{ db *shardFakeDB }

func (c *shardConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (c *shardConn) Close() error                        { return nil }
func (c *shardConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }

func (c *shardConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	c.db.execSQL = append(c.db.execSQL, q)
	c.db.execArgs = append(c.db.execArgs, len(args))
	if c.db.failOn > 0 && len(c.db.execArgs) == c.db.failOn {
		return nil, errors.New("boom: injected shard failure")
	}
	return driver.RowsAffected(0), nil
}

func (c *shardConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.db.mu.Lock()
	c.db.queryCalls++
	n := c.db.queryCalls
	c.db.mu.Unlock()
	if n == 1 {
		return &shardFakeRows{cols: c.db.queryCols, vals: c.db.queryRows}, nil
	}
	// subsequent queries = MIN(watermark) jump lookup: empty (NULL) result
	return &shardFakeRows{cols: []string{"min"}, vals: [][]driver.Value{{nil}}}, nil
}

type shardFakeRows struct {
	cols []string
	vals [][]driver.Value
	i    int
}

func (r *shardFakeRows) Columns() []string { return r.cols }
func (r *shardFakeRows) Close() error      { return nil }
func (r *shardFakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}

type shardConnector struct{ db *shardFakeDB }

func (c shardConnector) Connect(context.Context) (driver.Conn, error) {
	return &shardConn{db: c.db}, nil
}
func (c shardConnector) Driver() driver.Driver { return shardDriver{} }

type shardDriver struct{}

func (shardDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

func openShardDB(f *shardFakeDB) *sql.DB { return sql.OpenDB(shardConnector{db: f}) }

func shardCols(n int) []string {
	cols := make([]string, n)
	for i := range cols {
		cols[i] = "c" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	return cols
}

// T1: shard-size boundaries of incShardRows.
func TestIncShardRowsBoundaries(t *testing.T) {
	cases := []struct {
		cols, rows, want int
	}{
		{65, 1000, 1000},   // 65000/65 = 1000 exactly: batch fits whole
		{66, 1000, 984},    // 65000/66 = 984.8 -> 984
		{65000, 100000, 1}, // one row per INSERT at the extreme width
		{0, 1000, 1000},    // unknown cols: never shrink (defensive)
		{-3, 500, 500},     // ditto
		{70000, 5, 1},      // wider than the ceiling itself: clamp to 1
		{3, 10, 10},        // small widths: batch already fits (cap 21666)
		{3, 100000, 21666}, // 65000/3 = 21666.7 -> 21666
	}
	for _, c := range cases {
		if got := incShardRows(c.cols, c.rows); got != c.want {
			t.Fatalf("incShardRows(%d,%d)=%d want %d", c.cols, c.rows, got, c.want)
		}
	}
}

// T2: 66-column wide table x batch 1000 -> exactly two INSERTs (984+16 rows),
// every statement within the placeholder ceiling, and the SQL row-count
// matches the shard's arg count (no lost/duplicated rows in the statement
// shape itself).
func TestIncShardWideTableInsert(t *testing.T) {
	const ncols = 66
	cols := shardCols(ncols)
	batch := make([][]any, 1000)
	for i := range batch {
		batch[i] = make([]any, ncols)
		for j := range batch[i] {
			batch[i][j] = i*1000 + j
		}
	}
	f := &shardFakeDB{}
	db := openShardDB(f)
	defer db.Close()

	written, err := incExecShardedInsert(context.Background(), db, "tgtdb", "big", cols, batch, "replace")
	if err != nil || written != 1000 {
		t.Fatalf("written=%d err=%v (want 1000, nil)", written, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execArgs) != 2 || f.execArgs[0] != 984*ncols || f.execArgs[1] != 16*ncols {
		t.Fatalf("shard shape must be 984+16 rows: %+v", f.execArgs)
	}
	for i, n := range f.execArgs {
		if n > incMaxPlaceholders {
			t.Fatalf("statement %d exceeds placeholder ceiling: %d > %d", i, n, incMaxPlaceholders)
		}
		if n%ncols != 0 {
			t.Fatalf("statement %d arg count %d not a whole row multiple of %d cols", i, n, ncols)
		}
		oneRow := "(" + cols2Placeholders(ncols) + ")"
		wantRows := n / ncols
		if got := countRowTuples(f.execSQL[i], oneRow); got != wantRows {
			t.Fatalf("statement %d VALUES tuples=%d want %d", i, got, wantRows)
		}
	}
}

func cols2Placeholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ", "
		}
		s += "?"
	}
	return s
}

func countRowTuples(sqlStr, oneRow string) int {
	count, pos := 0, 0
	for {
		next := indexOfFrom(sqlStr, oneRow, pos)
		if next < 0 {
			break
		}
		count++
		pos = next + len(oneRow)
	}
	return count
}

func indexOfFrom(s, sub string, from int) int {
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// T3: the same-value drain flush routes through the same sharded insert —
// driving incDrainWatermark with fake PG/MySQL handles shows the flush of a
// 1000-row same-watermark batch on a 66-column table sharding 984+16 too.
func TestIncShardDrainFlushSharded(t *testing.T) {
	const ncols = 66
	cols := shardCols(ncols)
	src := &shardFakeDB{queryCols: cols}
	for i := 0; i < 1000; i++ {
		row := make([]driver.Value, ncols)
		for j := range row {
			row[j] = int64(i*1000 + j)
		}
		src.queryRows = append(src.queryRows, row)
	}
	tgt := &shardFakeDB{}
	s := &Server{}
	job := &incJob{BatchSize: 1000, ConflictStrategy: "ignore"}
	pgDB, myDB := openShardDB(src), openShardDB(tgt)
	defer pgDB.Close()
	defer myDB.Close()

	rows, _, _, err := s.incDrainWatermark(context.Background(), pgDB, myDB,
		config.SourceConfig{}, config.TargetConfig{}, job,
		incTableConfig{Table: "wide", WatermarkColumn: cols[0]}, cols, "w1")
	if err != nil || rows != 1000 {
		t.Fatalf("drain rows=%d err=%v (want 1000, nil)", rows, err)
	}
	if got := tgt.execCalls(); got != 2 {
		t.Fatalf("drain flush must shard into 2 INSERTs, got %d", got)
	}
	tgt.mu.Lock()
	defer tgt.mu.Unlock()
	if tgt.execArgs[0] != 984*ncols || tgt.execArgs[1] != 16*ncols {
		t.Fatalf("drain shard shape must be 984+16 rows: %+v", tgt.execArgs)
	}
}

// T4: a failing shard aborts with its error while already-written shards are
// kept (returned written counts only the surviving prefix).
func TestIncShardMidFailureKeepsWrittenShards(t *testing.T) {
	const ncols = 66
	cols := shardCols(ncols)
	batch := make([][]any, 2000)
	for i := range batch {
		batch[i] = make([]any, ncols)
		for j := range batch[i] {
			batch[i][j] = i
		}
	}
	f := &shardFakeDB{failOn: 2} // first shard OK, second fails
	db := openShardDB(f)
	defer db.Close()

	written, err := incExecShardedInsert(context.Background(), db, "tgtdb", "big", cols, batch, "replace")
	if err == nil || err.Error() != "boom: injected shard failure" {
		t.Fatalf("second-shard failure must surface: %v", err)
	}
	if written != 984 {
		t.Fatalf("written shards must be kept: %d (want 984)", written)
	}
	if got := f.execCalls(); got != 2 {
		t.Fatalf("must stop at the failing shard: %d execs", got)
	}
}

// T5: error-strategy variant of T4 — a mid-shard failure with rows already
// written carries the actionable duplicate-key hint (written count included).
func TestIncShardMidFailureErrorStrategyHint(t *testing.T) {
	const ncols = 66
	cols := shardCols(ncols)
	batch := make([][]any, 2000)
	for i := range batch {
		batch[i] = make([]any, ncols)
		for j := range batch[i] {
			batch[i][j] = i
		}
	}
	f := &shardFakeDB{failOn: 2}
	db := openShardDB(f)
	defer db.Close()

	written, err := incExecShardedInsert(context.Background(), db, "tgtdb", "big", cols, batch, "error")
	if err == nil {
		t.Fatal("error strategy mid-shard failure must surface")
	}
	if !strings.Contains(err.Error(), "boom: injected shard failure") {
		t.Fatalf("original error must be wrapped verbatim: %v", err)
	}
	if !strings.Contains(err.Error(), "984") || !strings.Contains(err.Error(), "撞主键") {
		t.Fatalf("hint must carry written count and duplicate-key guidance: %v", err)
	}
	if written != 984 {
		t.Fatalf("written shards must be kept: %d (want 984)", written)
	}
}

// T6: replace control — idempotent strategies keep the bare error format
// (no hint) so retry semantics stay silent-clean.
func TestIncShardMidFailureReplaceKeepsBareError(t *testing.T) {
	const ncols = 66
	cols := shardCols(ncols)
	batch := make([][]any, 2000)
	for i := range batch {
		batch[i] = make([]any, ncols)
		for j := range batch[i] {
			batch[i][j] = i
		}
	}
	for _, strategy := range []string{"replace", "ignore"} {
		f := &shardFakeDB{failOn: 2}
		db := openShardDB(f)
		_, err := incExecShardedInsert(context.Background(), db, "tgtdb", "big", cols, batch, strategy)
		db.Close()
		if err == nil || err.Error() != "boom: injected shard failure" {
			t.Fatalf("%s must keep the bare error format: %v", strategy, err)
		}
	}
}
