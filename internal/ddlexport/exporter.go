// Package ddlexport exports native PostgreSQL DDL (tables, indexes, views,
// sequences, functions, procedures, triggers, custom types) per schema as
// plain .sql files, optionally alongside a TiDB-converted tables script.
package ddlexport

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/schema"
	"go.uber.org/zap"
)

// TypeSet selects which object types are exported.
type TypeSet struct {
	Tables     bool
	Indexes    bool
	Views      bool
	Sequences  bool
	Functions  bool
	Procedures bool
	Triggers   bool
	Types      bool
}

// DefaultTypes returns all object types enabled.
func DefaultTypes() TypeSet {
	return TypeSet{Tables: true, Indexes: true, Views: true, Sequences: true, Functions: true, Procedures: true, Triggers: true, Types: true}
}

// Options controls one export run.
type Options struct {
	Schemas     []string
	Types       TypeSet
	IncludeTiDB bool
	// MaxObjects caps total exported objects across schemas (0 = default 20000).
	MaxObjects int
}

const defaultMaxObjects = 20000

// ObjectCounts maps schema → file name → exported statement count.
type ObjectCounts map[string]map[string]int

// SkippedObject records an object skipped due to an error (e.g. no permission).
type SkippedObject struct {
	Schema string `json:"schema"`
	Type   string `json:"type"`
	Object string `json:"object"`
	Reason string `json:"reason"`
}

// Manifest summarizes an export run.
type Manifest struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Counts      ObjectCounts    `json:"counts"`
	Skipped     []SkippedObject `json:"skipped,omitempty"`
	Total       int             `json:"total_objects"`
}

// Exporter walks a PostgreSQL catalog and renders DDL files.
type Exporter struct {
	db       *sql.DB
	opts     Options
	manifest Manifest
}

// NewExporter creates an exporter bound to an open PG connection.
func NewExporter(db *sql.DB, opts Options) *Exporter {
	if opts.MaxObjects <= 0 {
		opts.MaxObjects = defaultMaxObjects
	}
	if len(opts.Schemas) == 0 {
		opts.Schemas = []string{"public"}
	}
	return &Exporter{db: db, opts: opts, manifest: Manifest{GeneratedAt: time.Now(), Counts: ObjectCounts{}}}
}

// ListSchemas returns all non-system schemas of the connected database.
func ListSchemas(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT schema_name FROM information_schema.schemata
WHERE schema_name NOT IN ('pg_catalog','information_schema')
  AND schema_name NOT LIKE 'pg\_%'
ORDER BY schema_name`)
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
		out = append(out, s)
	}
	return out, rows.Err()
}

func qi(ident string) string { return fmt.Sprintf(`"%s"`, strings.ReplaceAll(ident, `"`, `""`)) }

// terminated appends the statement-terminating semicolon that
// pg_get_functiondef / pg_get_triggerdef output lacks (unlike
// pg_get_viewdef). Without it, multi-object files concatenate two
// CREATE statements into one broken statement.
func terminated(ddl string) string {
	return strings.TrimRight(ddl, " \t\r\n") + ";\n"
}

// ValidateSchemaName rejects schema names that could escape the export
// package layout (zip-slip / path traversal): separators, dot segments.
// Callers (CLI and webapi) must run it before building an Exporter.
func ValidateSchemaName(s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\") || strings.Contains(s, "..") {
		return fmt.Errorf("invalid schema name %q", s)
	}
	return nil
}

func (e *Exporter) countN(schemaName, file string, n int) {
	if n == 0 {
		return
	}
	m := e.manifest.Counts[schemaName]
	if m == nil {
		m = map[string]int{}
		e.manifest.Counts[schemaName] = m
	}
	m[file] += n
	e.manifest.Total += n
}

func (e *Exporter) skip(schemaName, typ, obj, reason string) {
	// Log each skip at the point it happens: the manifest inside the zip is
	// only seen if the user opens it, but the server log is where an
	// "empty export" incident gets diagnosed first.
	zap.L().Warn("ddl export: object skipped",
		zap.String("schema", schemaName), zap.String("type", typ),
		zap.String("object", obj), zap.String("reason", reason))
	e.manifest.Skipped = append(e.manifest.Skipped, SkippedObject{Schema: schemaName, Type: typ, Object: obj, Reason: reason})
}

// renderRows runs query with args and renders each row via render(row).
// Rows whose render returns "" without error are skipped silently.
func (e *Exporter) renderRows(ctx context.Context, label string, query string, args []interface{},
	render func(*sql.Rows) (string, error)) (string, int, error) {
	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", 0, fmt.Errorf("export %s: %w", label, err)
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		ddl, err := render(rows)
		if err != nil {
			return "", 0, fmt.Errorf("export %s: %w", label, err)
		}
		if ddl == "" {
			continue
		}
		b.WriteString(ddl)
		n++
	}
	return b.String(), n, rows.Err()
}

// schemaFiles renders every selected file for one schema.
func (e *Exporter) schemaFiles(ctx context.Context, schemaName string) (map[string]string, error) {
	files := map[string]string{}
	t := e.opts.Types

	if t.Indexes || t.Tables {
		idxDDL, idxN, err := e.exportIndexes(ctx, schemaName)
		if err != nil {
			return nil, err
		}
		if t.Indexes {
			files["indexes.sql"] = idxDDL
			e.countN(schemaName, "indexes.sql", idxN)
		}
		if t.Tables {
			tables, err := e.listTables(ctx, schemaName)
			if err != nil {
				return nil, err
			}
			var b strings.Builder
			written := 0
			for _, tbl := range tables {
				ddl, err := e.tableDDL(ctx, schemaName, tbl.name, tbl.comment)
				if err != nil {
					e.skip(schemaName, "table", tbl.name, err.Error())
					continue
				}
				b.WriteString(ddl)
				written++
			}
			// Foreign keys are appended after every table exists, so circular
			// references between tables cannot break a fresh rebuild.
			fkDDL, fkN, err := e.renderRows(ctx, "foreign keys", `
SELECT rt.relname, con.conname, pg_get_constraintdef(con.oid)
FROM pg_constraint con
JOIN pg_class rt ON rt.oid = con.conrelid
JOIN pg_namespace n ON n.oid = con.connamespace
WHERE n.nspname = $1 AND con.contype = 'f'
ORDER BY rt.relname, con.conname`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
				var table, name, def string
				if err := row.Scan(&table, &name, &def); err != nil {
					return "", err
				}
				return fmt.Sprintf("ALTER TABLE %s.%s ADD CONSTRAINT %s %s;\n", qi(schemaName), qi(table), qi(name), def), nil
			})
			if err != nil {
				return nil, err
			}
			if fkDDL != "" {
				b.WriteString("\n-- Foreign keys (added after all tables)\n")
				b.WriteString(fkDDL)
			}
			files["tables.sql"] = b.String()
			// Count tables actually written (skipped tables are listed in the
			// manifest's skipped section, not silently inflated here).
			e.countN(schemaName, "tables.sql", written)
			e.countN(schemaName, "foreign keys", fkN)
		}
	}

	if t.Views {
		ddl, n, err := e.renderRows(ctx, "views", `
SELECT c.relname, c.relkind, pg_get_viewdef(c.oid, true)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind IN ('v','m')
ORDER BY c.relname`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
			var name, kind, def sql.NullString
			if err := row.Scan(&name, &kind, &def); err != nil {
				return "", err
			}
			if !def.Valid || def.String == "" {
				return "", nil
			}
			kw := "VIEW"
			if kind.String == "m" {
				kw = "MATERIALIZED VIEW"
			}
			return fmt.Sprintf("CREATE %s %s.%s AS\n%s;\n", kw, qi(schemaName), qi(name.String), strings.TrimSpace(def.String)), nil
		})
		if err != nil {
			return nil, err
		}
		files["views.sql"] = ddl
		e.countN(schemaName, "views.sql", n)
	}

	if t.Sequences {
		ddl, n, err := e.renderRows(ctx, "sequences", `
SELECT s.sequencename, s.data_type, s.start_value, s.min_value, s.max_value, s.increment_by, s.cycle, s.cache_size
FROM pg_sequences s
WHERE s.schemaname = $1
ORDER BY s.sequencename`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
			var name, dataType sql.NullString
			var start, min, max, inc, cache sql.NullString
			var cycle bool
			if err := row.Scan(&name, &dataType, &start, &min, &max, &inc, &cycle, &cache); err != nil {
				return "", err
			}
			if !name.Valid {
				return "", nil
			}
			dt := dataType.String
			if dt == "" {
				dt = "bigint"
			}
			cyc := ""
			if cycle {
				cyc = " CYCLE"
			}
			return fmt.Sprintf("CREATE SEQUENCE %s.%s AS %s START WITH %s INCREMENT BY %s MINVALUE %s MAXVALUE %s CACHE %s%s;\n",
				qi(schemaName), qi(name.String), dt, nv(start), nv(inc), nv(min), nv(max), nv(cache), cyc), nil
		})
		if err != nil {
			return nil, err
		}
		files["sequences.sql"] = ddl
		e.countN(schemaName, "sequences.sql", n)
	}

	if t.Functions {
		ddl, n, err := e.renderRows(ctx, "functions", `
SELECT p.proname, pg_get_functiondef(p.oid)
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = $1 AND p.prokind = 'f'
ORDER BY p.proname`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
			var name, def sql.NullString
			if err := row.Scan(&name, &def); err != nil {
				return "", err
			}
			if !def.Valid || def.String == "" {
				return "", nil
			}
			return terminated(def.String), nil
		})
		if err != nil {
			return nil, err
		}
		files["functions.sql"] = ddl
		e.countN(schemaName, "functions.sql", n)
	}

	if t.Procedures {
		ddl, n, err := e.renderRows(ctx, "procedures", `
SELECT p.proname, pg_get_functiondef(p.oid)
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = $1 AND p.prokind = 'p'
ORDER BY p.proname`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
			var name, def sql.NullString
			if err := row.Scan(&name, &def); err != nil {
				return "", err
			}
			if !def.Valid || def.String == "" {
				return "", nil
			}
			return terminated(def.String), nil
		})
		if err != nil {
			return nil, err
		}
		files["procedures.sql"] = ddl
		e.countN(schemaName, "procedures.sql", n)
	}

	if t.Triggers {
		ddl, n, err := e.renderRows(ctx, "triggers", `
SELECT t.tgname, pg_get_triggerdef(t.oid)
FROM pg_trigger t
JOIN pg_class c ON c.oid = t.tgrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND NOT t.tgisinternal
ORDER BY t.tgname`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
			var name, def sql.NullString
			if err := row.Scan(&name, &def); err != nil {
				return "", err
			}
			if !def.Valid || def.String == "" {
				return "", nil
			}
			return terminated(def.String), nil
		})
		if err != nil {
			return nil, err
		}
		files["triggers.sql"] = ddl
		e.countN(schemaName, "triggers.sql", n)
	}

	if t.Types {
		ddl, n, err := e.exportTypes(ctx, schemaName)
		if err != nil {
			return nil, err
		}
		files["types.sql"] = ddl
		e.countN(schemaName, "types.sql", n)
	}

	if e.opts.IncludeTiDB && t.Tables {
		ddl, n, err := e.tidbTables(ctx, schemaName)
		if err != nil {
			e.skip(schemaName, "tidb-tables.sql", schemaName, err.Error())
		} else {
			files["tidb-tables.sql"] = ddl
			e.countN(schemaName, "tidb-tables.sql", n)
		}
	}

	return files, nil
}

func nv(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "1"
	}
	return s.String
}

type tableMeta struct {
	name    string
	comment sql.NullString
}

func (e *Exporter) listTables(ctx context.Context, schemaName string) ([]tableMeta, error) {
	rows, err := e.db.QueryContext(ctx, `
SELECT c.relname, obj_description(c.oid, 'pg_class')
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind = 'r'
ORDER BY c.relname`, schemaName)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var out []tableMeta
	for rows.Next() {
		var m tableMeta
		if err := rows.Scan(&m.name, &m.comment); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// exportIndexes renders indexes.sql. Indexes backing a PRIMARY KEY or UNIQUE
// constraint are rendered inline in tables.sql and excluded here.
func (e *Exporter) exportIndexes(ctx context.Context, schemaName string) (string, int, error) {
	return e.renderRows(ctx, "indexes", `
SELECT t.relname, i.relname, pg_get_indexdef(i.oid)
FROM pg_index x
JOIN pg_class i ON i.oid = x.indexrelid
JOIN pg_class t ON t.oid = x.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
LEFT JOIN pg_constraint con ON con.conindid = i.oid
WHERE n.nspname = $1 AND NOT x.indisprimary AND con.oid IS NULL
ORDER BY t.relname, i.relname`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
		var table, name, def string
		if err := row.Scan(&table, &name, &def); err != nil {
			return "", err
		}
		return def + ";\n", nil
	})
}

// tableDDL assembles CREATE TABLE + PK/UNIQUE constraints + COMMENT ON statements.
func (e *Exporter) tableDDL(ctx context.Context, schemaName, table string, comment sql.NullString) (string, error) {
	cols, err := e.db.QueryContext(ctx, `
SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
       pg_get_expr(ad.adbin, ad.adrelid), col_description(a.attrelid, a.attnum),
       a.attidentity, a.attgenerated
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, schemaName, table)
	if err != nil {
		return "", fmt.Errorf("columns of %s: %w", table, err)
	}
	defer cols.Close()

	var lines []string
	type colComment struct{ name, comment string }
	var colComments []colComment
	for cols.Next() {
		var name, colType string
		var nullable bool
		var def, cmt sql.NullString
		var identity, generated string
		if err := cols.Scan(&name, &colType, &nullable, &def, &cmt, &identity, &generated); err != nil {
			return "", err
		}
		l := fmt.Sprintf("    %s %s", qi(name), colType)
		if !nullable {
			l += " NOT NULL"
		}
		switch {
		case identity == "a":
			l += " GENERATED ALWAYS AS IDENTITY"
		case identity == "d":
			l += " GENERATED BY DEFAULT AS IDENTITY"
		case generated == "s":
			// Stored generated column: the expression comes from the attrdef
			// row; must be rendered as GENERATED ALWAYS AS (...) STORED,
			// never as a plain DEFAULT.
			l += " GENERATED ALWAYS AS (" + def.String + ") STORED"
		case def.Valid && def.String != "":
			l += " DEFAULT " + def.String
		}
		lines = append(lines, l)
		if cmt.Valid && cmt.String != "" {
			colComments = append(colComments, colComment{name, cmt.String})
		}
	}
	if err := cols.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("no columns found")
	}

	cons, err := e.db.QueryContext(ctx, `
SELECT con.contype, con.conname, pg_get_constraintdef(con.oid)
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND con.contype IN ('p','u')
ORDER BY con.contype, con.conname`, schemaName, table)
	if err != nil {
		return "", fmt.Errorf("constraints of %s: %w", table, err)
	}
	defer cons.Close()
	for cons.Next() {
		var typ, name, def string
		if err := cons.Scan(&typ, &name, &def); err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("    CONSTRAINT %s %s", qi(name), def))
	}
	if err := cons.Err(); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (\n%s\n);\n", qi(schemaName), qi(table), strings.Join(lines, ",\n"))
	if comment.Valid && comment.String != "" {
		fmt.Fprintf(&b, "COMMENT ON TABLE %s.%s IS '%s';\n", qi(schemaName), qi(table), strings.ReplaceAll(comment.String, "'", "''"))
	}
	for _, c := range colComments {
		fmt.Fprintf(&b, "COMMENT ON COLUMN %s.%s.%s IS '%s';\n", qi(schemaName), qi(table), qi(c.name), strings.ReplaceAll(c.comment, "'", "''"))
	}
	b.WriteString("\n")
	return b.String(), nil
}

// exportTypes renders enum + composite custom types.
func (e *Exporter) exportTypes(ctx context.Context, schemaName string) (string, int, error) {
	var b strings.Builder
	n := 0

	ddl, en, err := e.renderRows(ctx, "enum types", `
SELECT t.typname, array_agg(e.enumlabel ORDER BY e.enumsortorder)::text
FROM pg_type t
JOIN pg_namespace ns ON ns.oid = t.typnamespace
JOIN pg_enum e ON e.enumtypid = t.oid
WHERE ns.nspname = $1 AND t.typtype = 'e'
GROUP BY t.typname ORDER BY t.typname`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
		var name string
		var labels string
		if err := row.Scan(&name, &labels); err != nil {
			return "", err
		}
		vals := ParsePGArray(labels)
		quoted := make([]string, len(vals))
		for i, v := range vals {
			quoted[i] = fmt.Sprintf("'%s'", strings.ReplaceAll(strings.TrimSpace(v), "'", "''"))
		}
		return fmt.Sprintf("CREATE TYPE %s.%s AS ENUM (%s);\n", qi(schemaName), qi(name), strings.Join(quoted, ", ")), nil
	})
	if err != nil {
		return "", 0, err
	}
	b.WriteString(ddl)
	n += en

	compRows, err := e.db.QueryContext(ctx, `
SELECT t.typname, a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_type t
JOIN pg_namespace ns ON ns.oid = t.typnamespace
JOIN pg_class c ON c.oid = t.typrelid
JOIN pg_attribute a ON a.attrelid = t.typrelid
WHERE ns.nspname = $1 AND t.typtype = 'c' AND c.relkind = 'c'
  AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY t.typname, a.attnum`, schemaName)
	if err != nil {
		return "", 0, fmt.Errorf("export composite types: %w", err)
	}
	defer compRows.Close()
	byType := map[string][]string{}
	var order []string
	for compRows.Next() {
		var typ, attr, attrType string
		if err := compRows.Scan(&typ, &attr, &attrType); err != nil {
			return "", 0, err
		}
		if _, ok := byType[typ]; !ok {
			order = append(order, typ)
		}
		byType[typ] = append(byType[typ], fmt.Sprintf("    %s %s", qi(attr), attrType))
	}
	if err := compRows.Err(); err != nil {
		return "", 0, err
	}
	for _, typ := range order {
		fmt.Fprintf(&b, "CREATE TYPE %s.%s AS (\n%s\n);\n", qi(schemaName), qi(typ), strings.Join(byType[typ], ",\n"))
		n++
	}

	// Domains (typtype='d'): base type + NOT NULL + DEFAULT, with CHECK
	// constraints attached via ALTER DOMAIN. A column of a domain type
	// cannot rebuild unless the domain itself is exported.
	domRows, err := e.db.QueryContext(ctx, `
SELECT t.typname, format_type(t.typbasetype, t.typtypmod), t.typnotnull, t.typdefault
FROM pg_type t
JOIN pg_namespace ns ON ns.oid = t.typnamespace
WHERE ns.nspname = $1 AND t.typtype = 'd'
ORDER BY t.typname`, schemaName)
	if err != nil {
		return "", 0, fmt.Errorf("export domains: %w", err)
	}
	type domainDef struct {
		notNull     bool
		hasDefault  bool
		defaultExpr string
	}
	domains := map[string]domainDef{}
	var domOrder []string
	for domRows.Next() {
		var name, baseType string
		var notNull bool
		var def sql.NullString
		if err := domRows.Scan(&name, &baseType, &notNull, &def); err != nil {
			domRows.Close()
			return "", 0, err
		}
		d := domainDef{notNull: notNull, hasDefault: def.Valid && def.String != "", defaultExpr: def.String}
		if _, ok := domains[name]; !ok {
			domOrder = append(domOrder, name)
		}
		domains[name] = d
		fmt.Fprintf(&b, "CREATE DOMAIN %s.%s AS %s", qi(schemaName), qi(name), baseType)
		if d.hasDefault {
			fmt.Fprintf(&b, " DEFAULT %s", d.defaultExpr)
		}
		if d.notNull {
			b.WriteString(" NOT NULL")
		}
		b.WriteString(";\n")
		n++
	}
	if err := domRows.Err(); err != nil {
		domRows.Close()
		return "", 0, err
	}
	domRows.Close()

	chkDDL, chkN, err := e.renderRows(ctx, "domain checks", `
SELECT dt.typname, con.conname, pg_get_constraintdef(con.oid)
FROM pg_constraint con
JOIN pg_type dt ON dt.oid = con.contypid
JOIN pg_namespace n ON n.oid = con.connamespace
WHERE n.nspname = $1 AND con.contype = 'c' AND con.contypid <> 0
ORDER BY dt.typname, con.conname`, []interface{}{schemaName}, func(row *sql.Rows) (string, error) {
		var dom, name, def string
		if err := row.Scan(&dom, &name, &def); err != nil {
			return "", err
		}
		return fmt.Sprintf("ALTER DOMAIN %s.%s ADD CONSTRAINT %s %s;\n", qi(schemaName), qi(dom), qi(name), def), nil
	})
	if err != nil {
		return "", 0, err
	}
	b.WriteString(chkDDL)
	n += chkN
	return b.String(), n, nil
}

// tidbTables renders the TiDB-converted CREATE TABLE script via the existing
// schema collector + DDL builder (D2 switch).
func (e *Exporter) tidbTables(ctx context.Context, schemaName string) (string, int, error) {
	collector := schema.NewCollector(e.db)
	tables, err := collector.CollectTables(ctx, schemaName, nil)
	if err != nil {
		return "", 0, fmt.Errorf("collect tables for tidb conversion: %w", err)
	}
	var b strings.Builder
	n := 0
	for _, t := range tables {
		builder := schema.NewDDLBuilder()
		if err := builder.BuildTableDDL(t); err != nil {
			e.skip(schemaName, "tidb-table", t.Name, err.Error())
			continue
		}
		stmts := builder.Statements()
		for _, idx := range t.Indexes {
			if ddl := builder.BuildIndexDDL(idx); ddl != "" {
				stmts = append(stmts, ddl)
			}
		}
		b.WriteString(strings.Join(stmts, "\n"))
		b.WriteString("\n\n")
		n++
	}
	return b.String(), n, nil
}

// ParsePGArray parses a Postgres array literal like {a,b,c} into elements.
func ParsePGArray(s string) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return []string{}
	}
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		switch {
		case ch == '"':
			if inQuote && i+1 < len(inner) && inner[i+1] == '"' {
				cur.WriteByte('"')
				i++
			} else {
				inQuote = !inQuote
			}
		case ch == ',' && !inQuote:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	out = append(out, strings.TrimSpace(cur.String()))
	return out
}

func readmeFile(schema string) string {
	var b strings.Builder
	b.WriteString("# DDL Export\n\n")
	b.WriteString("Recommended apply order per schema (native PG DDL):\n")
	b.WriteString("types.sql -> sequences.sql -> tables.sql -> indexes.sql -> functions.sql -> procedures.sql -> triggers.sql -> views.sql\n\n")
	b.WriteString("Notes:\n")
	b.WriteString("- tables.sql contains columns, PK and UNIQUE constraints, and COMMENT ON statements.\n")
	b.WriteString("- Secondary indexes are in indexes.sql; PK / UNIQUE constraint backing indexes are inline in tables.sql.\n")
	b.WriteString("- Sequences are exported before tables so serial DEFAULT nextval(...) references resolve.\n")
	b.WriteString("- tidb-tables.sql (if present) is a TiDB-converted reference script, not native PG DDL.\n")
	b.WriteString("- manifest.json lists exported object counts and any skipped objects (e.g. permission denied).\n\n")
	b.WriteString("Schema: " + schema + "\n")
	return b.String()
}

// manifestFor builds the self-contained per-schema manifest (counts and
// skipped entries of that schema only), so each schema folder in the
// export package stands on its own.
func (e *Exporter) manifestFor(schema string) Manifest {
	m := Manifest{GeneratedAt: e.manifest.GeneratedAt, Counts: ObjectCounts{}}
	if c, ok := e.manifest.Counts[schema]; ok {
		m.Counts[schema] = c
		for _, n := range c {
			m.Total += n
		}
	}
	for _, sk := range e.manifest.Skipped {
		if sk.Schema == schema {
			m.Skipped = append(m.Skipped, sk)
		}
	}
	return m
}

// allFiles renders every file for every selected schema, in deterministic order.
func (e *Exporter) allFiles(ctx context.Context) ([]string, map[string]string, error) {
	var names []string
	files := map[string]string{}
	for _, s := range e.opts.Schemas {
		sf, err := e.schemaFiles(ctx, s)
		if err != nil {
			return nil, nil, fmt.Errorf("schema %s: %w", s, err)
		}
		for name, content := range sf {
			key := s + "/" + name
			files[key] = content
			names = append(names, key)
		}
		// Per-schema self-containment: README + manifest live inside the
		// schema folder (zip and --out directory layouts stay identical).
		files[s+"/README.txt"] = readmeFile(s)
		files[s+"/manifest.json"] = marshalManifest(e.manifestFor(s))
		names = append(names, s+"/README.txt", s+"/manifest.json")
		if e.manifest.Total > e.opts.MaxObjects {
			return nil, nil, fmt.Errorf("object count %d exceeds limit %d", e.manifest.Total, e.opts.MaxObjects)
		}
	}
	sort.Strings(names)
	return names, files, nil
}

// ExportZip streams the export as a zip archive to w and returns the manifest.
func (e *Exporter) ExportZip(ctx context.Context, w io.Writer) (Manifest, error) {
	names, files, err := e.allFiles(ctx)
	if err != nil {
		return e.manifest, err
	}
	zw := zip.NewWriter(w)
	for _, name := range names {
		fw, err := zw.Create(name)
		if err != nil {
			return e.manifest, err
		}
		if _, err := io.WriteString(fw, files[name]); err != nil {
			return e.manifest, err
		}
	}
	if err := zw.Close(); err != nil {
		return e.manifest, err
	}
	return e.manifest, nil
}

// ExportDir writes the export as plain files under dir (CLI mode, no zip).
func (e *Exporter) ExportDir(ctx context.Context, dir string) (Manifest, error) {
	names, files, err := e.allFiles(ctx)
	if err != nil {
		return e.manifest, err
	}
	for _, name := range names {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return e.manifest, err
		}
		if err := os.WriteFile(full, []byte(files[name]), 0o644); err != nil {
			return e.manifest, err
		}
	}
	return e.manifest, nil
}

func marshalManifest(m Manifest) string {
	var b strings.Builder
	b.WriteString("{\n  \"generated_at\": \"")
	b.WriteString(m.GeneratedAt.Format(time.RFC3339))
	b.WriteString("\",\n  \"total_objects\": ")
	fmt.Fprintf(&b, "%d", m.Total)
	b.WriteString(",\n  \"counts\": {")
	schemas := make([]string, 0, len(m.Counts))
	for s := range m.Counts {
		schemas = append(schemas, s)
	}
	sort.Strings(schemas)
	for si, s := range schemas {
		if si > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n    %q: {", s)
		fs := make([]string, 0, len(m.Counts[s]))
		for f := range m.Counts[s] {
			fs = append(fs, f)
		}
		sort.Strings(fs)
		for i, f := range fs {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "\n      %q: %d", f, m.Counts[s][f])
		}
		b.WriteString("\n    }")
	}
	b.WriteString("\n  },\n  \"skipped\": [")
	for i, sk := range m.Skipped {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n    {\"schema\": %q, \"type\": %q, \"object\": %q, \"reason\": %q}", sk.Schema, sk.Type, sk.Object, sk.Reason)
	}
	b.WriteString("\n  ]\n}\n")
	return b.String()
}
