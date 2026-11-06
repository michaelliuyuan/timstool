package validator

// count_chunked.go implements the BUG-CMP-COUNT-P1 chunked count engine:
// the C2 estimate-driven routing plus the C3 keyset chunk COUNT machinery.
//
// Motivation: the P0 fix (30m default + timeout retry ×2) healed the
// whole-table COUNT failure mode, but a tens-of-millions-row COUNT(*) is
// mechanically still ONE huge scan. This engine splits the count into
// per-PK-range chunk COUNTs: every query stays a small range scan that
// cannot outrun a sane deadline, boundaries come from a key-only keyset
// probe of the SOURCE (never OFFSET), and the (prev, boundary] ranges are
// counted on BOTH sides with the same key values and the same predicates —
// including the watermark fragment (same universe on both ends).
//
// Red lines honored here: checksum.go's hash path is untouched (the seek
// predicate renderer is only CALLED); every existing seam
// (countWithTimeoutRetry, chunkSeekPredicate, wmArgs, EstimateRows,
// DetectTableKey) is only consumed; the report strings keep their legacy
// shapes ("source/target count: …", "row count mismatch: source=%d
// target=%d diff=%d"); the whole engine runs inside the ONE concSem slot
// the caller already holds (chunks serial, #t4 discipline).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"go.uber.org/zap"
)

// Defaults for the two chunked-count knobs (C1). They live here next to
// their consumers; DefaultConfig pre-fills them so an absent config key
// yields the defaults while an explicit chunked_count_threshold: 0 keeps
// the whole-table path (operator opt-out).
const (
	defaultChunkedCountThreshold int64 = 5_000_000
	defaultChunkedCountTarget    int64 = 1_000_000
)

// errNullChunkKey marks a NULL value in the PK columns during the boundary
// scan: a real PK is NOT NULL by constraint, so this is metadata lying to
// us — the keyset ranges cannot be carved safely and the table falls back
// to the whole-table count (with disclosure).
var errNullChunkKey = errors.New("chunk key column holds NULL")

// chunkedCountThresholdOf resolves the routing threshold: 0 = disabled,
// negative = the 5M default (an operator typo must not silently disable
// the engine), positive = as configured.
func chunkedCountThresholdOf(cc config.CompareConfig) int64 {
	if cc.ChunkedCountThreshold < 0 {
		return defaultChunkedCountThreshold
	}
	return cc.ChunkedCountThreshold
}

// chunkedCountTargetOf resolves the per-chunk target row count (<= 0 →
// the 1M default; there is no disable semantics for the window size).
func chunkedCountTargetOf(cc config.CompareConfig) int64 {
	if cc.ChunkedCountTarget <= 0 {
		return defaultChunkedCountTarget
	}
	return cc.ChunkedCountTarget
}

// maybeCountChunked is the C2 routing head of the count path: it decides,
// from the two dialects' cheap row ESTIMATES, whether this table counts
// via the chunked engine. Returns nil when the legacy whole-table path
// must run unchanged (feature disabled, or estimate below threshold).
// When routing engages, the returned report is FINAL for the table —
// including the fallbacks that still count whole-table (no real PK, NULL
// key) but add the Suggestion disclosure.
func (v *Validator) maybeCountChunked(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn, table, schema string) *reporter.TableReport {
	thr := chunkedCountThresholdOf(v.compareCfg())
	if thr <= 0 {
		return nil // chunked counting disabled (chunked_count_threshold: 0)
	}

	// Both estimates under one query deadline (quick-mode pattern): these
	// are metadata reads, not scans.
	ectx, cancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
	srcEst, srcErr := v.srcDialect.EstimateRows(ectx, pgDB, schema, table)
	tgtEst, tgtErr := v.tgtDialect.EstimateRows(ectx, tidbConn, schema, table)
	cancel()

	chunked := false
	estNote := ""
	switch {
	case srcErr != nil && tgtErr != nil:
		// Both estimates unavailable (stats missing / probe failure):
		// conservatively treat the table as BIG — rather chunk (harmless
		// when wrong: chunk sums equal the whole-table count) than gamble
		// on one unbounded scan (the exact P0 incident shape).
		chunked = true
		estNote = "双侧行数估算均失败，保守走分块"
		zap.L().Warn("chunked count: both row estimates failed, treating table as large",
			zap.String("table", table), zap.Error(srcErr), zap.Error(tgtErr))
	default:
		if srcErr != nil {
			zap.L().Warn("chunked count: source estimate failed, routing on target estimate",
				zap.String("table", table), zap.Error(srcErr))
		}
		if tgtErr != nil {
			zap.L().Warn("chunked count: target estimate failed, routing on source estimate",
				zap.String("table", table), zap.Error(tgtErr))
		}
		max := srcEst
		if tgtEst > max {
			max = tgtEst
		}
		chunked = max >= thr
	}
	if !chunked {
		return nil // small table: legacy path, byte-identical
	}

	// Only a REAL primary key gives safe keyset ranges: a unique index is
	// nullable (NULL breaks the seek's three-valued logic) and no key at
	// all leaves nothing to range on. Both fall back to whole-table count
	// WITH disclosure (C3 no-PK red line).
	keyInfo, kerr := v.srcDialect.DetectTableKey(ctx, pgDB, schema, table)
	if kerr != nil {
		zap.L().Warn("chunked count: table key detection failed, falling back to whole-table count",
			zap.String("table", table), zap.Error(kerr))
	}
	if kerr != nil || keyInfo == nil || !keyInfo.HasPK || len(keyInfo.PKColumns) == 0 {
		tr := v.wholeTableCount(ctx, pgDB, tidbConn, table, schema)
		tr.Suggestion = "无真主键无法分块，已回退整表 COUNT"
		if estNote != "" {
			tr.Suggestion = estNote + "；" + tr.Suggestion
		}
		return &tr
	}

	return v.countRowsChunked(ctx, pgDB, tidbConn, table, schema, keyInfo.PKColumns, estNote)
}

// countRowsChunked runs the C3 engine for one table: boundary scan →
// serial chunk COUNTs on both sides → summed totals. Every count goes
// through countWithTimeoutRetry (per-chunk timeout + retry ×2 built in);
// a chunk that still fails after its retries fails the table with the
// legacy "source/target count:" shape plus the chunk range disclosure.
// Chunks are SERIAL and share the caller's single concSem slot.
func (v *Validator) countRowsChunked(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn,
	table, schema string, keyCols []string, estNote string) *reporter.TableReport {

	qt := v.compareCfg().QueryTimeoutDuration()
	target := chunkedCountTargetOf(v.compareCfg())
	wm := v.wmFilterFor(ctx)

	bounds, berr := v.chunkCountBoundaries(ctx, pgDB, schema, table, keyCols, wm, target)
	if berr != nil {
		if errors.Is(berr, errNullChunkKey) {
			// NULL key: ranges cannot be carved safely — count whole-table
			// (P0 machinery still applies) and disclose why.
			tr := v.wholeTableCount(ctx, pgDB, tidbConn, table, schema)
			tr.Suggestion = "主键列含 NULL，无法分块，已回退整表 COUNT"
			return &tr
		}
		tr := reporter.TableReport{TableName: table, Status: reporter.StatusFail,
			Error: fmt.Sprintf("source count: %v", annotateQueryTimeout(fmt.Errorf("chunk boundary scan: %w", berr), qt))}
		return &tr
	}

	// Chunks: (prev, bounds[0]], (bounds[0], bounds[1]], …, tail
	// (last, +∞) — the tail catches rows beyond the last source boundary
	// (target drift: extra target rows past the source max key).
	totalChunks := len(bounds) + 1
	tr := reporter.TableReport{TableName: table, Status: reporter.StatusPass}

	var srcSum, dstSum int64
	var diffDescs []string
	diffCount := 0
	var prev []interface{}
	for i := 0; i < totalChunks; i++ {
		haveUpper := i < len(bounds)
		var upper []interface{}
		if haveUpper {
			upper = bounds[i]
		}
		desc := chunkRangeDesc(prev, haveUpper, upper)

		srcN, serr := v.countChunkRangeSide(ctx, pgDB, true, schema, table, keyCols, wm, prev, haveUpper, upper)
		if serr != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("source count: chunk %s: %v", desc, annotateQueryTimeout(serr, qt))
			return &tr
		}
		dstN, derr := v.countChunkRangeSide(ctx, tidbConn, false, schema, table, keyCols, wm, prev, haveUpper, upper)
		if derr != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("target count: chunk %s: %v", desc, annotateQueryTimeout(derr, qt))
			return &tr
		}

		srcSum += srcN
		dstSum += dstN
		if srcN != dstN {
			diffCount++
			if len(diffDescs) < 3 {
				diffDescs = append(diffDescs, fmt.Sprintf("rows differ in %s: src=%d dst=%d", desc, srcN, dstN))
			}
		}

		// C5: per-chunk progress on the existing checksum callback — the
		// FE shows 「chunk i/N」 and the possibly-stuck heartbeat sees
		// movement. 0-based ordinal, fired once both sides settled.
		if v.onChunkProgress != nil {
			v.onChunkProgress(table, i, totalChunks)
		}

		if haveUpper {
			prev = upper
		}
	}

	tr.SourceRows = srcSum
	tr.TargetRows = dstSum
	tr.DiffRows = srcSum - dstSum
	tr.Suggestion = fmt.Sprintf("chunked count: %d chunks (keyset on %s)", totalChunks, strings.Join(keyCols, ","))
	if estNote != "" {
		tr.Suggestion += "；" + estNote
	}

	if tr.DiffRows != 0 {
		// Legacy verdict shape: totals decide, exactly like the
		// whole-table count. The per-chunk localization is DISCLOSED via
		// Suggestion (first 3 differing ranges) — C4.
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("row count mismatch: source=%d target=%d diff=%d", srcSum, dstSum, tr.DiffRows)
		tr.Suggestion += "; " + strings.Join(diffDescs, "; ")
	} else if diffCount > 0 {
		// Totals match but individual chunks differ (differences cancel
		// out). The whole-table count would also PASS here, so the verdict
		// stays PASS — but the chunk-level evidence is disclosed rather
		// than silently dropped.
		tr.Suggestion += fmt.Sprintf("; WARNING: %d chunks differ but cancel out (net diff 0); first 3: %s",
			diffCount, strings.Join(diffDescs, "; "))
	}

	return &tr
}

// chunkCountBoundaries scans the SOURCE primary key space with key-only
// keyset windows (never OFFSET): window i selects only the key columns
// WHERE [wm AND] (key > prev) ORDER BY key LIMIT target, and its last
// native key becomes boundary i. The scan stops at the first short window
// (that window's last key is the table's max in the wm universe). Every
// probe runs under its own query deadline; probes are cheap index-only
// reads, so no retry loop here — a persistent failure fails the chunked
// path honestly (the caller's error keeps the "source count:" shape).
func (v *Validator) chunkCountBoundaries(ctx context.Context, pgDB *sql.DB, schema, table string,
	keyCols []string, wm *config.WatermarkFilter, target int64) ([][]interface{}, error) {

	pgPlaceholders := false
	if _, ok := v.srcDialect.(postgresDialect); ok {
		pgPlaceholders = true
	}
	quoted := make([]string, len(keyCols))
	for i, k := range keyCols {
		quoted[i] = v.srcDialect.QuoteIdent(k)
	}
	orderBy := strings.Join(keyCols, ", ")

	var wmFrag string
	var wmA []interface{}
	if wm != nil {
		wmFrag = v.srcDialect.WmPredicateFragment(wm)
		wmA = wmArgs(wm)
	}

	var bounds [][]interface{}
	var seek []interface{} // nil = first window (unbounded below)
	for {
		where, args := v.chunkSeekPredicate(pgPlaceholders, v.srcDialect.QuoteIdent, wmFrag, wmA, orderBy, seek)
		query := fmt.Sprintf("SELECT %s FROM %s.%s%s ORDER BY %s LIMIT %d",
			strings.Join(quoted, ", "), v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table),
			where, strings.Join(quoted, ", "), target)

		qctx, cancel := context.WithTimeout(ctx, v.compareCfg().QueryTimeoutDuration())
		rows, err := pgDB.QueryContext(qctx, query, args...)
		if err != nil {
			cancel()
			return nil, err
		}
		var lastKey []interface{}
		nullKey := false
		fetched := 0
		for rows.Next() {
			vals := make([]interface{}, len(keyCols))
			ptrs := make([]interface{}, len(keyCols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				cancel()
				return nil, fmt.Errorf("scan chunk boundary key: %w", err)
			}
			lastKey = make([]interface{}, len(keyCols))
			for i := range vals {
				if vals[i] == nil {
					nullKey = true
				}
				lastKey[i] = sanitizeNativeValue(vals[i])
			}
			fetched++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			cancel()
			return nil, fmt.Errorf("iterate chunk boundary keys: %w", err)
		}
		rows.Close()
		cancel()

		if nullKey {
			return nil, errNullChunkKey
		}
		if fetched == 0 {
			return bounds, nil // empty table (in the wm universe)
		}
		bounds = append(bounds, lastKey)
		if int64(fetched) < target {
			return bounds, nil // short window: last key IS the max
		}
		seek = lastKey
	}
}

// countChunkRangeSide counts ONE chunk range (lower, upper] on one side.
// The SAME native key values from the source boundary scan bound both
// sides' predicates; the watermark fragment AND's on the same universe
// (watermark args first, range args after — the chunkSeekPredicate
// ordering). Source = PG-shaped $N numbering; target = positional ?.
// The count itself goes through countWithTimeoutRetry (per-chunk query
// deadline + timeout retry ×2, P0-A2 machinery reused verbatim).
func (v *Validator) countChunkRangeSide(ctx context.Context, q countQueryer, source bool,
	schema, table string, keyCols []string, wm *config.WatermarkFilter,
	lower []interface{}, haveUpper bool, upper []interface{}) (int64, error) {

	var quote func(string) string
	var wmFrag string
	var wmA []interface{}
	pgPlaceholders := false
	ref := ""
	if source {
		quote = v.srcDialect.QuoteIdent
		if _, ok := v.srcDialect.(postgresDialect); ok {
			pgPlaceholders = true
		}
		if wm != nil {
			wmFrag = v.srcDialect.WmPredicateFragment(wm)
			wmA = wmArgs(wm)
		}
		ref = v.srcDialect.QuoteIdent(schema) + "." + v.srcDialect.QuoteIdent(table)
	} else {
		quote = v.tgtDialect.QuoteIdent // positional ? always on the target
		if wm != nil {
			wmFrag = v.tgtDialect.WmPredicateFragment(wm)
			wmA = wmArgs(wm)
		}
		ref = v.tgtDialect.QuoteIdent(table)
	}

	var upperArg []interface{}
	if haveUpper {
		upperArg = upper
	}
	rangeFrag, rangeArgs := renderKeyRange(pgPlaceholders, len(wmA)+1, quote, keyCols, lower, upperArg)

	conds := make([]string, 0, 2)
	if wmFrag != "" {
		conds = append(conds, wmFrag)
	}
	conds = append(conds, rangeFrag)
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", ref, strings.Join(conds, " AND "))

	side := "target"
	if source {
		side = "source"
	}
	args := append(append([]interface{}{}, wmA...), rangeArgs...)
	return v.countWithTimeoutRetry(ctx, q, table, side, query, args...)
}

// renderKeyRange renders the half-open keyset range predicate
// (key) > lower AND (key) <= upper over the quoted key columns, spelled
// as OR chains per bound — the row-constructor tuple form is forbidden on
// the same ruling as rowDiffSeek (MySQL tuple-comparison history):
//
//	lower: k1 > $a OR (k1 = $a AND k2 > $b) …
//	upper: k1 < $a OR (k1 = $a AND k2 < $b) … OR (… AND kN <= $z)
//
// A nil lower/upper leaves that side unbounded (empty fragment). Single-
// column keys use the flat "k > $n AND k <= $m" form. Placeholders: PG
// numbers $N continuously from pgStart with one FRESH number per
// comparison (the two bounds bind different values, unlike the seek's
// per-column memoization); MySQL/TiDB use positional ? per occurrence.
func renderKeyRange(pgPlaceholders bool, pgStart int, quote func(string) string,
	keyCols []string, lower, upper []interface{}) (string, []interface{}) {

	next := pgStart
	args := make([]interface{}, 0, 2*len(keyCols))
	ph := func(v interface{}) string {
		args = append(args, v)
		if !pgPlaceholders {
			return "?"
		}
		s := fmt.Sprintf("$%d", next)
		next++
		return s
	}

	if len(keyCols) == 1 {
		k := quote(keyCols[0])
		conds := make([]string, 0, 2)
		if lower != nil {
			conds = append(conds, fmt.Sprintf("%s > %s", k, ph(lower[0])))
		}
		if upper != nil {
			conds = append(conds, fmt.Sprintf("%s <= %s", k, ph(upper[0])))
		}
		return strings.Join(conds, " AND "), args
	}

	chain := func(vals []interface{}, isUpper bool) string {
		groups := make([]string, 0, len(keyCols))
		for last := 0; last < len(keyCols); last++ {
			eqs := make([]string, 0, last+1)
			for j := 0; j < last; j++ {
				eqs = append(eqs, fmt.Sprintf("%s = %s", quote(keyCols[j]), ph(vals[j])))
			}
			cmp := ">"
			if isUpper {
				cmp = "<"
				if last == len(keyCols)-1 {
					cmp = "<=" // inclusive tuple upper bound
				}
			}
			eqs = append(eqs, fmt.Sprintf("%s %s %s", quote(keyCols[last]), cmp, ph(vals[last])))
			g := strings.Join(eqs, " AND ")
			if last > 0 {
				g = "(" + g + ")"
			}
			groups = append(groups, g)
		}
		return strings.Join(groups, " OR ")
	}

	var parts []string
	if lower != nil {
		parts = append(parts, "("+chain(lower, false)+")")
	}
	if upper != nil {
		parts = append(parts, "("+chain(upper, true)+")")
	}
	return strings.Join(parts, " AND "), args
}

// chunkRangeDesc renders the human-readable range for disclosure:
// "(-inf, 5]", "(5, 10]", "(10, +inf]" (single-column keys print bare
// values; composite keys print the tuple joined with ", ").
func chunkRangeDesc(lower []interface{}, haveUpper bool, upper []interface{}) string {
	fmtVals := func(vals []interface{}) string {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%v", v)
		}
		return strings.Join(parts, ", ")
	}
	lo := "-inf"
	if lower != nil {
		lo = fmtVals(lower)
	}
	hi := "+inf"
	if haveUpper {
		hi = fmtVals(upper)
	}
	return "(" + lo + ", " + hi + "]"
}
