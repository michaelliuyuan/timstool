package assess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// BUG-0934 anchors: pg_proc.prokind exists only on PG 11+, so scanFunctions
// must switch to a legacy query (no prokind) on old servers. A tiny stub
// driver serves canned results for SHOW server_version_num and the two
// function-catalog query shapes.

type stubDriver struct{ version string }

func (d stubDriver) Open(name string) (driver.Conn, error) { return stubConn{d.version}, nil }

// stubConn answers every query; the caller records what was asked.
type stubConn struct{ version string }

var lastQuery string

func (stubConn) Prepare(q string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (stubConn) Close() error                          { return nil }
func (stubConn) Begin() (driver.Tx, error)             { return nil, errors.New("not implemented") }

func (c stubConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	lastQuery = q
	if strings.Contains(q, "server_version_num") {
		if c.version == "fail" {
			return nil, errors.New("probe boom")
		}
		return &stubRows{cols: []string{"server_version_num"}, rows: [][]driver.Value{{c.version}}}, nil
	}
	if strings.Contains(q, "prokind") {
		// Modern shape: 7 columns; return one function row.
		return &stubRows{
			cols: []string{"nspname", "proname", "result", "lanname", "prosrc", "isproc", "ddl"},
			rows: [][]driver.Value{{"public", "f1", "integer", "plpgsql", "BEGIN END", false, "CREATE FUNCTION f1..."}},
		}, nil
	}
	// Legacy shape: 7 columns with literal FALSE; same row, FALSE column.
	return &stubRows{
		cols: []string{"nspname", "proname", "result", "lanname", "prosrc", "false", "ddl"},
		rows: [][]driver.Value{{"public", "f1", "integer", "plpgsql", "BEGIN END", false, "CREATE FUNCTION f1..."}},
	}, nil
}

type stubRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func openStub(t *testing.T, version string) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("stub-%s", version)
	sql.Register(name, stubDriver{version})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open stub: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestScanFunctionsModernUsesProkind(t *testing.T) {
	db := openStub(t, "150000")
	s := NewScanner(db, "public")
	fns, err := s.scanFunctions(context.Background())
	if err != nil {
		t.Fatalf("scanFunctions: %v", err)
	}
	if !strings.Contains(lastQuery, "prokind") {
		t.Errorf("modern server must use prokind query, got: %s", lastQuery)
	}
	if n := strings.Count(lastQuery, "ORDER BY"); n != 1 {
		t.Errorf("modern query must have exactly one ORDER BY, got %d: %s", n, lastQuery)
	}
	if len(fns) != 1 || fns[0].Name != "f1" {
		t.Errorf("expected one function row, got %+v", fns)
	}
}

func TestScanFunctionsLegacyOmitsProkind(t *testing.T) {
	db := openStub(t, "100012") // PG 10.12
	s := NewScanner(db, "public")
	fns, err := s.scanFunctions(context.Background())
	if err != nil {
		t.Fatalf("scanFunctions: %v", err)
	}
	if strings.Contains(lastQuery, "prokind") {
		t.Errorf("legacy server must NOT use prokind query, got: %s", lastQuery)
	}
	if !strings.Contains(lastQuery, "FALSE") {
		t.Errorf("legacy query must select literal FALSE, got: %s", lastQuery)
	}
	if n := strings.Count(lastQuery, "ORDER BY"); n != 1 {
		t.Errorf("legacy query must have exactly one ORDER BY, got %d: %s", n, lastQuery)
	}
	if len(fns) != 1 || fns[0].IsProcedure {
		t.Errorf("legacy functions must have IsProcedure=false, got %+v", fns)
	}
	// Version probe cached: second call keeps the legacy shape.
	lastQuery = ""
	if _, err := s.scanFunctions(context.Background()); err != nil {
		t.Fatalf("scanFunctions#2: %v", err)
	}
	if strings.Contains(lastQuery, "prokind") {
		t.Errorf("cached version must keep legacy query, got: %s", lastQuery)
	}
}

// Probe failure must fall back to the modern query so the original error
// surfaces from the catalog query itself (no silent legacy downgrade).
func TestScanFunctionsProbeFailureUsesModern(t *testing.T) {
	db := openStub(t, "fail")
	s := NewScanner(db, "public")
	if _, err := s.scanFunctions(context.Background()); err != nil {
		t.Fatalf("scanFunctions: %v", err)
	}
	if !strings.Contains(lastQuery, "prokind") {
		t.Errorf("probe failure must use the modern query, got: %s", lastQuery)
	}
	if s.pgVersion != 0 {
		t.Errorf("probe failure must not be cached, got pgVersion=%d", s.pgVersion)
	}
}
