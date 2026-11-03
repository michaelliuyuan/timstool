package ddlexport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// MS-12 anchors: TiDB-source dialect boundaries (docs/MS12-TIDB-DIALECT-MAP.md)
// — sequences real export (idx1), note files, IncludeTiDB ignore ledger,
// placement/resource-group ledger, README template, database filter.

// --- TiDB walk stub: answers the SHOW CREATE walk's queries ---

type ms12Driver struct{ conn *ms12Conn }

func (d ms12Driver) Open(string) (driver.Conn, error) { return d.conn, nil }

// ms12Conn answers the tidb-dialect walk. seqShort toggles the adversarial
// sequence shape (a 1-column SHOW CREATE SEQUENCE result) that must fail
// loudly into the skip ledger (never a silent mis-parse).
type ms12Conn struct {
	queries  []string
	seqShort bool
}

func (c *ms12Conn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("not implemented") }
func (c *ms12Conn) Close() error                        { return nil }
func (c *ms12Conn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("not implemented") }

func (c *ms12Conn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	switch {
	case q == "SHOW DATABASES":
		// Mixed case on purpose: the filter compares strings.ToLower.
		return &ms12Rows{cols: []string{"Database"}, rows: [][]driver.Value{
			{"INFORMATION_SCHEMA"}, {"PERFORMANCE_SCHEMA"}, {"METRICS_SCHEMA"},
			{"mysql"}, {"sys"}, {"migration_test"}, {"UserDB"},
		}}, nil
	case strings.Contains(q, "TABLE_TYPE = 'BASE TABLE'"):
		return &ms12Rows{cols: []string{"TABLE_NAME"}, rows: [][]driver.Value{{"t1"}}}, nil
	case strings.Contains(q, "TABLE_TYPE = 'SEQUENCE'"):
		return &ms12Rows{cols: []string{"TABLE_NAME"}, rows: [][]driver.Value{{"s0"}, {"s1"}}}, nil
	case strings.Contains(q, "information_schema.VIEWS"):
		return &ms12Rows{cols: []string{"TABLE_NAME"}, rows: nil}, nil
	case strings.HasPrefix(q, "SHOW CREATE TABLE"):
		return &ms12Rows{cols: []string{"Table", "Create Table"}, rows: [][]driver.Value{
			{"t1", "CREATE TABLE `t1` (\n  `id` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM(5) */,\n  `doc` vector DEFAULT NULL,\n  PRIMARY KEY (`id`) /*T![clustered_index] CLUSTERED */\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin /*T! SHARD_ROW_ID_BITS=4 */"},
		}}, nil
	case strings.HasPrefix(q, "SHOW CREATE SEQUENCE"):
		if c.seqShort {
			// Adversarial shape: server returns a single column — the
			// ddlCol=1 pin must fail loudly (column-count guard).
			return &ms12Rows{cols: []string{"Create Sequence"}, rows: [][]driver.Value{
				{"CREATE SEQUENCE `s0` start with 1 increment by 1"},
			}}, nil
		}
		// Keyed on the quoted name in the statement (the walk issues one
		// SHOW CREATE SEQUENCE per listed sequence).
		if strings.Contains(q, "`.`s1`") {
			return &ms12Rows{cols: []string{"Sequence", "Create Sequence"}, rows: [][]driver.Value{
				{"s1", "CREATE SEQUENCE `s1` start with 100 increment by 5 cache 10 nocycle ENGINE=InnoDB"},
			}}, nil
		}
		return &ms12Rows{cols: []string{"Sequence", "Create Sequence"}, rows: [][]driver.Value{
			{"s0", "CREATE SEQUENCE `s0` start with 1 minvalue 1 maxvalue 9223372036854775806 increment by 1 cache 1 nocycle ENGINE=InnoDB"},
		}}, nil
	}
	return nil, fmt.Errorf("ms12 stub: unexpected query: %s", q)
}

type ms12Rows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *ms12Rows) Columns() []string { return r.cols }
func (r *ms12Rows) Close() error      { return nil }
func (r *ms12Rows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

var ms12Names []string

func openMS12DB(t *testing.T, conn *ms12Conn) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("ms12-%d", len(ms12Names))
	ms12Names = append(ms12Names, name)
	sql.Register(name, ms12Driver{conn: conn})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open ms12 stub: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestTiDBDatabaseFilter pins the tidb walk's schema surface: SHOW DATABASES
// minus the shared system set (mysql/information_schema/performance_schema/
// sys/METRICS_SCHEMA), case-insensitive, sorted (MS12-TIDB-DIALECT-MAP.md
// note 6 — the filter set itself is anchored by TestMySQLDatabaseFilter).
func TestTiDBDatabaseFilter(t *testing.T) {
	conn := &ms12Conn{}
	got, err := ListSchemasFor(context.Background(), openMS12DB(t, conn), "tidb")
	if err != nil {
		t.Fatalf("ListSchemasFor tidb: %v", err)
	}
	want := []string{"UserDB", "migration_test"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("tidb schemas = %v, want %v (system set + case-insensitive + sorted)", got, want)
	}
}

// TestTiDBSequencesRealExport pins the 裁-1 contract: sequences are listed
// from information_schema.TABLES (TABLE_TYPE='SEQUENCE') and rendered via
// SHOW CREATE SEQUENCE with the DDL at idx1 — the (Sequence, Create
// Sequence) column shape, same reader shape as SHOW CREATE TABLE's
// (Table, Create Table). The DDL text lands verbatim; sequences never
// leak into tables.sql (its listing pins BASE TABLE).
func TestTiDBSequencesRealExport(t *testing.T) {
	conn := &ms12Conn{}
	e := NewExporter(openMS12DB(t, conn), Options{SourceType: "tidb", Types: TypeSet{Tables: true, Sequences: true}})
	files, err := e.mysqlSchemaFiles(context.Background(), "db1")
	if err != nil {
		t.Fatalf("mysqlSchemaFiles tidb: %v", err)
	}
	seq := files["sequences.sql"]
	if !strings.Contains(seq, "CREATE SEQUENCE `s0` start with 1 minvalue 1") ||
		!strings.Contains(seq, "CREATE SEQUENCE `s1` start with 100 increment by 5") {
		t.Fatalf("sequences.sql must carry both SHOW CREATE SEQUENCE DDLs verbatim:\n%s", seq)
	}
	// Ordered walk: s0 renders before s1 (ORDER BY TABLE_NAME).
	if strings.Index(seq, "`s0`") > strings.Index(seq, "`s1`") {
		t.Error("sequences must render in TABLE_NAME order")
	}
	// Tables walk must not list sequences (BASE TABLE pin) and the TiDB
	// clause shapes ride verbatim (裁 3 reference shapes).
	tab := files["tables.sql"]
	if strings.Contains(tab, "SEQUENCE") {
		t.Error("tables.sql must not contain sequence objects")
	}
	for _, shape := range []string{
		"/*T![auto_rand] AUTO_RANDOM(5) */",
		"/*T![clustered_index] CLUSTERED */",
		"/*T! SHARD_ROW_ID_BITS=4 */",
	} {
		if !strings.Contains(tab, shape) {
			t.Errorf("tables.sql missing TiDB clause shape %q:\n%s", shape, tab)
		}
	}
	if e.manifest.Counts["db1"]["sequences.sql"] != 2 {
		t.Errorf("sequence count = %d, want 2", e.manifest.Counts["db1"]["sequences.sql"])
	}
	// The listing queries pin their TABLE_TYPE discriminators.
	var basePin, seqPin bool
	for _, q := range conn.queries {
		if strings.Contains(q, "TABLE_TYPE = 'BASE TABLE'") {
			basePin = true
		}
		if strings.Contains(q, "TABLE_TYPE = 'SEQUENCE'") {
			seqPin = true
		}
	}
	if !basePin || !seqPin {
		t.Errorf("listing queries must pin TABLE_TYPE discriminators (base=%v seq=%v)", basePin, seqPin)
	}
}

// TestTiDBSequenceShapeGuardLoud pins the adversarial boundary: a server
// whose SHOW CREATE SEQUENCE returns a different column shape fails
// LOUDLY per object (showCreate's column-count guard) and lands in the
// skip ledger — never a silent mis-parse (裁 1's reader-unification
// discipline).
func TestTiDBSequenceShapeGuardLoud(t *testing.T) {
	conn := &ms12Conn{seqShort: true}
	e := NewExporter(openMS12DB(t, conn), Options{SourceType: "tidb", Types: TypeSet{Sequences: true}})
	files, err := e.mysqlSchemaFiles(context.Background(), "db1")
	if err != nil {
		t.Fatalf("mysqlSchemaFiles tidb: %v", err)
	}
	if files["sequences.sql"] != "" {
		t.Fatalf("shape-guard failure must write no DDL, got:\n%s", files["sequences.sql"])
	}
	var loud bool
	for _, s := range e.manifest.Skipped {
		if s.Type == "sequence" && strings.Contains(s.Reason, "DDL column 1 missing") {
			loud = true
		}
	}
	if !loud {
		t.Fatalf("column-shape failure must land in the skip ledger loudly, got %+v", e.manifest.Skipped)
	}
}

// TestTiDBNoteFilesAndLedger pins the degrade contract: the four TiDB note
// files, the IncludeTiDB ignore (no tidb-tables.sql + manifest ledger) and
// the placement-policy / resource-group v1 ledger notes.
func TestTiDBNoteFilesAndLedger(t *testing.T) {
	conn := &ms12Conn{}
	e := NewExporter(openMS12DB(t, conn), Options{
		SourceType: "tidb", IncludeTiDB: true,
		Types: TypeSet{Tables: true, Indexes: true, Views: true, Sequences: true, Functions: true, Procedures: true, Triggers: true, Types: true},
	})
	files, err := e.mysqlSchemaFiles(context.Background(), "db1")
	if err != nil {
		t.Fatalf("mysqlSchemaFiles tidb: %v", err)
	}
	for _, f := range []struct{ file, note string }{
		{"functions.sql", "-- TiDB: no stored-function objects"},
		{"procedures.sql", "-- TiDB: no stored-procedure objects"},
		{"triggers.sql", "-- TiDB: no trigger objects"},
		{"types.sql", "-- TiDB: no user-defined-type objects"},
		{"indexes.sql", "-- TiDB: indexes have no standalone namespace"},
	} {
		if !strings.Contains(files[f.file], f.note) {
			t.Errorf("%s must be a TiDB note file %q, got:\n%s", f.file, f.note, files[f.file])
		}
	}
	if _, ok := files["tidb-tables.sql"]; ok {
		t.Error("tidb source must not generate tidb-tables.sql (source already is TiDB)")
	}
	var ignoreLedger, placement, rg bool
	for _, s := range e.manifest.Skipped {
		if s.Type == "tidb-tables.sql" && strings.Contains(s.Reason, "IncludeTiDB ignored") {
			ignoreLedger = true
		}
		if s.Type == "placement-policy" {
			placement = true
		}
		if s.Type == "resource-group" {
			rg = true
		}
	}
	if !ignoreLedger || !placement || !rg {
		t.Errorf("manifest ledger misses entries (ignore=%v placement=%v rg=%v): %+v", ignoreLedger, placement, rg, e.manifest.Skipped)
	}
}

// TestTiDBQueryAnchors pins the source-level tidb-dialect anchors: the
// SHOW CREATE SEQUENCE statement + its idx1 DDL pin, the four tidbNoteFile
// call sites, the !tidb IncludeTiDB guard, the ledger appends and the
// tidbSource flag wiring (five-pin #3: the kind truth lives in the
// registry predicates).
func TestTiDBQueryAnchors(t *testing.T) {
	b, err := os.ReadFile("exporter_mysql.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// SHOW CREATE SEQUENCE with the idx1 DDL pin adjacent (same shape as
	// the MS-10c table/view pins).
	i := strings.Index(s, `"SHOW CREATE SEQUENCE %s.%s"`)
	if i < 0 {
		t.Fatal("SHOW CREATE SEQUENCE statement not found")
	}
	if !strings.Contains(s[i:i+120], ", 1)") {
		t.Error("SHOW CREATE SEQUENCE: DDL column pin 1 not adjacent")
	}
	if strings.Count(s, "tidbNoteFile(") < 5 {
		t.Errorf("tidbNoteFile call sites (4) + definition expected, got %d", strings.Count(s, "tidbNoteFile("))
	}
	if !strings.Contains(s, "e.opts.IncludeTiDB && t.Tables && !tidb") {
		t.Error("IncludeTiDB render must carry the !tidb guard (source already is TiDB)")
	}
	for _, ledger := range []string{`"placement-policy"`, `"resource-group"`, "IncludeTiDB ignored"} {
		if !strings.Contains(s, ledger) {
			t.Errorf("ledger anchor missing: %q", ledger)
		}
	}
	src2, err := os.ReadFile("exporter.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src2), "tidbSource: source.IsTiDB(") {
		t.Error("exporter.go must wire tidbSource via the registry predicate")
	}
}

// TestTiDBSchemaDefaultKept mirrors TestMySQLSchemaDefaultKept: the public
// fallback is PG-only; a tidb run keeps the caller's list and sets the
// dialect flag.
func TestTiDBSchemaDefaultKept(t *testing.T) {
	e := NewExporter(nil, Options{SourceType: "tidb"})
	if len(e.opts.Schemas) != 0 {
		t.Fatalf("tidb default schemas = %v, want empty (no public fallback)", e.opts.Schemas)
	}
	if !e.tidbSource {
		t.Error("SourceType=tidb must set tidbSource")
	}
	if m := NewExporter(nil, Options{SourceType: "mysql"}); m.tidbSource {
		t.Error("SourceType=mysql must not set tidbSource")
	}
}

// TestReadmeTiDBTemplate pins the TiDB README (readmeFileTiDB): apply
// order with sequences first, the verbatim clause list WITHOUT
// PRE_SPLIT_REGIONS, the 裁-2a PRE_SPLIT_REGIONS non-echo annotation, the
// 裁-2b VECTOR fold inside the replay-target floor, the note files and the
// system-schema line. Negative anchors scan TEMPLATE LITERALS ONLY — the
// schema name is caller data (same contract as TestReadmeMySQLTemplate).
func TestReadmeTiDBTemplate(t *testing.T) {
	got := readmeFile("db1", "tidb")
	wants := []string{
		"Recommended apply order per schema (native TiDB DDL):",
		"sequences.sql -> tables.sql -> views.sql",
		"SHOW CREATE TABLE output verbatim",
		"(AUTO_RANDOM, SHARD_ROW_ID_BITS, CLUSTERED, TTL, ...)",
		"PRE_SPLIT_REGIONS is NOT echoed by SHOW CREATE TABLE",
		"faithful export, not a defect",
		"add PRE_SPLIT_REGIONS manually",
		"target TiDB version must be >= the source TiDB version",
		"vector-index feature enabled",
		"TABLE_TYPE='SEQUENCE'",
		"functions.sql, procedures.sql, triggers.sql and types.sql are note files",
		"INFORMATION_SCHEMA, PERFORMANCE_SCHEMA, METRICS_SCHEMA, mysql, sys",
		"Schema: db1",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("TiDB README missing %q\ngot:\n%s", w, got)
		}
	}
	for _, f := range []string{"Postgre", "pg_", "nextval", "COMMENT ON"} {
		if strings.Contains(got, f) {
			t.Errorf("TiDB README must not contain PG wording %q\ngot:\n%s", f, got)
		}
	}
	// The verbatim-preserved clause list must NOT claim PRE_SPLIT_REGIONS
	// (it is not echoed — 裁 2a; it only appears in the annotation).
	for _, bad := range []string{"PRE_SPLIT_REGIONS, CLUSTERED", "CLUSTERED, PRE_SPLIT_REGIONS", "TTL, PRE_SPLIT_REGIONS"} {
		if strings.Contains(got, bad) {
			t.Errorf("verbatim clause list must not include PRE_SPLIT_REGIONS (%q found)", bad)
		}
	}
	// Caller data passes through untouched.
	if got := readmeFile("pg_migration", "tidb"); !strings.Contains(got, "Schema: pg_migration") {
		t.Errorf("schema name is data, not template: %q must pass through", "pg_migration")
	}
}
