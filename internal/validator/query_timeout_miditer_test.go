package validator

// query_timeout_miditer_test.go anchors the #t95 B1-A minor1 fix: a
// mid-iteration driver deadline (rows.Next() drains then rows.Err() reports
// context.DeadlineExceeded) must surface with the "query timeout" disclosure
// — not a bare deadline error — so ops can triage a slow hash scan as a
// timeout instead of an opaque failure.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// iterErrRows serves its rows then returns err from the NEXT iteration call
// after the last row — mirroring how a real driver surfaces a mid-scan
// deadline (Next() returns the error, which database/sql funnels into
// Rows.Err() for the caller to read after the loop). Err() reports it too.
type iterErrRows struct {
	cols     []string
	colTypes []string
	rows     [][]driver.Value
	pos      int
	err      error
}

func (r *iterErrRows) Columns() []string                         { return r.cols }
func (r *iterErrRows) Close() error                              { return nil }
func (r *iterErrRows) ColumnTypeDatabaseTypeName(idx int) string { return r.colTypes[idx] }
func (r *iterErrRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return r.err
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}
func (r *iterErrRows) Err() error { return r.err }

// midIterConn wraps a sideScript but poisons the chunk window rows with a
// mid-iteration context deadline — every "SELECT * LIMIT" window is served
// normally and then reports DeadlineExceeded through Err().
type midIterConn struct {
	script *sideScript
}

func (c *midIterConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *midIterConn) Close() error                        { return nil }
func (c *midIterConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *midIterConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}
func (c *midIterConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.script.respond(q, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(q, "SELECT *") && strings.Contains(q, "LIMIT") {
		if sr, ok := rows.(*scriptedRows); ok {
			return &iterErrRows{cols: sr.cols, colTypes: sr.colTypes, rows: sr.rows, err: context.DeadlineExceeded}, nil
		}
	}
	return rows, nil
}

type midIterConnector struct{ script *sideScript }

func (c *midIterConnector) Connect(context.Context) (driver.Conn, error) {
	return &midIterConn{script: c.script}, nil
}
func (c *midIterConnector) Driver() driver.Driver { return fakeDriver{} }

// TestComputeChunkHashPGMidIterationTimeoutDiscloses: a keyset chunk window
// whose scan hits the driver deadline mid-iteration returns an error that
// unwraps to context.DeadlineExceeded AND carries the "query timeout"
// disclosure (no bare "context deadline exceeded").
func TestComputeChunkHashPGMidIterationTimeoutDiscloses(t *testing.T) {
	src := &sideScript{count: 3, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "b"})}
	pgDB := sql.OpenDB(&midIterConnector{script: src})
	defer pgDB.Close()

	v := NewValidator(config.Config{Compare: config.CompareConfig{QueryTimeout: "100ms"}})
	_, _, _, fetched, err := v.computeChunkHashPG(context.Background(), pgDB, "db", "t1", "id", nil, 2, nil)
	if err == nil {
		t.Fatalf("mid-iteration deadline must error, got nil (fetched=%d)", fetched)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to unwrap to context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "query timeout") {
		t.Fatalf("err = %q, must disclose 'query timeout'", err.Error())
	}
}
