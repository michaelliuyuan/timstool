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
	"sync/atomic"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"go.uber.org/zap"
)

// validateChecksumChunked computes row hashes for each keyset chunk of the
// table and compares PG and TiDB chunk by chunk. Chunk boundaries are NOT
// pre-cut by OFFSET (P0-1): each window pages by a strict-greater keyset
// seek over the previous window's last native key values, so cost stays flat
// on deep tables. Windows within one table are CHAINED (window i+1's
// boundary is window i's last key), but every window is exactly ONE #t4
// budget unit — concurrent TABLES interleave freely across the global sem.
//
// #t4: connection orchestration is single-layer. The caller holds NO budget
// slot; this function acquires one for the count phase and one per chunk via
// the shared global sem, so no goroutine ever waits on the sem while holding
// a connection (the old table-level × chunk-level nesting deadlocked when
// outer workers held all 8 pool conns waiting for inner chunks).
//
// drillCtx bounds the row-diff drill's own queries (its NULL pre-scan and
// window fetches use the ctx directly — M2, adversarial review d5f0be9): a
// sick DB after a chunk mismatch must degrade the drill with a cancellation
// note, never hang the whole run on a diagnostic path.
func (v *Validator) drillCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
}

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
			dctx, dcancel := v.drillCtx(ctx)
			defer dcancel()
			v.maybeDrillRowDiffs(dctx, pgDB, tidbDB, sem, schema, table, &tr, nil, nil, "")
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
	// numChunks is the COUNT-derived UPPER BOUND of windows (the old OFFSET
	// cut). The cursor scan below stops at the actual last window (a short
	// source window), so the number of windows that RUN is what the
	// suggestion reports — never more than this bound.
	numChunks := int(totalRows / chunkSize)
	if totalRows%chunkSize > 0 {
		numChunks++
	}
	if numChunks == 0 {
		numChunks = 1
	}

	// The key columns driving the keyset cursor (shared by the seek and the
	// post-scan NULL coverage guardrail).
	keyCols := splitKeyColumns(orderByCols)

	logger := zap.L()
	logger.Info("checksum mode: chunked keyset hash",
		zap.String("table", table),
		zap.Int64("total_rows", totalRows),
		zap.Int("chunks", numChunks),
		zap.Int64("chunk_size", chunkSize))

	// P0-1: keyset cursor pagination replaces the OFFSET pre-cut. Chunk
	// boundaries are discovered at runtime: window 0 fetches the first
	// chunkSize rows; every later window re-fetches with a strict-greater
	// seek over the previous window's LAST NATIVE key values (single-key
	// `pk > $last`; composite reuses rowDiffSeek's OR chain), plus the
	// watermark predicate AND'd on — flat cost on deep tables (TiDB's deep
	// OFFSET = whole-table cop scan disappears). The scan ends when a source
	// window returns fewer than chunkSize rows (行数<chunkSize 即止).
	//
	// Concurrency model: windows are CHAINED (window i+1's boundary is
	// window i's last key, so they cannot run in parallel), but each window
	// remains exactly ONE #t4 budget unit — the PG+TiDB double-sided queries
	// share a single slot, and slots are released between windows, so
	// concurrent TABLES interleave freely across the global sem (a table's
	// window 2 may run while another table's window 1 runs). NULL keys are
	// detected in-flight (hash first, then stop): a NULL key makes the seek
	// three-valued-logic UNKNOWN, so the honest stop fails the table instead
	// of silently dropping every row after it (the drill's leader ruling 1
	// applies to the chunk phase too).
	var mu sync.Mutex
	var mismatchDetails []string
	var hashMismatch bool
	var noComparable bool
	var wg sync.WaitGroup
	var chunksRan int32
	var coveredRows int64
	// nullSeen records that a NULL key row was FETCHED inside a window (vs
	// being excluded by the seek). A small table fully covered in window 0
	// with NULL rows hits the in-flight NULL check at covered==count — the
	// guardrail must still re-run as hash_group (minor4, adversarial
	// review d5f0be9), so the NULL is attributed as the trigger instead of
	// being appended as a chunk mismatch that fails an identical table.
	var nullSeen int32

	// boundaries[i] carries window i's seek (previous window's last native
	// key values; window 0 has none). stop aborts the tail goroutines once
	// a short window, a NULL key or a chunk error ends the scan — wg.Wait()
	// stays bounded (#t94 P0-2: a hung query can never pin the scan).
	boundaries := make([]chan []interface{}, numChunks)
	for i := range boundaries {
		boundaries[i] = make(chan []interface{}, 1)
	}
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopScan := func() { stopOnce.Do(func() { close(stop) }) }

	for i := 0; i < numChunks; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			var seek []interface{}
			if idx > 0 {
				select {
				case seek = <-boundaries[idx]:
				case <-stop:
					return // the scan ended earlier; no boundary will arrive
				}
			}

			release, aerr := sem.acquire(ctx)
			if aerr != nil {
				mu.Lock()
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d: cancelled: %v", idx, aerr))
				mu.Unlock()
				stopScan()
				return
			}
			defer release()

			pgHash, pgLast, pgNull, pgFetched, err := v.computeChunkHashPG(ctx, pgDB, schema, table, orderByCols, seek, chunkSize, igEff)
			if err != nil {
				if errors.Is(err, ErrNoComparableCols) {
					mu.Lock()
					noComparable = true
					mu.Unlock()
				} else {
					mu.Lock()
					mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d: %s error: %v", idx, v.srcLabel(), err))
					mu.Unlock()
				}
				stopScan()
				return
			}

			tidbHash, _, tidbNull, _, err := v.computeChunkHashTiDB(ctx, tidbDB, table, orderByCols, seek, chunkSize, igEff)
			if err != nil {
				if errors.Is(err, ErrNoComparableCols) {
					mu.Lock()
					noComparable = true
					mu.Unlock()
				} else {
					mu.Lock()
					mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d: TiDB error: %v", idx, err))
					mu.Unlock()
				}
				stopScan()
				return
			}

			atomic.AddInt32(&chunksRan, 1)
			atomic.AddInt64(&coveredRows, int64(pgFetched))
			// #t95 P1: per-chunk progress (checksum mode only) — fired once
			// THIS window's PG+TiDB hashes settled, so the UI can show
			// 「正在校验 X，chunk i/N」 and prove the task is moving.
			if v.onChunkProgress != nil {
				v.onChunkProgress(table, idx, numChunks)
			}

			if pgHash != tidbHash {
				mu.Lock()
				hashMismatch = true
				// rows %d-%d is derived from the chunk ordinal and size (the
				// cursor's actual key range is reported via the drill).
				mismatchDetails = append(mismatchDetails, fmt.Sprintf("chunk %d (rows %d-%d): hash mismatch src=%s tidb=%s",
					idx, int64(idx)*chunkSize, (int64(idx)+1)*chunkSize, truncate(pgHash, 12), truncate(tidbHash, 12)))
				mu.Unlock()
			}

			// NULL key seen in a window: the seek cannot advance past it
			// (UNKNOWN on NULL), so the scan must stop — but the verdict is
			// the guardrail's (it re-runs as hash_group when covered<count
			// OR a NULL row was fetched, minor4). Recording the flag here
			// instead of appending a mismatch keeps an identical table
			// (NULL rows on both sides) from failing on the NULL alone.
			if pgNull || tidbNull {
				atomic.StoreInt32(&nullSeen, 1)
				stopScan()
				return
			}

			// Short source window = the last one; stop the tail goroutines.
			if int64(pgFetched) < chunkSize {
				stopScan()
				return
			}
			if idx+1 < numChunks {
				boundaries[idx+1] <- pgLast
			}
		}(i)
	}
	wg.Wait()

	// P0-1 coverage guardrail (post-scan): the keyset seek is
	// three-valued-logic UNKNOWN on NULL keys, so a nullable unique-index
	// key with NULL rows silently drops every post-NULL window — the chunk
	// hashes could match and PASS while rows go unchecked (the drill's
	// leader ruling 1 applied to the chunk phase). The in-flight NULL check
	// only fires when a NULL row lands INSIDE a fetched window; a NULL past
	// the first boundary is excluded by the seek and never surfaces.
	// Detect the blind spot via the coverage discrepancy (windows covered
	// fewer rows than the count phase counted — a NULL-keyed table always
	// does), then attribute it with the drill's NULL pre-scan (one #t4 unit
	// per side, same watermark predicate).
	//
	// M1 (adversarial review d5f0be9): a coverage shortfall is NEVER
	// trusted as benign "data movement". A key that is not actually
	// table-level unique (e.g. a PG PARTIAL unique index, whose WHERE
	// clause is mis-parsed by parseIndexColumns as the key column) makes
	// the strict-greater seek silently drop duplicate boundary values on
	// BOTH sides while the window hashes still align — covered < count with
	// a diff inside the dropped rows would PASS silently. The only
	// trustworthy verdict is the full hash_group comparison, so ANY
	// coverage shortfall re-runs the table exactly. The NULL pre-scan still
	// attributes NULL keys for the disclosure.
	//
	// M2 (adversarial review d5f0be9): the guardrail's OWN queries run
	// under the query-timeout ctx — the NULL pre-scan (a LIMIT 1 probe on
	// the very DB whose chunk scan just ended early) and the re-run's
	// connection acquisition must fail the table, never hang the run. The
	// hash_group re-run itself keeps its per-query timeouts (it takes the
	// run ctx; validateSamplingWithHashGroup wraps every query itself), so
	// a legitimately large re-run is not capped by a single guardrail
	// deadline.
	//
	// Minor4 (adversarial review d5f0be9): a NULL row FETCHED in a window
	// (covered == count, small table fully in window 0) also triggers the
	// re-run — the in-flight NULL check only records nullSeen now, so an
	// identical table (NULL rows on both sides) PASSes via the honest
	// hash_group comparison instead of failing on the NULL disclosure.
	if keyInfo != nil && !keyInfo.HasPK && !noComparable &&
		(atomic.LoadInt64(&coveredRows) < tr.SourceRows || atomic.LoadInt32(&nullSeen) == 1) {
		guardCtx, guardCancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
		defer guardCancel()

		nullHit := v.rowDiffNullKeyScan(guardCtx, pgDB, tidbDB, sem, schema, table, keyCols)

		reason := "chunk coverage below count (data moved, or key not table-level unique)"
		if nullHit {
			reason = "nullable unique key has NULL rows"
		}
		logger.Info("checksum mode: "+reason+", re-run as hash_group",
			zap.String("table", table))

		rel, aerr := sem.acquire(guardCtx)
		if aerr != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("checksum: cancelled before hash_group re-run: %v", aerr)
			return tr
		}
		conn, cerr := v.getTiDBConn(guardCtx, tidbDB)
		if cerr != nil {
			rel()
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("checksum: get TiDB connection: %v", cerr)
			return tr
		}
		out := v.validateSamplingWithHashGroup(ctx, pgDB, conn, table, 1.0, tr, schema)
		conn.Close()
		rel()
		// The NULL disclosure replaces the generic no-key note: the table
		// DID have an alignment key — it was just unsafe to page. A
		// non-NULL coverage shortfall keeps the hash_group mismatch details
		// (the honest full-comparison verdict).
		if nullHit && out.Status == reporter.StatusFail {
			out.RowDiffs = &reporter.RowDiffSummary{
				KeyColumns: append([]string(nil), keyCols...),
				Note:       rowDiffNullNote,
			}
		}
		return out
	}

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
		atomic.LoadInt32(&chunksRan), chunkSize, len(mismatchDetails))
	if len(igProtected) > 0 {
		tr.Suggestion = "主键列不可排除: " + strings.Join(igProtected, ", ") + "；" + tr.Suggestion
	}

	// Row-diff drill (chunk-mismatch face): only a genuine hash mismatch
	// has data divergence to locate — operational chunk errors (query
	// failures/cancellation) carry their own triage in tr.Error and would
	// only re-fail inside the drill. No slot is held here (all chunk units
	// released above); the drill takes its own per-window units (#t4).
	if tr.Status == reporter.StatusFail && hashMismatch {
		dctx, dcancel := v.drillCtx(ctx)
		defer dcancel()
		v.maybeDrillRowDiffs(dctx, pgDB, tidbDB, sem, schema, table, &tr, keyInfo, igEff, orderByCols)
	}

	return tr
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

// chunkSeekPredicate renders the WHERE clause pieces for one chunk window
// (P0-1 keyset cursor): the optional watermark predicate plus the exclusive
// strict-greater seek chain over the previous window's last native key
// values. It mirrors the row-diff drill's composition (rowdiff.go:598-622):
// WM first, seek second, joined by AND; PG sources number the seek
// placeholders CONTINUOUSLY after the watermark's ($N), MySQL/TiDB bind
// positional ? per occurrence. Returns the AND-joined WHERE fragment and the
// bound arguments (watermark args first, seek args after).
func (v *Validator) chunkSeekPredicate(dialectPlaceholders bool, quote func(string) string,
	wmFrag string, wmA []interface{}, orderBy string, seek []interface{}) (string, []interface{}) {
	conds := make([]string, 0, 2)
	args := append([]interface{}{}, wmA...)
	if wmFrag != "" {
		conds = append(conds, wmFrag)
	}
	if seek != nil {
		seekChain := newRowDiffSeek(dialectPlaceholders, len(wmA))
		quotedKeys := make([]string, 0, 2)
		for _, k := range splitKeyColumns(orderBy) {
			quotedKeys = append(quotedKeys, quote(k))
		}
		conds = append(conds, "("+seekChain.chain(quotedKeys, seek)+")")
		args = append(args, seekChain.args...)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// computeChunkHashPG computes an aggregate hash for a keyset chunk of PG
// rows (P0-1). With a watermark filter active the chunk rows are the
// FILTERED universe (validateRowCount already counted with the same
// predicate, so the cursor stays on the same filtered scope). ig is the
// PK-resolved user ignore set (nil = none): hits join the float/JSON skip
// channel — value hashing only, chunk ordering is untouched.
//
// Returns the aggregate hash, the LAST row's NATIVE key values (the next
// window's seek position; nil on a short/empty window), whether any KEY
// column value was NULL in the window (the cursor cannot advance past NULL —
// caller must stop), and the number of scanned rows. P0-2: the query runs
// under a context.WithTimeout so a hung database fails only this chunk.
func (v *Validator) computeChunkHashPG(ctx context.Context, pgDB *sql.DB, schema, table, orderBy string, seek []interface{}, limit int64, ig map[string]bool) (string, []interface{}, bool, int, error) {
	// P0-2: query-level timeout — a hung source query fails this chunk
	// (annotated "query timeout"), never the whole run.
	qctx, cancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
	defer cancel()

	keyNames := splitKeyColumns(orderBy)
	quotedKeys := make([]string, len(keyNames))
	for i, k := range keyNames {
		quotedKeys[i] = v.srcDialect.QuoteIdent(k)
	}

	var wmFrag string
	var wmA []interface{}
	if wm := v.wmFilterFor(qctx); wm != nil {
		wmFrag = v.srcDialect.WmPredicateFragment(wm)
		wmA = wmArgs(wm)
	}
	// PG sources use $N placeholders numbered after the watermark's;
	// MySQL sources bind positional ?.
	pgPlaceholders := false
	if _, ok := v.srcDialect.(postgresDialect); ok {
		pgPlaceholders = true
	}
	where, args := v.chunkSeekPredicate(pgPlaceholders, v.srcDialect.QuoteIdent, wmFrag, wmA, orderBy, seek)

	query := fmt.Sprintf("SELECT * FROM %s.%s%s ORDER BY %s LIMIT %d",
		v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), where, strings.Join(quotedKeys, ", "), limit)
	rows, err := pgDB.QueryContext(qctx, query, args...)
	if err != nil {
		return "", nil, false, 0, annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration())
	}
	defer rows.Close()

	cols, _ := rows.ColumnTypes()
	if cols == nil {
		return "", nil, false, 0, fmt.Errorf("no column types")
	}

	// Shared hash plan (row-diff drill uses the SAME builder — one
	// implementation, no drift).
	hashIdxs, err := buildChunkHashPlan(cols, ig)
	if err != nil {
		return "", nil, false, 0, err
	}

	// Key column indices: the cursor's last-key capture reads the key from
	// the row itself (same resolution as the row-diff drill, rowdiff.go:643).
	lowerName := make(map[string]int, len(cols))
	for i, c := range cols {
		lowerName[strings.ToLower(c.Name())] = i
	}
	keyIdxs := make([]int, 0, len(keyNames))
	for _, kc := range keyNames {
		idx, ok := lowerName[strings.ToLower(kc)]
		if !ok {
			return "", nil, false, 0, fmt.Errorf("key column %q missing on %s side", kc, v.srcLabel())
		}
		keyIdxs = append(keyIdxs, idx)
	}

	values := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}

	var rowHashes []string
	var lastKey []interface{}
	nullKey := false
	fetched := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", nil, false, 0, fmt.Errorf("scan row in chunk: %w", err)
		}
		rowHashes = append(rowHashes, rowMD5(values, hashIdxs))
		// Cursor advance: capture the last row's NATIVE key values (never
		// normalized strings — int64 stays int64, mirroring the drill).
		lastKey = make([]interface{}, len(keyIdxs))
		for i, k := range keyIdxs {
			if values[k] == nil {
				nullKey = true
			}
			lastKey[i] = sanitizeNativeValue(values[k])
		}
		fetched++
	}
	if err := rows.Err(); err != nil {
		return "", nil, false, 0, fmt.Errorf("iterate chunk rows: %w", err)
	}

	sort.Strings(rowHashes)
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(rowHashes, ",")))), lastKey, nullKey, fetched, nil
}

// computeChunkHashTiDB computes an aggregate hash for a keyset chunk of TiDB
// rows (P0-1). It gets its own dedicated connection with UTC timezone for
// parallel goroutines. ig mirrors the PG-side ignore set so both sides hash
// the same columns. Same return contract as computeChunkHashPG.
func (v *Validator) computeChunkHashTiDB(ctx context.Context, tidbDB *sql.DB, table, orderBy string, seek []interface{}, limit int64, ig map[string]bool) (string, []interface{}, bool, int, error) {
	// P0-2: query-level timeout (the dedicated conn acquisition and the
	// window query share one deadline).
	qctx, cancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
	defer cancel()

	// Get dedicated connection with UTC timezone for this goroutine.
	conn, err := v.getTiDBConn(qctx, tidbDB)
	if err != nil {
		return "", nil, false, 0, annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration())
	}
	defer conn.Close()

	keyNames := splitKeyColumns(orderBy)
	quotedKeys := make([]string, len(keyNames))
	for i, k := range keyNames {
		quotedKeys[i] = v.tgtDialect.QuoteIdent(k)
	}

	var wmFrag string
	var wmA []interface{}
	if wm := v.wmFilterFor(qctx); wm != nil {
		wmFrag = v.tgtDialect.WmPredicateFragment(wm)
		wmA = wmArgs(wm)
	}
	// TiDB always binds positional ? (never $N).
	where, args := v.chunkSeekPredicate(false, v.tgtDialect.QuoteIdent, wmFrag, wmA, orderBy, seek)

	query := fmt.Sprintf("SELECT * FROM %s%s ORDER BY %s LIMIT %d",
		v.tgtDialect.QuoteIdent(table), where, strings.Join(quotedKeys, ", "), limit)
	rows, err := conn.QueryContext(qctx, query, args...)
	if err != nil {
		return "", nil, false, 0, annotateQueryTimeout(err, v.compareCfg().QueryTimeoutDuration())
	}
	defer rows.Close()

	cols, _ := rows.ColumnTypes()
	if cols == nil {
		return "", nil, false, 0, fmt.Errorf("no column types")
	}

	// Shared hash plan (row-diff drill uses the SAME builder).
	hashIdxs, err := buildChunkHashPlan(cols, ig)
	if err != nil {
		return "", nil, false, 0, err
	}

	lowerName := make(map[string]int, len(cols))
	for i, c := range cols {
		lowerName[strings.ToLower(c.Name())] = i
	}
	keyIdxs := make([]int, 0, len(keyNames))
	for _, kc := range keyNames {
		idx, ok := lowerName[strings.ToLower(kc)]
		if !ok {
			return "", nil, false, 0, fmt.Errorf("key column %q missing on %s side", kc, v.sideLabel(true))
		}
		keyIdxs = append(keyIdxs, idx)
	}

	values := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}

	var rowHashes []string
	var lastKey []interface{}
	nullKey := false
	fetched := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", nil, false, 0, fmt.Errorf("scan row in chunk: %w", err)
		}
		rowHashes = append(rowHashes, rowMD5(values, hashIdxs))
		lastKey = make([]interface{}, len(keyIdxs))
		for i, k := range keyIdxs {
			if values[k] == nil {
				nullKey = true
			}
			lastKey[i] = sanitizeNativeValue(values[k])
		}
		fetched++
	}
	if err := rows.Err(); err != nil {
		return "", nil, false, 0, fmt.Errorf("iterate chunk rows: %w", err)
	}

	sort.Strings(rowHashes)
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(rowHashes, ",")))), lastKey, nullKey, fetched, nil
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
