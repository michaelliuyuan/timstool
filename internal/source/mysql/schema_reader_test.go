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
	// The stub emulates what a REAL 8.0.13+ server returns when the two
	// key-part columns are selected separately: plain parts carry
	// COLUMN_NAME with EXPRESSION NULL; functional parts carry COLUMN_NAME
	// NULL and the EXPRESSION original — in BOTH shapes seen in the wild:
	// parenthesized (fixtures) and the deployed-server form WITHOUT outer
	// parens (black-box seq848: lower(`a-b`)).
	return &idxStubRows{
		cols: []string{"INDEX_NAME", "COLUMN_NAME", "EXPRESSION", "NON_UNIQUE", "SEQ_IN_INDEX"},
		rows: [][]driver.Value{
			// Regular column part.
			{"idx_a", "a", nil, int64(1), int64(1)},
			// Functional key part, parenthesized fixture shape.
			{"idx_a", nil, "(`a` + 1)", int64(1), int64(2)},
			{"idx_fn", nil, "(lower(`b`))", int64(0), int64(1)},
			{"idx_live", nil, "lower(`a-b`)", int64(1), int64(1)},
			{"PRIMARY", "id", nil, int64(0), int64(1)},
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

// Functional key parts must survive the scan and arrive TYPE-AWARE
// (MS-10c2 pen-5): COLUMN_NAME NULL ⇒ EXPRESSION original in Value with
// IsExpression=true — including the deployed-server shape WITHOUT outer
// parens (black-box seq848); plain columns carry IsExpression=false.
// PRIMARY stays skipped.
func TestReadIndexesSurvivesFunctionalKeyParts(t *testing.T) {
	r := &schemaReader{src: &Source{}}
	indexes, err := r.readIndexes(context.Background(), openIdxStub(t), "db1", "t1")
	if err != nil {
		t.Fatalf("readIndexes: %v", err)
	}
	if len(indexes) != 3 {
		t.Fatalf("expected idx_a + idx_fn + idx_live (PRIMARY skipped), got %+v", indexes)
	}
	byName := map[string]sourceIndex{}
	for _, idx := range indexes {
		byName[idx.Name] = idx
	}
	if len(byName["idx_a"].Parts) != 2 || byName["idx_a"].Unique {
		t.Errorf("idx_a = %+v (two parts, non-unique)", byName["idx_a"])
	}
	if byName["idx_a"].Parts[0].IsExpression || byName["idx_a"].Parts[0].Value != "a" {
		t.Errorf("plain part must be column-flagged: %+v", byName["idx_a"].Parts[0])
	}
	if !byName["idx_a"].Parts[1].IsExpression || byName["idx_a"].Parts[1].Value != "(`a` + 1)" {
		t.Errorf("mixed functional part must be expression-flagged: %+v", byName["idx_a"].Parts[1])
	}
	fn := byName["idx_fn"]
	if len(fn.Parts) != 1 || !fn.Unique {
		t.Errorf("idx_fn = %+v (one functional part, unique)", fn)
	}
	if !fn.Parts[0].IsExpression || fn.Parts[0].Value != "(lower(`b`))" {
		t.Errorf("functional part must carry the expression original with the flag, got %+v", fn.Parts[0])
	}
	// Deployed-server shape: EXPRESSION WITHOUT outer parens stays
	// verbatim in Value (the renderer canonicalizes the wrapping).
	live := byName["idx_live"]
	if len(live.Parts) != 1 || !live.Parts[0].IsExpression || live.Parts[0].Value != "lower(`a-b`)" {
		t.Errorf("no-paren live shape must be preserved verbatim + flagged, got %+v", live.Parts[0])
	}
}

// SQL shape anchor (pen-5): the STATISTICS query selects COLUMN_NAME and
// EXPRESSION as SEPARATE columns — the type-aware flag is COLUMN_NAME IS
// NULL, not a string-shape guess (the old COALESCE probe retired).
func TestReadIndexesSelectsFunctionalPartsSeparately(t *testing.T) {
	r := &schemaReader{src: &Source{}}
	if _, err := r.readIndexes(context.Background(), openIdxStub(t), "db1", "t1"); err != nil {
		t.Fatalf("readIndexes: %v", err)
	}
	if !strings.Contains(idxStubQuery, "INDEX_NAME, COLUMN_NAME, EXPRESSION, NON_UNIQUE, SEQ_IN_INDEX") {
		t.Errorf("STATISTICS query must select the two key-part columns separately, got: %s", idxStubQuery)
	}
	if strings.Contains(idxStubQuery, "COALESCE") {
		t.Errorf("COALESCE string-shape probe retired (pen-5), got: %s", idxStubQuery)
	}
	if !strings.Contains(idxStubQuery, "TABLE_SCHEMA = ?") {
		t.Errorf("STATISTICS query must stay parameterized, got: %s", idxStubQuery)
	}
}
