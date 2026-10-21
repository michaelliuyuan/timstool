package validator

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// #ignore anchors: normalization (case/space), PK protection, the
// empty-comparable-set skip, the report note, and engine-path wiring.

// A1: ignoreSet normalizes — trims whitespace, lower-cases, drops empties;
// nil/empty config → empty set (zero regression).
func TestIgnoreSetNormalize(t *testing.T) {
	set := ignoreSet(config.CompareConfig{})
	if len(set) != 0 {
		t.Fatalf("empty config must yield empty set, got %v", set)
	}
	set = ignoreSet(config.CompareConfig{IgnoreColumns: []string{" created_at ", "Updated_At", "", " id "}})
	for want := range map[string]bool{"created_at": true, "updated_at": true, "id": true} {
		if !set[want] {
			t.Errorf("normalized set missing %q (set=%v)", want, set)
		}
	}
	if len(set) != 3 {
		t.Fatalf("expected 3 distinct normalized names, got %d (%v)", len(set), set)
	}
	if !ignoreConfigured(config.CompareConfig{IgnoreColumns: []string{"a"}}) {
		t.Fatal("configured list must be reported as configured")
	}
	if ignoreConfigured(config.CompareConfig{}) {
		t.Fatal("empty list must NOT be reported as configured")
	}
}

// A2: applyIgnoreSkips marks present ignore columns in skipCols; PK columns
// are protected (kept comparable) and surfaced in pkProtected.
func TestApplyIgnoreSkipsPKProtection(t *testing.T) {
	cols := colsFromNames(t, []string{"id", "created_at", "payload"})
	skipCols := map[int]bool{}
	applied, protected := applyIgnoreSkips(
		config.CompareConfig{IgnoreColumns: []string{"ID", "created_at"}},
		cols, []string{"id"}, skipCols)
	// "id" collides with PK → protected, NOT skipped; created_at skipped.
	if !skipCols[1] {
		t.Fatal("created_at must be skipped")
	}
	if skipCols[0] {
		t.Fatal("id (PK) must NOT be skipped")
	}
	if len(applied) != 1 || applied[0] != "created_at" {
		t.Fatalf("applied = %v, want [created_at]", applied)
	}
	if len(protected) != 1 || protected[0] != "id" {
		t.Fatalf("protected = %v, want [id]", protected)
	}
	// A column absent from the table is a no-op.
	applied2, _ := applyIgnoreSkips(config.CompareConfig{IgnoreColumns: []string{"nope"}}, cols, nil, skipCols)
	if len(applied2) != 0 {
		t.Fatalf("absent column must be a no-op, applied=%v", applied2)
	}
}

// A3: allColumnsSkipped — the empty-comparable-set guard.
func TestAllColumnsSkipped(t *testing.T) {
	cols := colsFromNames(t, []string{"a", "b"})
	if allColumnsSkipped(cols, map[int]bool{}) {
		t.Fatal("no skips must not read as all-skipped")
	}
	if allColumnsSkipped(cols, map[int]bool{0: true}) {
		t.Fatal("partial skips must not read as all-skipped")
	}
	if !allColumnsSkipped(cols, map[int]bool{0: true, 1: true}) {
		t.Fatal("all skips must be detected")
	}
}

// A4: ignoreColsNote / prependPKProtectedNote — the report disclosure shape.
func TestIgnoreColsNote(t *testing.T) {
	if n := ignoreColsNote(nil, nil); n != "" {
		t.Fatalf("empty note must be empty, got %q", n)
	}
	if n := ignoreColsNote([]string{"a", "b"}, nil); n != "排除字段: a, b" {
		t.Fatalf("applied note wrong: %q", n)
	}
	if n := ignoreColsNote(nil, []string{"id"}); n != "主键列不可排除: id" {
		t.Fatalf("protected note wrong: %q", n)
	}
	tr := prependPKProtectedNote(reporter.TableReport{Suggestion: "sampled"}, []string{"id"})
	if tr.Suggestion != "主键列不可排除: id；sampled" {
		t.Fatalf("prepend wrong: %q", tr.Suggestion)
	}
	tr = prependPKProtectedNote(reporter.TableReport{Suggestion: "sampled"}, nil)
	if tr.Suggestion != "sampled" {
		t.Fatalf("nil protected must not touch suggestion: %q", tr.Suggestion)
	}
}

// A5: tableIgnoreColumns resolves the global list against a table's PK.
func TestTableIgnoreColumns(t *testing.T) {
	applied, protected := tableIgnoreColumns(
		config.CompareConfig{IgnoreColumns: []string{"id", "updated_at"}},
		[]string{"id"})
	if len(applied) != 1 || applied[0] != "updated_at" {
		t.Fatalf("applied = %v", applied)
	}
	if len(protected) != 1 || protected[0] != "id" {
		t.Fatalf("protected = %v", protected)
	}
	applied, protected = tableIgnoreColumns(config.CompareConfig{}, []string{"id"})
	if len(applied) != 0 || len(protected) != 0 {
		t.Fatalf("empty config must be all-empty, applied=%v protected=%v", applied, protected)
	}
}

// A6: ErrNoComparableCols sentinel round-trips through errors.Is.
func TestErrNoComparableColsSentinel(t *testing.T) {
	if !errors.Is(ErrNoComparableCols, errNoComparableCols) {
		t.Fatal("exported sentinel must wrap the package sentinel")
	}
}

// A7: engine-path wiring — the hash_group no-PK strategy skips a table whose
// only comparable column is excluded (StatusSkip, not FAIL).
func TestApplyIgnoreNoPKEmptySetSkips(t *testing.T) {
	pgDB := sql.OpenDB(&fakeIgnoreConnector{conn: &fakeIgnoreConn{cols: []string{"id", "created_at"}}})
	defer pgDB.Close()
	tidbDB := sql.OpenDB(&fakeIgnoreConnector{conn: &fakeIgnoreConn{cols: []string{"id", "created_at"}}})
	defer tidbDB.Close()
	tidbConn, err := tidbDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("get tidb conn: %v", err)
	}
	defer tidbConn.Close()

	v := NewValidator(config.Config{})
	v.compareOverride = &config.CompareConfig{IgnoreColumns: []string{"id", "created_at"}}

	tr := v.validateSamplingWithHashGroup(context.Background(), pgDB, tidbConn, "t1", 1.0, reporter.TableReport{TableName: "t1"}, "public")
	if tr.Status != reporter.StatusSkip {
		t.Fatalf("empty comparable set must skip, got status=%s err=%q", tr.Status, tr.Error)
	}
	if tr.Error == "" {
		t.Fatal("skip must carry the disclosure reason")
	}
}

// --- scripted driver plumbing: serves scripted column metadata + rows so the
// engine reaches the skipCols construction (COUNT → DetectTableKey → SELECT *). ---

type ignoreRow struct {
	cols []string
	data [][]driver.Value
	idx  int
}

func (r *ignoreRow) Columns() []string { return r.cols }
func (r *ignoreRow) Close() error      { return nil }
func (r *ignoreRow) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}
func (r *ignoreRow) ColumnTypeDatabaseTypeName(i int) string { return "integer" }

type fakeIgnoreConn struct {
	cols []string
}

func (c *fakeIgnoreConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *fakeIgnoreConn) Close() error                        { return nil }
func (c *fakeIgnoreConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }

func (c *fakeIgnoreConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case containsSubstr(q, "SELECT COUNT(*)"):
		return &ignoreRow{cols: []string{"count"}, data: [][]driver.Value{{int64(1)}}}, nil
	case containsSubstr(q, "SELECT * FROM"):
		return &ignoreRow{cols: c.cols, data: [][]driver.Value{{"x", "y"}}}, nil
	}
	return &ignoreRow{cols: []string{"x"}, data: nil}, nil
}

func (c *fakeIgnoreConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

type fakeIgnoreConnector struct {
	conn *fakeIgnoreConn
}

func (f *fakeIgnoreConnector) Connect(context.Context) (driver.Conn, error) { return f.conn, nil }
func (f *fakeIgnoreConnector) Driver() driver.Driver                        { return fakeDriver{} }

// colsFromNames builds []*sql.ColumnType through a scripted Rows so the
// ColumnType objects carry the real Name() values (sql.ColumnType's fields
// are unexported — they must be obtained from a real rows query).
func colsFromNames(t *testing.T, names []string) []*sql.ColumnType {
	t.Helper()
	db := sql.OpenDB(&fakeIgnoreConnector{conn: &fakeIgnoreConn{cols: names}})
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT * FROM x")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("column types: %v", err)
	}
	return cts
}

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
