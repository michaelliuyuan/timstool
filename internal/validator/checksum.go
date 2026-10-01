package validator

import (
	"context"
	"crypto/md5"
	"database/sql"
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
	tidbConn, connErr := getTiDBConn(ctx, tidbDB)
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
	keyInfo, _ := v.detectTableKey(ctx, pgDB, schema, table)
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

			pgHash, err := v.computeChunkHashPG(ctx, pgDB, schema, table, orderByCols, ch)
			if err != nil {
				mu.Lock()
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d: PG error: %v", idx, err))
				mu.Unlock()
				return
			}

			tidbHash, err := v.computeChunkHashTiDB(ctx, tidbDB, table, orderByCols, ch)
			if err != nil {
				mu.Lock()
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d: TiDB error: %v", idx, err))
				mu.Unlock()
				return
			}

			if pgHash != tidbHash {
				mu.Lock()
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d (rows %d-%d): hash mismatch pg=%s tidb=%s",
					idx, ch.offset, ch.offset+ch.limit, truncate(pgHash, 12), truncate(tidbHash, 12)))
				mu.Unlock()
			}
		}(i, chunk)
	}
	wg.Wait()

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

	tr.Suggestion = fmt.Sprintf("checksum mode: %d chunks 脳 %d rows, %d mismatches",
		numChunks, chunkSize, len(mismatchDetails))

	return tr
}

type chunkRange struct {
	offset int64
	limit  int64
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
// boundaries derived from that count stay consistent on both sides).
func (v *Validator) computeChunkHashPG(ctx context.Context, pgDB *sql.DB, schema, table, orderBy string, ch chunkRange) (string, error) {
	var rows *sql.Rows
	var err error
	if wm := v.wmFilter(); wm != nil {
		query := fmt.Sprintf("SELECT * FROM %s.%s WHERE %s ORDER BY %s LIMIT %d OFFSET %d",
			quotePG(schema), quotePG(table), wmWherePG(wm), quoteOrderByCols(orderBy, quotePG), ch.limit, ch.offset)
		rows, err = pgDB.QueryContext(ctx, query, wm.Value)
	} else {
		query := fmt.Sprintf("SELECT * FROM %s.%s ORDER BY %s LIMIT %d OFFSET %d",
			quotePG(schema), quotePG(table), quoteOrderByCols(orderBy, quotePG), ch.limit, ch.offset)
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

	// Build skip cols and sorted column list
	skipCols := make(map[int]bool)
	colNames := make([]string, len(cols))
	colIdxMap := make(map[string]int)
	for i, c := range cols {
		colNames[i] = c.Name()
		colIdxMap[strings.ToLower(c.Name())] = i
		dt := strings.ToLower(c.DatabaseTypeName())
		if isApproximateFloatType(dt) || strings.Contains(dt, "json") {
			skipCols[i] = true
		}
	}

	sortedCols := make([]string, len(colNames))
	copy(sortedCols, colNames)
	sort.Strings(sortedCols)

	// Build ordered non-skipped column indices
	var hashIdxs []int
	for _, name := range sortedCols {
		idx := colIdxMap[strings.ToLower(name)]
		if !skipCols[idx] {
			hashIdxs = append(hashIdxs, idx)
		}
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
		var buf strings.Builder
		for i, idx := range hashIdxs {
			if i > 0 {
				buf.WriteByte('|')
			}
			val := normalizeValue(values[idx])
			buf.WriteString(val)
		}
		rowHashes = append(rowHashes, fmt.Sprintf("%x", md5.Sum([]byte(buf.String()))))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate chunk rows: %w", err)
	}

	sort.Strings(rowHashes)
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(rowHashes, ",")))), nil
}

// computeChunkHashTiDB computes an aggregate hash for a chunk of TiDB rows.
// It gets its own dedicated connection with UTC timezone for parallel goroutines.
func (v *Validator) computeChunkHashTiDB(ctx context.Context, tidbDB *sql.DB, table, orderBy string, ch chunkRange) (string, error) {
	// Get dedicated connection with UTC timezone for this goroutine.
	conn, err := getTiDBConn(ctx, tidbDB)
	if err != nil {
		return "", fmt.Errorf("get TiDB conn for chunk: %w", err)
	}
	defer conn.Close()

	var rows *sql.Rows
	if wm := v.wmFilter(); wm != nil {
		query := fmt.Sprintf("SELECT * FROM %s WHERE %s ORDER BY %s LIMIT %d OFFSET %d",
			quoteMySQL(table), wmWhereMySQL(wm), quoteOrderByCols(orderBy, quoteMySQL), ch.limit, ch.offset)
		rows, err = conn.QueryContext(ctx, query, wm.Value)
	} else {
		query := fmt.Sprintf("SELECT * FROM %s ORDER BY %s LIMIT %d OFFSET %d",
			quoteMySQL(table), quoteOrderByCols(orderBy, quoteMySQL), ch.limit, ch.offset)
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

	skipCols := make(map[int]bool)
	colNames := make([]string, len(cols))
	colIdxMap := make(map[string]int)
	for i, c := range cols {
		colNames[i] = c.Name()
		colIdxMap[strings.ToLower(c.Name())] = i
		dt := strings.ToLower(c.DatabaseTypeName())
		if isApproximateFloatType(dt) || strings.Contains(dt, "json") {
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
		var buf strings.Builder
		for i, idx := range hashIdxs {
			if i > 0 {
				buf.WriteByte('|')
			}
			val := normalizeValue(values[idx])
			buf.WriteString(val)
		}
		rowHashes = append(rowHashes, fmt.Sprintf("%x", md5.Sum([]byte(buf.String()))))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate chunk rows: %w", err)
	}

	sort.Strings(rowHashes)
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(rowHashes, ",")))), nil
}
