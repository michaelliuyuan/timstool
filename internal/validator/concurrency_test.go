package validator

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// #t4 anchors: unified concurrency budget resolution (legacy knob folding +
// clamp + default), ctx-cancellable semaphore acquisition, and peak-slot
// accounting under contention.

// A1: ResolveConcurrency 鈥?explicit wins; legacy knobs fold via max; default
// 4; hard clamp at MaxConcBudget (8).
func TestResolveConcurrency(t *testing.T) {
	cases := []struct {
		conc, par, chunk, want int
	}{
		{0, 0, 0, 4},  // nothing set 鈫?default
		{6, 2, 3, 6},  // explicit wins over both legacy knobs
		{0, 2, 3, 3},  // legacy max(chunk) wins
		{0, 5, 2, 5},  // legacy max(parallel) wins
		{0, 1, 1, 1},  // lower bound
		{0, 32, 0, 8}, // legacy over cap clamps
		{99, 0, 0, 8}, // explicit over cap clamps
		{8, 1, 1, 8},  // explicit at cap kept
		{-1, 0, 0, 4}, // garbage 鈫?default
	}
	for i, c := range cases {
		if got := ResolveConcurrency(c.conc, c.par, c.chunk); got != c.want {
			t.Errorf("case %d: ResolveConcurrency(%d,%d,%d)=%d want %d", i, c.conc, c.par, c.chunk, got, c.want)
		}
	}
}

// A2: validator.concurrency 鈥?CompareConfig.Concurrency (override path used
// by webapi compare tasks) beats the parallel override and config-file knobs.
func TestValidatorConcurrency(t *testing.T) {
	v := NewValidator(config.Config{})
	v.compareOverride = &config.CompareConfig{Concurrency: 3}
	v.parallelOverride = 7
	if got := v.concurrency(); got != 3 {
		t.Fatalf("compareCfg.Concurrency must win, got %d", got)
	}
	v2 := NewValidator(config.Config{})
	v2.compareOverride = &config.CompareConfig{ChecksumParallel: 6}
	v2.parallelOverride = 2
	if got := v2.concurrency(); got != 6 {
		t.Fatalf("legacy chunk parallel must fold via max, got %d", got)
	}
}

// A3: concSem.acquire honors ctx cancellation 鈥?a goroutine blocked on a
// full sem must return when the context is cancelled (the "cancel 鏃犳晥"
// half of the #t4 deadlock bug).
func TestConcSemAcquireCancel(t *testing.T) {
	sem := make(concSem, 1)
	release, err := sem.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire must succeed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sem.acquire(ctx)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked acquire must fail on ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not observe cancellation")
	}
	release()
}

// A4: peak-slot accounting 鈥?with a budget of B and many concurrent workers,
// the number of simultaneously held slots never exceeds B, and all workers
// finish (no deadlock, no slot leak).
func TestConcSemPeakWithinBudget(t *testing.T) {
	const budget = 4
	const workers = 64
	sem := make(concSem, budget)

	var cur, peak int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := sem.acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			c := atomic.AddInt32(&cur, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&cur, -1)
			release()
		}()
	}
	wg.Wait()
	if p := atomic.LoadInt32(&peak); p > budget {
		t.Fatalf("peak concurrent slots %d exceeded budget %d", p, budget)
	}
	if p := atomic.LoadInt32(&peak); p == 0 {
		t.Fatal("test sanity: peak must be positive")
	}
}

// --- scripted counting driver: serves canned result sets while counting the
// number of queries in flight across ALL connections, so peak concurrency of
// real validation code paths can be asserted against the budget. ---

type queryMeter struct {
	cur  int32
	peak int32
}

func (m *queryMeter) enter() int32 {
	c := atomic.AddInt32(&m.cur, 1)
	for {
		p := atomic.LoadInt32(&m.peak)
		if c <= p || atomic.CompareAndSwapInt32(&m.peak, p, c) {
			break
		}
	}
	return c
}
func (m *queryMeter) exit() { atomic.AddInt32(&m.cur, -1) }

type scriptConn struct {
	meter *queryMeter
	pg    bool
}

func (c *scriptConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *scriptConn) Close() error                        { return nil }
func (c *scriptConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }

func (c *scriptConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

func (c *scriptConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.meter.enter()
	defer c.meter.exit()
	time.Sleep(2 * time.Millisecond) // force overlap between concurrent units

	if strings.Contains(q, "SELECT COUNT(*)") {
		return &scriptRows{cols: []string{"count"}, data: [][]driver.Value{{int64(200000)}}}, nil
	}
	if c.pg && strings.Contains(q, "table_constraints") {
		// PK detection: one PK column "id".
		return &scriptRows{cols: []string{"column_name"}, data: [][]driver.Value{{"id"}}}, nil
	}
	if strings.Contains(q, "SELECT * FROM") {
		// Chunk scans (and any sample scans): empty result set 鈥?both sides
		// hash to the same value.
		return &scriptRows{cols: []string{"id"}, data: nil}, nil
	}
	// information_schema column checks (watermark precheck), pg_indexes, etc.
	return &scriptRows{cols: []string{"x"}, data: nil}, nil
}

type scriptRows struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *scriptRows) Columns() []string { return r.cols }
func (r *scriptRows) Close() error      { return nil }
func (r *scriptRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

type scriptConnector struct {
	meter *queryMeter
	pg    bool
}

func (f *scriptConnector) Connect(context.Context) (driver.Conn, error) {
	return &scriptConn{meter: f.meter, pg: f.pg}, nil
}
func (f *scriptConnector) Driver() driver.Driver { return fakeDriver{} }

// A5 (#t4 appendix 鈶?: one checksum chunk's PG+TiDB double-sided queries
// share ONE budget slot 鈥?with a budget of 2 and 4 chunks (200k rows / 50k
// chunk size), peak in-flight queries across both pools must stay 鈮?2. The
// chunk path itself is proven live by the "4 chunks" suggestion stamp (no
// hash_group fallback).
func TestChecksumChunkDoubleSidedSingleSlot(t *testing.T) {
	meter := &queryMeter{}
	pgDB := sql.OpenDB(&scriptConnector{meter: meter, pg: true})
	defer pgDB.Close()
	tidbDB := sql.OpenDB(&scriptConnector{meter: meter, pg: false})
	defer tidbDB.Close()

	v := NewValidator(config.Config{})
	v.compareOverride = &config.CompareConfig{ChecksumChunkSize: 50000, Concurrency: 2}

	tr := v.validateChecksumChunked(context.Background(), pgDB, tidbDB, "t1", make(concSem, 2))
	if tr.Status != "pass" {
		t.Fatalf("scripted chunk compare must pass, got %s err=%q", tr.Status, tr.Error)
	}
	if !strings.Contains(tr.Suggestion, "4 chunks") {
		t.Fatalf("chunk path not taken (suggestion=%q) 鈥?fix the script", tr.Suggestion)
	}
	if p := atomic.LoadInt32(&meter.peak); p > 2 {
		t.Fatalf("peak in-flight queries %d exceeded budget 2 (chunk double-sided must share one slot)", p)
	}
}

// A6 (#t4 appendix 鈶?: watermark-mode table units run through the same
// single-layer budget 鈥?the watermark precheck + count queries all happen
// inside slots, so peak in-flight queries 鈮?budget across concurrent tables.
func TestWatermarkUnitSharesBudget(t *testing.T) {
	meter := &queryMeter{}
	pgDB := sql.OpenDB(&scriptConnector{meter: meter, pg: true})
	defer pgDB.Close()
	tidbDB := sql.OpenDB(&scriptConnector{meter: meter, pg: false})
	defer tidbDB.Close()

	v := NewValidator(config.Config{})
	wm := &config.WatermarkFilter{Column: "id", Value: "100", Op: "<=", BaseMode: "quick"}
	sem := make(concSem, 2)

	const tables = 8
	var wg sync.WaitGroup
	for i := 0; i < tables; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.runTableUnit(context.Background(), pgDB, tidbDB, sem, "t1", wm, "quick", 0.01)
		}()
	}
	wg.Wait()
	if p := atomic.LoadInt32(&meter.peak); p > 2 {
		t.Fatalf("peak in-flight queries %d exceeded budget 2 in watermark mode", p)
	}
	if p := atomic.LoadInt32(&meter.peak); p == 0 {
		t.Fatal("test sanity: queries must have run")
	}
}
