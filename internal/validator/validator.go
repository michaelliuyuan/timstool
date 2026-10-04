package validator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/michaelliuyuan/timstool/internal/common"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	cerrors "github.com/michaelliuyuan/timstool/internal/common/errors"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"go.uber.org/zap"
)

type Validator struct {
	cfg config.Config

	// Standalone-comparison overrides (webapi compare tasks). Zero values
	// fall back to cfg so Run() behavior is unchanged for orchestrator/CLI.
	pgDSNOverride    string
	tidbDSNOverride  string
	schemaOverride   string
	compareOverride  *config.CompareConfig
	parallelOverride int

	// onTableDone is an optional per-table progress callback (tables done so
	// far, total tables, latest table report). Called from worker goroutines;
	// implementations must be safe for concurrent use.
	onTableDone func(done, total int, tr reporter.TableReport)

	// MS-03 dialect injection: source-side and target-side specifics live
	// behind CompareDialect/TargetDialect (pure relocation; see
	// docs/MS03-DIALECT-MAP.md). Defaults preserve the pre-MS-03 behavior.
	srcDialect CompareDialect
	tgtDialect TargetDialect
}

// NewValidator assembles the validator with dialects dispatched on the
// NORMALIZED source type (MS-08): mysql gets the MySQL source dialect +
// go-sql-driver DSN/driver; everything else (including the empty default,
// which SourceType() normalizes to postgres) keeps the legacy PG assembly
// byte-identically. Unknown types are unreachable behind the webapi
// capability guard (srcCapable) and fall to the PG shape here.
func NewValidator(cfg config.Config) *Validator {
	v := &Validator{cfg: cfg, tgtDialect: tidbDialect{}}
	if cfg.Source.SourceType() == "mysql" {
		v.srcDialect = mysqlDialect{}
	} else {
		v.srcDialect = postgresDialect{}
	}
	return v
}

// srcLabel returns the user-facing source label for mismatch report
// formatting, dispatched on the assembled source dialect ("PG" for the
// legacy postgres assembly, "MySQL" for the mysql one) so reports stop
// hard-coding PG when comparing a MySQL source.
func (v *Validator) srcLabel() string {
	if _, ok := v.srcDialect.(mysqlDialect); ok {
		return "MySQL"
	}
	return "PG"
}

// srcDriverName returns the database/sql driver for the source side,
// dispatched on the assembled dialect (pgx for PG, mysql for MySQL/TiDB
// wire).
func (v *Validator) srcDriverName() string {
	if _, ok := v.srcDialect.(mysqlDialect); ok {
		return "mysql"
	}
	return "pgx"
}

// sourceDSN returns the PostgreSQL DSN to connect to (override first).
func (v *Validator) sourceDSN() string {
	if v.pgDSNOverride != "" {
		return v.pgDSNOverride
	}
	return v.cfg.Source.DSN()
}

// targetDSN returns the TiDB DSN to connect to (override first).
func (v *Validator) targetDSN() string {
	if v.tidbDSNOverride != "" {
		return v.tidbDSNOverride
	}
	return v.cfg.Target.DSN()
}

// sourceSchema returns the source schema name (override first, may be "").
// MS-08: a MySQL source has no PG-style schema (schema == database), so an
// empty schema defaults to the connection database instead of "public".
func (v *Validator) sourceSchema() string {
	if v.schemaOverride != "" {
		return v.schemaOverride
	}
	if v.cfg.Source.Schema != "" {
		return v.cfg.Source.Schema
	}
	if _, ok := v.srcDialect.(mysqlDialect); ok {
		return v.cfg.Source.Database
	}
	return v.cfg.Source.Schema
}

// compareCfg returns the effective compare configuration (override first).
func (v *Validator) compareCfg() config.CompareConfig {
	if v.compareOverride != nil {
		return *v.compareOverride
	}
	return v.cfg.Compare
}

// parallelism returns the effective table-level parallelism (override first).
func (v *Validator) parallelism() int {
	if v.parallelOverride > 0 {
		return v.parallelOverride
	}
	return v.cfg.Migration.Parallel
}

// MaxConcBudget is the hard cap for the unified concurrency budget. Both DB
// pools are capped at 8 open conns, so a budget above 8 could re-create the
// pool-exhaustion deadlock (#t4 root cause).
const MaxConcBudget = 8

// ResolveConcurrency maps the unified budget plus the two legacy knobs onto
// one effective value (#t4): an explicit concurrency wins; otherwise the
// larger of the legacy table-level parallel and chunk-level parallel is
// adopted; the result is clamped into [1, MaxConcBudget] with a default of 4.
// Exported so webapi folds deprecated request knobs into the same value.
func ResolveConcurrency(concurrency, legacyParallel, legacyChunkParallel int) int {
	c := concurrency
	if c <= 0 {
		c = legacyParallel
		if legacyChunkParallel > c {
			c = legacyChunkParallel
		}
	}
	if c <= 0 {
		c = 4
	}
	if c > MaxConcBudget {
		c = MaxConcBudget
	}
	return c
}

// concurrency returns the effective unified concurrency budget for Run.
func (v *Validator) concurrency() int {
	cc := v.compareCfg()
	return ResolveConcurrency(cc.Concurrency, v.parallelism(), cc.ChecksumParallel)
}

// concSem is the single-layer global work-unit semaphore (#t4). Every DB
// connection acquisition (dedicated TiDB conns and pool-backed queries)
// happens while holding a slot, and no goroutine waits on the sem while
// holding a connection — the invariant that structurally rules out the
// old pool-vs-WaitGroup deadlock.
type concSem chan struct{}

// acquire blocks until a slot is free or ctx is done. Holding the returned
// release func's result is required: acquire MUST be followed by release.
func (s concSem) acquire(ctx context.Context) (func(), error) {
	select {
	case s <- struct{}{}:
		return func() { <-s }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// OnTableDone registers a per-table progress callback used by Run (and thus
// RunWithDSNs). Safe to call before starting a run.
func (v *Validator) OnTableDone(fn func(done, total int, tr reporter.TableReport)) {
	v.onTableDone = fn
}

// RunWithDSNs runs a standalone comparison against explicit DSNs instead of
// the validator's config file. pgDSN/tidbDSN are used verbatim; schema,
// compare and parallel override the corresponding config values when non-zero
// (schema non-empty). Behavior is otherwise identical to Run.
func (v *Validator) RunWithDSNs(ctx context.Context, pgDSN, tidbDSN, schema string, compare config.CompareConfig, parallel int, opts common.ValidateOpts) (*reporter.Report, error) {
	clone := *v
	clone.pgDSNOverride = pgDSN
	clone.tidbDSNOverride = tidbDSN
	if schema != "" {
		clone.schemaOverride = schema
	}
	clone.compareOverride = &compare
	if parallel > 0 {
		clone.parallelOverride = parallel
	}
	return clone.Run(ctx, opts)
}

// getTiDBConn gets a dedicated connection from the TiDB connection pool and
// initializes the session (timezone to UTC) via the target dialect - this
// ensures TIMESTAMP values are returned in UTC, matching PostgreSQL's
// timestamptz output. P-INC-TZ touchpoint: session setup relocated
// byte-identically (ruling seq 204-2); the fix lands in segment 2 only.
func (v *Validator) getTiDBConn(ctx context.Context, tidbDB *sql.DB) (*sql.Conn, error) {
	conn, err := tidbDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("get TiDB connection: %w", err)
	}
	if err := v.tgtDialect.SessionInit(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
func (v *Validator) Run(ctx context.Context, opts common.ValidateOpts) (*reporter.Report, error) {
	logger := zap.L()
	logger.Info("starting data validation", zap.String("level", opts.Level), zap.String("mode", opts.Mode))

	// Resolve effective mode: CLI flag > config default
	mode := opts.Mode
	if mode == "" {
		mode = v.compareCfg().CompareMode
	}
	if mode == "" {
		mode = "sample"
	}

	rpt := reporter.NewReport("data-validation")

	// #t3: while a watermark filter is active the PG session runs in UTC so
	// timestamptz literals compare symmetrically with the TiDB UTC session.
	pgDSN := v.sourceDSN()
	wm := v.wmFilter()
	if wm != nil {
		pgDSN = v.srcDialect.AdjustDSN(pgDSN)
	}

	pgDB, err := sql.Open(v.srcDriverName(), pgDSN)
	if err != nil {
		return nil, cerrors.Wrap(cerrors.ErrSourceConnect, "connect to PostgreSQL", err)
	}
	defer pgDB.Close()

	tidbDB, err := sql.Open("mysql", v.targetDSN())
	if err != nil {
		return nil, cerrors.Wrap(cerrors.ErrTargetConnect, "connect to TiDB", err)
	}
	defer tidbDB.Close()

	pgDB.SetMaxOpenConns(8)
	pgDB.SetConnMaxLifetime(5 * time.Minute)
	tidbDB.SetMaxOpenConns(8)
	tidbDB.SetConnMaxLifetime(5 * time.Minute)

	tables, err := v.getTables(ctx, pgDB, opts.Tables)
	if err != nil {
		return nil, cerrors.Wrap(cerrors.ErrValidateRowCount, "get table list", err)
	}

	// #t4: single unified concurrency budget. Table goroutines no longer
	// hold a semaphore slot (and a TiDB conn) for the whole table: quick and
	// sample validations are one work unit each; checksum mode acquires a
	// slot for the count phase and one slot per chunk, so a table worker
	// never waits on the sem while holding a connection.
	budget := v.concurrency()
	logger.Info("validation concurrency budget",
		zap.Int("budget", budget), zap.Int("tables", len(tables)))

	var mu sync.Mutex
	sem := make(concSem, budget)
	var wg sync.WaitGroup
	var tablesDone int32

	for _, table := range tables {
		wg.Add(1)
		go func(tableName string) {
			defer wg.Done()

			var tr reporter.TableReport
			switch mode {
			case "quick", "sample":
				tr = v.runTableUnit(ctx, pgDB, tidbDB, sem, tableName, wm, mode, opts.SampleRatio)
			case "checksum":
				// Watermark column precheck first (1 slot, pool-backed PG
				// query inside the slot), then the chunked comparison which
				// manages its own per-unit slots (count = 1 unit, each
				// chunk = 1 unit) — no slot held across waits.
				if wm != nil {
					release, aerr := sem.acquire(ctx)
					if aerr != nil {
						tr = reporter.TableReport{TableName: tableName, Status: reporter.StatusFail,
							Error: fmt.Sprintf("cancelled before start: %v", aerr)}
						break
					}
					werr := v.srcDialect.ValidateWatermarkColumn(ctx, pgDB, schemaOrDefault(v.sourceSchema()), tableName, wm)
					release()
					if werr != nil {
						tr = reporter.TableReport{
							TableName: tableName,
							Status:    reporter.StatusFail,
							Error:     werr.Error(),
						}
						break
					}
				}
				tr = v.validateChecksumChunked(ctx, pgDB, tidbDB, tableName, sem)
			default:
				tr = reporter.TableReport{
					TableName: tableName,
					Status:    reporter.StatusFail,
					Error:     fmt.Sprintf("unknown validation mode: %s", mode),
				}
			}

			// #t3: stamp the watermark scope on every table report
			// (SourceRows/TargetRows are already the filtered counts —
			// validateRowCount runs first in every mode).
			if wm != nil {
				note := fmt.Sprintf("水位过滤 %s %s %s（源 %d 行/目标 %d 行）",
					wm.Column, wmOp(wm), wm.Value, tr.SourceRows, tr.TargetRows)
				if tr.Suggestion == "" {
					tr.Suggestion = note
				} else {
					tr.Suggestion = note + "；" + tr.Suggestion
				}
			}

			mu.Lock()
			rpt.AddTableReport(tr)
			mu.Unlock()

			if v.onTableDone != nil {
				done := int(atomic.AddInt32(&tablesDone, 1))
				v.onTableDone(done, len(tables), tr)
			}

			logger.Info("table validation result",
				zap.String("table", tableName),
				zap.String("status", string(tr.Status)),
				zap.Int64("diff", tr.DiffRows))
		}(table)
	}

	wg.Wait()

	// Log summary of failed/warned tables for visibility
	failTables := rpt.FailedTables()
	if len(failTables) > 0 {
		for _, t := range failTables {
			logger.Warn("table validation FAILED",
				zap.String("table", t.TableName),
				zap.String("error", t.Error),
				zap.Int64("diff", t.DiffRows))
		}
		logger.Warn("data validation summary",
			zap.Int("failed", len(failTables)),
			zap.Int("total", len(tables)))
	}

	rpt.Finish(rpt.OverallStatus(), fmt.Sprintf("validated %d tables at level %s", len(tables), opts.Level))

	if opts.ReportFile != "" {
		if err := rpt.Save(opts.ReportFile); err != nil {
			logger.Warn("failed to save report", zap.Error(err))
		}
	}

	return rpt, nil
}

// runTableUnit runs ONE budget work unit for quick/sample modes (#t4): the
// slot is acquired first, the dedicated UTC TiDB conn + the optional
// watermark precheck (pool-backed PG query) + the validation body all happen
// inside the slot, then conn and slot are released. Watermark predicate
// construction itself is untouched (#t3 paths preserved verbatim).
func (v *Validator) runTableUnit(ctx context.Context, pgDB, tidbDB *sql.DB, sem concSem,
	table string, wm *config.WatermarkFilter, mode string, sampleRatio float64) reporter.TableReport {
	// 1 table validation = 1 work unit: the dedicated UTC TiDB conn lives
	// entirely inside the budget slot.
	release, aerr := sem.acquire(ctx)
	if aerr != nil {
		return reporter.TableReport{TableName: table, Status: reporter.StatusFail,
			Error: fmt.Sprintf("cancelled before start: %v", aerr)}
	}
	tidbConn, connErr := v.getTiDBConn(ctx, tidbDB)
	if connErr != nil {
		release()
		return reporter.TableReport{TableName: table, Status: reporter.StatusFail,
			Error: fmt.Sprintf("get TiDB connection: %v", connErr)}
	}
	var tr reporter.TableReport
	if mode == "quick" {
		tr = v.validateTableUnit(ctx, pgDB, tidbConn, table, wm, func(c *sql.Conn) reporter.TableReport {
			return v.validateRowCount(ctx, pgDB, c, table)
		})
	} else {
		tr = v.validateTableUnit(ctx, pgDB, tidbConn, table, wm, func(c *sql.Conn) reporter.TableReport {
			return v.validateSampling(ctx, pgDB, c, tidbDB, table, sampleRatio)
		})
	}
	tidbConn.Close()
	release()
	return tr
}

// validateTableUnit runs one single-slot work unit for quick/sample modes:
// the optional watermark column precheck (pool-backed PG query) plus the mode
// body, all while the caller holds a budget slot and the dedicated TiDB conn.
func (v *Validator) validateTableUnit(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn, table string,
	wm *config.WatermarkFilter, fn func(*sql.Conn) reporter.TableReport) reporter.TableReport {
	if wm != nil {
		if werr := v.srcDialect.ValidateWatermarkColumn(ctx, pgDB, schemaOrDefault(v.sourceSchema()), table, wm); werr != nil {
			return reporter.TableReport{
				TableName: table,
				Status:    reporter.StatusFail,
				Error:     werr.Error(),
			}
		}
	}
	return fn(tidbConn)
}

func (v *Validator) validateRowCount(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn, table string) reporter.TableReport {
	tr := reporter.TableReport{TableName: table, Status: reporter.StatusPass}

	schema := v.sourceSchema()
	if schema == "" {
		schema = "public"
	}

	var sourceCount int64
	var err error
	wm := v.wmFilter()
	if wm != nil {
		err = pgDB.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s.%s WHERE %s",
				v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), v.srcDialect.WmPredicateFragment(wm)), wm.Value).Scan(&sourceCount)
	} else {
		err = pgDB.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s.%s", v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table))).Scan(&sourceCount)
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("source count: %v", err)
		return tr
	}

	var targetCount int64
	if wm != nil {
		err = tidbConn.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s",
				v.tgtDialect.QuoteIdent(table), v.tgtDialect.WmPredicateFragment(wm)), wm.Value).Scan(&targetCount)
	} else {
		err = tidbConn.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s", v.tgtDialect.QuoteIdent(table))).Scan(&targetCount)
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("target count: %v", err)
		return tr
	}

	tr.SourceRows = sourceCount
	tr.TargetRows = targetCount
	tr.DiffRows = sourceCount - targetCount

	if tr.DiffRows != 0 {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("row count mismatch: source=%d target=%d diff=%d", sourceCount, targetCount, tr.DiffRows)
	}

	return tr
}

// sampleOffset picks a random OFFSET for the sampling window. F-12 defense:
// when sampleSize >= rows (ratio > 1 against a table smaller than the window,
// or a 1-row table) the naive span rows-sampleSize+1 goes <= 0 and
// rand.Int63n panics on the negative argument — take the whole table from
// offset 0 instead.
func sampleOffset(rows int64, sampleSize int) int64 {
	if rows <= 0 || int64(sampleSize) >= rows {
		return 0
	}
	return rand.Int63n(rows - int64(sampleSize) + 1)
}
func (v *Validator) validateSampling(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn, tidbDB *sql.DB, table string, ratio float64) reporter.TableReport {
	tr := v.validateRowCount(ctx, pgDB, tidbConn, table)
	// Same fallthrough guard as checksum.go: a COUNT error (DiffRows==0,
	// Status=Fail) must not reach the SourceRows==0 PASS branch (F-05).
	if tr.Status == reporter.StatusFail {
		return tr
	}

	if tr.SourceRows == 0 {
		tr.Status = reporter.StatusPass
		return tr
	}

	schema := v.sourceSchema()
	if schema == "" {
		schema = "public"
	}

	// Detect table structure: does it have a primary key or unique index?
	keyInfo, err := v.srcDialect.DetectTableKey(ctx, pgDB, schema, table)
	if err != nil {
		logger := zap.L()
		logger.Warn("failed to detect table key, assuming no PK", zap.String("table", table), zap.Error(err))
		keyInfo = &TableKeyInfo{} // treat as no-PK
	}
	needsNoPKStrategy := !keyInfo.HasPK && !keyInfo.HasUniqueIndex

	// For no-PK tables, decide which strategy to use
	if needsNoPKStrategy {
		strategy := v.compareCfg().NoPKStrategy
		if strategy == "" {
			strategy = "auto"
		}

		// Auto-select strategy based on table size
		if strategy == "auto" {
			threshold := v.compareCfg().NoPKTableThreshold
			if threshold <= 0 {
				threshold = 1000000
			}
			if tr.SourceRows <= threshold {
				strategy = "hash_group"
			} else {
				strategy = "aggregate" // Phase 2 will implement this
			}
		}

		if strategy == "hash_group" {
			return v.validateSamplingWithHashGroup(ctx, pgDB, tidbConn, table, ratio, tr, schema)
		}
		if strategy == "aggregate" {
			return v.validateNoPKWithAggregate(ctx, pgDB, tidbConn, table, tr, schema)
		}
		if strategy == "bucket" {
			return v.validateNoPKWithBucket(ctx, pgDB, tidbConn, table, tr, schema)
		}
		// Unknown strategy falls through to existing sampling logic
	}

	sampleSize := int(float64(tr.SourceRows) * ratio)
	if sampleSize < 1 {
		sampleSize = 1
	}
	if sampleSize > 1000 {
		sampleSize = 1000
	}

	offset := sampleOffset(tr.SourceRows, sampleSize)

	wm := v.wmFilter()
	var pgRows *sql.Rows
	if wm != nil {
		pgQuery := fmt.Sprintf("SELECT * FROM %s.%s WHERE %s ORDER BY 1 LIMIT %d OFFSET %d",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), v.srcDialect.WmPredicateFragment(wm), sampleSize, offset)
		pgRows, err = pgDB.QueryContext(ctx, pgQuery, wm.Value)
	} else {
		pgQuery := fmt.Sprintf("SELECT * FROM %s.%s ORDER BY 1 LIMIT %d OFFSET %d",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), sampleSize, offset)
		pgRows, err = pgDB.QueryContext(ctx, pgQuery)
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("sample source: %v", err)
		return tr
	}
	defer pgRows.Close()

	pgCols, _ := pgRows.ColumnTypes()
	if pgCols == nil {
		tr.Status = reporter.StatusFail
		tr.Error = "failed to get source column types"
		return tr
	}

	// Build sets of column indices to skip or trim in comparison.
	// Floating point types have inherent precision differences between PG and TiDB.
	// CHAR/VARCHAR/TEXT types may differ in trailing spaces (MySQL auto-trims CHAR).
	skipCols := make(map[int]bool)
	trimCols := make(map[int]bool)
	for i, c := range pgCols {
		dt := strings.ToLower(c.DatabaseTypeName())
		if isApproximateFloatType(dt) ||
			strings.Contains(dt, "json") {
			skipCols[i] = true
		}
		if dt == "character" || dt == "char" || dt == "bpchar" || dt == "character varying" || dt == "varchar" || dt == "text" {
			trimCols[i] = true
		}
	}

	pgValues := make([]interface{}, len(pgCols))
	pgPtrs := make([]interface{}, len(pgCols))
	for i := range pgValues {
		pgPtrs[i] = &pgValues[i]
	}

	var pgData [][]string
	for pgRows.Next() {
		if err := pgRows.Scan(pgPtrs...); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("scan %s row: %v", v.srcLabel(), err)
			return tr
		}
		row := make([]string, len(pgCols))
		for i, val := range pgValues {
			row[i] = normalizeValue(val)
		}
		pgData = append(pgData, row)
	}

	// The Go code below should be inserted at the right indentation level.
	// Determine key columns for matching.
	// If the table has a PK (single or composite), use ALL PK columns as the key.
	var keyColIndices []int
	if keyInfo != nil && keyInfo.HasPK && len(keyInfo.PKColumns) > 0 {
		for _, pkCol := range keyInfo.PKColumns {
			for i, c := range pgCols {
				if strings.ToLower(c.Name()) == strings.ToLower(pkCol) {
					allNonNULL := true
					for _, row := range pgData {
						if i >= len(row) || row[i] == "\\N" {
							allNonNULL = false
							break
						}
					}
					if allNonNULL {
						keyColIndices = append(keyColIndices, i)
					}
					break
				}
			}
		}
	}

	// Fallback: if no PK or PK columns have NULLs, find first non-skipped column
	if len(keyColIndices) == 0 {
		for colIdx := 0; colIdx < len(pgCols); colIdx++ {
			if skipCols[colIdx] {
				continue
			}
			allNonNULL := true
			for _, row := range pgData {
				if colIdx >= len(row) || row[colIdx] == "\\N" {
					allNonNULL = false
					break
				}
			}
			if allNonNULL {
				keyColIndices = []int{colIdx}
				break
			}
		}
	}

	buildKey := func(row []string) string {
		var parts []string
		for _, idx := range keyColIndices {
			if idx < len(row) {
				parts = append(parts, row[idx])
			} else {
				parts = append(parts, "\\N")
			}
		}
		return strings.Join(parts, "|")
	}

	var mismatchCount int
	var mismatchDetails []string

	if len(keyColIndices) > 0 {
		isCompositePK := len(keyColIndices) > 1
		var tidbQuery string
		var tidbArgs []interface{}

		if isCompositePK {
			var colNames []string
			for _, idx := range keyColIndices {
				colNames = append(colNames, v.tgtDialect.QuoteIdent(pgCols[idx].Name()))
			}
			seen := make(map[string]bool)
			var tupleParts []string
			for _, row := range pgData {
				key := buildKey(row)
				if seen[key] {
					continue
				}
				seen[key] = true
				var vals []string
				for _, idx := range keyColIndices {
					escaped := strings.ReplaceAll(row[idx], "'", "\\'")
					vals = append(vals, fmt.Sprintf("'%s'", escaped))
				}
				tupleParts = append(tupleParts, fmt.Sprintf("(%s)", strings.Join(vals, ",")))
			}
			if len(tupleParts) == 0 {
				tr.Status = reporter.StatusPass
				return tr
			}
			tidbQuery = fmt.Sprintf("SELECT * FROM %s WHERE (%s) IN (%s)",
				v.tgtDialect.QuoteIdent(table), strings.Join(colNames, ","), strings.Join(tupleParts, ","))
			if wm != nil {
				tidbQuery += fmt.Sprintf(" AND %s", v.tgtDialect.WmPredicateFragment(wm))
				tidbArgs = []interface{}{wm.Value}
			}
		} else {
			keyColName := pgCols[keyColIndices[0]].Name()
			var whereParts []string
			seen := make(map[string]bool)
			for _, row := range pgData {
				val := row[keyColIndices[0]]
				if val == "\\N" || seen[val] {
					continue
				}
				seen[val] = true
				escaped := strings.ReplaceAll(val, "'", "\\'")
				whereParts = append(whereParts, fmt.Sprintf("'%s'", escaped))
			}
			if len(whereParts) == 0 {
				tr.Status = reporter.StatusPass
				return tr
			}
			tidbQuery = fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s)",
				v.tgtDialect.QuoteIdent(table), v.tgtDialect.QuoteIdent(keyColName), strings.Join(whereParts, ","))
			if wm != nil {
				tidbQuery += fmt.Sprintf(" AND %s", v.tgtDialect.WmPredicateFragment(wm))
				tidbArgs = []interface{}{wm.Value}
			}
		}

		var tidbRows *sql.Rows
		if len(tidbArgs) > 0 {
			tidbRows, err = tidbConn.QueryContext(ctx, tidbQuery, tidbArgs...)
		} else {
			tidbRows, err = tidbConn.QueryContext(ctx, tidbQuery)
		}
		if err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("sample target: %v", err)
			return tr
		}
		defer tidbRows.Close()

		tidbCols, _ := tidbRows.ColumnTypes()
		if tidbCols == nil {
			tr.Status = reporter.StatusFail
			tr.Error = "failed to get TiDB column types"
			return tr
		}
		tidbValues := make([]interface{}, len(tidbCols))
		tidbPtrs := make([]interface{}, len(tidbCols))
		for i := range tidbValues {
			tidbPtrs[i] = &tidbValues[i]
		}

		// Build sorted column list for hash-based row comparison.
		// Hash-based comparison is more robust than per-column comparison
		// because it reuses the same normalizeValue + trim pipeline that
		// the hash_group and aggregate modes already use and test against.
		pgColNames := make([]string, len(pgCols))
		pgColNameToIdx := make(map[string]int)
		for i, c := range pgCols {
			pgColNames[i] = c.Name()
			pgColNameToIdx[strings.ToLower(c.Name())] = i
		}
		sortedPGColNames := make([]string, len(pgColNames))
		copy(sortedPGColNames, pgColNames)
		sort.Strings(sortedPGColNames)

		tidbColNameToIdx := make(map[string]int)
		for i, c := range tidbCols {
			tidbColNameToIdx[strings.ToLower(c.Name())] = i
		}

		// Build unified skip set (column name lowercase -> skip).
		// Skip a column if it should be skipped on EITHER side.
		unifiedSkipCols := make(map[string]bool)
		for _, name := range sortedPGColNames {
			lowerName := strings.ToLower(name)
			pgIdx, pgOk := pgColNameToIdx[lowerName]
			if pgOk && skipCols[pgIdx] {
				unifiedSkipCols[lowerName] = true
				continue
			}
			tidbIdx, tidbOk := tidbColNameToIdx[lowerName]
			if tidbOk {
				dt := strings.ToLower(tidbCols[tidbIdx].DatabaseTypeName())
				if isApproximateFloatType(dt) || strings.Contains(dt, "json") {
					unifiedSkipCols[lowerName] = true
					continue
				}
			}
		}

		// Build unified trim set (column name lowercase -> trim).
		trimColNames := make(map[string]bool)
		for _, name := range sortedPGColNames {
			lowerName := strings.ToLower(name)
			pgIdx, pgOk := pgColNameToIdx[lowerName]
			if pgOk {
				dt := strings.ToLower(pgCols[pgIdx].DatabaseTypeName())
				if isTextType(dt) {
					trimColNames[lowerName] = true
				}
			}
			tidbIdx, tidbOk := tidbColNameToIdx[lowerName]
			if tidbOk {
				dt := strings.ToLower(tidbCols[tidbIdx].DatabaseTypeName())
				if isTextType(dt) {
					trimColNames[lowerName] = true
				}
			}
		}

		// Build PG hash column mapping (sorted by name, skipping unified skips).
		var pgHashCols []colMapping
		for _, name := range sortedPGColNames {
			lowerName := strings.ToLower(name)
			if unifiedSkipCols[lowerName] {
				continue
			}
			idx := pgColNameToIdx[lowerName]
			if _, tidbOk := tidbColNameToIdx[lowerName]; !tidbOk {
				continue
			}
			pgHashCols = append(pgHashCols, colMapping{pgIdx: idx, name: name})
		}

		// Build TiDB hash column mapping using same unified set.
		var tidbHashCols []tidbColMapping
		for _, name := range sortedPGColNames {
			lowerName := strings.ToLower(name)
			if unifiedSkipCols[lowerName] {
				continue
			}
			idx, ok := tidbColNameToIdx[lowerName]
			if !ok {
				continue
			}
			tidbHashCols = append(tidbHashCols, tidbColMapping{tidbIdx: idx, name: name})
		}

		// Compute row hashes for PG sample data (hash -> list of keys for
		// disambiguation when multiple rows share the same hash).
		type pgRowEntry struct {
			hash string
			key  string
		}
		pgHashMap := make(map[string][]pgRowEntry) // hash -> entries
		pgKeyToIdx := make(map[string]int)         // key -> pgData index for diagnostics
		for rowIdx, row := range pgData {
			if len(keyColIndices) == 0 {
				continue
			}
			h := computeRowHashTrimmed(row, pgHashCols, trimColNames)
			key := buildKey(row)
			pgKeyToIdx[key] = rowIdx
			pgHashMap[h] = append(pgHashMap[h], pgRowEntry{hash: h, key: key})
		}

		// Build TiDB key builder for error reporting.
		tidbKeyColIndices := make(map[string]int) // PG col name (lower) -> TiDB col index
		for _, pgIdx := range keyColIndices {
			colName := strings.ToLower(pgCols[pgIdx].Name())
			for i, c := range tidbCols {
				if strings.ToLower(c.Name()) == colName {
					tidbKeyColIndices[colName] = i
					break
				}
			}
		}
		buildTiDBKeyFromRow := func(row []string) string {
			var parts []string
			for _, pgIdx := range keyColIndices {
				colName := strings.ToLower(pgCols[pgIdx].Name())
				if ti, ok := tidbKeyColIndices[colName]; ok && ti < len(row) {
					parts = append(parts, row[ti])
				} else {
					parts = append(parts, "\\N")
				}
			}
			return strings.Join(parts, "|")
		}
		for tidbRows.Next() {
			if err := tidbRows.Scan(tidbPtrs...); err != nil {
				continue
			}
			tidbRow := make([]string, len(tidbValues))
			for i, val := range tidbValues {
				tidbRow[i] = normalizeValue(val)
			}
			if len(tidbRow) == 0 {
				continue
			}

			h := computeTiDBRowHashTrimmed(tidbRow, tidbHashCols, trimColNames)
			entries, found := pgHashMap[h]
			if !found || len(entries) == 0 {
				key := buildTiDBKeyFromRow(tidbRow)
				mismatchCount++
				diag := ""
				if mismatchCount <= 3 {
					if pgIdx, ok := pgKeyToIdx[key]; ok {
						diag = diagnoseRowDiff(v.srcLabel(), pgData[pgIdx], tidbRow, pgHashCols, tidbHashCols, trimColNames, pgCols, tidbCols)
					}
				}
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("hash=%s not found in %s (key=%s)%s", truncate(h, 16), v.srcLabel(), truncate(key, 40), diag))
				continue
			}
			// Remove first matching entry
			if len(entries) > 1 {
				pgHashMap[h] = entries[1:]
			} else {
				delete(pgHashMap, h)
			}
		}

		// Any remaining PG entries were not matched by TiDB.
		for _, entries := range pgHashMap {
			for _, e := range entries {
				mismatchCount++
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("hash=%s in %s but not found in TiDB (key=%s)", truncate(e.hash, 16), v.srcLabel(), truncate(e.key, 40)))
			}
		}

	} else {
		// Fallback for NULL first column: use positional comparison
		// (less reliable but necessary when key column is NULL)
		var tidbRows *sql.Rows
		if wm != nil {
			tidbQuery := fmt.Sprintf("SELECT * FROM %s WHERE %s LIMIT %d OFFSET %d",
				v.tgtDialect.QuoteIdent(table), v.tgtDialect.WmPredicateFragment(wm), sampleSize, offset)
			tidbRows, err = tidbConn.QueryContext(ctx, tidbQuery, wm.Value)
		} else {
			tidbQuery := fmt.Sprintf("SELECT * FROM %s LIMIT %d OFFSET %d",
				v.tgtDialect.QuoteIdent(table), sampleSize, offset)
			tidbRows, err = tidbConn.QueryContext(ctx, tidbQuery)
		}
		if err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("sample target: %v", err)
			return tr
		}
		defer tidbRows.Close()

		tidbCols, _ := tidbRows.ColumnTypes()
		tidbValues := make([]interface{}, len(tidbCols))
		tidbPtrs := make([]interface{}, len(tidbCols))
		for i := range tidbValues {
			tidbPtrs[i] = &tidbValues[i]
		}
		rowIdx := 0
		for tidbRows.Next() {
			if err := tidbRows.Scan(tidbPtrs...); err != nil {
				continue
			}
			if rowIdx < len(pgData) {
				for colIdx, val := range tidbValues {
					if skipCols[colIdx] {
						continue
					}
					pgVal := ""
					if colIdx < len(pgData[rowIdx]) {
						pgVal = pgData[rowIdx][colIdx]
					}
					tidbVal := normalizeValue(val)
					if trimCols[colIdx] {
						pgVal = trimTrailingWhitespace(pgVal)
						tidbVal = trimTrailingWhitespace(tidbVal)
					}
					if pgVal != tidbVal {
						mismatchCount++
						colName := tidbCols[colIdx].Name()
						mismatchDetails = append(mismatchDetails, fmt.Sprintf("row %d col %q: %s=%q TiDB=%q", rowIdx+int(offset)+1, colName, v.srcLabel(), truncate(pgVal, 80), truncate(tidbVal, 80)))
						break
					}
				}
			}
			rowIdx++
		}
	}

	if mismatchCount > 0 {
		tr.Status = reporter.StatusFail
		maxShow := 10
		if len(mismatchDetails) > maxShow {
			mismatchDetails = mismatchDetails[:maxShow]
		}
		detailStr := strings.Join(mismatchDetails, "; ")
		tr.Error = fmt.Sprintf("%d/%d rows mismatch in sampling (%s)", mismatchCount, len(pgData), detailStr)
	} else {
		tr.Status = reporter.StatusPass
	}
	tr.Suggestion = fmt.Sprintf("sampled %d rows (%.1f%%), %d mismatches", len(pgData), ratio*100, mismatchCount)
	return tr

}

// validateSamplingWithHashGroup handles no-PK table validation using hash group
// comparison. It queries ALL rows from PG (hash_group is an exact strategy,
// not a sampled one), then uses validateHashGroup to compare the multiset of
// row hashes against TiDB's full table.
func (v *Validator) validateSamplingWithHashGroup(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn, table string, ratio float64, tr reporter.TableReport, schema string) reporter.TableReport {
	logger := zap.L()

	// Hash group is an exact strategy 鈥?query the full PG table, not a sample.
	// Sampling would cause mismatches because TiDB is also queried in full.
	var pgRows *sql.Rows
	var err error
	if wm := v.wmFilter(); wm != nil {
		pgRows, err = pgDB.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s.%s WHERE %s",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), v.srcDialect.WmPredicateFragment(wm)), wm.Value)
	} else {
		pgRows, err = pgDB.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s.%s",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table)))
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("sample source (no-PK): %v", err)
		return tr
	}
	defer pgRows.Close()

	pgCols, _ := pgRows.ColumnTypes()
	if pgCols == nil {
		tr.Status = reporter.StatusFail
		tr.Error = "failed to get source column types"
		return tr
	}

	// Build skip/trim column sets (same logic as validateSampling)
	skipCols := make(map[int]bool)
	for i, c := range pgCols {
		dt := strings.ToLower(c.DatabaseTypeName())
		if isApproximateFloatType(dt) ||
			strings.Contains(dt, "json") {
			skipCols[i] = true
		}
	}

	pgValues := make([]interface{}, len(pgCols))
	pgPtrs := make([]interface{}, len(pgCols))
	for i := range pgValues {
		pgPtrs[i] = &pgValues[i]
	}

	var pgData [][]string
	for pgRows.Next() {
		if err := pgRows.Scan(pgPtrs...); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("scan %s row: %v", v.srcLabel(), err)
			return tr
		}
		row := make([]string, len(pgCols))
		for i, val := range pgValues {
			row[i] = normalizeValue(val)
		}
		pgData = append(pgData, row)
	}

	logger.Info("no-PK table: querying full PG table for hash group comparison",
		zap.String("table", table),
		zap.Int("row_count", len(pgData)))

	return v.validateHashGroup(ctx, pgDB, tidbConn, table, tr, pgCols, pgData, skipCols)
}

// validateNoPKWithAggregate wraps full-table PG query + aggregate hash validation.
func (v *Validator) validateNoPKWithAggregate(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn, table string, tr reporter.TableReport, schema string) reporter.TableReport {
	var pgRows *sql.Rows
	var err error
	if wm := v.wmFilter(); wm != nil {
		pgRows, err = pgDB.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s.%s WHERE %s",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), v.srcDialect.WmPredicateFragment(wm)), wm.Value)
	} else {
		pgRows, err = pgDB.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s.%s",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table)))
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash: query %s: %v", v.srcLabel(), err)
		return tr
	}
	defer pgRows.Close()

	pgCols, _ := pgRows.ColumnTypes()
	if pgCols == nil {
		tr.Status = reporter.StatusFail
		tr.Error = "aggregate hash: failed to get source column types"
		return tr
	}

	skipCols := make(map[int]bool)
	for i, c := range pgCols {
		dt := strings.ToLower(c.DatabaseTypeName())
		if isApproximateFloatType(dt) || strings.Contains(dt, "json") {
			skipCols[i] = true
		}
	}

	pgValues := make([]interface{}, len(pgCols))
	pgPtrs := make([]interface{}, len(pgCols))
	for i := range pgValues {
		pgPtrs[i] = &pgValues[i]
	}

	var pgData [][]string
	for pgRows.Next() {
		if err := pgRows.Scan(pgPtrs...); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("aggregate hash: scan %s row: %v", v.srcLabel(), err)
			return tr
		}
		row := make([]string, len(pgCols))
		for i, val := range pgValues {
			row[i] = normalizeValue(val)
		}
		pgData = append(pgData, row)
	}

	return v.validateAggregateHash(ctx, pgDB, tidbConn, table, tr, pgCols, pgData, skipCols)
}

// validateNoPKWithBucket wraps full-table PG query + bucket validation.
func (v *Validator) validateNoPKWithBucket(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn, table string, tr reporter.TableReport, schema string) reporter.TableReport {
	var pgRows *sql.Rows
	var err error
	if wm := v.wmFilter(); wm != nil {
		pgRows, err = pgDB.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s.%s WHERE %s",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), v.srcDialect.WmPredicateFragment(wm)), wm.Value)
	} else {
		pgRows, err = pgDB.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s.%s",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table)))
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("bucket compare: query %s: %v", v.srcLabel(), err)
		return tr
	}
	defer pgRows.Close()

	pgCols, _ := pgRows.ColumnTypes()
	if pgCols == nil {
		tr.Status = reporter.StatusFail
		tr.Error = "bucket compare: failed to get source column types"
		return tr
	}

	skipCols := make(map[int]bool)
	for i, c := range pgCols {
		dt := strings.ToLower(c.DatabaseTypeName())
		if isApproximateFloatType(dt) || strings.Contains(dt, "json") {
			skipCols[i] = true
		}
	}

	pgValues := make([]interface{}, len(pgCols))
	pgPtrs := make([]interface{}, len(pgCols))
	for i := range pgValues {
		pgPtrs[i] = &pgValues[i]
	}

	var pgData [][]string
	for pgRows.Next() {
		if err := pgRows.Scan(pgPtrs...); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("bucket compare: scan %s row: %v", v.srcLabel(), err)
			return tr
		}
		row := make([]string, len(pgCols))
		for i, val := range pgValues {
			row[i] = normalizeValue(val)
		}
		pgData = append(pgData, row)
	}

	return v.validateBucketCompare(ctx, pgDB, tidbConn, table, tr, pgCols, pgData, skipCols)
}

func (v *Validator) getTables(ctx context.Context, pgDB *sql.DB, include []string) ([]string, error) {
	if len(include) > 0 {
		return include, nil
	}

	return v.srcDialect.ListTables(ctx, pgDB, schemaOrDefault(v.sourceSchema()))
}

// MS-03: quotePG/quoteMySQL relocated verbatim into the dialect implementations QuoteIdent methods.

func normalizeValue(val interface{}) string {
	if val == nil {
		return "\\N"
	}
	switch v := val.(type) {
	case bool:
		if v {
			return "1"
		}
		return "0"
	case float64:
		// Use strconv.FormatFloat with 'f' and -1 precision to preserve
		// full float64 precision (e.g., 123.4567 not "123").
		return normalizeString(strconv.FormatFloat(v, 'f', -1, 64))
	case float32:
		return normalizeString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	case int64:
		return normalizeString(strconv.FormatInt(v, 10))
	case uint64:
		// MS-08 MySQL source seam (dialect.go:22): UNSIGNED BIGINT may scan
		// as uint64 on the MySQL driver (text protocol yields []byte, the
		// binary/interpolated paths can yield uint64). PG never returns
		// uint64, so this case is unreachable-and-inert for PG sources -
		// extending the single shared implementation, not a per-dialect fork.
		return normalizeString(strconv.FormatUint(v, 10))
	case int:
		return normalizeString(strconv.Itoa(v))
	case []byte:
		return normalizeString(string(v))
	case time.Time:
		return v.UTC().Format("2006-01-02 15:04:05")
	case string:
		return normalizeString(v)
	case fmt.Stringer:
		return normalizeString(v.String())
	default:
		return normalizeString(fmt.Sprintf("%v", v))
	}
}

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
var pgArrayRe = regexp.MustCompile(`^\{.*\}$`)

// decimalRe matches numeric strings like "10.50", "-3.1400", "0.00"
var decimalRe = regexp.MustCompile(`^-?[0-9]+[.][0-9]+$`)

// normalizeDecimalString strips trailing zeros from decimal-looking strings.
// "10.50" -> "10.5", "10.00" -> "10", "10" -> "10" (unchanged, no decimal point).
func normalizeDecimalString(s string) string {
	if !decimalRe.MatchString(s) {
		return s
	}
	// Strip trailing zeros
	s = strings.TrimRight(s, "0")
	// Strip trailing decimal point if no fractional digits remain
	s = strings.TrimRight(s, ".")
	return s
}

// timestampRe matches common timestamp formats: "2006-01-02 15:04:05" with optional
// fractional seconds ".123456" and optional timezone "Z" or "+08:00".
var timestampRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})(\.\d+)?(.*)$`)

// normalizeTimestampString strips fractional seconds and normalizes timezone from
// timestamp-looking strings, so both PG and TiDB produce the same value.
func normalizeTimestampString(s string) string {
	m := timestampRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	// Return just the base timestamp without fractional seconds or timezone suffix
	return m[1]
}

func normalizeString(s string) string {
	// Normalize line endings: \r\n 鈫?\n, then standalone \r 鈫?\n.
	// MySQL/TiDB may strip or normalize carriage returns differently than PG.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	// Normalize decimal numbers: strip trailing zeros so "10.50" and "10.5"
	// compare equal. This handles PG (string "10.50") vs TiDB (float64->"10.5").
	s = normalizeDecimalString(s)

	// Normalize timestamps: strip fractional seconds and timezone suffix
	// so "2024-01-01 12:30:00.000000" and "2024-01-01 12:30:00" compare equal.
	s = normalizeTimestampString(s)

	// Normalize UUID to lowercase
	s = uuidRe.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ToLower(m)
	})
	// Normalize PG array format {1,2,3} -> [1,2,3] (must be before JSON check)
	if pgArrayRe.MatchString(s) {
		return normalizePGArray(s)
	}
	// Normalize JSON whitespace
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		return normalizeJSON(s)
	}
	return s
}

func normalizePGArray(s string) string {
	// Convert PG array format {elem1,elem2,...} to JSON array ["elem1","elem2",...]
	// then normalize JSON whitespace for consistent comparison with TiDB JSON values.
	return normalizeJSON(pgArrayToJSON(s))
}

func pgArrayToJSON(s string) string {
	inner := s[1 : len(s)-1] // strip outer { }
	if inner == "" {
		return "[]"
	}
	elements := splitPGArrayElements(inner)
	parts := make([]string, 0, len(elements))
	for _, elem := range elements {
		elem = strings.TrimSpace(elem)
		if elem == "" || elem == "NULL" || elem == "null" {
			parts = append(parts, "null")
		} else if elem == "t" {
			parts = append(parts, "true")
		} else if elem == "f" {
			parts = append(parts, "false")
		} else if len(elem) >= 2 && elem[0] == '"' && elem[len(elem)-1] == '"' {
			// Already quoted in PG syntax 鈥?unescape PG "" 鈫?JSON \"
			unquoted := elem[1 : len(elem)-1]
			unquoted = strings.ReplaceAll(unquoted, `""`, `"`)
			b, _ := json.Marshal(unquoted)
			parts = append(parts, string(b))
		} else if len(elem) >= 2 && elem[0] == '{' && elem[len(elem)-1] == '}' {
			parts = append(parts, pgArrayToJSON(elem))
		} else {
			// Try number; otherwise treat as string and JSON-quote it
			if _, err := strconv.ParseFloat(elem, 64); err == nil {
				parts = append(parts, elem)
			} else {
				b, _ := json.Marshal(elem)
				parts = append(parts, string(b))
			}
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func splitPGArrayElements(s string) []string {
	var elements []string
	current := ""
	inQuote := false
	escape := false
	depth := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escape {
			current += string(ch)
			escape = false
			continue
		}
		if ch == '\\' {
			escape = true
			current += string(ch)
			continue
		}
		if ch == '"' {
			inQuote = !inQuote
			current += string(ch)
			continue
		}
		if ch == '{' && !inQuote {
			depth++
			current += string(ch)
		} else if ch == '}' && !inQuote {
			depth--
			current += string(ch)
		} else if ch == ',' && !inQuote && depth == 0 {
			elements = append(elements, current)
			current = ""
		} else {
			current += string(ch)
		}
	}
	if current != "" || len(elements) > 0 {
		elements = append(elements, current)
	}
	return elements
}

func normalizeJSON(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	inString := false
	escaped := false
	for _, r := range s {
		if escaped {
			buf.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && inString {
			buf.WriteRune(r)
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			buf.WriteRune(r)
			continue
		}
		if inString {
			buf.WriteRune(r)
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		buf.WriteRune(r)
	}
	return buf.String()
}

// trimTrailingWhitespace removes trailing whitespace characters (space, tab,
// newline, carriage return) from a string. This is used for text-type column
// comparison because MySQL/TiDB may strip trailing whitespace differently than
// PostgreSQL (e.g., PG preserves \r\n while TiDB strips it).
func trimTrailingWhitespace(s string) string {
	return strings.TrimRight(s, " \t\n\r")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// diagnoseRowDiff compares a source row and TiDB row column-by-column and
// returns a diagnostic string listing the first few column differences with
// precise diff location. srcLabel is the user-facing source label ("PG" or
// "MySQL") so the type tags match the actual source kind.
func diagnoseRowDiff(
	srcLabel string,
	pgRow []string, tidbRow []string,
	pgHashCols []colMapping, tidbHashCols []tidbColMapping,
	trimColNames map[string]bool,
	pgCols []*sql.ColumnType, tidbCols []*sql.ColumnType,
) string {
	var diffs []string
	maxDiffs := 3
	for i := 0; i < len(pgHashCols) && i < len(tidbHashCols) && len(diffs) < maxDiffs; i++ {
		pgHC := pgHashCols[i]
		tidbHC := tidbHashCols[i]

		pgVal := "\\N"
		if pgHC.pgIdx < len(pgRow) {
			pgVal = pgRow[pgHC.pgIdx]
		}
		tidbVal := "\\N"
		if tidbHC.tidbIdx < len(tidbRow) {
			tidbVal = tidbRow[tidbHC.tidbIdx]
		}

		// Apply trim if this is a text column (same as hash computation)
		if trimColNames[strings.ToLower(pgHC.name)] {
			pgVal = trimTrailingWhitespace(pgVal)
			tidbVal = trimTrailingWhitespace(tidbVal)
		}

		if pgVal != tidbVal {
			pgType := "?"
			if pgHC.pgIdx < len(pgCols) {
				pgType = pgCols[pgHC.pgIdx].DatabaseTypeName()
			}
			tidbType := "?"
			if tidbHC.tidbIdx < len(tidbCols) {
				tidbType = tidbCols[tidbHC.tidbIdx].DatabaseTypeName()
			}

			// Find first byte difference position
			diffPos := 0
			minLen := len(pgVal)
			if len(tidbVal) < minLen {
				minLen = len(tidbVal)
			}
			for diffPos = 0; diffPos < minLen; diffPos++ {
				if pgVal[diffPos] != tidbVal[diffPos] {
					break
				}
			}

			// Show hex of bytes starting from diff position
			pgHex := fmt.Sprintf("%x", []byte(pgVal[diffPos:]))
			if len(pgHex) > 80 {
				pgHex = pgHex[:80] + "..."
			}
			tidbHex := fmt.Sprintf("%x", []byte(tidbVal[diffPos:]))
			if len(tidbHex) > 80 {
				tidbHex = tidbHex[:80] + "..."
			}

			diffs = append(diffs, fmt.Sprintf("%s %s(%s)[len=%d] TiDB(%s)[len=%d] diff@byte%d src_hex_after=%s tidb_hex_after=%s",
				pgHC.name, srcLabel, pgType, len(pgVal), tidbType, len(tidbVal), diffPos,
				pgHex, tidbHex))
		}
	}
	if len(diffs) == 0 {
		return " [hash-diff-but-all-cols-match?]"
	}
	return " diff=[" + strings.Join(diffs, "; ") + "]"
}
