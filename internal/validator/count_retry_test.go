package validator

// count_retry_test.go anchors the BUG-CMP-COUNT-P0 P0-A2 count backoff
// retry (a huge cold table's COUNT(*) outrunning the query deadline must
// not fail the table on the FIRST timeout):
//
//	T1  timeout-class failure twice then success → retries kick in, the
//	    table PASSES and the source COUNT ran exactly 3 times (1+2).
//	T2  a driver-style net.Error i/o timeout is the same retry class.
//	T3  a NON-timeout error never retries (fail fast, 1 attempt) — a
//	    syntax error or missing table will not heal.
//	T4  the retry cap is exactly 2: persistent timeouts stop after the
//	    3rd attempt with the unchanged "source count: query timeout (…)"
//	    report shape.
//	T5  the production backoffs are pinned (30s then 2m) and the seam
//	    overrides them (zero-wait tests).
//	T6  the fresh-deadline property: a COUNT that really blocks until the
//	    deadline fails attempt 1 (real 50ms timeout), and the retry runs
//	    under a NEW deadline and succeeds.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// fakeNetTimeout is a driver-shaped i/o timeout (net.Error with
// Timeout()=true — the "read: i/o timeout" family go-sql-driver / pgx
// surface on network deadlines).
type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "read: i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

var _ net.Error = fakeNetTimeout{}

// countFailConn serves a sideScript but fails the FIRST failN COUNT
// queries with failErr (returned before the script logs them — attempts
// are counted by the connector itself).
type countFailConn struct {
	script   *sideScript
	failN    int
	failErr  error
	mu       sync.Mutex
	attempts int
}

func (c *countFailConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *countFailConn) Close() error                        { return nil }
func (c *countFailConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *countFailConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

func (c *countFailConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "COUNT(*)") {
		c.mu.Lock()
		c.attempts++
		n := c.attempts
		c.mu.Unlock()
		if n <= c.failN {
			return nil, c.failErr
		}
	}
	return c.script.respond(q, args)
}

func (c *countFailConn) countAttempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

type countFailConnector struct {
	conn *countFailConn
}

func (f *countFailConnector) Connect(context.Context) (driver.Conn, error) { return f.conn, nil }
func (f *countFailConnector) Driver() driver.Driver                        { return fakeDriver{} }

func openCountFail(script *sideScript, failN int, failErr error) (*sql.DB, *countFailConn) {
	c := &countFailConn{script: script, failN: failN, failErr: failErr}
	return sql.OpenDB(&countFailConnector{conn: c}), c
}

// zeroBackoff is the injectable seam the anchors use (no real waiting).
func zeroBackoff(int) time.Duration { return 0 }

func newRetryValidator() *Validator {
	v := NewValidator(config.Config{Compare: config.CompareConfig{QueryTimeout: "50ms"}})
	v.countRetryBackoffFn = zeroBackoff
	return v
}

func tidbConnOf(t *testing.T, dst *sideScript) *sql.Conn {
	t.Helper()
	db := dst.open()
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("get tidb conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// T1: DeadlineExceeded twice, then success — the table passes and the
// source COUNT ran exactly 3 times (initial + 2 retries).
func TestCountRetryTimeoutThenSuccess(t *testing.T) {
	src := &sideScript{count: 5}
	dst := &sideScript{count: 5}
	srcDB, srcConn := openCountFail(src, 2, context.DeadlineExceeded)
	defer srcDB.Close()

	v := newRetryValidator()
	tr := v.validateRowCount(context.Background(), srcDB, tidbConnOf(t, dst), "t1")

	if tr.Status != reporter.StatusPass {
		t.Fatalf("timeout×2 then success must PASS via retries, got status=%s err=%q", tr.Status, tr.Error)
	}
	if tr.SourceRows != 5 || tr.TargetRows != 5 {
		t.Fatalf("counts must be served by the scripted sides, got src=%d dst=%d", tr.SourceRows, tr.TargetRows)
	}
	if got := srcConn.countAttempts(); got != 3 {
		t.Fatalf("source COUNT attempts = %d, want exactly 3 (1 initial + 2 retries)", got)
	}
}

// T2: a driver-style net.Error i/o timeout is the same retry class.
func TestCountRetryDriverNetTimeout(t *testing.T) {
	src := &sideScript{count: 7}
	dst := &sideScript{count: 7}
	srcDB, srcConn := openCountFail(src, 1, fakeNetTimeout{})
	defer srcDB.Close()

	v := newRetryValidator()
	tr := v.validateRowCount(context.Background(), srcDB, tidbConnOf(t, dst), "t1")

	if tr.Status != reporter.StatusPass {
		t.Fatalf("driver i/o timeout then success must PASS via retry, got status=%s err=%q", tr.Status, tr.Error)
	}
	if got := srcConn.countAttempts(); got != 2 {
		t.Fatalf("source COUNT attempts = %d, want 2 (1 initial + 1 retry)", got)
	}
}

// T3: a NON-timeout error never retries — fail fast on the first attempt.
func TestCountNoRetryOnNonTimeout(t *testing.T) {
	src := &sideScript{count: 5}
	dst := &sideScript{count: 5}
	boom := errors.New("pq: relation does not exist")
	srcDB, srcConn := openCountFail(src, 5, boom)
	defer srcDB.Close()

	v := newRetryValidator()
	tr := v.validateRowCount(context.Background(), srcDB, tidbConnOf(t, dst), "t1")

	if tr.Status != reporter.StatusFail {
		t.Fatalf("non-timeout COUNT error must fail, got status=%s", tr.Status)
	}
	if !strings.Contains(tr.Error, "source count:") || !strings.Contains(tr.Error, "relation does not exist") {
		t.Fatalf("error must keep the source-count disclosure with the cause, got %q", tr.Error)
	}
	if strings.Contains(tr.Error, "query timeout (") {
		t.Fatalf("non-timeout error must not be annotated as a query timeout, got %q", tr.Error)
	}
	if got := srcConn.countAttempts(); got != 1 {
		t.Fatalf("non-timeout error must NOT retry, attempts = %d, want 1", got)
	}
}

// T4: persistent timeouts stop after exactly 2 retries (3 attempts) with
// the unchanged report shape.
func TestCountRetryCapIsTwo(t *testing.T) {
	src := &sideScript{count: 5}
	dst := &sideScript{count: 5}
	srcDB, srcConn := openCountFail(src, 100, context.DeadlineExceeded)
	defer srcDB.Close()

	v := newRetryValidator()
	qt := v.compareCfg().QueryTimeoutDuration()
	tr := v.validateRowCount(context.Background(), srcDB, tidbConnOf(t, dst), "t1")

	if tr.Status != reporter.StatusFail {
		t.Fatalf("persistent timeouts must fail the table, got status=%s", tr.Status)
	}
	wantPrefix := fmt.Sprintf("source count: query timeout (%s): context deadline exceeded", qt)
	if tr.Error != wantPrefix {
		t.Fatalf("final-failure string shape changed:\n got %q\nwant %q", tr.Error, wantPrefix)
	}
	if got := srcConn.countAttempts(); got != 3 {
		t.Fatalf("retry cap must be exactly 2 (3 attempts total), got %d", got)
	}
}

// T5: production backoffs are pinned; the seam overrides them.
func TestCountRetryBackoffDefaults(t *testing.T) {
	v := NewValidator(config.Config{})
	if got := v.countRetryBackoff(1); got != 30*time.Second {
		t.Fatalf("backoff before 1st retry = %v, want 30s", got)
	}
	if got := v.countRetryBackoff(2); got != 2*time.Minute {
		t.Fatalf("backoff before 2nd retry = %v, want 2m", got)
	}
	v.countRetryBackoffFn = zeroBackoff
	if got := v.countRetryBackoff(1); got != 0 {
		t.Fatalf("seam must override the backoff, got %v", got)
	}
}

// T6: fresh deadline per attempt — a COUNT that really blocks burns the
// real 50ms deadline on attempt 1, and the retry (new deadline) succeeds.
func TestCountRetryFreshDeadlineRealTimeout(t *testing.T) {
	src := &sideScript{count: 3}
	dst := &sideScript{count: 3}
	srcDB := sql.OpenDB(&countHangConnector{script: src, hangs: 1})
	defer srcDB.Close()

	v := newRetryValidator()
	start := time.Now()
	tr := v.validateRowCount(context.Background(), srcDB, tidbConnOf(t, dst), "t1")
	elapsed := time.Since(start)

	if tr.Status != reporter.StatusPass {
		t.Fatalf("blocked-then-served count must PASS on the retried attempt, got status=%s err=%q", tr.Status, tr.Error)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("attempt 1 must have burned the real 50ms deadline, elapsed %v", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("retry must be bounded, elapsed %v", elapsed)
	}
}

// countHangConn blocks the FIRST `hangs` COUNT queries on ctx.Done() (the
// app-level query timeout fires for real), then serves the script.
type countHangConn struct {
	script  *sideScript
	hangs   int32
	blocked atomic.Int32
}

func (c *countHangConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *countHangConn) Close() error                        { return nil }
func (c *countHangConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *countHangConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

func (c *countHangConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "COUNT(*)") && c.blocked.Add(1) <= c.hangs {
		c.script.logQuery(q, args)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, fmt.Errorf("timeout driver: ctx never cancelled (test bug)")
		}
	}
	return c.script.respond(q, args)
}

type countHangConnector struct {
	script *sideScript
	hangs  int32
}

func (h *countHangConnector) Connect(context.Context) (driver.Conn, error) {
	return &countHangConn{script: h.script, hangs: h.hangs}, nil
}
func (h *countHangConnector) Driver() driver.Driver { return fakeDriver{} }
