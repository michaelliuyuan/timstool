package webapi

// MS-11b pen 1 anchors: the MySQL precheck probes that inline values (some
// MySQL 8 builds refuse the `?` placeholder inside SHOW/information_schema
// shapes) — literal renderers + the master-status scan shapes (5-column with
// *any discard sinks, 2-column fallback). Same-source renderer-test paradigm
// as TestIncLogsRenderersQuoteFragments.

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"gopkg.in/yaml.v3"
)

func TestCDCConfigPutCDCSubfieldsPersist(t *testing.T) {
	// MS-11b pen 3: PUT /cdc/config with a cdc sub-object must persist
	// server_id / mode to config.yaml (previously never written — prod had
	// to hand-edit the file).
	s, _, _ := newCDCServer(t)

	// Persist both fields.
	w, req := doReq("PUT", "/api/v1/cdc/config", `{"cdc":{"server_id":910808,"mode":"incr_only"}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("put cdc = %d body=%s", w.Code, w.Body.String())
	}
	cfg, err := config.Load(s.cdcCfgFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CDC.ServerID != 910808 || cfg.CDC.Mode != "incr_only" {
		t.Fatalf("cdc fields not persisted: %+v", cfg.CDC)
	}
	// Comments/neighbors survive (writeCDCConfig is a structured round-trip).
	raw, _ := os.ReadFile(s.cdcCfgFile())
	if !strings.Contains(string(raw), "slot_name: pg2tidb_cdc") {
		t.Fatalf("neighbor cdc field lost:\n%s", raw)
	}

	// cdc-only PUT is valid (no source/target needed).
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"cdc":{"server_id":42}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cdc-only put = %d %s", w.Code, w.Body.String())
	}
	cfg, _ = config.Load(s.cdcCfgFile())
	if cfg.CDC.ServerID != 42 || cfg.CDC.Mode != "incr_only" {
		t.Fatalf("partial cdc put clobbered: %+v", cfg.CDC)
	}

	// Guards: server_id <= 0 and unknown mode are 400, never a silent zero.
	for _, body := range []string{
		`{"cdc":{"server_id":0}}`,
		`{"cdc":{"server_id":-1}}`,
		`{"cdc":{"mode":"yolo"}}`,
	} {
		w, req = doReq("PUT", "/api/v1/cdc/config", body)
		s.router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("guard body %s = %d (want 400)", body, w.Code)
		}
	}

	// MS-11b pen 5: upper bound — 2^32 overflows uint32 truncation, must 400.
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"cdc":{"server_id":4294967296}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("server_id 2^32 = %d (want 400)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "server_id") {
		t.Fatalf("upper-bound guard must name server_id, got %s", w.Body.String())
	}
	// 2^32-1 is the uint32 ceiling and must pass.
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"cdc":{"server_id":4294967295}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("server_id 2^32-1 = %d (want 200): %s", w.Code, w.Body.String())
	}
	cfg, _ = config.Load(s.cdcCfgFile())
	if cfg.CDC.ServerID != 4294967295 {
		t.Fatalf("boundary server_id not stored verbatim: %d", cfg.CDC.ServerID)
	}
	// Restore the small value so the zero-mutation check below stays sharp.
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"cdc":{"server_id":42}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("restore put = %d", w.Code)
	}

	cfg, _ = config.Load(s.cdcCfgFile())
	if cfg.CDC.ServerID != 42 || cfg.CDC.Mode != "incr_only" {
		t.Fatalf("rejected put mutated config: %+v", cfg.CDC)
	}

	// Section auto-create: a config.yaml without a cdc section gains one.
	plain := filepath.Join(t.TempDir(), "config2.yaml")
	os.WriteFile(plain, []byte("source:\n  host: a\n  port: 1\ntarget:\n  host: b\n  port: 2\n"), 0o600)
	s.SetCDCConfigFile(plain)
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"cdc":{"server_id":7}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("section-create put = %d %s", w.Code, w.Body.String())
	}
	cfg2, err := config.Load(plain)
	if err != nil || cfg2.CDC.ServerID != 7 {
		t.Fatalf("cdc section not created: %+v %v", cfg2.CDC, err)
	}
	// MS-11e E3: the materialized section carries the mode default too
	// (full write path parity — a section with server_id but no mode would
	// silently depend on Load-time defaults forever).
	rawPlain, _ := os.ReadFile(plain)
	if !strings.Contains(string(rawPlain), "mode: full_incr") {
		t.Fatalf("materialized cdc section missing mode default:\n%s", rawPlain)
	}
	// And it round-trips as yaml (not a stringified int).
	var doc yaml.Node
	raw2, _ := os.ReadFile(plain)
	_ = yaml.Unmarshal(raw2, &doc)
	if !strings.Contains(string(raw2), "server_id: 7") {
		t.Fatalf("server_id not written as int scalar:\n%s", raw2)
	}

	// MS-11b pen 6: a hand-edited non-mapping / case-variant `cdc:` key must
	// be refused (400), never rewritten by appending a duplicate key (would
	// break the next load). yaml struct unmarshal is case-insensitive, so
	// `CDC:` loads into cfg.CDC while mappingValue("cdc") misses it — the
	// reachable duplicate-append path.
	broken := filepath.Join(t.TempDir(), "config3.yaml")
	os.WriteFile(broken, []byte("source:\n  host: a\n  port: 1\ntarget:\n  host: b\n  port: 2\nCDC: oops\n"), 0o600)
	s.SetCDCConfigFile(broken)
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"cdc":{"server_id":9}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("scalar cdc section = %d (want 400): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cdc") {
		t.Fatalf("refusal must name the cdc section: %s", w.Body.String())
	}
	// File untouched — no duplicate `cdc:` key appended.
	raw3, _ := os.ReadFile(broken)
	if strings.Count(strings.ToLower(string(raw3)), "cdc:") != 1 {
		t.Fatalf("duplicate cdc key appended (config would break next load):\n%s", raw3)
	}
}

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
	// MS-11e pen 3: anti-join shape — LEFT JOIN TABLE_CONSTRAINTS with the
	// CONSTRAINT_NAME = 'PRIMARY' predicate in the ON clause and an
	// IS NULL filter (the NOT EXISTS KEY_COLUMN_USAGE form misreported
	// on te's 8.0.26 build).
	if !strings.Contains(got, "LEFT JOIN information_schema.TABLE_CONSTRAINTS") {
		t.Fatalf("LEFT JOIN anti-join missing:\n%s", got)
	}
	if !strings.Contains(got, "tc.CONSTRAINT_NAME = 'PRIMARY'") {
		t.Fatalf("PRIMARY predicate not in the ON clause:\n%s", got)
	}
	if !strings.Contains(got, "tc.CONSTRAINT_NAME IS NULL") {
		t.Fatalf("IS NULL filter missing (anti-join broken):\n%s", got)
	}
	if strings.Contains(got, "NOT EXISTS") {
		t.Fatalf("old NOT EXISTS form survived:\n%s", got)
	}
	if strings.Contains(got, "KEY_COLUMN_USAGE") {
		t.Fatalf("old KEY_COLUMN_USAGE probe survived:\n%s", got)
	}
	// Quote in the schema name must be ''-doubled (injection shape closes).
	evil := mysqlNoPKSQL("db' --")
	if !strings.Contains(evil, "'db'' --'") {
		t.Fatalf("schema quote not doubled:\n%s", evil)
	}
	if strings.Contains(evil, "' --'") && !strings.Contains(evil, "'' --") {
		t.Fatalf("unescaped quote leaked:\n%s", evil)
	}
	// MS-11b pen 6: backslash injection — a raw \' terminates the literal
	// early under MySQL default backslash escapes; the fix doubles \ too.
	bs := mysqlNoPKSQL(`db\' OR 1=1 -- `)
	if !strings.Contains(bs, `'db\\'' OR 1=1 -- '`) {
		t.Fatalf("backslash not doubled (literal breakable):\n%s", bs)
	}
	if strings.Contains(bs, "db\\'") {
		t.Fatalf("raw backslash-quote pair survived:\n%s", bs)
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
