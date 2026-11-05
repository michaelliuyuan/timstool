package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// vScriptConn scripts ONE side of the validation DBs (source or target):
// it serves COUNT(*) as a constant, optionally sleeps per query (the
// concurrency timing anchor), and can fail the SOURCE count for a named
// table (the failed-table anchor). These anchors run row-count-only
// (sampleSize=0), so no SELECT/SAMPLE queries are ever issued.
type vScriptConn struct {
	sleep   time.Duration
	count   int64
	isSrc   bool
	failSrc map[string]error // table -> source COUNT error
}

func (c *vScriptConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *vScriptConn) Close() error                        { return nil }
func (c *vScriptConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *vScriptConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, fmt.Errorf("unsupported")
}

var vFromRe = regexp.MustCompile("FROM `([^`]+)`")

func (c *vScriptConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.sleep > 0 {
		select {
		case <-time.After(c.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m := vFromRe.FindStringSubmatch(q)
	if m == nil || !strings.Contains(q, "COUNT(*)") {
		return nil, fmt.Errorf("unsupported query: %s", q)
	}
	if c.isSrc {
		if perr, ok := c.failSrc[m[1]]; ok {
			return nil, perr
		}
	}
	return &vScriptRows{count: c.count}, nil
}

type vScriptConnector struct{ cfg vScriptConn }

func (f *vScriptConnector) Connect(context.Context) (driver.Conn, error) {
	return &vScriptConn{
		sleep:   f.cfg.sleep,
		count:   f.cfg.count,
		isSrc:   f.cfg.isSrc,
		failSrc: f.cfg.failSrc,
	}, nil
}
func (f *vScriptConnector) Driver() driver.Driver { return fakeTargetDriver{} }

type fakeTargetDriver struct{}

func (fakeTargetDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type vScriptRows struct {
	count int64
	done  bool
}

func (r *vScriptRows) Columns() []string { return []string{"count"} }
func (r *vScriptRows) Close() error      { return nil }
func (r *vScriptRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.count
	return nil
}

func vSrcDB(t *testing.T, cfg vScriptConn) *sql.DB {
	t.Helper()
	cfg.isSrc = true
	db := sql.OpenDB(&vScriptConnector{cfg: cfg})
	t.Cleanup(func() { db.Close() })
	return db
}

func vTgtDB(t *testing.T, cfg vScriptConn) *sql.DB {
	t.Helper()
	cfg.isSrc = false
	db := sql.OpenDB(&vScriptConnector{cfg: cfg})
	t.Cleanup(func() { db.Close() })
	return db
}

func vSchema(names ...string) *source.Schema {
	tables := make([]source.Table, len(names))
	for i, n := range names {
		tables[i] = source.Table{Schema: "db", Name: n}
	}
	return &source.Schema{Tables: tables}
}

// TestValidateMigrationParallelTiming (并发时序锚): 12 tables × 2 COUNT
// queries (sampleSize=0) × 40ms sleep. With validateParallel=1 the wall
// time ≈ 12×80ms ≈ 960ms; with the pool at 4 it ≈ 3×80ms ≈ 240ms — the
// parallel run must be clearly faster (≤ ½ the serial time). #t95 P0-5.
func TestValidateMigrationParallelTiming(t *testing.T) {
	const (
		tables = 12
		sleep  = 40 * time.Millisecond
	)
	names := make([]string, tables)
	for i := range names {
		names[i] = fmt.Sprintf("t%d", i+1)
	}
	prev := validateParallel
	defer func() { validateParallel = prev }()

	src := vSrcDB(t, vScriptConn{count: 7, sleep: sleep})
	tgt := vTgtDB(t, vScriptConn{count: 7, sleep: sleep})

	validateParallel = 1
	start := time.Now()
	if _, err := ValidateMigration(context.Background(), src, tgt, vSchema(names...), 0); err != nil {
		t.Fatalf("serial ValidateMigration: %v", err)
	}
	serial := time.Since(start)

	validateParallel = 4
	start = time.Now()
	if _, err := ValidateMigration(context.Background(), src, tgt, vSchema(names...), 0); err != nil {
		t.Fatalf("parallel ValidateMigration: %v", err)
	}
	parallel := time.Since(start)

	if serial < 700*time.Millisecond {
		t.Fatalf("serial anchor too fast (%dms) — sleep too low for a reliable timing anchor", serial.Milliseconds())
	}
	if parallel*2 >= serial {
		t.Fatalf("parallel run (%dms) not ~¼ of serial (%dms) — concurrency pool ineffective", parallel.Milliseconds(), serial.Milliseconds())
	}
}

// TestValidateMigrationPerTableHookExactlyOnce: the per-table completion
// hook fires EXACTLY once per table with the settled rows — the progress
// surface the orchestrator checkpoint depends on. #t95 P0-5.
func TestValidateMigrationPerTableHookExactlyOnce(t *testing.T) {
	defer SetTableValidatedHook(nil)
	var (
		mu    sync.Mutex
		calls = map[string]int{}
		rows  = map[string]int64{}
	)
	SetTableValidatedHook(func(tv TableValidation, err error) {
		mu.Lock()
		defer mu.Unlock()
		calls[tv.Name]++
		rows[tv.Name] = tv.SourceRows
	})
	prev := validateParallel
	defer func() { validateParallel = prev }()
	validateParallel = 4

	src := vSrcDB(t, vScriptConn{count: 7})
	tgt := vTgtDB(t, vScriptConn{count: 7})
	rpt, err := ValidateMigration(context.Background(), src, tgt, vSchema("t1", "t2", "t3", "t4"), 0)
	if err != nil {
		t.Fatalf("ValidateMigration: %v", err)
	}
	if rpt.TotalTables != 4 || rpt.FailedTables != 0 || !rpt.AllPassed {
		t.Fatalf("report = %+v, want 4/0/all-passed", rpt)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"t1", "t2", "t3", "t4"} {
		if calls[name] != 1 {
			t.Fatalf("table %s hook calls = %d, want exactly 1", name, calls[name])
		}
		if rows[name] != 7 {
			t.Fatalf("table %s rows = %d, want 7", name, rows[name])
		}
	}
	if len(calls) != 4 {
		t.Fatalf("hook fired for %d tables, want 4", len(calls))
	}
}

// TestValidateMigrationFailedTableFailsHook (失败表锚): a table whose
// source COUNT errors still gets its hook call (with the error), and
// ValidateMigration returns (nil, err) with the same per-table message as
// the serial implementation — the report contract is unchanged. #t95 P0-5.
func TestValidateMigrationFailedTableFailsHook(t *testing.T) {
	defer SetTableValidatedHook(nil)
	var (
		mu  sync.Mutex
		got = map[string]error{}
	)
	SetTableValidatedHook(func(tv TableValidation, err error) {
		mu.Lock()
		defer mu.Unlock()
		got[tv.Name] = err
	})
	prev := validateParallel
	defer func() { validateParallel = prev }()
	validateParallel = 4

	boom := errors.New("simulated source count failure")
	src := vSrcDB(t, vScriptConn{count: 7, failSrc: map[string]error{"bad": boom}})
	tgt := vTgtDB(t, vScriptConn{count: 7})

	rpt, err := ValidateMigration(context.Background(), src, tgt, vSchema("ok1", "bad", "ok2"), 0)
	if err == nil {
		t.Fatalf("expected error, got report %+v", rpt)
	}
	if rpt != nil {
		t.Fatalf("failed validation must return nil report, got %+v", rpt)
	}
	if !strings.Contains(err.Error(), `validate: count source "bad"`) {
		t.Fatalf("err = %v, want per-table count-source message", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err must wrap the underlying error (errors.Is), got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"ok1", "bad", "ok2"} {
		if _, seen := got[name]; !seen {
			t.Fatalf("hook not fired for %s", name)
		}
	}
	if got["bad"] == nil || !errors.Is(got["bad"], boom) {
		t.Fatalf("bad table hook err = %v, want the simulated error", got["bad"])
	}
	if got["ok1"] != nil || got["ok2"] != nil {
		t.Fatalf("ok tables hook err = %v/%v, want nil", got["ok1"], got["ok2"])
	}
}

// TestValidateMigrationParallelOneCompatible (parallel=1 兼容): with the
// pool at 1 the run is fully serial and the report preserves schema order —
// the deterministic output a pool-1 deployment (or the orchestrator's
// report logging) relies on. #t95 P0-5.
func TestValidateMigrationParallelOneCompatible(t *testing.T) {
	prev := validateParallel
	defer func() { validateParallel = prev }()
	validateParallel = 1

	src := vSrcDB(t, vScriptConn{count: 7})
	tgt := vTgtDB(t, vScriptConn{count: 7})
	rpt, err := ValidateMigration(context.Background(), src, tgt, vSchema("t1", "t2", "t3"), 0)
	if err != nil {
		t.Fatalf("ValidateMigration: %v", err)
	}
	if rpt.TotalTables != 3 || rpt.FailedTables != 0 || !rpt.AllPassed {
		t.Fatalf("report = %+v, want 3/0/all-passed", rpt)
	}
	for i, want := range []string{"t1", "t2", "t3"} {
		if rpt.Tables[i].Name != want {
			t.Fatalf("report order mismatch at %d: %s vs %s (pool=1 must stay deterministic)", i, rpt.Tables[i].Name, want)
		}
	}
}

// TestValidateMigrationConcurrentReportConsistency (rpt 竞态锚): 16 tables
// on a pool of 8 — every table lands in the report exactly once, every hook
// fires exactly once, and no duplicate/missing entries. Run under -race it
// is the concurrent-report race detector. #t95 P0-5.
func TestValidateMigrationConcurrentReportConsistency(t *testing.T) {
	defer SetTableValidatedHook(nil)
	names := make([]string, 16)
	for i := range names {
		names[i] = fmt.Sprintf("t%02d", i)
	}
	prev := validateParallel
	defer func() { validateParallel = prev }()
	validateParallel = 8

	var (
		mu    sync.Mutex
		hooks = map[string]int{}
	)
	SetTableValidatedHook(func(tv TableValidation, _ error) {
		mu.Lock()
		defer mu.Unlock()
		hooks[tv.Name]++
	})

	src := vSrcDB(t, vScriptConn{count: 5})
	tgt := vTgtDB(t, vScriptConn{count: 5})
	rpt, err := ValidateMigration(context.Background(), src, tgt, vSchema(names...), 0)
	if err != nil {
		t.Fatalf("ValidateMigration: %v", err)
	}
	if rpt.TotalTables != 16 || rpt.FailedTables != 0 || !rpt.AllPassed {
		t.Fatalf("report = %+v, want 16/0/all-passed", rpt)
	}
	seen := map[string]bool{}
	for _, tv := range rpt.Tables {
		if seen[tv.Name] {
			t.Fatalf("duplicate report entry %s", tv.Name)
		}
		seen[tv.Name] = true
	}
	if len(seen) != 16 {
		t.Fatalf("report holds %d distinct tables, want 16", len(seen))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hooks) != 16 {
		t.Fatalf("hook fired for %d tables, want 16", len(hooks))
	}
	for _, n := range names {
		if hooks[n] != 1 {
			t.Fatalf("%s hook calls = %d, want 1", n, hooks[n])
		}
	}
}
