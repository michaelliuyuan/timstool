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

// mysqlSystemDatabases mirrors the mysqlWatermarkDialect SystemSchemas
// set (MS-10a): SHOW DATABASES output minus these is the user surface.
var mysqlSystemDatabases = map[string]bool{
	"mysql":              true,
	"information_schema": true,
	"performance_schema": true,
	"sys":                true,
	// TiDB (MySQL-compatible surface) reports its own system schemas.
	"metrics_schema": true,
}

// qiB quotes a MySQL identifier with backticks (doubling embedded ones).
func qiB(ident string) string {
	return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
}

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
		if mysqlSystemDatabases[strings.ToLower(s)] {
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

// mysqlSchemaFiles renders one MySQL database. Degrades (recorded in
// MS10C-DIALECT-MAP.md, not silently dropped):
//   - indexes.sql: MySQL indexes are part of SHOW CREATE TABLE output
//     (no standalone index namespace) — a note file is written instead;
//   - sequences/types: MySQL has no such object types — note files;
//   - tidb-tables.sql: since MS-10c2 the TiDB conversion is rendered via
//     the source adapter's CIR path (information_schema walk + type
//     mapper + target renderer); the old PG-catalog skip is gone.
func (e *Exporter) mysqlSchemaFiles(ctx context.Context, schemaName string) (map[string]string, error) {
	files := map[string]string{}
	t := e.opts.Types

	if t.Tables {
		ddl, n := e.mysqlRenderObjects(ctx, schemaName, "table", `
SELECT TABLE_NAME FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
ORDER BY TABLE_NAME`, "SHOW CREATE TABLE %s.%s", 1)
		files["tables.sql"] = ddl
		e.countN(schemaName, "tables.sql", n)
	}

	if t.Indexes {
		files["indexes.sql"] = "-- MySQL: indexes have no standalone namespace; they are part of the\n-- SHOW CREATE TABLE output in tables.sql (MS-10c dialect map).\n"
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
		files["sequences.sql"] = "-- MySQL: no sequence objects (AUTO_INCREMENT lives in the table DDL;\n-- see MS10C-DIALECT-MAP.md).\n"
	}

	if t.Functions {
		ddl, n := e.mysqlRenderObjects(ctx, schemaName, "function", `
SELECT ROUTINE_NAME FROM information_schema.ROUTINES
WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'FUNCTION'
ORDER BY ROUTINE_NAME`, "SHOW CREATE FUNCTION %s.%s", 2)
		files["functions.sql"] = ddl
		e.countN(schemaName, "functions.sql", n)
	}

	if t.Procedures {
		ddl, n := e.mysqlRenderObjects(ctx, schemaName, "procedure", `
SELECT ROUTINE_NAME FROM information_schema.ROUTINES
WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE'
ORDER BY ROUTINE_NAME`, "SHOW CREATE PROCEDURE %s.%s", 2)
		files["procedures.sql"] = ddl
		e.countN(schemaName, "procedures.sql", n)
	}

	if t.Triggers {
		ddl, n := e.mysqlRenderObjects(ctx, schemaName, "trigger", `
SELECT TRIGGER_NAME FROM information_schema.TRIGGERS
WHERE TRIGGER_SCHEMA = ?
ORDER BY TRIGGER_NAME`, "SHOW CREATE TRIGGER %s.%s", 2)
		files["triggers.sql"] = ddl
		e.countN(schemaName, "triggers.sql", n)
	}

	if t.Types {
		files["types.sql"] = "-- MySQL: no user-defined type objects (see MS10C-DIALECT-MAP.md).\n"
	}

	if e.opts.IncludeTiDB && t.Tables {
		ddl, n := e.mysqlTiDBTables(ctx, schemaName)
		files["tidb-tables.sql"] = ddl
		e.countN(schemaName, "tidb-tables.sql", n)
	}

	return files, nil
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
func (e *Exporter) mysqlTiDBTables(ctx context.Context, schemaName string) (string, int) {
	sch, err := mysql.NewSchemaReaderForDB(e.db, schemaName).ReadSchema(ctx, source.Filter{})
	if err != nil {
		e.skip(schemaName, "tidb-tables.sql", schemaName, err.Error())
		return "", 0
	}
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
	return b.String(), n
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
