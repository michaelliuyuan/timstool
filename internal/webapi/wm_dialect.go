package webapi

// MS-04 WatermarkDialect: the source-side dialect seam for the watermark
// incremental engine. Same-package counterpart of the validator's
// CompareDialect (internal/validator is a different package - the two
// QuoteIdent implementations are SAME-SOURCE DIFFERENT-PACKAGE and must NOT
// be unified; MS-08/09 revisit the seam inventory).
//
// Interface inventory approved in thread seq 265/268/269 (v2): every method
// below is a verbatim relocation of the PG-specific code it replaces; the
// main flow keeps target-side writes (incQuoteMySQL/incBuildInsertSQL/
// incTargetDSN - MS-09 seam), the watermark cursor state machine
// (incCursorStep/incJumpAfterDrain/incAdvanceWatermark), placeholder
// sharding, and worker-pool orchestration.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// incWatermarkTypes lists the information_schema.data_type values eligible as
// watermark columns (comparable, monotonic-ish types). Relocated verbatim
// from incremental.go (MS-04 commit 2/3); the package-level NAME is kept so
// the same-package white-box anchors (incremental_test.go, watermark_suggest
// drift anchor) keep compiling unchanged.
// CONSUMPTION WHITELIST (ruling seq 269): the only legal readers are
// pgWatermarkDialect methods (WatermarkEligible / QueryColumns) and those
// white-box anchors - the main flow must NOT read this map directly.
var incWatermarkTypes = map[string]bool{
	"timestamp with time zone":    true,
	"timestamp without time zone": true,
	"date":                        true,
	"integer":                     true,
	"bigint":                      true,
}

// srcCapable is the generalized D4 capability read (MS-06, ruling seq 347):
// kind "" is rejected EXPLICITLY before the capability lookup (the caller
// hands in the RAW stored type; legacy raw-compare guards rejected empty,
// so semantics must not flip), and an unknown kind resolves to false so the
// caller keeps its 400 same-text path (never escalates to 500). Callers
// that normalize first (e.g. cfg.Source.SourceType(), which defaults "" to
// "postgres") pass the NORMALIZED value and therefore keep their legacy
// empty-means-postgres ALLOW behavior - see the TestSrcCapable two-shape
// anchor and MS06-DIALECT-MAP C1 table.
func srcCapable(kind string, cap source.Capability) bool {
	if kind == "" {
		return false
	}
	ok, err := source.Capable(kind, cap)
	if err != nil {
		return false
	}
	return ok
}

// incSourceWatermarkCapable is the D4 guard's capability read (MS-04
// absorption, ruling seq 271); since MS-06 it is a thin delegate to the
// generalized srcCapable (same-package anchors keep compiling unchanged).
func incSourceWatermarkCapable(kind string) bool {
	return srcCapable(kind, source.CapWatermark)
}

// WatermarkDialect is the source-side dialect consumed by the incremental
// engine and the watermark suggest flow: identifier quoting, the three
// scan-rendering shapes, watermark type eligibility, the column catalog
// probe, and the suggest catalog (system schemas / catalog query /
// default-now detection - MS-05, ruling seq 310). v1 has exactly one
// implementation (pgWatermarkDialect) - the D4 capability guard
// (Capable(kind, CapWatermark)) guarantees only watermark-capable kinds
// reach the engine; MS-09 swaps the package-level incSourceDialect seam.
type WatermarkDialect interface {
	// QuoteIdent quotes a source identifier (relocated from incQuotePG,
	// incremental.go :142).
	QuoteIdent(s string) string

	// BuildSelectSQL renders the keyset batch scan (relocated verbatim from
	// incBuildSelectSQL, incremental.go :141-160) - SQL byte-identical.
	BuildSelectSQL(schema, table string, cols []string, wmCol string, strict bool) string

	// BuildDrainSQL renders the same-value drain scan (relocated verbatim
	// from incBuildDrainSQL, incremental.go :162-172).
	BuildDrainSQL(schema, table string, cols []string, wmCol string) string

	// BuildNextWatermarkSQL renders the post-drain MIN probe (relocated
	// verbatim from incBuildNextWatermarkSQL, incremental.go :174-179).
	BuildNextWatermarkSQL(schema, table string, wmCol string) string

	// WatermarkEligible reports whether an information_schema.data_type
	// value may serve as a watermark column (delegates to the
	// incWatermarkTypes catalog).
	WatermarkEligible(dataType string) bool

	// QueryColumns runs the watermark-eligibility column query for one
	// table (relocated verbatim from queryIncColumns, incremental.go
	// :671-702; the batch KEY query queryIncTableKeys/:716-751 stays in the
	// main flow - MS-09 seam).
	QueryColumns(ctx context.Context, db *sql.DB, schema, table string) ([]incColumnView, error)

	// SystemSchemas lists the schemas never scanned for watermark
	// candidates (MS-05: the single source of the suggest-flow system
	// schema filter; relocated verbatim from wmSystemSchemas,
	// watermark_suggest.go :44-47).
	SystemSchemas() map[string]bool

	// QuerySuggestCatalog fetches all columns of all user tables for the
	// watermark suggest flow in one query (relocated verbatim from
	// queryWMCatalog, watermark_suggest.go :79-98, with the DefaultNow
	// judgment routed through DefaultNowMatch - ruling seq 310: single
	// source, no twin regex).
	QuerySuggestCatalog(ctx context.Context, db *sql.DB, schema string) ([]wmCatalogColumn, error)

	// DefaultNowMatch reports whether a column default expression is an
	// automatically-maintained timestamp default (relocated verbatim from
	// the wmDefaultNowRe match at watermark_suggest.go :94).
	DefaultNowMatch(def string) bool

	// QueryColumnNames runs the run-time column discovery for one table
	// (MS-10a: relocated from the inline $1/$2 query at incremental.go
	// syncOneTable): ordered column names plus the watermark column's
	// data_type ("" when absent).
	QueryColumnNames(ctx context.Context, db *sql.DB, schema, table, wmCol string) (cols []string, wmType string, err error)

	// QueryTableKeys returns the dedup-key disclosure per table for the
	// batch key warning (MS-10a: relocated from queryIncTableKeys,
	// incremental.go - pg_index/ANY($2) is PG-wire-only). Tables absent
	// from the map have no qualifying key.
	QueryTableKeys(ctx context.Context, db *sql.DB, schema string, tables []string) (map[string]incKeyInfo, error)
}

// pgWatermarkDialect is the PostgreSQL implementation of WatermarkDialect.
type pgWatermarkDialect struct{}

// wmSystemSchemas are never scanned for watermark candidates. Relocated
// verbatim from watermark_suggest.go (MS-05); the package-level NAME is
// kept so the same-package anchors keep compiling unchanged.
// CONSUMPTION WHITELIST (ruling seq 310): the only legal readers are
// pgWatermarkDialect methods (SystemSchemas / QuerySuggestCatalog) and the
// white-box dual-track anchor - the main flow and handlers must NOT read
// this map directly; wmCatalogSQL's inline NOT IN is pinned equal to these
// keys by the dual-track anchor (drift in either direction is red).
var wmSystemSchemas = map[string]bool{
	"pg_catalog": true, "information_schema": true, "pg_toast": true,
}

// wmDefaultNowRe detects automatically-maintained timestamp defaults.
// The (^|[^']) guard rejects string LITERALS like 'now()'::text (a quoted
// default is a constant, not auto-maintenance); RE2 has no lookbehind, so
// the preceding-character class stands in.
// Relocated verbatim from watermark_suggest.go (MS-05).
// CONSUMPTION WHITELIST: the only legal reader is DefaultNowMatch (and the
// white-box anchors) - NO twin regex may exist (ruling seq 310).
var wmDefaultNowRe = regexp.MustCompile(`(?i)(^|[^'])(now\(\)|current_timestamp|localtimestamp|transaction_timestamp\(\))`)

// wmCatalogSQL is the suggest catalog query. Relocated verbatim from
// watermark_suggest.go (MS-05) - byte-identical SQL, structural guards
// (partition exclusion, valid-index-only, system-schema NOT IN) stay
// pinned by the same-package anchors.
// DUAL-TRACK NOTE: the inline NOT IN below and the wmSystemSchemas map are
// the same source of truth in two renderings; the dual-track anchor pins
// them equal in BOTH directions (map keys must appear quoted in the SQL,
// and the SQL literal set must equal the map key set).
const wmCatalogSQL = `
		SELECT c.table_name, c.column_name, c.data_type,
		       COALESCE(c.column_default, ''),
		       EXISTS (SELECT 1 FROM pg_index i
		                JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum, ord) ON true
		                JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		               WHERE i.indrelid = t.oid AND i.indisvalid AND a.attname = c.column_name)
		FROM information_schema.columns c
		JOIN pg_class t ON t.relname = c.table_name
		JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = c.table_schema
		WHERE c.table_schema = $1
		  AND t.relkind IN ('r', 'p')
		  AND NOT t.relispartition
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
		ORDER BY c.table_name, c.ordinal_position`

// incSourceDialect is the package-level single dial point (ruling seq 269):
// every consumer (scan renderers, MIN initial-watermark probe, log-preview
// renderers, suggest eligibility, column catalog, suggest catalog) goes
// through this instance; MS-09 swaps the selection by source kind at this
// one seam.
var incSourceDialect WatermarkDialect = pgWatermarkDialect{}

// QuoteIdent relocates incQuotePG (incremental.go :141-142) verbatim.
func (pgWatermarkDialect) QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// BuildSelectSQL relocates incBuildSelectSQL (incremental.go :141-160)
// verbatim; identifiers go through the dialect's own QuoteIdent.
func (d pgWatermarkDialect) BuildSelectSQL(schema, table string, cols []string, wmCol string, strict bool) string {
	op := ">="
	if strict {
		op = ">"
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = d.QuoteIdent(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s %s $1 ORDER BY %s LIMIT $2",
		strings.Join(quoted, ", "), d.QuoteIdent(schema), d.QuoteIdent(table), d.QuoteIdent(wmCol), op, d.QuoteIdent(wmCol))
}

// BuildDrainSQL relocates incBuildDrainSQL (incremental.go :162-172)
// verbatim.
func (d pgWatermarkDialect) BuildDrainSQL(schema, table string, cols []string, wmCol string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = d.QuoteIdent(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s = $1",
		strings.Join(quoted, ", "), d.QuoteIdent(schema), d.QuoteIdent(table), d.QuoteIdent(wmCol))
}

// BuildNextWatermarkSQL relocates incBuildNextWatermarkSQL (incremental.go
// :174-179) verbatim.
func (d pgWatermarkDialect) BuildNextWatermarkSQL(schema, table, wmCol string) string {
	return fmt.Sprintf("SELECT MIN(%s) FROM %s.%s WHERE %s > $1",
		d.QuoteIdent(wmCol), d.QuoteIdent(schema), d.QuoteIdent(table), d.QuoteIdent(wmCol))
}

// WatermarkEligible delegates to the incWatermarkTypes catalog (the catalog
// var itself stays a package-level name - see its header note).
func (pgWatermarkDialect) WatermarkEligible(dataType string) bool {
	return incWatermarkTypes[dataType]
}

// QueryColumns relocates queryIncColumns (incremental.go :671-702) verbatim
// (catalog SQL + row scan + eligibility flagging).
func (pgWatermarkDialect) QueryColumns(ctx context.Context, db *sql.DB, schema, table string) ([]incColumnView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.column_name, c.data_type,
		       (SELECT COUNT(*) FROM pg_index i
		         JOIN pg_class tc ON tc.oid = i.indrelid
		         JOIN pg_namespace ns ON ns.oid = tc.relnamespace
		         JOIN pg_attribute a ON a.attrelid = tc.oid AND a.attname = c.column_name
		         JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum, ord) ON true
		        WHERE ns.nspname = c.table_schema AND tc.relname = c.table_name
		          AND k.attnum = a.attnum AND k.ord = 1) AS indexed_first
		FROM information_schema.columns c
		WHERE c.table_schema = $1 AND c.table_name = $2
		ORDER BY c.ordinal_position`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := []incColumnView{}
	for rows.Next() {
		var c incColumnView
		var idxed int
		if err := rows.Scan(&c.Name, &c.DataType, &idxed); err != nil {
			return nil, err
		}
		c.Comparable = incWatermarkTypes[c.DataType]
		c.Indexed = idxed > 0
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// SystemSchemas relocates the wmSystemSchemas catalog read (MS-05).
func (pgWatermarkDialect) SystemSchemas() map[string]bool {
	return wmSystemSchemas
}

// QuerySuggestCatalog relocates queryWMCatalog (watermark_suggest.go
// :79-98) verbatim, with the DefaultNow judgment routed through
// DefaultNowMatch (single source, ruling seq 310).
func (d pgWatermarkDialect) QuerySuggestCatalog(ctx context.Context, db *sql.DB, schema string) ([]wmCatalogColumn, error) {
	rows, err := db.QueryContext(ctx, wmCatalogSQL, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wmCatalogColumn
	for rows.Next() {
		var c wmCatalogColumn
		var def string
		var idxed bool
		if err := rows.Scan(&c.Table, &c.Column, &c.DataType, &def, &idxed); err != nil {
			return nil, err
		}
		c.Indexed = idxed
		c.DefaultNow = d.DefaultNowMatch(def)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DefaultNowMatch relocates the wmDefaultNowRe match (watermark_suggest.go
// :94) - the ONLY consumption point of wmDefaultNowRe outside anchors.
func (pgWatermarkDialect) DefaultNowMatch(def string) bool {
	return wmDefaultNowRe.MatchString(def)
}

// QueryColumnNames relocates the syncOneTable column discovery (formerly an
// inline $1/$2 information_schema query) - PG wire shape, verbatim semantics
// (the watermark column's type is flagged in the scan loop).
func (pgWatermarkDialect) QueryColumnNames(ctx context.Context, db *sql.DB, schema, table, wmCol string) ([]string, string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var cols []string
	var wmType string
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, "", err
		}
		if name == wmCol {
			wmType = dataType
		}
		cols = append(cols, name)
	}
	return cols, wmType, rows.Err()
}

// QueryTableKeys relocates queryIncTableKeys (incremental.go :659-679)
// verbatim - pg_index + ANY($2) text[] is PG-wire-only, which is why the
// key disclosure had to join the dialect seam for MS-10a.
func (pgWatermarkDialect) QueryTableKeys(ctx context.Context, db *sql.DB, schema string, tables []string) (map[string]incKeyInfo, error) {
	if len(tables) == 0 {
		return map[string]incKeyInfo{}, nil
	}
	// pgx stdlib encodes []string natively as a text[] argument for ANY($2).
	rows, err := db.QueryContext(ctx, incTableKeysSQL, schema, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]incKeyInfo{}
	for rows.Next() {
		var name string
		var ki incKeyInfo
		if err := rows.Scan(&name, &ki.HasPK, &ki.HasUnique); err != nil {
			return nil, err
		}
		out[name] = ki
	}
	return out, rows.Err()
}

// --- MySQL dialect (MS-10a) ---

// mysqlWatermarkTypes lists the information_schema.DATA_TYPE values eligible
// as MySQL watermark columns. MySQL renders types lower-case and unprefixed
// ("int", not "integer"); datetime is the MySQL counterpart of the PG
// timestamp weight. SINGLE SOURCE: mysqlWatermarkDialect.WatermarkEligible
// and the suggest scorer's dual-catalog read both go through this map - no
// twin set may appear (same ruling shape as incWatermarkTypes seq 269).
var mysqlWatermarkTypes = map[string]bool{
	"timestamp": true,
	"datetime": true,
	"date":     true,
	"int":      true,
	"bigint":   true,
}

// mysqlSystemSchemas are the MySQL schemas never scanned for watermark
// candidates. Same consumption-whitelist shape as wmSystemSchemas: only
// mysqlWatermarkDialect.SystemSchemas / QuerySuggestCatalog and anchors.
var mysqlSystemSchemas = map[string]bool{
	"mysql": true, "sys": true, "performance_schema": true, "information_schema": true,
}

// mysqlWatermarkDialect is the MySQL implementation of WatermarkDialect
// (MS-10a): backtick quoting, ? placeholders, information_schema catalog
// probes (STATISTICS for index metadata - MySQL has no pg_index). The
// source read session arrives UTC-pinned via DSNByType (MS-08d
// time_zone='+00:00'), so timestamp/datetime scans render as UTC instants
// and the cursor string round-trips without offset drift (F-13 family
// discipline).
type mysqlWatermarkDialect struct{}

// QuoteIdent quotes a MySQL identifier (backticks, doubled on escape).
func (mysqlWatermarkDialect) QuoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// BuildSelectSQL renders the keyset batch scan with ? placeholders and
// LIMIT ? (MySQL prepared statements accept a parameterized LIMIT).
func (d mysqlWatermarkDialect) BuildSelectSQL(schema, table string, cols []string, wmCol string, strict bool) string {
	op := ">="
	if strict {
		op = ">"
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = d.QuoteIdent(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s %s ? ORDER BY %s LIMIT ?",
		strings.Join(quoted, ", "), d.QuoteIdent(schema), d.QuoteIdent(table), d.QuoteIdent(wmCol), op, d.QuoteIdent(wmCol))
}

// BuildDrainSQL renders the same-value drain scan with ? placeholder.
func (d mysqlWatermarkDialect) BuildDrainSQL(schema, table string, cols []string, wmCol string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = d.QuoteIdent(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s = ?",
		strings.Join(quoted, ", "), d.QuoteIdent(schema), d.QuoteIdent(table), d.QuoteIdent(wmCol))
}

// BuildNextWatermarkSQL renders the post-drain MIN probe.
func (d mysqlWatermarkDialect) BuildNextWatermarkSQL(schema, table, wmCol string) string {
	return fmt.Sprintf("SELECT MIN(%s) FROM %s.%s WHERE %s > ?",
		d.QuoteIdent(wmCol), d.QuoteIdent(schema), d.QuoteIdent(table), d.QuoteIdent(wmCol))
}

// WatermarkEligible delegates to the mysqlWatermarkTypes catalog.
func (mysqlWatermarkDialect) WatermarkEligible(dataType string) bool {
	return mysqlWatermarkTypes[dataType]
}

// QueryColumns mirrors pgWatermarkDialect.QueryColumns over MySQL's
// information_schema: the indexed_first flag comes from STATISTICS
// (SEQ_IN_INDEX=1 = the column leads some index).
func (mysqlWatermarkDialect) QueryColumns(ctx context.Context, db *sql.DB, schema, table string) ([]incColumnView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.COLUMN_NAME, c.DATA_TYPE,
		       EXISTS (SELECT 1 FROM information_schema.STATISTICS s
		                WHERE s.TABLE_SCHEMA = c.TABLE_SCHEMA AND s.TABLE_NAME = c.TABLE_NAME
		                  AND s.COLUMN_NAME = c.COLUMN_NAME AND s.SEQ_IN_INDEX = 1) AS indexed_first
		FROM information_schema.COLUMNS c
		WHERE c.TABLE_SCHEMA = ? AND c.TABLE_NAME = ?
		ORDER BY c.ORDINAL_POSITION`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := []incColumnView{}
	for rows.Next() {
		var c incColumnView
		var idxed int
		if err := rows.Scan(&c.Name, &c.DataType, &idxed); err != nil {
			return nil, err
		}
		c.Comparable = mysqlWatermarkTypes[c.DataType]
		c.Indexed = idxed > 0
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// SystemSchemas returns the MySQL system schema set.
func (mysqlWatermarkDialect) SystemSchemas() map[string]bool {
	return mysqlSystemSchemas
}

// QuerySuggestCatalog mirrors pgWatermarkDialect.QuerySuggestCatalog over
// MySQL's information_schema (BASE TABLE only - no views); the DefaultNow
// judgment reuses the same regex source (CURRENT_TIMESTAMP matches).
func (d mysqlWatermarkDialect) QuerySuggestCatalog(ctx context.Context, db *sql.DB, schema string) ([]wmCatalogColumn, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, COALESCE(c.COLUMN_DEFAULT, ''),
		       EXISTS (SELECT 1 FROM information_schema.STATISTICS s
		                WHERE s.TABLE_SCHEMA = c.TABLE_SCHEMA AND s.TABLE_NAME = c.TABLE_NAME
		                  AND s.COLUMN_NAME = c.COLUMN_NAME AND s.SEQ_IN_INDEX = 1) AS indexed_first
		FROM information_schema.COLUMNS c
		JOIN information_schema.TABLES t
		  ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME AND t.TABLE_TYPE = 'BASE TABLE'
		WHERE c.TABLE_SCHEMA = ?
		ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wmCatalogColumn
	for rows.Next() {
		var c wmCatalogColumn
		var def string
		var idxed bool
		if err := rows.Scan(&c.Table, &c.Column, &c.DataType, &def, &idxed); err != nil {
			return nil, err
		}
		c.Indexed = idxed
		c.DefaultNow = d.DefaultNowMatch(def)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DefaultNowMatch reuses wmDefaultNowRe (MySQL's CURRENT_TIMESTAMP matches
// the same pattern; no twin regex - ruling seq 310).
func (mysqlWatermarkDialect) DefaultNowMatch(def string) bool {
	return wmDefaultNowRe.MatchString(def)
}

// QueryColumnNames mirrors pgWatermarkDialect.QueryColumnNames on the MySQL
// wire (? placeholders); the watermark column's type is flagged in the scan
// loop exactly like the PG side.
func (mysqlWatermarkDialect) QueryColumnNames(ctx context.Context, db *sql.DB, schema, table, wmCol string) ([]string, string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME, DATA_TYPE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, schema, table)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var cols []string
	var wmType string
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, "", err
		}
		if name == wmCol {
			wmType = dataType
		}
		cols = append(cols, name)
	}
	return cols, wmType, rows.Err()
}

// QueryTableKeys mirrors pgWatermarkDialect.QueryTableKeys over MySQL's
// STATISTICS: has_pk = PRIMARY leads the table, has_unique = any valid
// non-primary unique index. go-sql-driver has no array parameter, so the
// table list becomes a parameterized IN (?, ...) - table names are already
// behind the incIdentifierOK allow-list at every caller.
func (mysqlWatermarkDialect) QueryTableKeys(ctx context.Context, db *sql.DB, schema string, tables []string) (map[string]incKeyInfo, error) {
	if len(tables) == 0 {
		return map[string]incKeyInfo{}, nil
	}
	ph := make([]string, len(tables))
	args := make([]any, 0, len(tables)+1)
	args = append(args, schema)
	for i, t := range tables {
		ph[i] = "?"
		args = append(args, t)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.TABLE_NAME,
		       MAX(s.INDEX_NAME = 'PRIMARY') AS has_pk,
		       MAX(s.NON_UNIQUE = 0 AND s.INDEX_NAME <> 'PRIMARY') AS has_unique
		FROM information_schema.STATISTICS s
		WHERE s.TABLE_SCHEMA = ? AND s.TABLE_NAME IN (`+strings.Join(ph, ", ")+`)
		GROUP BY s.TABLE_NAME`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]incKeyInfo{}
	for rows.Next() {
		var name string
		var ki incKeyInfo
		if err := rows.Scan(&name, &ki.HasPK, &ki.HasUnique); err != nil {
			return nil, err
		}
		out[name] = ki
	}
	return out, rows.Err()
}

// incDialectFor is the kind-based seam swap announced on incSourceDialect
// (MS-09 note; landed with MS-10a): every consumer that knows the source
// kind resolves its dialect here. The package-level incSourceDialect stays
// the PG default so the same-package white-box anchors keep pinning the PG
// shapes byte-identically.
func incDialectFor(kind string) WatermarkDialect {
	if kind == "mysql" {
		return mysqlWatermarkDialect{}
	}
	return pgWatermarkDialect{}
}
