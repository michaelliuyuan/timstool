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

// incSourceWatermarkCapable is the D4 guard's capability read (MS-04
// absorption, ruling seq 271): kind "" is rejected EXPLICITLY before the
// capability lookup (NormalizeKind("") would default it to postgres - the
// legacy guard rejected empty, so semantics must not flip), and an unknown
// kind resolves to false so the caller keeps its 400 same-text path (never
// escalates to 500).
func incSourceWatermarkCapable(kind string) bool {
	if kind == "" {
		return false
	}
	ok, err := source.Capable(kind, source.CapWatermark)
	if err != nil {
		return false
	}
	return ok
}

// WatermarkDialect is the source-side dialect consumed by the incremental
// engine: identifier quoting, the three scan-rendering shapes, watermark
// type eligibility, and the column catalog probe. v1 has exactly one
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
	BuildNextWatermarkSQL(schema, table, wmCol string) string

	// WatermarkEligible reports whether an information_schema.data_type
	// value may serve as a watermark column (delegates to the
	// incWatermarkTypes catalog).
	WatermarkEligible(dataType string) bool

	// QueryColumns runs the watermark-eligibility column query for one
	// table (relocated verbatim from queryIncColumns, incremental.go
	// :671-702; the batch KEY query queryIncTableKeys/:716-751 stays in the
	// main flow - MS-09 seam).
	QueryColumns(ctx context.Context, db *sql.DB, schema, table string) ([]incColumnView, error)
}

// pgWatermarkDialect is the PostgreSQL implementation of WatermarkDialect.
type pgWatermarkDialect struct{}

// incSourceDialect is the package-level single dial point (ruling seq 269):
// every consumer (scan renderers, MIN initial-watermark probe, log-preview
// renderers, suggest eligibility, column catalog) goes through this
// instance; MS-09 swaps the selection by source kind at this one seam.
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
