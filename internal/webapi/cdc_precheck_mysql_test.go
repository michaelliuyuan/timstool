package webapi

// MS-11b pen 1 anchors: the MySQL precheck probes that inline values (some
// MySQL 8 builds refuse the `?` placeholder inside SHOW/information_schema
// shapes) — literal renderers + the master-status scan shapes (5-column with
// *any discard sinks, 2-column fallback). Same-source renderer-test paradigm
// as TestIncLogsRenderersQuoteFragments.

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestMySQLVarSQLLiteralForm(t *testing.T) {
	for _, name := range []string{"log_bin", "binlog_format", "binlog_row_image"} {
		got := mysqlVarSQL(name)
		want := "SHOW GLOBAL VARIABLES LIKE '" + name + "'"
		if got != want {
			t.Fatalf("mysqlVarSQL(%q) = %q, want %q", name, got, want)
		}
		if strings.Contains(got, "?") {
			t.Fatalf("parameterized form survived: %q", got)
		}
	}
}

func TestMySQLNoPKSQLLiteralEscaping(t *testing.T) {
	got := mysqlNoPKSQL("migration_test")
	if strings.Contains(got, "TABLE_SCHEMA = ?") {
		t.Fatalf("parameterized form survived:\n%s", got)
	}
	if !strings.Contains(got, "TABLE_SCHEMA = 'migration_test'") {
		t.Fatalf("schema literal missing:\n%s", got)
	}
	// Quote in the schema name must be ''-doubled (injection shape closes).
	evil := mysqlNoPKSQL("db' --")
	if !strings.Contains(evil, "'db'' --'") {
		t.Fatalf("schema quote not doubled:\n%s", evil)
	}
	if strings.Contains(evil, "' --'") && !strings.Contains(evil, "'' --") {
		t.Fatalf("unescaped quote leaked:\n%s", evil)
	}
}

// fakeRowScan captures the dest slice handed to Scan.
type fakeRowScan struct {
	scanErr error
	capture []any
}

func (f *fakeRowScan) Scan(dest ...any) error {
	f.capture = dest
	return f.scanErr
}

func TestScanMasterStatus5DiscardSinks(t *testing.T) {
	row := &fakeRowScan{}
	file, pos, err := scanMasterStatus5(row)
	if err != nil || file != "" || pos.Valid {
		t.Fatalf("zero row: %q %v %v", file, pos, err)
	}
	if len(row.capture) != 5 {
		t.Fatalf("5-column scan dests = %d, want 5", len(row.capture))
	}
	// The three trailing dests must be non-nil pointers (nil dests panic on
	// some drivers — the MS-11b fix).
	for i, d := range row.capture[2:] {
		if d == nil {
			t.Fatalf("dest %d is nil — nil dest form must not return", i+2)
		}
	}
	// A column-count mismatch surfaces as the error that drives the fallback.
	mismatch := errors.New("sql: expected 5 destination arguments in Scan, not 2")
	if _, _, err := scanMasterStatus5(&fakeRowScan{scanErr: mismatch}); err == nil {
		t.Fatal("mismatched scan must error so the 2-column fallback runs")
	}
}

func TestScanMasterStatus2FallbackShape(t *testing.T) {
	row := &fakeRowScan{}
	if _, _, err := scanMasterStatus2(row); err != nil {
		t.Fatal(err)
	}
	if len(row.capture) != 2 {
		t.Fatalf("2-column scan dests = %d, want 2", len(row.capture))
	}
	// sql.ErrNoRows must stay distinguishable (explicit empty-result error).
	if _, _, err := scanMasterStatus5(&fakeRowScan{scanErr: sql.ErrNoRows}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("ErrNoRows must pass through untouched")
	}
}
