package validator

// query_timeout_test.go anchors the P0-2 query-level timeout (#t94
// BUG-COMPARE-STUCK half 2: a hung query must never pin the run):
//
//	T1  timeout injection on a later chunk — the FIRST window runs normally,
//	    the hung window fails with the "query timeout" disclosure, the chain
//	    stops (no seek after the failed boundary), and the whole
//	    validateChecksumChunked returns BOUNDED (wg.Wait() cannot hang).
//	T2  the run-level outcome: the timed-out chunk fails the TABLE, not the
//	    process — other tables in a multi-table Run are structurally isolated
//	    (each runs in its own goroutine), so a table failure never leaks.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// hangConn wraps a sideScript's respond but BLOCKS the first keyset-seek
// query on ctx.Done() — the app-level context.WithTimeout fires, the driver
// returns the deadline error, and the chunk is marked failed (no hang).
type hangConn struct {
	script *sideScript
	// blockOnSeek: block the FIRST query containing the seek " > ".
	blockOnSeek bool
	blocked     atomic.Bool
}

func (c *hangConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *hangConn) Close() error                        { return nil }
func (c *hangConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *hangConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

func (c *hangConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.blockOnSeek && strings.Contains(q, " > ") && c.blocked.CompareAndSwap(false, true) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, fmt.Errorf("timeout driver: ctx never cancelled (test bug)")
		}
	}
	return c.script.respond(q, args)
}

type hangConnector struct {
	script      *sideScript
	blockOnSeek bool
}

func (h *hangConnector) Connect(context.Context) (driver.Conn, error) {
	return &hangConn{script: h.script, blockOnSeek: h.blockOnSeek}, nil
}
func (h *hangConnector) Driver() driver.Driver { return fakeDriver{} }

func (s *sideScript) openHang(blockOnSeek bool) *sql.DB {
	return sql.OpenDB(&hangConnector{script: s, blockOnSeek: blockOnSeek})
}

// TestChecksumChunkQueryTimeoutNoHang (T1): chunk size 2 over 5 rows → 3
// windows. Window 0 runs and hashes cleanly; window 1's PG seek query hangs
// until the 100ms query-timeout fires, fails that chunk with the disclosure,
// stops the chain (window 2 never runs), and validateChecksumChunked
// returns well within a bounded time — the whole run cannot pin.
func TestChecksumChunkQueryTimeoutNoHang(t *testing.T) {
	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "a"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})
	src := &sideScript{count: 5, pkCols: []string{"id"}, data: same}
	dst := &sideScript{count: 5, data: same}

	v := NewValidator(config.Config{Compare: config.CompareConfig{
		ChecksumChunkSize: 2,
		Concurrency:       4,
		QueryTimeout:      "100ms",
	}})

	start := time.Now()
	tr := v.validateChecksumChunked(context.Background(), src.openHang(true), dst.open(), "t1", make(concSem, 4))
	elapsed := time.Since(start)

	// Bounded return: the 100ms timeout plus scheduling must finish quickly.
	if elapsed > 5*time.Second {
		t.Fatalf("validateChecksumChunked hung after a query timeout (%.1fs), wg.Wait() not bounded", elapsed.Seconds())
	}
	if tr.Status != reporter.StatusFail {
		t.Fatalf("timed-out chunk must fail the table, got status=%s", tr.Status)
	}
	if !strings.Contains(tr.Error, "query timeout") {
		t.Fatalf("table error must disclose the query timeout, got %q", tr.Error)
	}

	// Window 0 ran normally on both sides; the hung window 1 never reached
	// the TiDB side (the PG failure stops the chain before it), and window 2
	// never ran at all.
	if got := len(src.sawQueries("SELECT *")); got != 1 {
		t.Fatalf("expected only window 0 on src before the hang, got %d: %v", got, src.sawQueries("SELECT *"))
	}
	if got := len(dst.sawQueries("SELECT *")); got != 1 {
		t.Fatalf("expected only window 0 on dst (window 1 never reached), got %d: %v", got, dst.sawQueries("SELECT *"))
	}
}

// TestChecksumChunkQueryTimeoutBindsDefault (T2 companion): with an EMPTY
// query_timeout the validator falls back to the 5m default — the same
// scripted run completes (no timeout fires) and passes.
func TestChecksumChunkQueryTimeoutBindsDefault(t *testing.T) {
	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "a"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})
	src := &sideScript{count: 5, pkCols: []string{"id"}, data: same}
	dst := &sideScript{count: 5, data: same}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	if d := v.compareCfg().QueryTimeoutDuration(); d != 5*time.Minute {
		t.Fatalf("empty query_timeout must bind the 5m default, got %v", d)
	}
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))
	if tr.Status != reporter.StatusPass {
		t.Fatalf("normal path must pass under the default timeout, got %s err=%q", tr.Status, tr.Error)
	}
}
