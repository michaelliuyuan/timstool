package validator

import (
	"context"
	"crypto/md5"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"go.uber.org/zap"
)

// validateChecksumChunked computes row hashes for each chunk of the table
// in parallel, then compares PG and TiDB chunk by chunk.
//
// #t4: connection orchestration is single-layer. The caller holds NO budget
// slot; this function acquires one for the count phase and one per chunk via
// the shared global sem, so no goroutine ever waits on the sem while holding
// a connection (the old table-level × chunk-level nesting deadlocked when
// outer workers held all 8 pool conns waiting for inner chunks).
func (v *Validator) validateChecksumChunked(ctx context.Context, pgDB, tidbDB *sql.DB, table string, sem concSem) reporter.TableReport {
	tr := reporter.TableReport{TableName: table, Status: reporter.StatusPass}

	// Unit 1: exact row count + chunk-key detection, inside one slot.
	release, aerr := sem.acquire(ctx)
	if aerr != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("checksum: cancelled before start: %v", aerr)
		return tr
	}
	// Get a dedicated TiDB connection with UTC timezone for row count.
	tidbConn, connErr := v.getTiDBConn(ctx, tidbDB)
	if connErr != nil {
		release()
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("checksum: get TiDB connection: %v", connErr)
		return tr
	}

	// First do exact row count
	tr = v.validateRowCount(ctx, pgDB, tidbConn, table)
	// Any row-count failure (COUNT error or mismatch) aborts the table:
	// falling through would hit the SourceRows==0 branch below and report a
	// count ERROR as PASS (F-05).
	if tr.Status == reporter.StatusFail {
		tidbConn.Close()
		release()
		// Row-diff drill (count-mismatch face): a COUNT *error* leaves both
		// counts at zero (operational failure — nothing to align); a count
		// *mismatch* has at least one non-zero count and is exactly the
		// src_only/dst_only case the drill classifies.
		if tr.SourceRows+tr.TargetRows > 0 {
			schema := v.sourceSchema()
			if schema == "" {
				schema = "public"
			}
			v.maybeDrillRowDiffs(ctx, pgDB, tidbDB, sem, schema, table, &tr, nil, nil, "")
		}
		return tr
	}

	if tr.SourceRows == 0 {
		tr.Status = reporter.StatusPass
		tidbConn.Close()
		release()
		return tr
	}

	schema := v.sourceSchema()
	if schema == "" {
		schema = "public"
	}

	// Detect key columns for chunking
	keyInfo, _ := v.srcDialect.DetectTableKey(ctx, pgDB, schema, table)

	// #ignore: resolve the user exclusion set once (PK collisions softened
	// away) and thread the effective set into both chunk hashers; the
	// protected collisions are disclosed on the table report.
	var igProtected []string
	igEff := map[string]bool(nil)
	if ignoreConfigured(v.compareCfg()) {
		igEff, igProtected = v.resolveIgnoreSet(keyInfo)
		if len(igProtected) > 0 {
			zap.L().Info("ignore columns hit primary key, kept comparable",
				zap.String("table", table), zap.Strings("columns", igProtected))
		}
	}

	var orderByCols string
	if keyInfo != nil && keyInfo.HasPK {
		orderByCols = strings.Join(keyInfo.PKColumns, ", ")
	} else if keyInfo != nil && keyInfo.HasUniqueIndex {
		orderByCols = strings.Join(keyInfo.UniqueColumns, ", ")
	} else {
		// No key — fall back to hash_group comparison (already implemented),
		// still inside the same slot (1 unit: the dedicated conn is held).
		logger := zap.L()
		logger.Info("checksum mode: no PK/unique for chunking, falling back to hash_group", zap.String("table", table))
		out := v.validateSamplingWithHashGroup(ctx, pgDB, tidbConn, table, 1.0, tr, schema)
		tidbConn.Close()
		release()
		// Row-diff drill scope disclosure: a FAILED no-PK table cannot be
		// aligned row-by-row (hash_group compares multisets) — an explicit
		// note instead of a silent summary-free failure.
		if out.Status == reporter.StatusFail && v.diffDetailDir != "" {
			out.RowDiffs = &reporter.RowDiffSummary{
				Note: "该表无主键/唯一索引（hash_group 路径），无法行级定位差异",
			}
		}
		return out
	}
	tidbConn.Close()
	release()

	chunkSize := v.compareCfg().ChecksumChunkSize
	if chunkSize <= 0 {
		chunkSize = 50000
	}

	totalRows := tr.SourceRows
	numChunks := int(totalRows / chunkSize)
	if totalRows%chunkSize > 0 {
		numChunks++
	}
	if numChunks == 0 {
		numChunks = 1
	}

	logger := zap.L()
	logger.Info("checksum mode: chunked parallel hash",
		zap.String("table", table),
		zap.Int64("total_rows", totalRows),
		zap.Int("chunks", numChunks),
		zap.Int64("chunk_size", chunkSize))

	// Build chunk boundaries using the key column
	chunks := make([]chunkRange, numChunks)
	for i := 0; i < numChunks; i++ {
		chunks[i] = chunkRange{
			offset: int64(i) * chunkSize,
			limit:  chunkSize,
		}
		// Adjust last chunk
		if i == numChunks-1 {
			chunks[i].limit = totalRows - chunks[i].offset
		}
	}

	// Process chunks in parallel — each chunk is ONE global work unit: both
	// the PG pool-backed query and the dedicated TiDB conn happen inside the
	// slot (#t4).
	var mu sync.Mutex
	var mismatchDetails []string
	var hashMismatch bool
	var noComparable bool
	var wg sync.WaitGroup

	for i, chunk := range chunks {
		wg.Add(1)
		go func(idx int, ch chunkRange) {
			defer wg.Done()

			release, aerr := sem.acquire(ctx)
			if aerr != nil {
				mu.Lock()
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d: cancelled: %v", idx, aerr))
				mu.Unlock()
				return
			}
			defer release()

			pgHash, err := v.computeChunkHashPG(ctx, pgDB, schema, table, orderByCols, ch, igEff)
			if err != nil {
				if errors.Is(err, ErrNoComparableCols) {
					mu.Lock()
					noComparable = true
					mu.Unlock()
					return
				}
				mu.Lock()
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d: %s error: %v", idx, v.srcLabel(), err))
				mu.Unlock()
				return
			}

			tidbHash, err := v.computeChunkHashTiDB(ctx, tidbDB, table, orderByCols, ch, igEff)
			if err != nil {
				if errors.Is(err, ErrNoComparableCols) {
					mu.Lock()
					noComparable = true
					mu.Unlock()
					return
				}
				mu.Lock()
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d: TiDB error: %v", idx, err))
				mu.Unlock()
				return
			}

			if pgHash != tidbHash {
				mu.Lock()
				hashMismatch = true
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d (rows %d-%d): hash mismatch src=%s tidb=%s",
					idx, ch.offset, ch.offset+ch.limit, truncate(pgHash, 12), truncate(tidbHash, 12)))
				mu.Unlock()
			}
		}(i, chunk)
	}
	wg.Wait()

	// #ignore: every comparable column excluded ⇒ no evidence ⇒ runtime
	// skip with a warning note (never a FAIL).
	if noComparable {
		tr.Status = reporter.StatusSkip
		tr.Error = "无可比列（排除字段后无剩余可比列）"
		tr = prependPKProtectedNote(tr, igProtected)
		return tr
	}

	if len(mismatchDetails) > 0 {
		tr.Status = reporter.StatusFail
		maxShow := 10
		if len(mismatchDetails) > maxShow {
			mismatchDetails = mismatchDetails[:maxShow]
		}
		tr.Error = fmt.Sprintf("checksum mismatch in %d/%d chunks: %s",
			len(mismatchDetails), numChunks, strings.Join(mismatchDetails, "; "))
	} else {
		tr.Status = reporter.StatusPass
	}

	tr.Suggestion = fmt.Sprintf("checksum mode: %d chunks × %d rows, %d mismatches",
		numChunks, chunkSize, len(mismatchDetails))
	if len(igProtected) > 0 {
		tr.Suggestion = "主键列不可排除: " + strings.Join(igProtected, ", ") + "；" + tr.Suggestion
	}

	// Row-diff drill (chunk-mismatch face): only a genuine hash mismatch
	// has data divergence to locate — operational chunk errors (query
	// failures/cancellation) carry their own triage in tr.Error and would
	// only re-fail inside the drill. No slot is held here (all chunk units
	// released above); the drill takes its own per-window units (#t4).
	if tr.Status == reporter.StatusFail && hashMismatch {
		v.maybeDrillRowDiffs(ctx, pgDB, tidbDB, sem, schema, table, &tr, keyInfo, igEff, orderByCols)
	}

	return tr
}

type chunkRange struct {
	offset int64
	limit  int64
}

// resolveIgnoreSet resolves the effective user-exclusion set for one table
// (PK collisions softened away): the effective set feeds the chunk hashers
// and the row-diff drill (same inputs, same skip channel), and the protected
// collisions return for report disclosure. Extracted so the count-mismatch
// drill site resolves the identical set without duplicating the logic.
func (v *Validator) resolveIgnoreSet(keyInfo *TableKeyInfo) (map[string]bool, []string) {
	var pkCols []string
	if keyInfo != nil && keyInfo.HasPK {
		pkCols = keyInfo.PKColumns
	}
	igApplied, igProtected := tableIgnoreColumns(v.compareCfg(), pkCols)
	return ignoreSetMap(igApplied), igProtected
}

// quoteOrderByCols quotes a comma-separated key column list per column.
// Wrapping the whole "col1, col2" string in one pair of quotes produces a
// single bogus identifier and breaks SQL for every composite-key table (F-05).
func quoteOrderByCols(orderBy string, quote func(string) string) string {
	parts := strings.Split(orderBy, ",")
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			quoted = append(quoted, quote(p))
		}
	}
	return strings.Join(quoted, ", ")
}

// computeChunkHashPG computes an aggregate hash for a chunk of PG rows.
// With a watermark filter active the chunk rows are the FILTERED universe
// (validateRowCount already counted with the same predicate, so chunk
// boundaries derived from that count stay consistent on both sides). ig is
// the PK-resolved user ignore set (nil = none): hits join the float/JSON
// skip channel — value hashing only, chunk ordering is untouched.
func (v *Validator) computeChunkHashPG(ctx context.Context, pgDB *sql.DB, schema, table, orderBy string, ch chunkRange, ig map[string]bool) (string, error) {
	var rows *sql.Rows
	var err error
	if wm := v.wmFilterFor(ctx); wm != nil {
		query := fmt.Sprintf("SELECT * FROM %s.%s WHERE %s ORDER BY %s LIMIT %d OFFSET %d",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), v.srcDialect.WmPredicateFragment(wm), quoteOrderByCols(orderBy, v.srcDialect.QuoteIdent), ch.limit, ch.offset)
		rows, err = pgDB.QueryContext(ctx, query, wmArgs(wm)...)
	} else {
		query := fmt.Sprintf("SELECT * FROM %s.%s ORDER BY %s LIMIT %d OFFSET %d",
			v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), quoteOrderByCols(orderBy, v.srcDialect.QuoteIdent), ch.limit, ch.offset)
		rows, err = pgDB.QueryContext(ctx, query)
	}
	if err != nil {
		return "", err
	}
	defer rows.Close()

	cols, _ := rows.ColumnTypes()
	if cols == nil {
		return "", fmt.Errorf("no column types")
	}

	// Shared hash plan (row-diff drill uses the SAME builder — one
	// implementation, no drift).
	hashIdxs, err := buildChunkHashPlan(cols, ig)
	if err != nil {
		return "", err
	}

	values := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}

	var rowHashes []string
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("scan row in chunk: %w", err)
		}
		rowHashes = append(rowHashes, rowMD5(values, hashIdxs))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate chunk rows: %w", err)
	}

	sort.Strings(rowHashes)
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(rowHashes, ",")))), nil
}

// computeChunkHashTiDB computes an aggregate hash for a chunk of TiDB rows.
// It gets its own dedicated connection with UTC timezone for parallel goroutines.
// ig mirrors the PG-side ignore set so both sides hash the same columns.
func (v *Validator) computeChunkHashTiDB(ctx context.Context, tidbDB *sql.DB, table, orderBy string, ch chunkRange, ig map[string]bool) (string, error) {
	// Get dedicated connection with UTC timezone for this goroutine.
	conn, err := v.getTiDBConn(ctx, tidbDB)
	if err != nil {
		return "", fmt.Errorf("get TiDB conn for chunk: %w", err)
	}
	defer conn.Close()

	var rows *sql.Rows
	if wm := v.wmFilterFor(ctx); wm != nil {
		query := fmt.Sprintf("SELECT * FROM %s WHERE %s ORDER BY %s LIMIT %d OFFSET %d",
			v.tgtDialect.QuoteIdent(table), v.tgtDialect.WmPredicateFragment(wm), quoteOrderByCols(orderBy, v.tgtDialect.QuoteIdent), ch.limit, ch.offset)
		rows, err = conn.QueryContext(ctx, query, wmArgs(wm)...)
	} else {
		query := fmt.Sprintf("SELECT * FROM %s ORDER BY %s LIMIT %d OFFSET %d",
			v.tgtDialect.QuoteIdent(table), quoteOrderByCols(orderBy, v.tgtDialect.QuoteIdent), ch.limit, ch.offset)
		rows, err = conn.QueryContext(ctx, query)
	}
	if err != nil {
		return "", err
	}
	defer rows.Close()

	cols, _ := rows.ColumnTypes()
	if cols == nil {
		return "", fmt.Errorf("no column types")
	}

	// Shared hash plan (row-diff drill uses the SAME builder).
	hashIdxs, err := buildChunkHashPlan(cols, ig)
	if err != nil {
		return "", err
	}

	values := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}

	var rowHashes []string
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("scan row in chunk: %w", err)
		}
		rowHashes = append(rowHashes, rowMD5(values, hashIdxs))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate chunk rows: %w", err)
	}

	sort.Strings(rowHashes)
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(rowHashes, ",")))), nil
}

// buildChunkHashPlan is the single shared hash-column plan of the checksum
// chunk hashers AND the row-diff drill: skip approximate floats / JSON /
// user-ignored columns, hash the remaining columns in name order. Returning
// ErrNoComparableCols keeps the historic no-comparable-columns semantics.
func buildChunkHashPlan(cols []*sql.ColumnType, ig map[string]bool) ([]int, error) {
	skipCols := make(map[int]bool)
	colNames := make([]string, len(cols))
	colIdxMap := make(map[string]int)
	for i, c := range cols {
		lower := strings.ToLower(c.Name())
		colNames[i] = c.Name()
		colIdxMap[lower] = i
		dt := strings.ToLower(c.DatabaseTypeName())
		if isApproximateFloatType(dt) || strings.Contains(dt, "json") || ig[lower] {
			skipCols[i] = true
		}
	}
	sortedCols := make([]string, len(colNames))
	copy(sortedCols, colNames)
	sort.Strings(sortedCols)
	var hashIdxs []int
	for _, name := range sortedCols {
		idx := colIdxMap[strings.ToLower(name)]
		if !skipCols[idx] {
			hashIdxs = append(hashIdxs, idx)
		}
	}
	if len(hashIdxs) == 0 {
		return nil, ErrNoComparableCols
	}
	return hashIdxs, nil
}

// rowMD5 hashes one row over the shared plan: normalized values joined with
// '|' in the plan's (name-sorted) order — the exact per-row hash the chunk
// aggregators fold, extracted so the row-diff drill hashes identically.
func rowMD5(values []interface{}, hashIdxs []int) string {
	var buf strings.Builder
	for i, idx := range hashIdxs {
		if i > 0 {
			buf.WriteByte('|')
		}
		buf.WriteString(normalizeValue(values[idx]))
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(buf.String())))
}
