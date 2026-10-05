package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// F-15 anchors: readIndexes must survive functional key parts (MySQL
// 8.0.13+ STATISTICS rows carry COLUMN_NAME NULL / EXPRESSION non-NULL).
// Before the fix the NULL scan aborted the whole /sources/tables listing
// with "Scan error COLUMN_NAME: converting NULL to string"; the SQL-side
// COALESCE(COLUMN_NAME, EXPRESSION) is byte-shape identical to the MS-10b
// assess scanner probe.

type idxStubDriver struct{}

func (idxStubDriver) Open(string) (driver.Conn, error) { return idxStubConn{}, nil }

type idxStubConn struct{}

var idxStubQuery string

func (idxStubConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("not implemented") }
func (idxStubConn) Close() error                        { return nil }
func (idxStubConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("not implemented") }

func (idxStubConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	idxStubQuery = q
	// The stub emulates what a REAL 8.0.13+ server returns for the
	// COALESCE(COLUMN_NAME, EXPRESSION) select: the functional part arrives
	// as its expression text, never NULL.
	return &idxStubRows{
		cols: []string{"INDEX_NAME", "COLUMN_NAME", "NON_UNIQUE", "SEQ_IN_INDEX"},
		rows: [][]driver.Value{
			// Regular column part.
			{"idx_a", "a", int64(1), int64(1)},
			// Functional key part coalesced to its expression text.
			{"idx_a", "(`a` + 1)", int64(1), int64(2)},
			{"idx_fn", "(lower(`b`))", int64(0), int64(1)},
			{"PRIMARY", "id", int64(0), int64(1)},
		},
	}, nil
}

type idxStubRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *idxStubRows) Columns() []string { return r.cols }
func (r *idxStubRows) Close() error      { return nil }
func (r *idxStubRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func openIdxStub(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("idx-stub-%d", len(idxStubNames))
	idxStubNames = append(idxStubNames, name)
	sql.Register(name, idxStubDriver{})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open stub: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var idxStubNames []string

type sourceIndex = source.Index

// Functional key parts must arrive as expression text (server-side
// COALESCE), enter the Columns list, and never abort the scan; PRIMARY
// stays skipped.
func TestReadIndexesSurvivesFunctionalKeyParts(t *testing.T) {
	r := &schemaReader{src: &Source{}}
	indexes, err := r.readIndexes(context.Background(), openIdxStub(t), "db1", "t1")
	if err != nil {
		t.Fatalf("readIndexes: %v", err)
	}
	if len(indexes) != 2 {
		t.Fatalf("expected idx_a + idx_fn (PRIMARY skipped), got %+v", indexes)
	}
	byName := map[string]sourceIndex{}
	for _, idx := range indexes {
		byName[idx.Name] = idx
	}
	if len(byName["idx_a"].Columns) != 2 || byName["idx_a"].Unique {
		t.Errorf("idx_a = %+v (two parts, non-unique)", byName["idx_a"])
	}
	if len(byName["idx_fn"].Columns) != 1 || !byName["idx_fn"].Unique {
		t.Errorf("idx_fn = %+v (one functional part, unique)", byName["idx_fn"])
	}
	if byName["idx_fn"].Columns[0] != "(lower(`b`))" {
		t.Errorf("functional part must carry the expression text, got %q", byName["idx_fn"].Columns[0])
	}
}

// SQL shape anchor: the COALESCE probe must ride the STATISTICS query.
func TestReadIndexesSQLCoalescesFunctionalParts(t *testing.T) {
	r := &schemaReader{src: &Source{}}
	if _, err := r.readIndexes(context.Background(), openIdxStub(t), "db1", "t1"); err != nil {
		t.Fatalf("readIndexes: %v", err)
	}
	if !strings.Contains(idxStubQuery, "COALESCE(COLUMN_NAME, EXPRESSION)") {
		t.Errorf("STATISTICS query must coalesce functional key parts, got: %s", idxStubQuery)
	}
	if !strings.Contains(idxStubQuery, "TABLE_SCHEMA = ?") {
		t.Errorf("STATISTICS query must stay parameterized, got: %s", idxStubQuery)
	}
}
