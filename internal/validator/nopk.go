package validator

import (
	"context"
	"crypto/md5"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"go.uber.org/zap"
)

// TableKeyInfo holds information about a table's primary key and unique indexes.
type TableKeyInfo struct {
	HasPK          bool
	PKColumns      []string
	HasUniqueIndex bool
	UniqueColumns  []string // columns from the first unique index found
}

// colMapping maps a column name to its PG result-set index for hash computation.
type colMapping struct {
	pgIdx int
	name  string
}

// tidbColMapping maps a column name to its TiDB result-set index for hash computation.
type tidbColMapping struct {
	tidbIdx int
	name    string
}

// M1 spool: the three no-PK/degraded strategies below stream BOTH sides
// (the caller hands over the already-open PG full-table cursor; the TiDB
// full-table query opens exactly as before), hash each row with the same
// normalizeValue + computeRowHash* pipeline, and count the hashes through
// a disk-backed spool instead of whole-table in-memory maps. Verdict
// semantics, mismatch messages and suggestions are preserved
// set-for-set; detail ordering is deterministic ascending-hash where the
// legacy maps iterated in Go-map random order (byte-identical output for
// ≤1 mismatch by construction). The lockstep (hash, count) walk replaces
// the legacy per-side map lookups (see rowhashspool.go).

// hashGroupMaxDetails caps the mismatch detail list rendered into the
// error message (legacy maxShow). The cap is applied INSIDE the walk
// callback so memory stays O(1) in the mismatch count (review 阻断1: the
// accumulator was the last unbounded structure on the spool path).
const hashGroupMaxDetails = 10

// tidbPeekColumns fetches TiDB column metadata with a zero-row probe so
// the unified skip/trim sets can be built BEFORE the PG scan streams (the
// legacy code obtained the same metadata from the full TiDB query after PG
// was already materialized). Types are identical to the full query's.
func (v *Validator) tidbPeekColumns(ctx context.Context, tidbConn *sql.Conn, table, errPrefix string) ([]*sql.ColumnType, error) {
	qctx, cancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
	defer cancel()
	rows, err := tidbConn.QueryContext(qctx,
		fmt.Sprintf("SELECT * FROM %s WHERE 1=0", v.tgtDialect.QuoteIdent(table)))
	if err != nil {
		return nil, fmt.Errorf("%squery TiDB: %v", errPrefix, annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
	}
	defer rows.Close()
	cols, _ := rows.ColumnTypes()
	if cols == nil {
		return nil, fmt.Errorf("%sfailed to get TiDB column types", errPrefix)
	}
	return cols, nil
}

// (spool disclosure rendering lives in rowhashspool.go: spoolSuffixFor)

// validateHashGroup performs hash-group-based validation for tables without
// a reliable unique key. It computes a hash of each row's values (sorted by
// column name for cross-DB consistency) and compares the multiset of hashes
// between PG and TiDB. M1: both sides stream through a bounded-memory
// row-hash spool (semantics identical to the legacy in-memory maps).
func (v *Validator) validateHashGroup(ctx context.Context, tidbConn *sql.Conn, table string, tr reporter.TableReport, pgCols []*sql.ColumnType, pgRows *sql.Rows, skipCols map[int]bool) reporter.TableReport {
	logger := zap.L()
	logger.Info("using hash group validation for no-PK table", zap.String("table", table))

	// Peek TiDB column metadata (zero-row probe) so the unified skip/trim
	// sets are known before the PG scan streams its hashes.
	tidbCols, err := v.tidbPeekColumns(ctx, tidbConn, table, "hash group: ")
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = err.Error()
		return tr
	}

	// Build ordered list of PG column names for consistent hashing.
	// Sort by column name so PG and TiDB hash the same column order.
	pgColNames := make([]string, len(pgCols))
	pgColNameToIdx := make(map[string]int)
	for i, c := range pgCols {
		pgColNames[i] = c.Name()
		pgColNameToIdx[strings.ToLower(c.Name())] = i
	}
	sortedPGColNames := make([]string, len(pgColNames))
	copy(sortedPGColNames, pgColNames)
	sort.Strings(sortedPGColNames)

	// Map sorted column names to PG column indices (excluding skipped columns)
	var hashCols []colMapping
	for _, name := range sortedPGColNames {
		idx := pgColNameToIdx[strings.ToLower(name)]
		if skipCols[idx] {
			continue
		}
		hashCols = append(hashCols, colMapping{pgIdx: idx, name: name})
	}

	// Build TiDB column name -> index mapping
	tidbColNameToIdx := make(map[string]int)
	for i, c := range tidbCols {
		tidbColNameToIdx[strings.ToLower(c.Name())] = i
	}

	// Build UNIFIED skip set: skip a column if it should be skipped in EITHER PG or TiDB.
	// This ensures both sides hash the same column set, preventing systematic mismatches
	// caused by type name differences (e.g., PG "integer[]" vs TiDB "json").
	unifiedSkipCols := make(map[string]bool) // column name (lowercase) -> skip
	for _, name := range sortedPGColNames {
		lowerName := strings.ToLower(name)
		// Check PG side
		pgIdx, pgOk := pgColNameToIdx[lowerName]
		if pgOk && skipCols[pgIdx] {
			unifiedSkipCols[lowerName] = true
			continue
		}
		// Check TiDB side
		tidbIdx, tidbOk := tidbColNameToIdx[lowerName]
		if tidbOk {
			dt := strings.ToLower(tidbCols[tidbIdx].DatabaseTypeName())
			if isApproximateFloatType(dt) || strings.Contains(dt, "json") {
				unifiedSkipCols[lowerName] = true
				continue
			}
		}
	}

	// Detect text-type columns for trim handling (both PG and TiDB)
	trimCols := make(map[string]bool) // column name (lowercase) -> trim
	for _, name := range sortedPGColNames {
		lowerName := strings.ToLower(name)
		pgIdx, pgOk := pgColNameToIdx[lowerName]
		if pgOk {
			dt := strings.ToLower(pgCols[pgIdx].DatabaseTypeName())
			if isTextType(dt) {
				trimCols[lowerName] = true
			}
		}
		tidbIdx, tidbOk := tidbColNameToIdx[lowerName]
		if tidbOk {
			dt := strings.ToLower(tidbCols[tidbIdx].DatabaseTypeName())
			if isTextType(dt) {
				trimCols[lowerName] = true
			}
		}
	}

	// Rebuild hashCols using unified skip set
	hashCols = nil
	for _, name := range sortedPGColNames {
		lowerName := strings.ToLower(name)
		if unifiedSkipCols[lowerName] {
			continue
		}
		idx := pgColNameToIdx[lowerName]
		// Also skip if column doesn't exist in TiDB
		if _, tidbOk := tidbColNameToIdx[lowerName]; !tidbOk {
			continue
		}
		hashCols = append(hashCols, colMapping{pgIdx: idx, name: name})
	}

	// Build TiDB hash column mapping using same unified set
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

	logger.Info("hash group column mapping",
		zap.String("table", table),
		zap.Int("pg_hash_cols", len(hashCols)),
		zap.Int("tidb_hash_cols", len(tidbHashCols)),
		zap.Int("skipped_cols", len(unifiedSkipCols)),
		zap.Int("trim_cols", len(trimCols)))

	// M1: stream PG rows straight into the spool (the caller's pgRows
	// cursor is unscanned). Fail-loud on any spool/disk error.
	pgSpool := v.newSideSpool()
	defer pgSpool.Close()
	tidbSpool := v.newSideSpool()
	defer tidbSpool.Close()

	pgValues := make([]interface{}, len(pgCols))
	pgPtrs := make([]interface{}, len(pgCols))
	for i := range pgValues {
		pgPtrs[i] = &pgValues[i]
	}

	firstPGRow := true
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

		// Debug: log first PG row hash details
		if firstPGRow && len(hashCols) > 0 {
			var parts []string
			for _, hc := range hashCols {
				val := "\\N"
				if hc.pgIdx < len(row) {
					val = row[hc.pgIdx]
				}
				if trimCols[strings.ToLower(hc.name)] {
					val = trimTrailingWhitespace(val)
				}
				parts = append(parts, fmt.Sprintf("%s=%s", hc.name, truncate(val, 30)))
			}
			logger.Debug("hash group: first PG row", zap.String("table", table), zap.String("cols", strings.Join(parts, ",")))
			firstPGRow = false
		}

		h := computeRowHashTrimmed(row, hashCols, trimCols)
		if err := pgSpool.Add(h); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("hash group: %v", err)
			return tr
		}
	}
	// A mid-scan failure (timeout included) must surface — with the spool
	// as the only copy of the side's hashes, silently truncating here
	// would compare partial multisets (review 高风险3).
	if err := pgRows.Err(); err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("hash group: scan %s: %v", v.srcLabel(), annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}

	logger.Info("no-PK table: querying full PG table for hash group comparison",
		zap.String("table", table),
		zap.Uint64("row_count", pgSpool.Rows()))

	// Query TiDB for the full table and stream its hashes into the spool.
	// P0-2: query-level deadline on the full-table scan.
	qctx, cancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
	defer cancel()
	var tidbRows *sql.Rows
	if wm := v.wmFilterFor(qctx); wm != nil {
		tidbRows, err = tidbConn.QueryContext(qctx,
			fmt.Sprintf("SELECT * FROM %s WHERE %s", v.tgtDialect.QuoteIdent(table), v.tgtDialect.WmPredicateFragment(wm)), wmArgs(wm)...)
	} else {
		tidbRows, err = tidbConn.QueryContext(qctx,
			fmt.Sprintf("SELECT * FROM %s", v.tgtDialect.QuoteIdent(table)))
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("hash group: query TiDB: %v", annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}
	defer tidbRows.Close()

	tidbValues := make([]interface{}, len(tidbCols))
	tidbPtrs := make([]interface{}, len(tidbCols))
	for i := range tidbValues {
		tidbPtrs[i] = &tidbValues[i]
	}

	tidbRowCount := 0
	firstTiDBRow := true
	for tidbRows.Next() {
		if err := tidbRows.Scan(tidbPtrs...); err != nil {
			continue
		}
		tidbRow := make([]string, len(tidbValues))
		for i, val := range tidbValues {
			tidbRow[i] = normalizeValue(val)
		}

		// Debug: log first TiDB row hash details
		if firstTiDBRow && len(tidbHashCols) > 0 {
			var parts []string
			for _, hc := range tidbHashCols {
				val := "\\N"
				if hc.tidbIdx < len(tidbRow) {
					val = tidbRow[hc.tidbIdx]
				}
				if trimCols[strings.ToLower(hc.name)] {
					val = trimTrailingWhitespace(val)
				}
				parts = append(parts, fmt.Sprintf("%s=%s", hc.name, truncate(val, 30)))
			}
			logger.Debug("hash group: first TiDB row", zap.String("table", table), zap.String("cols", strings.Join(parts, ",")))
			firstTiDBRow = false
		}

		h := computeTiDBRowHashTrimmed(tidbRow, tidbHashCols, trimCols)
		if err := tidbSpool.Add(h); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("hash group: %v", err)
			return tr
		}
		tidbRowCount++
	}
	// See the PG-side note: a truncated TiDB scan must fail, never
	// silently compare partial multisets (review 高风险3).
	if err := tidbRows.Err(); err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("hash group: scan TiDB: %v", annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}

	// Compare multisets via the sorted spool iterators (M1: streaming
	// lockstep walk; the legacy per-side map lookups produced exactly this
	// set of differing (hash, pgCount, tidbCount) triples).
	//
	// Bounded detail accumulation (review 阻断1): details cap INSIDE the
	// walk callback — O(1) memory in the mismatch count. mismatchCount
	// keeps the true total; the Suggestion renders len(mismatchDetails) =
	// min(true, hashGroupMaxDetails), exactly the legacy post-truncation
	// length.
	pgIt, err := pgSpool.Finish()
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("hash group: %v", err)
		return tr
	}
	defer pgIt.Close()
	tidbIt, err := tidbSpool.Finish()
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("hash group: %v", err)
		return tr
	}
	defer tidbIt.Close()

	var mismatchDetails []string
	mismatchCount := 0
	pgDistinct := 0
	walkErr := walkHashCounters(pgIt, tidbIt, func(h string, pgCnt, tidbCnt int) {
		if pgCnt > 0 {
			pgDistinct++ // every distinct hash present on the PG side
		}
		if pgCnt == tidbCnt {
			return
		}
		mismatchCount++
		if len(mismatchDetails) >= hashGroupMaxDetails {
			return
		}
		if tidbCnt == 0 {
			mismatchDetails = append(mismatchDetails,
				fmt.Sprintf("%s has %d row(s) with hash %s not found in TiDB", v.srcLabel(), pgCnt, truncate(h, 16)))
		} else if pgCnt == 0 {
			mismatchDetails = append(mismatchDetails,
				fmt.Sprintf("TiDB has %d row(s) with hash %s not found in %s", tidbCnt, truncate(h, 16), v.srcLabel()))
		} else {
			mismatchDetails = append(mismatchDetails,
				fmt.Sprintf("hash %s: %s count=%d TiDB count=%d", truncate(h, 16), v.srcLabel(), pgCnt, tidbCnt))
		}
	})

	if walkErr != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("hash group: %v", walkErr)
		return tr
	}

	if mismatchCount > 0 {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("hash group mismatch: %s", strings.Join(mismatchDetails, "; "))
		if mismatchCount > len(mismatchDetails) {
			logger.Info("hash group mismatch details truncated",
				zap.String("table", table),
				zap.Int("mismatches", mismatchCount),
				zap.Int("shown", len(mismatchDetails)))
		}
	} else {
		tr.Status = reporter.StatusPass
	}

	tr.Suggestion = fmt.Sprintf("hash group validation: %d %s hashes vs %d TiDB rows, %d mismatches%s",
		pgDistinct, v.srcLabel(), tidbRowCount, len(mismatchDetails), spoolSuffixFor(pgSpool, tidbSpool))

	if pgSpool.Spilled() || tidbSpool.Spilled() {
		logger.Info("hash group used row-hash spool",
			zap.String("table", table),
			zap.Uint64("pg_rows", pgSpool.Rows()),
			zap.Uint64("tidb_rows", tidbSpool.Rows()),
			zap.Bool("pg_spilled", pgSpool.Spilled()),
			zap.Bool("tidb_spilled", tidbSpool.Spilled()))
	}

	return tr
}

// computeRowHash computes MD5 of a PG row's values, using only the columns
// specified in hashCols, joined by "|" in sorted column name order.
func computeRowHash(row []string, hashCols []colMapping) string {
	var buf strings.Builder
	for i, hc := range hashCols {
		if i > 0 {
			buf.WriteByte('|')
		}
		val := "\\N"
		if hc.pgIdx < len(row) {
			val = row[hc.pgIdx]
		}
		buf.WriteString(val)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(buf.String())))
}

// computeRowHashTrimmed computes MD5 with trim handling for text columns.
func computeRowHashTrimmed(row []string, hashCols []colMapping, trimCols map[string]bool) string {
	var buf strings.Builder
	for i, hc := range hashCols {
		if i > 0 {
			buf.WriteByte('|')
		}
		val := "\\N"
		if hc.pgIdx < len(row) {
			val = row[hc.pgIdx]
		}
		if trimCols[strings.ToLower(hc.name)] {
			val = trimTrailingWhitespace(val)
		}
		buf.WriteString(val)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(buf.String())))
}

// computeTiDBRowHash computes MD5 of a TiDB row's values, using only the
// columns specified in hashCols, joined by "|" in sorted column name order.
func computeTiDBRowHash(row []string, hashCols []tidbColMapping) string {
	var buf strings.Builder
	for i, hc := range hashCols {
		if i > 0 {
			buf.WriteByte('|')
		}
		val := "\\N"
		if hc.tidbIdx < len(row) {
			val = row[hc.tidbIdx]
		}
		buf.WriteString(val)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(buf.String())))
}

// computeTiDBRowHashTrimmed computes MD5 with trim handling for text columns.
func computeTiDBRowHashTrimmed(row []string, hashCols []tidbColMapping, trimCols map[string]bool) string {
	var buf strings.Builder
	for i, hc := range hashCols {
		if i > 0 {
			buf.WriteByte('|')
		}
		val := "\\N"
		if hc.tidbIdx < len(row) {
			val = row[hc.tidbIdx]
		}
		if trimCols[strings.ToLower(hc.name)] {
			val = trimTrailingWhitespace(val)
		}
		buf.WriteString(val)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(buf.String())))
}

// isApproximateFloatType checks if a database type name is an approximate
// floating-point type that has inherent precision differences between PG and TiDB.
// DECIMAL/NUMERIC are exact types and should NOT be skipped — they have identical
// precision in both databases.
func isApproximateFloatType(dt string) bool {
	return dt == "real" || dt == "float" || dt == "float4" || dt == "float8" ||
		dt == "double" || dt == "double precision"
}

// streamAggregateHash feeds the sorted (hash, count) stream into md5 in
// exactly the legacy "join(sortedHashesWithDuplicates, \",\")" byte order.
func streamAggregateHash(it hashCountIter) (string, int64, error) {
	h := md5.New()
	var rows int64
	first := true
	for {
		hash, count, err := it.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, err
		}
		rows += int64(count)
		for i := 0; i < count; i++ {
			if !first {
				h.Write([]byte(","))
			}
			h.Write([]byte(hash))
			first = false
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), rows, nil
}

// validateAggregateHash computes a single aggregate hash for each side (PG and TiDB)
// by sorting all individual row hashes and hashing the concatenation. This gives a
// fast yes/no answer: if the aggregate hashes match, the tables are identical.
// If they differ, the caller should fall back to hash_group or bucket for details.
// M1: the sorted hash lists live in the bounded-memory spool, never in RAM.
func (v *Validator) validateAggregateHash(ctx context.Context, tidbConn *sql.Conn, table string, tr reporter.TableReport, pgCols []*sql.ColumnType, pgRows *sql.Rows, skipCols map[int]bool) reporter.TableReport {
	logger := zap.L()
	logger.Info("using aggregate hash validation for no-PK table", zap.String("table", table))

	tidbCols, err := v.tidbPeekColumns(ctx, tidbConn, table, "aggregate hash: ")
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = err.Error()
		return tr
	}

	// Build sorted column list for consistent hashing
	pgColNameToIdx := make(map[string]int)
	var pgColNames []string
	for i, c := range pgCols {
		name := c.Name()
		pgColNames = append(pgColNames, name)
		pgColNameToIdx[strings.ToLower(name)] = i
	}
	sort.Strings(pgColNames)

	var hashCols []colMapping
	for _, name := range pgColNames {
		idx := pgColNameToIdx[strings.ToLower(name)]
		if skipCols[idx] {
			continue
		}
		hashCols = append(hashCols, colMapping{pgIdx: idx, name: name})
	}

	pgSpool := v.newSideSpool()
	defer pgSpool.Close()
	tidbSpool := v.newSideSpool()
	defer tidbSpool.Close()

	// Stream PG row hashes into the spool (legacy: materialized [][]string)
	pgValues := make([]interface{}, len(pgCols))
	pgPtrs := make([]interface{}, len(pgCols))
	for i := range pgValues {
		pgPtrs[i] = &pgValues[i]
	}
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
		if err := pgSpool.Add(computeRowHash(row, hashCols)); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("aggregate hash: %v", err)
			return tr
		}
	}
	// Mid-scan failure must fail the table — never compare a partial
	// multiset (review 高风险3).
	if err := pgRows.Err(); err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash: scan %s: %v", v.srcLabel(), annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}

	// Query TiDB full table. P0-2: query-level deadline on the full-table scan.
	qctx, cancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
	defer cancel()
	var tidbRows *sql.Rows
	if wm := v.wmFilterFor(qctx); wm != nil {
		tidbRows, err = tidbConn.QueryContext(qctx,
			fmt.Sprintf("SELECT * FROM %s WHERE %s", v.tgtDialect.QuoteIdent(table), v.tgtDialect.WmPredicateFragment(wm)), wmArgs(wm)...)
	} else {
		tidbRows, err = tidbConn.QueryContext(qctx,
			fmt.Sprintf("SELECT * FROM %s", v.tgtDialect.QuoteIdent(table)))
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash: query TiDB: %v", annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}
	defer tidbRows.Close()

	// MS-03: target-side hash-column matching relocated into the target
	// dialect (docs/MS03-DIALECT-MAP.md, TargetDialect.MatchHashColumns).
	tidbHashCols := v.tgtDialect.MatchHashColumns(pgColNames, tidbCols)

	tidbValues := make([]interface{}, len(tidbCols))
	tidbPtrs := make([]interface{}, len(tidbCols))
	for i := range tidbValues {
		tidbPtrs[i] = &tidbValues[i]
	}

	for tidbRows.Next() {
		if err := tidbRows.Scan(tidbPtrs...); err != nil {
			continue
		}
		tidbRow := make([]string, len(tidbValues))
		for i, val := range tidbValues {
			tidbRow[i] = normalizeValue(val)
		}
		if err := tidbSpool.Add(computeTiDBRowHash(tidbRow, tidbHashCols)); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("aggregate hash: %v", err)
			return tr
		}
	}
	// See the PG-side note (review 高风险3).
	if err := tidbRows.Err(); err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash: scan TiDB: %v", annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}

	// Sorted-hash-list aggregate, streamed through md5 (byte-identical to
	// the legacy sort+join+md5 over the in-memory slices).
	pgIt, err := pgSpool.Finish()
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash: %v", err)
		return tr
	}
	defer pgIt.Close()
	pgAggregate, pgHashRows, aerr := streamAggregateHash(pgIt)
	if aerr != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash: %v", aerr)
		return tr
	}

	tidbIt, err := tidbSpool.Finish()
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash: %v", err)
		return tr
	}
	defer tidbIt.Close()
	tidbAggregate, tidbHashRows, aerr := streamAggregateHash(tidbIt)
	if aerr != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash: %v", aerr)
		return tr
	}

	if pgAggregate != tidbAggregate {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("aggregate hash mismatch: src=%s tidb=%s", truncate(pgAggregate, 16), truncate(tidbAggregate, 16))
	} else {
		tr.Status = reporter.StatusPass
	}

	tr.Suggestion = fmt.Sprintf("aggregate hash validation: src_hash=%s tidb_hash=%s, %d %s rows vs %d TiDB rows%s",
		truncate(pgAggregate, 16), truncate(tidbAggregate, 16), pgHashRows, v.srcLabel(), tidbHashRows, spoolSuffixFor(pgSpool, tidbSpool))

	if pgSpool.Spilled() || tidbSpool.Spilled() {
		logger.Info("aggregate hash used row-hash spool",
			zap.String("table", table),
			zap.Uint64("pg_rows", pgSpool.Rows()),
			zap.Uint64("tidb_rows", tidbSpool.Rows()))
	}

	return tr
}

// bucketDiffInfo remembers the first (lowest-hash) differing hash in a
// mismatched bucket plus its signed count delta (src - tidb), reproducing
// the legacy per-bucket multiset detail form. Legacy picked a map-random
// differing hash; lowest-hash is the deterministic normalization.
type bucketDiffInfo struct {
	hash  string
	delta int
}

// validateBucketCompare divides table rows into N buckets based on row hash modulo,
// compares row counts per bucket, and drills into mismatched buckets using hash
// counts. M1: hash buckets derive from the sorted spool walk — a bucket
// mismatches iff it contains at least one hash whose counts differ across
// sides (exactly the legacy per-bucket count+multiset verdict), with the
// same detail messages.
func (v *Validator) validateBucketCompare(ctx context.Context, tidbConn *sql.Conn, table string, tr reporter.TableReport, pgCols []*sql.ColumnType, pgRows *sql.Rows, skipCols map[int]bool) reporter.TableReport {
	logger := zap.L()
	bucketCount := v.compareCfg().NoPKBucketCount
	if bucketCount <= 0 {
		bucketCount = 100
	}

	logger.Info("using bucket validation for no-PK table",
		zap.String("table", table),
		zap.Int("bucket_count", bucketCount))

	tidbCols, err := v.tidbPeekColumns(ctx, tidbConn, table, "bucket compare: ")
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = err.Error()
		return tr
	}

	// Build sorted column list
	pgColNameToIdx := make(map[string]int)
	var pgColNames []string
	for i, c := range pgCols {
		name := c.Name()
		pgColNames = append(pgColNames, name)
		pgColNameToIdx[strings.ToLower(name)] = i
	}
	sort.Strings(pgColNames)

	var hashCols []colMapping
	for _, name := range pgColNames {
		idx := pgColNameToIdx[strings.ToLower(name)]
		if skipCols[idx] {
			continue
		}
		hashCols = append(hashCols, colMapping{pgIdx: idx, name: name})
	}

	pgSpool := v.newSideSpool()
	defer pgSpool.Close()
	tidbSpool := v.newSideSpool()
	defer tidbSpool.Close()

	// Per-bucket row totals are plain int64 arrays (bounded by
	// bucketCount, same exposure the legacy per-bucket slices had).
	pgBucketTotals := make([]int64, bucketCount)
	tidbBucketTotals := make([]int64, bucketCount)

	pgValues := make([]interface{}, len(pgCols))
	pgPtrs := make([]interface{}, len(pgCols))
	for i := range pgValues {
		pgPtrs[i] = &pgValues[i]
	}
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
		h := computeRowHash(row, hashCols)
		if err := pgSpool.Add(h); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("bucket compare: %v", err)
			return tr
		}
		pgBucketTotals[bucketOf(h, bucketCount)]++
	}
	// Mid-scan failure must fail the table — never compare a partial
	// multiset (review 高风险3).
	if err := pgRows.Err(); err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("bucket compare: scan %s: %v", v.srcLabel(), annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}

	// Query TiDB full table. P0-2: query-level deadline.
	qctx, cancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
	defer cancel()
	var tidbRows *sql.Rows
	if wm := v.wmFilterFor(qctx); wm != nil {
		tidbRows, err = tidbConn.QueryContext(qctx,
			fmt.Sprintf("SELECT * FROM %s WHERE %s", v.tgtDialect.QuoteIdent(table), v.tgtDialect.WmPredicateFragment(wm)), wmArgs(wm)...)
	} else {
		tidbRows, err = tidbConn.QueryContext(qctx,
			fmt.Sprintf("SELECT * FROM %s", v.tgtDialect.QuoteIdent(table)))
	}
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("bucket compare: query TiDB: %v", annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}
	defer tidbRows.Close()

	// MS-03: target-side hash-column matching relocated into the target
	// dialect (docs/MS03-DIALECT-MAP.md, TargetDialect.MatchHashColumns).
	tidbHashCols := v.tgtDialect.MatchHashColumns(pgColNames, tidbCols)

	tidbValues := make([]interface{}, len(tidbCols))
	tidbPtrs := make([]interface{}, len(tidbCols))
	for i := range tidbValues {
		tidbPtrs[i] = &tidbValues[i]
	}

	for tidbRows.Next() {
		if err := tidbRows.Scan(tidbPtrs...); err != nil {
			continue
		}
		tidbRow := make([]string, len(tidbValues))
		for i, val := range tidbValues {
			tidbRow[i] = normalizeValue(val)
		}
		h := computeTiDBRowHash(tidbRow, tidbHashCols)
		if err := tidbSpool.Add(h); err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("bucket compare: %v", err)
			return tr
		}
		tidbBucketTotals[bucketOf(h, bucketCount)]++
	}
	// See the PG-side note (review 高风险3).
	if err := tidbRows.Err(); err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("bucket compare: scan TiDB: %v", annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration()))
		return tr
	}

	// Bucket verdicts from the lockstep hash-count walk: a bucket
	// mismatches iff some hash in it differs across sides. bucketDiffs
	// keeps the first (lowest-hash) differing hash + signed delta for the
	// equal-totals detail form.
	pgIt, err := pgSpool.Finish()
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("bucket compare: %v", err)
		return tr
	}
	defer pgIt.Close()
	tidbIt, err := tidbSpool.Finish()
	if err != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("bucket compare: %v", err)
		return tr
	}
	defer tidbIt.Close()

	bucketDiffs := make(map[int]bucketDiffInfo)
	walkErr := walkHashCounters(pgIt, tidbIt, func(h string, pgCnt, tidbCnt int) {
		if pgCnt == tidbCnt {
			return
		}
		b := bucketOf(h, bucketCount)
		if _, seen := bucketDiffs[b]; !seen {
			bucketDiffs[b] = bucketDiffInfo{hash: h, delta: pgCnt - tidbCnt}
		}
	})
	if walkErr != nil {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("bucket compare: %v", walkErr)
		return tr
	}

	// Compare buckets in bucket order (deterministic, matching the legacy
	// 0..N-1 loop): count-mismatched buckets take form 1; equal-count
	// buckets with multiset diffs take form 2.
	var mismatchDetails []string
	mismatchedBuckets := len(bucketDiffs)

	for i := 0; i < bucketCount && len(mismatchDetails) < 5; i++ {
		d, ok := bucketDiffs[i]
		if !ok {
			continue
		}
		if pgBucketTotals[i] != tidbBucketTotals[i] {
			mismatchDetails = append(mismatchDetails,
				fmt.Sprintf("bucket %d: %s=%d rows TiDB=%d rows", i, v.srcLabel(), pgBucketTotals[i], tidbBucketTotals[i]))
		} else {
			mismatchDetails = append(mismatchDetails,
				fmt.Sprintf("bucket %d: hash %s count differs by %d", i, truncate(d.hash, 16), d.delta))
		}
	}

	if mismatchedBuckets > 0 {
		tr.Status = reporter.StatusFail
		// Legacy detail shape: at most 5 bucket details and NOTHING more.
		// (The legacy "… and N more mismatched buckets" suffix branch was
		// dead code — the detail loop above caps at 5 — and is deliberately
		// NOT revived; the true bucket count already leads the message.)
		tr.Error = fmt.Sprintf("bucket mismatch (%d/%d buckets): %s",
			mismatchedBuckets, bucketCount, strings.Join(mismatchDetails, "; "))
	} else {
		tr.Status = reporter.StatusPass
	}

	tr.Suggestion = fmt.Sprintf("bucket validation: %d buckets, %d mismatched, %d %s rows%s",
		bucketCount, mismatchedBuckets, pgSpool.Rows(), v.srcLabel(), spoolSuffixFor(pgSpool, tidbSpool))

	if pgSpool.Spilled() || tidbSpool.Spilled() {
		logger.Info("bucket compare used row-hash spool",
			zap.String("table", table),
			zap.Uint64("pg_rows", pgSpool.Rows()),
			zap.Uint64("tidb_rows", tidbSpool.Rows()))
	}

	return tr
}

// bucketOf maps a hex hash string to a bucket number [0, bucketCount).
func bucketOf(hash string, bucketCount int) int {
	// Use first 8 hex chars as a uint32, then mod bucketCount
	var v uint32
	for i := 0; i < 8 && i < len(hash); i++ {
		v = v<<4 | hexVal(hash[i])
	}
	return int(v % uint32(bucketCount))
}

// hexVal converts a hex char to its value.
func hexVal(c byte) uint32 {
	switch {
	case c >= '0' && c <= '9':
		return uint32(c - '0')
	case c >= 'a' && c <= 'f':
		return uint32(c - 'a' + 10)
	case c >= 'A' && c <= 'F':
		return uint32(c - 'A' + 10)
	default:
		return 0
	}
}

// isTextType checks if a database type is a text/character type that may
// have trailing space differences between PG and TiDB/MySQL.
func isTextType(dt string) bool {
	switch dt {
	case "character", "char", "bpchar", "character varying", "varchar", "text",
		"tinytext", "mediumtext", "longtext":
		return true
	default:
		return false
	}
}
