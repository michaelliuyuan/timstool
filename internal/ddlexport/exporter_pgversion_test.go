package ddlexport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// BUG-0934 anchors for the exporter side: on PG < 11 the functions query
// must drop prokind and filter by language instead, and the procedures
// query must be skipped entirely. A stub driver records every query and
// serves canned rows for the version probe and the two catalog shapes.

type expStubDriver struct {
	version string
	queries *[]string
}

func (d expStubDriver) Open(name string) (driver.Conn, error) {
	return expStubConn{d.version, d.queries}, nil
}

type expStubConn struct {
	version string
	queries *[]string
}

var expMu sync.Mutex

func (expStubConn) Prepare(q string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (expStubConn) Close() error                           { return nil }
func (expStubConn) Begin() (driver.Tx, error)              { return nil, errors.New("not implemented") }

func (c expStubConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	expMu.Lock()
	*c.queries = append(*c.queries, q)
	expMu.Unlock()
	if strings.Contains(q, "server_version_num") {
		return &expStubRows{cols: []string{"server_version_num"}, rows: [][]driver.Value{{c.version}}}, nil
	}
	if strings.Contains(q, "prokind = 'f'") {
		return &expStubRows{
			cols: []string{"proname", "def"},
			rows: [][]driver.Value{{"f1", "CREATE FUNCTION public.f1() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$"}},
		}, nil
	}
	if strings.Contains(q, "prokind = 'p'") {
		return &expStubRows{
			cols: []string{"proname", "def"},
			rows: [][]driver.Value{{"p1", "CREATE PROCEDURE public.p1() LANGUAGE plpgsql AS $$ BEGIN END $$"}},
		}, nil
	}
	if strings.Contains(q, "lanname IN ('plpgsql', 'sql')") {
		return &expStubRows{
			cols: []string{"proname", "def"},
			rows: [][]driver.Value{{"f1", "CREATE FUNCTION public.f1() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$"}},
		}, nil
	}
	return &expStubRows{cols: []string{"proname", "def"}}, nil
}

type expStubRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *expStubRows) Columns() []string { return r.cols }
func (r *expStubRows) Close() error      { return nil }
func (r *expStubRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func openExpStub(t *testing.T, version string, queries *[]string) *Exporter {
	t.Helper()
	name := fmt.Sprintf("expstub-%s-%p", version, queries)
	sql.Register(name, expStubDriver{version, queries})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open stub: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewExporter(db, Options{Schemas: []string{"public"}, Types: TypeSet{Functions: true, Procedures: true}})
}

func TestExporterFunctionsLegacyDropsProkind(t *testing.T) {
	var queries []string
	e := openExpStub(t, "100012", &queries)
	files, err := e.schemaFiles(context.Background(), "public")
	if err != nil {
		t.Fatalf("schemaFiles: %v", err)
	}
	sawFunctions, sawProcedures := false, false
	for _, q := range queries {
		if strings.Contains(q, "lanname IN ('plpgsql', 'sql')") {
			sawFunctions = true
		}
		if strings.Contains(q, "prokind") {
			t.Errorf("legacy server must not query prokind, got: %s", q)
		}
		if strings.Contains(q, "procedures") || strings.Contains(q, "prokind = 'p'") {
			sawProcedures = true
		}
	}
	if !sawFunctions {
		t.Errorf("legacy server must run language-filtered functions query, saw: %v", queries)
	}
	if sawProcedures {
		t.Errorf("legacy server must skip the procedures query entirely, saw: %v", queries)
	}
	if ddl, ok := files["functions.sql"]; !ok || !strings.Contains(ddl, "CREATE FUNCTION public.f1") {
		t.Errorf("functions.sql must carry the exported function, got %q ok=%v", ddl, ok)
	}
	if _, ok := files["procedures.sql"]; ok {
		t.Error("legacy server must not emit procedures.sql")
	}
}

func TestExporterFunctionsModernUsesProkind(t *testing.T) {
	var queries []string
	e := openExpStub(t, "150000", &queries)
	files, err := e.schemaFiles(context.Background(), "public")
	if err != nil {
		t.Fatalf("schemaFiles: %v", err)
	}
	sawFunc, sawProc := false, false
	for _, q := range queries {
		if strings.Contains(q, "prokind = 'f'") {
			sawFunc = true
		}
		if strings.Contains(q, "prokind = 'p'") {
			sawProc = true
		}
	}
	if !sawFunc || !sawProc {
		t.Errorf("modern server must run prokind functions+procedures queries, saw: %v", queries)
	}
	if _, ok := files["procedures.sql"]; !ok {
		t.Error("modern server must emit procedures.sql")
	}
}
