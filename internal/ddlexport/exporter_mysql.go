// MySQL DDL export walk (MS-10c): every object type is rendered via its
// SHOW CREATE statement, listed from information_schema first so the walk
// is ordered and independent of the session's default database. The
// per-dialect boundaries (what degrades, what has no MySQL equivalent)
// are recorded in docs/MS10C-DIALECT-MAP.md.
package ddlexport

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/source"
	"github.com/michaelliuyuan/timstool/internal/source/mysql"
	"github.com/michaelliuyuan/timstool/internal/target"
)

// System-schema filtering reads the shared exported set from the MySQL
// source adapter (MS-10b2 item 8: single source of truth; assess consumes
// the same copy — no private mirrors).
// MS-10a note: SHOW DATABASES output minus these is the user surface.

// qiB quotes a MySQL identifier with backticks (doubling embedded ones).
func qiB(ident string) string {
	return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
}

// Shared information_schema listings for the stored-program classes: the
// MySQL and TiDB walks run the IDENTICAL queries (MS-12c — the TiDB
// catalog is column-for-column isomorphic per the 2026-11-03 live probe
// on Pingkai TiDB v7.1.9, schema ddl_te_test: information_schema.ROUTINES/
// TRIGGERS name the objects, SHOW CREATE carries the DDL at idx2).
const (
	functionListQuery = `
SELECT ROUTINE_NAME FROM information_schema.ROUTINES
WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'FUNCTION'
ORDER BY ROUTINE_NAME`
	procedureListQuery = `
SELECT ROUTINE_NAME FROM information_schema.ROUTINES
WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE'
ORDER BY ROUTINE_NAME`
	triggerListQuery = `
SELECT TRIGGER_NAME FROM information_schema.TRIGGERS
WHERE TRIGGER_SCHEMA = ?
ORDER BY TRIGGER_NAME`
)

// listMySQLDatabases returns non-system databases (SHOW DATABASES), sorted
// for a deterministic zip regardless of server collation ordering.
func listMySQLDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SHOW DATABASES`)
	if err != nil {
		return nil, fmt.Errorf("list schemas: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		// Case-insensitive: TiDB (and Windows MySQL builds) report the
		// system databases upper/mixed-case (INFORMATION_SCHEMA etc.).
		if mysql.SystemDatabases[strings.ToLower(s)] {
			continue
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// showCreate runs a SHOW CREATE statement and returns the DDL column at
// index ddlCol. The SHOW CREATE ... result shapes vary per object type
// (table: 2 columns, view: 4, function/procedure/trigger: the DDL sits
// at index 2); callers pin the index next to each statement.
func (e *Exporter) showCreate(ctx context.Context, label, stmt string, ddlCol int) (string, error) {
	rows, err := e.db.QueryContext(ctx, stmt)
	if err != nil {
		return "", fmt.Errorf("export %s: %w", label, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return "", fmt.Errorf("export %s: no row returned", label)
	}
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if ddlCol >= len(cols) {
		return "", fmt.Errorf("export %s: DDL column %d missing (server returned %d columns)", label, ddlCol, len(cols))
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", fmt.Errorf("export %s: %w", label, err)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("export %s: %w", label, err)
	}
	if !vals[ddlCol].Valid || strings.TrimSpace(vals[ddlCol].String) == "" {
		return "", fmt.Errorf("export %s: empty DDL returned", label)
	}
	return vals[ddlCol].String, nil
}

// mysqlObjectNames runs an ordered information_schema listing query with
// one ? schema parameter.
func (e *Exporter) mysqlObjectNames(ctx context.Context, label, query string, schema string) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, query, schema)
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", label, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n sql.NullString
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if n.Valid && n.String != "" {
			out = append(out, n.String)
		}
	}
	return out, rows.Err()
}

// mysqlRenderObjects lists names via query, renders each with SHOW CREATE
// (ddlCol), and skips per-object failures (skip() contract: log + manifest).
func (e *Exporter) mysqlRenderObjects(ctx context.Context, schema, typ, listQuery, showFmt string, ddlCol int) (string, int) {
	names, err := e.mysqlObjectNames(ctx, typ, listQuery, schema)
	if err != nil {
		e.skip(schema, typ+".sql", schema, err.Error())
		return "", 0
	}
	return e.mysqlRenderNames(ctx, schema, typ, names, showFmt, ddlCol)
}

// mysqlRenderNames renders an already-listed name set via SHOW CREATE
// (ddlCol); a per-object failure skips that object only (log + manifest)
// and the walk continues — shared by the MySQL walk and the MS-12c TiDB
// stored-program walk.
func (e *Exporter) mysqlRenderNames(ctx context.Context, schema, typ string, names []string, showFmt string, ddlCol int) (string, int) {
	var b strings.Builder
	written := 0
	for _, name := range names {
		ddl, err := e.showCreate(ctx, typ, fmt.Sprintf(showFmt, qiB(schema), qiB(name)), ddlCol)
		if err != nil {
			e.skip(schema, typ, name, err.Error())
			continue
		}
		b.WriteString(terminated(strings.TrimRight(ddl, "\n")))
		b.WriteString("\n")
		written++
	}
	return b.String(), written
}

// mysqlSchemaFiles renders one MySQL-wire database (mysql or tidb
// dialect; the shared SHOW CREATE walk). Shared degrades (recorded in
// MS10C-DIALECT-MAP.md / MS12-TIDB-DIALECT-MAP.md, never silently
// dropped):
//   - indexes.sql: MySQL-wire indexes are part of SHOW CREATE TABLE
//     output (no standalone index namespace) — a note file is written;
//   - tidb-tables.sql: since MS-10c2 the TiDB conversion is rendered via
//     the source adapter's CIR path (information_schema walk + type
//     mapper + target renderer); the old PG-catalog skip is gone.
//
// TiDB-dialect boundaries (MS-12, e.tidbSource):
//   - sequences are REAL objects: information_schema.TABLES
//     TABLE_TYPE='SEQUENCE' listing + SHOW CREATE SEQUENCE;
//   - functions/procedures/triggers are REAL exports since MS-12c: the
//     same information_schema + SHOW CREATE walk as MySQL (Pingkai/
//     Enterprise TiDB supports stored programs — tidb_enable_procedure —
//     and triggers exist since community TiDB v5.0). A server without a
//     class degrades to a note file PLUS a manifest ledger entry —
//     never a silent empty export;
//   - types degrade to a TiDB note file (no user-defined types in TiDB);
//   - tidb-tables.sql is never generated (the source already is TiDB);
//     IncludeTiDB is ignored and ledger-noted;
//   - placement policies / resource groups are not exported (v1 ruling);
//     both are ledger-noted per schema in the manifest.
func (e *Exporter) mysqlSchemaFiles(ctx context.Context, schemaName string) (map[string]string, error) {
	files := map[string]string{}
	t := e.opts.Types
	tidb := e.tidbSource

	if tidb {
		// Manifest ledger notes (direct append — informational coverage
		// notes, not error skips; no per-schema Warn spam): the two
		// DBA-managed object classes that exist on the server but are
		// deliberately out of scope in v1.
		e.manifest.Skipped = append(e.manifest.Skipped,
			SkippedObject{Schema: schemaName, Type: "placement-policy", Object: schemaName,
				Reason: "TiDB placement policies are not exported in v1 (DBA-managed; MS12-TIDB-DIALECT-MAP.md)"},
			SkippedObject{Schema: schemaName, Type: "resource-group", Object: schemaName,
				Reason: "TiDB resource groups are not exported in v1 (DBA-managed; MS12-TIDB-DIALECT-MAP.md)"})
		if e.opts.IncludeTiDB {
			e.manifest.Skipped = append(e.manifest.Skipped,
				SkippedObject{Schema: schemaName, Type: "tidb-tables.sql", Object: schemaName,
					Reason: "source is TiDB — tidb-tables.sql conversion reference is not generated; IncludeTiDB ignored (MS12-TIDB-DIALECT-MAP.md)"})
		}
	}

	if t.Tables {
		// TABLE_TYPE='BASE TABLE' keeps TiDB sequences (TABLE_TYPE=
		// 'SEQUENCE') out of tables.sql on the tidb dialect; they render
		// into sequences.sql below.
		ddl, n := e.mysqlRenderObjects(ctx, schemaName, "table", `
SELECT TABLE_NAME FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
ORDER BY TABLE_NAME`, "SHOW CREATE TABLE %s.%s", 1)
		files["tables.sql"] = ddl
		e.countN(schemaName, "tables.sql", n)
	}

	if t.Indexes {
		if tidb {
			files["indexes.sql"] = "-- TiDB: indexes have no standalone namespace; they are part of the\n-- SHOW CREATE TABLE output in tables.sql (MS-12 dialect map).\n"
		} else {
			files["indexes.sql"] = "-- MySQL: indexes have no standalone namespace; they are part of the\n-- SHOW CREATE TABLE output in tables.sql (MS-10c dialect map).\n"
		}
	}

	if t.Views {
		ddl, n := e.mysqlRenderObjects(ctx, schemaName, "view", `
SELECT TABLE_NAME FROM information_schema.VIEWS
WHERE TABLE_SCHEMA = ?
ORDER BY TABLE_NAME`, "SHOW CREATE VIEW %s.%s", 1)
		files["views.sql"] = ddl
		e.countN(schemaName, "views.sql", n)
	}

	if t.Sequences {
		if tidb {
			// Real export (decision ①, TE-104 column shape = hard gate):
			// sequences are listed from information_schema.TABLES
			// (TABLE_TYPE='SEQUENCE') and rendered verbatim via SHOW
			// CREATE SEQUENCE. ddlCol=1 pins the (Sequence, Create
			// Sequence) result shape; a server that returns a different
			// shape fails LOUDLY per object (showCreate's column-count
			// guard) and lands in the skip ledger — never a silent
			// mis-parse.
			ddl, n := e.mysqlRenderObjects(ctx, schemaName, "sequence", `
SELECT TABLE_NAME FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'SEQUENCE'
ORDER BY TABLE_NAME`, "SHOW CREATE SEQUENCE %s.%s", 1)
			files["sequences.sql"] = ddl
			e.countN(schemaName, "sequences.sql", n)
		} else {
			files["sequences.sql"] = "-- MySQL: no sequence objects (AUTO_INCREMENT lives in the table DDL;\n-- see MS10C-DIALECT-MAP.md).\n"
		}
	}

	if t.Functions {
		if tidb {
			// MS-12c: real export — objects that exist must never be
			// silently dropped (tidbRenderStoredPrograms owns the
			// ledgered degrade contract for servers without the class).
			ddl, n := e.tidbRenderStoredPrograms(ctx, schemaName, "function", "stored-function",
				functionListQuery, "SHOW CREATE FUNCTION %s.%s", 2)
			files["functions.sql"] = ddl
			e.countN(schemaName, "functions.sql", n)
		} else {
			ddl, n := e.mysqlRenderObjects(ctx, schemaName, "function",
				functionListQuery, "SHOW CREATE FUNCTION %s.%s", 2)
			files["functions.sql"] = ddl
			e.countN(schemaName, "functions.sql", n)
		}
	}

	if t.Procedures {
		if tidb {
			ddl, n := e.tidbRenderStoredPrograms(ctx, schemaName, "procedure", "stored-procedure",
				procedureListQuery, "SHOW CREATE PROCEDURE %s.%s", 2)
			files["procedures.sql"] = ddl
			e.countN(schemaName, "procedures.sql", n)
		} else {
			ddl, n := e.mysqlRenderObjects(ctx, schemaName, "procedure",
				procedureListQuery, "SHOW CREATE PROCEDURE %s.%s", 2)
			files["procedures.sql"] = ddl
			e.countN(schemaName, "procedures.sql", n)
		}
	}

	if t.Triggers {
		if tidb {
			ddl, n := e.tidbRenderStoredPrograms(ctx, schemaName, "trigger", "trigger",
				triggerListQuery, "SHOW CREATE TRIGGER %s.%s", 2)
			files["triggers.sql"] = ddl
			e.countN(schemaName, "triggers.sql", n)
		} else {
			ddl, n := e.mysqlRenderObjects(ctx, schemaName, "trigger",
				triggerListQuery, "SHOW CREATE TRIGGER %s.%s", 2)
			files["triggers.sql"] = ddl
			e.countN(schemaName, "triggers.sql", n)
		}
	}

	if t.Types {
		if tidb {
			files["types.sql"] = tidbNoteFile("user-defined-type")
		} else {
			files["types.sql"] = "-- MySQL: no user-defined type objects (see MS10C-DIALECT-MAP.md).\n"
		}
	}

	if e.opts.IncludeTiDB && t.Tables && !tidb {
		// Error path: the skip ledger entry is the whole record — no empty
		// tidb-tables.sql file lands in the zip (an empty file reads as
		// "zero tables converted", MS-10c2 adversarial note 1).
		ddl, n, convErr := e.mysqlTiDBTables(ctx, schemaName)
		if convErr == nil {
			files["tidb-tables.sql"] = ddl
			e.countN(schemaName, "tidb-tables.sql", n)
		}
	}

	return files, nil
}

// tidbRenderStoredPrograms (MS-12c) runs the REAL SHOW CREATE walk for one
// stored-program class on a TiDB source — the identical information_schema
// listing and SHOW CREATE shapes as the MySQL walk (live probe 2026-11-03
// on Pingkai TiDB v7.1.9-0.2, schema ddl_te_test: information_schema.
// ROUTINES/TRIGGERS name the objects; SHOW CREATE FUNCTION/PROCEDURE/
// TRIGGER carry the DDL at idx2, column-for-column isomorphic with MySQL).
// The MS-12-era note files assumed "TiDB has no stored programs /
// triggers" and were written without even querying — falsified on that
// box (tidb_enable_procedure=ON; triggers exist since community v5.0):
// real objects were silently dropped out of both the counts and the skip
// ledger. Degrade contract: only a FAILING listing (server without the
// class, e.g. community TiDB exposes no information_schema.ROUTINES)
// degrades — to a note file PLUS a manifest ledger entry carrying the
// actual error, never a silent empty export. A successful listing with
// zero objects writes a self-describing "none found" note (zero objects
// is a result, not a skip — no ledger noise). Per-object SHOW failures
// skip per object via the shared mysqlRenderNames loop.
func (e *Exporter) tidbRenderStoredPrograms(ctx context.Context, schema, typ, class, listQuery, showFmt string, ddlCol int) (string, int) {
	names, err := e.mysqlObjectNames(ctx, typ, listQuery, schema)
	if err != nil {
		e.skip(schema, typ, schema, "TiDB server does not expose "+class+
			" objects — catalog listing failed: "+err.Error()+" (see MS12-TIDB-DIALECT-MAP.md)")
		return "-- TiDB: " + class + " export unavailable on this server — see manifest.json (skipped) and MS12-TIDB-DIALECT-MAP.md.\n", 0
	}
	ddl, n := e.mysqlRenderNames(ctx, schema, typ, names, showFmt, ddlCol)
	if len(names) == 0 {
		return "-- TiDB: no " + class + " objects found (catalog listing returned none; see MS12-TIDB-DIALECT-MAP.md).\n", 0
	}
	return ddl, n
}

// tidbNoteFile renders the TiDB note file for an object class TiDB does
// not have (user-defined types) — the mysql sequence/types notes' TiDB
// twin (MS-12 dialect map).
func tidbNoteFile(objects string) string {
	return "-- TiDB: no " + objects + " objects (see MS12-TIDB-DIALECT-MAP.md).\n"
}

// mysqlTiDBTables renders the TiDB-converted CREATE TABLE script for a
// MySQL database (MS-10c2): the source adapter's SchemaReader walks
// information_schema into CIR (columns already carry the mysqlTypeMapper's
// TiDBType), and the target renderer emits one CREATE TABLE per table.
// Functional key parts replay their information_schema EXPRESSION text
// verbatim (no re-wrap, no paren strip). Column types that pass through
// 1:1 (no conversion applied — ENUM/SET fidelity, the geometry family
// fallback) are recorded per distinct type in the manifest skip ledger
// instead of silently passing (coverage accounting, MS10C-DIALECT-MAP.md).
func (e *Exporter) mysqlTiDBTables(ctx context.Context, schemaName string) (string, int, error) {
	sch, err := mysql.NewSchemaReaderForDB(e.db, schemaName).ReadSchema(ctx, source.Filter{})
	if err != nil {
		e.skip(schemaName, "tidb-tables.sql", schemaName, err.Error())
		return "", 0, err
	}
	// Deterministic replay: sort tables by name before rendering so the
	// same request twice renders a byte-identical tidb-tables.sql
	// (defense on top of the reader's ORDER BY; MS-10c2 black-box note B).
	sort.Slice(sch.Tables, func(i, j int) bool { return sch.Tables[i].Name < sch.Tables[j].Name })
	seen := map[string]bool{}
	var b strings.Builder
	n := 0
	for _, tbl := range sch.Tables {
		b.WriteString(target.RenderCreateTable(tbl))
		b.WriteString(";\n\n")
		n++
		for _, c := range tbl.Columns {
			if passthroughType(c) && !seen[c.TiDBType] {
				seen[c.TiDBType] = true
				e.skip(schemaName, "tidb-tables.sql", "type:"+c.TiDBType,
					"column type passes through 1:1 (no TiDB conversion applied) — coverage ledger: MS10C-DIALECT-MAP.md")
			}
		}
	}
	return b.String(), n, nil
}

// passthroughType reports a column whose TiDB type is a 1:1 fallback, not
// a conversion: either the type mapper's default branch returned the
// source string verbatim (unmapped type), or it is the geometry family
// (TiDB keeps the spelling but applies no conversion). Native 1:1 integer
// families (BIGINT etc.) are mapped spellings, not fallbacks, and stay
// off the ledger — case-sensitive compare separates "BIGINT" (mapped)
// from a verbatim echo.
func passthroughType(c source.Column) bool {
	if c.TiDBType == c.SourceType {
		return true
	}
	base := strings.ToUpper(strings.TrimSpace(c.TiDBType))
	if idx := strings.IndexByte(base, '('); idx >= 0 {
		base = base[:idx]
	}
	switch base {
	case "GEOMETRY", "POINT", "LINESTRING", "POLYGON", "MULTIPOINT",
		"MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION":
		return true
	}
	return false
}
