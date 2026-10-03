package validator

import (
	"context"
	"database/sql"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// MS-03 CompareDialect interfaces - PURE RELOCATION scaffolding.
//
// Ruling anchors (thread seq 200-203): the four watermark guards
// (wmIdentRe/wmAllowedColumnTypes/wmFilter/wmOp), the #t4 concSem slot and its
// three deadlock invariants, the three-algorithm orchestration, md5 hash
// aggregation and the report shape ALL stay in the main flow. Dialects only
// provide identifier quoting, table qualification, predicate fragments,
// select-statement shapes, row estimates/exact counts and session setup.
// Dialect implementations must not contain any concurrency of their own.
//
// normalizeValue stays a single shared implementation for MS-03 (PG and TiDB
// return shapes are on the same path today); if MS-08 (MySQL source) diverges,
// extend AT THIS SEAM with a comment - do not pre-build empty methods here.
//
// Line references below point at the pre-relocation code (d08e661):
// validator.go / wmfilter.go / checksum.go / quick.go / nopk.go.

// dialectQueryer is the minimal query surface satisfied by both *sql.DB
// (source pool) and *sql.Conn (pinned target session).
type dialectQueryer interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// CompareDialect abstracts the SOURCE-side database specifics of the compare
// module. MS-03 ships exactly one implementation (postgres); MS-08 adds MySQL.
// No method may carry fallback policy: when EstimateRows errors, the DECISION
// to fall back to CountExact belongs to the main flow (ruling seq 202-2).
type CompareDialect interface {
	// QuoteIdent quotes one identifier. PG: "x" (validator.go quotePG :1173).
	QuoteIdent(name string) string

	// Qualify renders the table reference. PG: "schema"."table" - the
	// source keeps the two-part qualification; targets qualify single-part.
	Qualify(schema, table string) string

	// EstimateRows returns the quick-mode row ESTIMATE (not a scan).
	// PG: pg_stat_user_tables.n_live_tup (quick.go :26-30).
	// An error means "estimate unavailable" - the caller decides whether to
	// fall back to CountExact (legacy trigger: query error only, quick.go
	// :31-37 / :54-64; parse failures yield 0+nil, preserving legacy).
	EstimateRows(ctx context.Context, q dialectQueryer, schema, table string) (int64, error)

	// CountExact returns the exact COUNT(*) shape.
	// PG: SELECT COUNT(*) FROM "s"."t" (quick.go :35-36, validator.go :404).
	CountExact(ctx context.Context, q dialectQueryer, schema, table string) (int64, error)

	// WmPredicateFragment renders the watermark predicate WITHOUT the
	// WHERE keyword, including its placeholder. PG: col <= $1
	// (wmfilter.go wmWherePG :56-58). The watermark VALUE stays the single
	// query argument, supplied by the main flow; wmOp/identifier
	// allow-list/type whitelist remain in the main flow.
	WmPredicateFragment(wm *config.WatermarkFilter) string

	// Row-fetch statement shapes are NOT dialect methods: every call site
	// composes "SELECT * FROM %s [WHERE %s] [ORDER BY ...] [LIMIT/OFFSET]"
	// in the main flow via fmt.Sprintf + QuoteIdent/WmPredicateFragment
	// (byte-identical to the pre-relocation templates; ORDER BY 1 literals
	// stay literal). MS-08 adds builder methods here only if a new source
	// genuinely diverges (leader seq 214: dead surface pruned).

	// ValidateWatermarkColumn checks column existence + comparability.
	// PG: information_schema probe (wmfilter.go checkWatermarkColumn
	// :68-84) - the whitelist MAP (wmAllowedColumnTypes) stays in the main
	// flow; its application moved with the function (leader seq 209-3).
	ValidateWatermarkColumn(ctx context.Context, q dialectQueryer, schema, table string, wm *config.WatermarkFilter) error

	// DetectTableKey reads PK / unique-index metadata for the no-PK path.
	// PG: information_schema + pg_indexes probe (nopk.go detectTableKey
	// :37-100, including parseIndexColumns).
	DetectTableKey(ctx context.Context, q dialectQueryer, schema, table string) (*TableKeyInfo, error)

	// AdjustDSN applies source-side pool-level session defaults.
	// PG: appendPGDSNUTC (wmfilter.go :97-103) - P-INC-TZ touchpoint;
	// segment-2 must relocate this behavior BYTE-identically (ruling
	// seq 203-2: no drive-by fixes).
	AdjustDSN(dsn string) string
}

// TargetDialect abstracts the TARGET-side specifics. MS-03 ships exactly one
// implementation (TiDB). Method sets genuinely diverge from CompareDialect
// (ruling seq 202-1): sources own watermark-column validation + key metadata
// + DSN adjustment; targets own session init and hash-column matching.
// Methods shared by both sides appear in BOTH interfaces - no empty stubs.
type TargetDialect interface {
	// QuoteIdent quotes one identifier. TiDB/MySQL: `x` (validator.go
	// quoteMySQL :1177).
	QuoteIdent(name string) string

	// Qualify renders the table reference - single-part (the database is a
	// connection property on the target side): `t`.
	Qualify(schema, table string) string

	// EstimateRows returns the quick-mode ESTIMATE. TiDB: SHOW TABLE STATUS
	// LIKE + 17-column scan + Sscanf (quick.go :44-73). Error means
	// "estimate unavailable" - the fallback decision stays in the main flow
	// (legacy trigger: query error only; a Rows value that fails to parse
	// yields 0+nil, preserving quick.go :67-72 semantics).
	EstimateRows(ctx context.Context, q dialectQueryer, schema, table string) (int64, error)

	// CountExact returns the exact COUNT(*) shape.
	// TiDB: SELECT COUNT(*) FROM `t` (quick.go :58-59, validator.go :419).
	CountExact(ctx context.Context, q dialectQueryer, schema, table string) (int64, error)

	// WmPredicateFragment renders the watermark predicate with the driver's
	// placeholder. TiDB: `col` <= ? (wmfilter.go wmWhereMySQL :61-63).
	WmPredicateFragment(wm *config.WatermarkFilter) string

	// Row-fetch shapes are NOT dialect methods (see CompareDialect note):
	// main-flow composition + QuoteIdent/WmPredicateFragment.

	// SessionInit applies target-side per-connection session defaults.
	// TiDB: SET time_zone UTC on every pooled/pinned connection
	// (validator.go getTiDBConn). P-INC-TZ touchpoint - segment 2 only.
	SessionInit(ctx context.Context, conn *sql.Conn) error

	// MatchHashColumns maps source hash columns onto target columns for the
	// no-PK bucket path, applying the target-side type filter
	// (nopk.go :646-662: lowercase name match + approximate-float/json
	// skip). Rows themselves are hashed by the main flow's paired functions
	// computeRowHash (PG rows) / computeTiDBRowHash (TiDB rows) - the
	// pairing must NOT be unified during relocation (ruling seq 203-1).
	MatchHashColumns(sourceColNames []string, targetCols []*sql.ColumnType) []tidbColMapping
}
