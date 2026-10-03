package validator

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// MS-08 assembly anchor: NewValidator dispatches the source dialect on the
// NORMALIZED type (empty = postgres legacy default) and srcDriverName follows
// the assembled dialect.
func TestNewValidatorSourceDialectAssembly(t *testing.T) {
	pg := NewValidator(config.Config{})
	if _, ok := pg.srcDialect.(postgresDialect); !ok {
		t.Fatalf("empty config assembled %T; want postgresDialect (legacy default)", pg.srcDialect)
	}
	if pg.srcDriverName() != "pgx" {
		t.Fatalf("pg validator driver = %q; want pgx", pg.srcDriverName())
	}

	my := NewValidator(config.Config{Source: config.SourceConfig{Type: "mysql"}})
	if _, ok := my.srcDialect.(mysqlDialect); !ok {
		t.Fatalf("mysql config assembled %T; want mysqlDialect", my.srcDialect)
	}
	if my.srcDriverName() != "mysql" {
		t.Fatalf("mysql validator driver = %q; want mysql", my.srcDriverName())
	}

	// Unknown types are unreachable behind srcCapable; the assembly falls to
	// the PG shape (documented default, not a silent pass-through).
	if _, ok := NewValidator(config.Config{Source: config.SourceConfig{Type: "oracle"}}).srcDialect.(postgresDialect); !ok {
		t.Fatal("oracle config assembled a non-postgres dialect; unknown types must default to the PG shape")
	}
}

// MS-08 schema-default anchor: a MySQL source defaults an empty schema to the
// connection DATABASE (schema == database on MySQL), never to "public".
func TestMySQLSourceSchemaDefaultsToDatabase(t *testing.T) {
	my := NewValidator(config.Config{Source: config.SourceConfig{Type: "mysql", Database: "sales"}})
	if got := schemaOrDefault(my.sourceSchema()); got != "sales" {
		t.Fatalf("mysql empty-schema default = %q; want the connection database %q", got, "sales")
	}
	if got := my.sourceSchema(); got != "sales" {
		t.Fatalf("mysql sourceSchema = %q; want sales", got)
	}
	// Explicit schema keeps winning over the database default.
	my2 := NewValidator(config.Config{Source: config.SourceConfig{Type: "mysql", Database: "sales", Schema: "custom"}})
	if got := my2.sourceSchema(); got != "custom" {
		t.Fatalf("mysql explicit schema = %q; want custom", got)
	}
	// PG keeps the legacy public default.
	pg := NewValidator(config.Config{})
	if got := schemaOrDefault(pg.sourceSchema()); got != "public" {
		t.Fatalf("pg empty-schema default = %q; want public (legacy)", got)
	}
}

// MS-08 cross-parity anchor: the SAME logical value must normalize
// BYTE-IDENTICALLY whether it arrives in the pgx scan shape or the
// go-sql-driver scan shape (action-a verified shapes: VARCHAR/DECIMAL/
// UNSIGNED BIGINT -> []byte, DATETIME -> time.Time UTC).
func TestNormalizeValuePGMySQLParity(t *testing.T) {
	tsPG := time.Date(2026, 1, 2, 13, 0, 0, 0, time.UTC)
	tsMY := time.Date(2026, 1, 2, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		pg   interface{}
		my   interface{}
	}{
		{"varchar", "hello", []byte("hello")},
		{"decimal pg-text vs my-bytes", "10.50", []byte("10.50")},
		{"decimal negative", "-3.1400", []byte("-3.14")},
		{"datetime", tsPG, tsMY},
		{"int", int64(7), []byte("7")},
		{"unsigned bigint text vs uint64", "18446744073709551615", uint64(18446744073709551615)},
		{"null", nil, nil},
	}
	for _, c := range cases {
		if got, want := normalizeValue(c.pg), normalizeValue(c.my); got != want {
			t.Errorf("%s: pg shape %v (%q) != mysql shape %v (%q)", c.name, c.pg, got, c.my, want)
		}
	}
	// The uint64 case itself (MS-08 seam): formats decimally, not via %v.
	if got := normalizeValue(uint64(42)); got != "42" {
		t.Fatalf("normalizeValue(uint64(42)) = %q; want 42", got)
	}
}

// MS-08 watermark whitelist anchor: the MySQL probe speaks DATA_TYPE names.
func TestMySQLWatermarkWhitelist(t *testing.T) {
	for _, ok := range []string{"datetime", "timestamp", "date", "int", "bigint"} {
		if !wmAllowedColumnTypesMySQL[ok] {
			t.Errorf("mysql whitelist missing %q", ok)
		}
	}
	for _, bad := range []string{"varchar", "text", "float", "", "DATETIME"} {
		if wmAllowedColumnTypesMySQL[bad] {
			t.Errorf("mysql whitelist must not contain %q (case-sensitive DATA_TYPE)", bad)
		}
	}
}

// MS-08 dialect shape anchors: quoting, qualification and the `?`
// placeholder fragment (mirrors the tidb wire shapes with source-side
// two-part qualification).
func TestMySQLDialectShapes(t *testing.T) {
	d := mysqlDialect{}
	if got := d.QuoteIdent("t`x"); got != "`t``x`" {
		t.Fatalf("QuoteIdent = %q", got)
	}
	if got := d.Qualify("s", "t"); got != "`s`.`t`" {
		t.Fatalf("Qualify = %q; want two-part `s`.`t`", got)
	}
	wm := &config.WatermarkFilter{Column: "updated_at"}
	if got := d.WmPredicateFragment(wm); got != "`updated_at` <= ?" {
		t.Fatalf("WmPredicateFragment = %q; want `updated_at` <= ?", got)
	}
	if got := d.AdjustDSN("user:pass@tcp(1.2.3.4:3306)/db"); got != "user:pass@tcp(1.2.3.4:3306)/db" {
		t.Fatalf("AdjustDSN must be a no-op (Loc=UTC baked into BuildMySQLDSN); got %q", got)
	}
	// DSN dispatch: DSNByType keeps the PG DSN byte-identically and switches
	// to the driver DSN for mysql (the compare handler :590 propagation).
	sc := config.SourceConfig{Type: "mysql", Host: "h", Port: 3306, User: "u", Password: "p", Database: "db"}
	if got := sc.DSNByType(); got == sc.DSN() || got == "" {
		t.Fatalf("mysql DSNByType = %q; want a driver DSN distinct from the PG shape", got)
	}
	sc.Type = ""
	if got := sc.DSNByType(); got != sc.DSN() {
		t.Fatalf("empty-type DSNByType must equal the legacy DSN(); got %q want %q", got, sc.DSN())
	}
}

// MS-08 no-DB smoke: the MySQL ListTables probe uses the `?` placeholder and
// binds the schema as its single argument (a minimal stub records the query).
type listTablesStub struct {
	lastQuery string
	lastArgs  []driver.Value
}

func (s *listTablesStub) Connect(context.Context) (driver.Conn, error) { return s, nil }
func (s *listTablesStub) Driver() driver.Driver                        { return stubListDriver{} }
func (s *listTablesStub) Close() error                                 { return nil }
func (s *listTablesStub) Begin() (driver.Tx, error)                    { return nil, errors.New("no tx") }
func (s *listTablesStub) Prepare(q string) (driver.Stmt, error) {
	s.lastQuery = q
	return &listTablesStmt{stub: s}, nil
}

type stubListDriver struct{}

func (stubListDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unreachable") }

type listTablesStmt struct {
	stub  *listTablesStub
	query string
}

func (st *listTablesStmt) Close() error  { return nil }
func (st *listTablesStmt) NumInput() int { return -1 }
func (st *listTablesStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("no exec")
}
func (st *listTablesStmt) Query(args []driver.Value) (driver.Rows, error) {
	st.stub.lastArgs = args
	return &listTablesRows{}, nil
}

type listTablesRows struct {
	names []string
	i     int
}

func (r *listTablesRows) Columns() []string { return []string{"table_name"} }
func (r *listTablesRows) Close() error      { return nil }
func (r *listTablesRows) Next(dest []driver.Value) error {
	if r.i >= len(r.names) {
		return io.EOF
	}
	dest[0] = []byte(r.names[r.i])
	r.i++
	return nil
}

func TestMySQLListTablesPlaceholder(t *testing.T) {
	stub := &listTablesStub{}
	db := sql.OpenDB(stub)
	defer db.Close()
	if _, err := (mysqlDialect{}).ListTables(context.Background(), db, "sales"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.lastQuery, "table_schema = ?") {
		t.Fatalf("ListTables query uses the wrong placeholder:\n%s", stub.lastQuery)
	}
	if strings.Contains(stub.lastQuery, "$1") {
		t.Fatalf("ListTables query leaked a pgx $1 placeholder:\n%s", stub.lastQuery)
	}
	if len(stub.lastArgs) != 1 || stub.lastArgs[0] != "sales" {
		t.Fatalf("ListTables args = %v; want [sales]", stub.lastArgs)
	}
}
