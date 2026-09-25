package validator

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// F-05 anchor: a comma-separated key list must be quoted per column in both
// dialects — quoting the whole "col1, col2" string breaks every composite-key
// chunk query.
func TestQuoteOrderByColsComposite(t *testing.T) {
	pg := quoteOrderByCols("col1, col2", quotePG)
	if pg != `"col1", "col2"` {
		t.Errorf("PG composite ORDER BY = %s", pg)
	}
	my := quoteOrderByCols("col1, col2", quoteMySQL)
	if my != "`col1`, `col2`" {
		t.Errorf("MySQL composite ORDER BY = %s", my)
	}
	if got := quoteOrderByCols(" id ", quoteMySQL); got != "`id`" {
		t.Errorf("single column should be quoted and trimmed, got %s", got)
	}
}

// --- fake driver plumbing: pg fails every query, tidb accepts execs ---

type fakeConn struct {
	failQueries bool
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }

func (c *fakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.failQueries {
		return nil, errors.New("simulated COUNT failure")
	}
	return nil, errors.New("no rows expected in this test")
}

func (c *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

type fakeConnector struct {
	failQueries bool
}

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{failQueries: f.failQueries}, nil
}
func (f *fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

// F-05 anchor: when the PG COUNT query fails (DiffRows==0, Status=Fail) the
// checksum validation must return FAIL — the old code fell through the
// SourceRows==0 branch and reported PASS.
func TestChecksumCountErrorDoesNotPass(t *testing.T) {
	v := NewValidator(config.Config{})

	pgDB := sql.OpenDB(&fakeConnector{failQueries: true})
	defer pgDB.Close()
	tidbDB := sql.OpenDB(&fakeConnector{failQueries: false})
	defer tidbDB.Close()

	tr := v.validateChecksumChunked(context.Background(), pgDB, tidbDB, "t1")

	if tr.Status != reporter.StatusFail {
		t.Fatalf("COUNT failure must fail the table, got status=%s error=%q", tr.Status, tr.Error)
	}
	if tr.Error == "" {
		t.Fatal("failure must carry the COUNT error for triage")
	}
}
