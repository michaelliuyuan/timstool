// rowdiff.go implements the row-level diff drill-down for FAILED checksum
// tables: when a table already failed validation (chunk hash mismatch or row
// count mismatch) and the caller provided a DiffDetailDir (standalone
// /compare tasks only — the orchestrator/CLI path leaves it empty and every
// function here is a no-op), both sides are re-scanned in key-ordered
// windows and merge-aligned by key tuple into three categories:
//
//	content_diff — same key on both sides, different row hash
//	src_only     — key present only in the source (within watermark scope)
//	dst_only     — key present only in the target
//
// The full diff is streamed to compare/{id}/diff/{table}-{hash8}.csv (capped
// at 1M detail rows; counts stay exact) and report.json carries only the
// bounded summary (reporter.RowDiffSummary).
//
// Design invariants:
//   - Equal tables/chunks never reach here: the drill runs only for tables
//     that already FAILED, so the equal-path cost stays byte-identical zero.
//   - The row hash column set is built by the SAME shared plan function as
//     the chunk hashers (buildChunkHashPlan + rowMD5) — two implementations
//     would drift and fabricate diffs.
//   - #t4 discipline: every window fetch is ONE budget unit (acquire slot →
//     open/query/close → release); no connection is ever held while waiting
//     for a slot.
//   - Windows page by KEYSET SEEK on each side independently: the next
//     window re-fetches with WHERE (<key tuple> > <last row's NATIVE key
//     values>) instead of OFFSET, so paging cost stays flat on deep tables
//     and the native-type seek order is strictly the ORDER BY order (int 9
//     < 10 never falls into the string-order trap). The merge aligns by
//     KEY, not by position, so insert/delete shifts cannot fabricate diffs
//     here.
//   - Sortedness guard: each side's stream must arrive non-decreasing in the
//     canonical (normalized tuple) order the merge compares by. A decrease
//     means the two sides' ORDER BY collations (or value normalization)
//     disagree with the canonical order — the drill aborts with an explicit
//     note instead of emitting fabricated only-rows.
//   - NULL keys abort: a nullable unique index may hold many NULL-keyed
//     rows that all normalize to the same key tuple; pairing them by
//     stream order would be luck, not alignment — the drill discloses
//     instead of guessing.
package validator

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"go.uber.org/zap"
)

// rowDiffWindow is the per-side fetch size (≤ 5 万行 windows, per the
// approved plan); memory stays bounded at two windows regardless of table
// size. A var (not const) so keyset-paging anchors can drive multi-window
// drills cheaply; never configurable outside the package.
var rowDiffWindow = 50000

// rowDiffDetailCap is the per-table CSV detail record cap. Beyond it the
// detail stops writing (Truncated=true) while the summary counts keep
// counting — the overview stays exact for arbitrarily diverged tables.
// A var (not const) so tests can anchor the truncation semantics without
// materializing a million fake rows; never configurable outside the
// package.
var rowDiffDetailCap int64 = 1000000

// rowDiffMegaRows is the mega-table guardrail: a FAILED table whose larger
// side row count reaches this bound gets its drill detail capped at
// rowDiffMegaDetailCap instead of rowDiffDetailCap — windowed keyset
// paging keeps the SCAN bounded, the guardrail keeps the CSV bounded on
// pathologically diverged giants. Row counts unknown (both zero) never
// trigger. A var for anchor injection only.
var rowDiffMegaRows int64 = 50000000

// rowDiffMegaDetailCap is the effective detail cap under the mega-table
// guardrail (rowDiffMegaRows). A var for anchor injection only.
var rowDiffMegaDetailCap int64 = 100000

// rowDiffSampleKeys is the sample-keys budget carried in the summary.
const rowDiffSampleKeys = 20

// rowDiffCategories are the CSV category labels (also the disclosure
// vocabulary in suggestions/reports).
const (
	rowDiffCatContent = "content_diff"
	rowDiffCatSrcOnly = "src_only"
	rowDiffCatDstOnly = "dst_only"
)

// rowDiffNullNote is the single NULL-key abort disclosure, shared by the
// pre-paging NULL pre-scan (unique-index keys) and the in-fetch NULL
// check (defense in depth — the two are a union, not a replacement).
const rowDiffNullNote = "键列含 NULL（可空唯一索引下同键多行无法确定配对），无法行级对齐"

// diffRow is one aligned (key, hash) observation from either side.
type diffRow struct {
	key []string // normalized key tuple, one element per key column
	// rawKey carries the row's NATIVE key values (driver types, []byte
	// sanitized to string) — the keyset seek binds these so the seek's
	// comparison order is the ORDER BY order, never the normalized-string
	// order; the content probe re-binds them for its point queries.
	rawKey []interface{}
	hash   string // rowMD5 over the shared hash plan
}

// maybeDrillRowDiffs is the post-fail hook: it decides whether a FAILED
// table gets a row-level drill (checksum mode + DiffDetailDir configured +
// a PK/unique alignment key exists) and stamps tr.RowDiffs with either the
// drill summary or an honest no-drill note. keyInfo may be nil (the count
// -mismatch fail site returns before key detection — the hook detects then);
// orderByCols/igEff may be empty when the caller has not resolved them.
func (v *Validator) maybeDrillRowDiffs(ctx context.Context, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table string, tr *reporter.TableReport, keyInfo *TableKeyInfo, igEff map[string]bool, orderByCols string) {
	if v.diffDetailDir == "" || tr == nil || tr.Status != reporter.StatusFail {
		return
	}

	// No key passed → detect (count-mismatch fail site). Detection errors
	// degrade to the no-key note — the table verdict (chunk/count) stands.
	// The catalog probe is one #t4 budget unit too (the count-mismatch
	// fail site reaches this hook with no unit held).
	if keyInfo == nil {
		if release, aerr := sem.acquire(ctx); aerr == nil {
			keyInfo, _ = v.srcDialect.DetectTableKey(ctx, pgDB, schema, table)
			release()
		}
	}

	var keyCols []string
	if keyInfo != nil && keyInfo.HasPK {
		keyCols = keyInfo.PKColumns
	} else if keyInfo != nil && keyInfo.HasUniqueIndex {
		keyCols = keyInfo.UniqueColumns
	}
	if len(keyCols) == 0 {
		tr.RowDiffs = &reporter.RowDiffSummary{
			Note: "该表无主键/唯一索引（hash_group 路径），无法行级定位差异",
		}
		return
	}
	if orderByCols == "" {
		orderByCols = strings.Join(keyCols, ", ")
	}
	if igEff == nil {
		igEff, _ = v.resolveIgnoreSet(keyInfo)
	}

	// 1b NULL pre-scan (adversarial-review finding, leader ruling 1):
	// keyset seek predicates are three-valued-logic UNKNOWN on NULL keys,
	// so a NULLS-LAST side's NULL-keyed rows would silently never surface
	// in any post-first window — the fetch-time NULL abort cannot fire.
	// Unique-index keys are pre-scanned (one #t4 unit per side, same
	// watermark predicate) and the drill aborts on the FIRST NULL hit;
	// PK keys are NOT NULL by PK semantics on both engines — skipped at
	// zero cost. The in-fetch NULL check stays as defense in depth
	// (union, not replacement).
	if keyInfo != nil && !keyInfo.HasPK {
		if v.rowDiffNullKeyScan(ctx, pgDB, tidbDB, sem, schema, table, keyCols) {
			tr.RowDiffs = &reporter.RowDiffSummary{
				KeyColumns: append([]string(nil), keyCols...),
				Note:       rowDiffNullNote,
			}
			return
		}
	}

	// Mega-table guardrail: the larger side's row count picks the
	// effective detail cap (unknown counts — both zero — never trigger).
	// Counts stay exact either way; only the CSV detail is bounded tighter.
	effCap := rowDiffDetailCap
	mega := tr.SourceRows >= rowDiffMegaRows || tr.TargetRows >= rowDiffMegaRows
	if mega {
		effCap = rowDiffMegaDetailCap
	}

	sum := v.drillRowDiffs(ctx, pgDB, tidbDB, sem, schema, table, orderByCols, keyCols, igEff, effCap)
	tr.RowDiffs = sum

	// DiffRows口径修正: a completed drill replaces the coarse delta (0 for
	// chunk-mismatch tables, src-dst for count-mismatch tables) with the
	// actual three-class total — the honest "不一致行数". A drill that
	// aborted (note carries the reason, no counts to trust) keeps the
	// original value.
	if sum != nil && sum.Note == "" {
		tr.DiffRows = sum.ContentDiff + sum.SrcOnly + sum.DstOnly
		if tr.Suggestion != "" {
			tr.Suggestion += "；"
		}
		tr.Suggestion += fmt.Sprintf("行级下钻: 内容不一致 %d · 仅源端 %d · 仅目标端 %d", sum.ContentDiff, sum.SrcOnly, sum.DstOnly)
		if sum.Truncated {
			if mega {
				tr.Suggestion += fmt.Sprintf("（超大表护栏：明细截断至 %d 万行，计数仍精确）", rowDiffMegaDetailCap/10000)
			} else {
				tr.Suggestion += fmt.Sprintf("（明细截断至 %d 万行，计数仍完整）", effCap/10000)
			}
		}
	}
}

// drillRowDiffs runs the windowed merge alignment and writes the CSV
// detail. It never fails the table harder: every error folds into
// RowDiffSummary.Note (the chunk/count verdict already stands). effCap is
// the guardrail-resolved CSV detail cap (rowDiffDetailCap, or
// rowDiffMegaDetailCap under the mega-table guardrail).
func (v *Validator) drillRowDiffs(ctx context.Context, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table, orderByCols string, keyCols []string, ig map[string]bool, effCap int64) *reporter.RowDiffSummary {
	sum := &reporter.RowDiffSummary{KeyColumns: append([]string(nil), keyCols...)}

	// fail folds every abort reason into the note AND leaves a server-side
	// trace (the report carries the reason; ops needs it in the logs too).
	fail := func(format string, args ...interface{}) *reporter.RowDiffSummary {
		sum.Note = fmt.Sprintf(format, args...)
		zap.L().Warn("row diff drill aborted", zap.String("table", table), zap.String("reason", sum.Note))
		return sum
	}

	src := newDiffStream()
	dst := newDiffStream()

	// Detail writer: created lazily on the first diff row (zero-diff tables
	// leave no artifact; the note explains).
	var detailFile *os.File
	var detailW *csv.Writer
	var detailRows int64
	defer func() {
		if detailW != nil {
			detailW.Flush()
		}
		if detailFile != nil {
			detailFile.Close()
			if sum.File == "" {
				// No committed file (zero diffs or aborted drill) — remove
				// the partial artifact rather than serve a stub.
				_ = os.Remove(detailFile.Name())
			}
		}
	}()

	beginDetail := func() error {
		if detailFile != nil {
			return nil
		}
		// Lazy dir creation: zero-diff and aborted drills leave NO
		// directory artifact at all.
		if err := os.MkdirAll(v.diffDetailDir, 0o755); err != nil {
			return fmt.Errorf("create diff dir: %w", err)
		}
		f, err := os.CreateTemp(v.diffDetailDir, ".rowdiff-*.csv.tmp")
		if err != nil {
			return err
		}
		detailFile = f
		// UTF-8 BOM so Excel opens the (Chinese-header-free but
		// user-key-bearing) file correctly; CRLF per the report family.
		if _, err := f.WriteString("\uFEFF"); err != nil {
			return err
		}
		detailW = csv.NewWriter(f)
		detailW.UseCRLF = true
		return detailW.Write([]string{"table", "key_columns", "category", "key"})
	}

	// Content probe capture: the first rowDiffContentBudget diff keys in
	// key order (the merge discovers diffs in key order) are re-fetched as
	// full rows AFTER the merge — rendering in-window was rejected because
	// holding full rows for an arbitrarily diverged table is unbounded
	// memory. Bounded at 200 × two point queries.
	var contentProbes []rowDiffProbe

	record := func(category string, key []string, srcRaw, dstRaw []interface{}) error {
		if len(contentProbes) < rowDiffContentBudget {
			contentProbes = append(contentProbes, rowDiffProbe{
				key: strings.Join(key, "|"), cat: category, srcRaw: srcRaw, dstRaw: dstRaw,
			})
		}
		if err := beginDetail(); err != nil {
			return err
		}
		if len(sum.SampleKeys) < rowDiffSampleKeys {
			sum.SampleKeys = append(sum.SampleKeys, strings.Join(key, "|"))
		}
		if detailRows >= effCap {
			sum.Truncated = true
			return nil
		}
		if err := detailW.Write([]string{
			rowDiffCSVCell(table), rowDiffCSVCell(strings.Join(keyCols, "|")),
			rowDiffCSVCell(category), rowDiffCSVCell(strings.Join(key, "|")),
		}); err != nil {
			return err
		}
		detailRows++
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return fail("行级下钻已取消: %v", err)
		}

		// Top up whichever side ran dry (a done side never refetches — its
		// short window was the last).
		if src.empty() && !src.done {
			if err := v.fetchDiffWindow(ctx, false, pgDB, tidbDB, sem, schema, table, orderByCols, ig, src); err != nil {
				if err == errDiffStreamDone {
					src.done = true
				} else if err == errNullKey {
					return fail("%s", rowDiffNullNote)
				} else {
					return fail("行级下钻失败: %v", err)
				}
			}
		}
		if dst.empty() && !dst.done {
			if err := v.fetchDiffWindow(ctx, true, pgDB, tidbDB, sem, schema, table, orderByCols, ig, dst); err != nil {
				if err == errDiffStreamDone {
					dst.done = true
				} else if err == errNullKey {
					return fail("%s", rowDiffNullNote)
				} else {
					return fail("行级下钻失败: %v", err)
				}
			}
		}

		// Both sides exhausted AND both buffers fully consumed — done.
		// (done alone is NOT enough: the final short window's rows are
		// still in the buffer waiting to be classified.)
		if src.done && src.empty() && dst.done && dst.empty() {
			break
		}
		// One side exhausted and consumed: everything left on the other
		// side is *_only (draining the buffer; the outer loop tops the
		// other side up if it is not done yet).
		if src.done && src.empty() {
			for !dst.empty() {
				r := dst.pop()
				sum.DstOnly++
				if err := record(rowDiffCatDstOnly, r.key, nil, r.rawKey); err != nil {
					return fail("行级下钻失败: 写差异明细: %v", err)
				}
			}
			continue
		}
		if dst.done && dst.empty() {
			for !src.empty() {
				r := src.pop()
				sum.SrcOnly++
				if err := record(rowDiffCatSrcOnly, r.key, r.rawKey, nil); err != nil {
					return fail("行级下钻失败: 写差异明细: %v", err)
				}
			}
			continue
		}

		c := compareKeyTuples(src.head().key, dst.head().key)
		switch {
		case c < 0:
			r := src.pop()
			sum.SrcOnly++
			if err := record(rowDiffCatSrcOnly, r.key, r.rawKey, nil); err != nil {
				return fail("行级下钻失败: 写差异明细: %v", err)
			}
		case c > 0:
			r := dst.pop()
			sum.DstOnly++
			if err := record(rowDiffCatDstOnly, r.key, nil, r.rawKey); err != nil {
				return fail("行级下钻失败: 写差异明细: %v", err)
			}
		default:
			s, d := src.pop(), dst.pop()
			if s.hash != d.hash {
				sum.ContentDiff++
				if err := record(rowDiffCatContent, s.key, s.rawKey, d.rawKey); err != nil {
					return fail("行级下钻失败: 写差异明细: %v", err)
				}
			}
		}
	}

	// Zero diffs after a chunk mismatch: the mismatch is real chunk
	// evidence (data moved during the compare, or the two sides' column
	// sets diverge) — disclose it, never pretend equality, and leave no
	// artifact.
	if sum.ContentDiff == 0 && sum.SrcOnly == 0 && sum.DstOnly == 0 {
		sum.Note = "块校验不一致但行级对齐未发现差异（比对期间数据在变动，或两侧列集不同）"
		return sum
	}

	// Commit the detail artifact under its whitelist-safe final name.
	if detailFile != nil {
		detailW.Flush()
		if err := detailW.Error(); err != nil {
			return fail("行级下钻失败: 写差异明细: %v", err)
		}
		final := rowDiffFileName(table)
		// Close BEFORE rename: Windows refuses to rename an open file.
		// detailFile stays non-nil on failure so the deferred cleanup
		// removes the temp artifact (sum.File still empty).
		if err := detailFile.Close(); err != nil {
			return fail("行级下钻失败: 落盘差异文件: %v", err)
		}
		if err := os.Rename(detailFile.Name(), filepath.Join(v.diffDetailDir, final)); err != nil {
			return fail("行级下钻失败: 落盘差异文件: %v", err)
		}
		detailFile = nil // committed; the deferred cleanup must not touch it
		sum.File = final

		// B-line content probe: point-query the captured diff keys and
		// drop {stem}.content.json next to the CSV. Auxiliary artifact —
		// a probe/write failure never demotes the drill (per-entry error
		// fields / a logged warning instead).
		v.writeRowDiffContent(ctx, pgDB, tidbDB, sem, schema, table, strings.TrimSuffix(final, ".csv")+".content.json", keyCols, contentProbes)
	}
	zap.L().Info("row diff drill complete",
		zap.String("table", table),
		zap.Int64("content_diff", sum.ContentDiff),
		zap.Int64("src_only", sum.SrcOnly),
		zap.Int64("dst_only", sum.DstOnly),
		zap.Bool("truncated", sum.Truncated))
	return sum
}

// diffStream is the bounded per-side buffer: at most one fetched window at a
// time, consumed by the merge loop. lastRawKey is the previous window's last
// yielded row's NATIVE key values — the keyset seek position (nil/empty =
// first window, no seek predicate); like hasLast/lastKey it survives the
// rows-reset in pop() so the guard and the seek both carry across windows.
type diffStream struct {
	rows []diffRow
	pos  int
	done bool // side exhausted (window fetched fewer than the window size)
	// hasLast/lastKey survive the rows-reset in pop() so the guard also
	// covers the first row of every new window (cross-window decrease).
	hasLast    bool
	lastKey    []string
	lastRawKey []interface{}
}

func newDiffStream() *diffStream { return &diffStream{} }

func (s *diffStream) empty() bool { return s.pos >= len(s.rows) }

func (s *diffStream) head() diffRow { return s.rows[s.pos] }

func (s *diffStream) pop() diffRow {
	r := s.rows[s.pos]
	s.pos++
	if s.pos >= len(s.rows) {
		s.rows, s.pos = nil, 0
	}
	return r
}

// push appends one scanned row and enforces the sortedness guard: the
// canonical order the merge compares by must never see a strict decrease,
// else the side's ORDER BY collation diverges from the canonical order and
// the merge would fabricate only-rows. rawKey becomes the side's keyset
// seek position (the last push wins — windows are consumed in order).
func (s *diffStream) push(r diffRow) bool {
	if s.hasLast && compareKeyTuples(s.lastKey, r.key) > 0 {
		return false
	}
	s.rows = append(s.rows, r)
	s.hasLast, s.lastKey, s.lastRawKey = true, r.key, r.rawKey
	return true
}

// errDiffStreamDone signals a side is exhausted (window returned fewer rows
// than requested).
var errDiffStreamDone = fmt.Errorf("diff stream exhausted")

// errNullKey aborts the drill when a KEY column value is NULL: a nullable
// unique index may hold many NULL-keyed rows, all normalizing to the same
// key tuple — pairing them by stream order would fabricate content_diff
// rows (adversarial review finding). Honest abort beats a lucky pairing.
var errNullKey = fmt.Errorf("row-diff key column contains NULL")

// fetchDiffWindow pulls the next window from one side (target=true → TiDB
// via a dedicated UTC session conn) as ONE #t4 budget unit, appending to the
// stream. Paging is KEYSET SEEK: every window after the first re-fetches
// with WHERE (<key tuple> strictly greater than the previous window's last
// row's native key values) ORDER BY <keys> LIMIT N — flat cost on deep
// tables, and the seek's comparison order IS the ORDER BY order because the
// bound values keep their native driver types (int64 10 never compares as
// the string "10"). The watermark predicate rides every query exactly like
// the chunk hashers' (the drill compares the same filtered universe); seek
// placeholders number AFTER the watermark's ($1.. on PG, ? positional on
// MySQL/TiDB).
func (v *Validator) fetchDiffWindow(ctx context.Context, target bool, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table, orderByCols string, ig map[string]bool, stream *diffStream) error {
	release, aerr := sem.acquire(ctx)
	if aerr != nil {
		return aerr
	}
	defer release()

	// Side-resolved SQL pieces: table qualification, watermark fragment,
	// identifier quoting and the placeholder family ($N on a PG source,
	// ? on MySQL sources and the TiDB target).
	var query func(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	var quote func(string) string
	var wmFrag string
	var wmA []interface{}
	pgPlaceholders := false
	if target {
		conn, err := v.getTiDBConn(ctx, tidbDB)
		if err != nil {
			return fmt.Errorf("get TiDB conn for row-diff window: %w", err)
		}
		defer conn.Close()
		query = func(ctx context.Context, q string, args ...interface{}) (*sql.Rows, error) {
			return conn.QueryContext(ctx, q, args...)
		}
		quote = v.tgtDialect.QuoteIdent
		if wm := v.wmFilterFor(ctx); wm != nil {
			wmFrag = v.tgtDialect.WmPredicateFragment(wm)
			wmA = wmArgs(wm)
		}
	} else {
		query = func(ctx context.Context, q string, args ...interface{}) (*sql.Rows, error) {
			return pgDB.QueryContext(ctx, q, args...)
		}
		quote = v.srcDialect.QuoteIdent
		if wm := v.wmFilterFor(ctx); wm != nil {
			wmFrag = v.srcDialect.WmPredicateFragment(wm)
			wmA = wmArgs(wm)
		}
		_, pgPlaceholders = v.srcDialect.(postgresDialect)
	}

	// Keyset seek: exclusive strict-greater chain over the previous
	// window's last native key values.
	qualified := quote(table)
	if !target {
		qualified = quote(schema) + "." + quote(table)
	}
	keyNames := splitKeyColumns(orderByCols)
	quotedKeys := make([]string, len(keyNames))
	for i, k := range keyNames {
		quotedKeys[i] = quote(k)
	}
	args := append([]interface{}{}, wmA...)
	var where string
	conds := make([]string, 0, 2)
	if wmFrag != "" {
		conds = append(conds, wmFrag)
	}
	if len(stream.lastRawKey) > 0 {
		seek := newRowDiffSeek(pgPlaceholders, len(wmA))
		conds = append(conds, "("+seek.chain(quotedKeys, stream.lastRawKey)+")")
		args = append(args, seek.args...)
	}
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := query(ctx, fmt.Sprintf(
		"SELECT * FROM %s%s ORDER BY %s LIMIT %d",
		qualified, where, strings.Join(quotedKeys, ", "), rowDiffWindow), args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols, _ := rows.ColumnTypes()
	if cols == nil {
		return fmt.Errorf("no column types")
	}
	hashIdxs, err := buildChunkHashPlan(cols, ig)
	if err != nil {
		return err
	}
	// Key column indices: the alignment key is read from the row itself,
	// independent of the hash plan (a unique-index column may sit in the
	// ignore set — its VALUE is excluded from hashing but the key still
	// aligns rows, mirroring the chunk hasher's ordering semantics).
	lowerName := make(map[string]int, len(cols))
	for i, c := range cols {
		lowerName[strings.ToLower(c.Name())] = i
	}
	keyIdxs := make([]int, 0, len(keyNames))
	for _, kc := range keyNames {
		idx, ok := lowerName[strings.ToLower(kc)]
		if !ok {
			return fmt.Errorf("key column %q missing on %s side", kc, v.sideLabel(target))
		}
		keyIdxs = append(keyIdxs, idx)
	}

	values := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}

	fetched := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("scan row in row-diff window: %w", err)
		}
		key := make([]string, len(keyIdxs))
		rawKey := make([]interface{}, len(keyIdxs))
		for i, idx := range keyIdxs {
			if values[idx] == nil {
				return errNullKey
			}
			key[i] = normalizeValue(values[idx])
			rawKey[i] = sanitizeNativeValue(values[idx])
		}
		if !stream.push(diffRow{key: key, rawKey: rawKey, hash: rowMD5(values, hashIdxs)}) {
			return fmt.Errorf("键序与对齐序不一致（%s 端排序规则或键值规范化差异），无法行级对齐", v.sideLabel(target))
		}
		fetched++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate row-diff rows: %w", err)
	}
	if fetched < rowDiffWindow {
		return errDiffStreamDone
	}
	return nil
}

// splitKeyColumns splits the shared orderByCols key list (comma-separated)
// into trimmed raw column names.
func splitKeyColumns(orderByCols string) []string {
	parts := strings.Split(orderByCols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sanitizeNativeValue makes one native key value safe to hold across rows
// and re-bind into later queries: the driver's []byte buffers are only
// valid until the next Next call, so they are copied to string (every
// other driver.Value — int64, float64, bool, string, time.Time — is a
// value type already).
func sanitizeNativeValue(v interface{}) interface{} {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// rowDiffNullKeyScan pre-scans BOTH sides for NULL-keyed rows in the
// watermark universe BEFORE any window is fetched (leader ruling 1): the
// keyset seek is three-valued-logic UNKNOWN on NULL, so a NULLS-LAST side
// would never surface its NULL rows in post-first windows and the
// in-fetch NULL abort could not fire — a silent undercount. Each side's
// probe is ONE #t4 budget unit (acquire → open/query/close → release)
// carrying the SAME watermark predicate as the drill windows. A probe
// ERROR does not abort the drill (the in-fetch NULL check remains as
// defense in depth); only a NULL HIT does. Only the unique-index key
// path calls this — PK keys are NOT NULL by PK semantics on both engines.
func (v *Validator) rowDiffNullKeyScan(ctx context.Context, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table string, keyCols []string) bool {
	for _, target := range []bool{false, true} {
		if v.rowDiffNullKeyScanSide(ctx, target, pgDB, tidbDB, sem, schema, table, keyCols) {
			return true
		}
	}
	return false
}

// rowDiffNullKeyScanSide probes one side: SELECT 1 … WHERE <wm> AND
// (k1 IS NULL OR … ) LIMIT 1 — true iff a NULL-keyed row exists in scope.
func (v *Validator) rowDiffNullKeyScanSide(ctx context.Context, target bool, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table string, keyCols []string) bool {
	release, aerr := sem.acquire(ctx)
	if aerr != nil {
		return false // cancelled: the fetch-time NULL check still stands
	}
	defer release()

	var q dialectQueryer
	var quote func(string) string
	var wmFrag string
	var wmA []interface{}
	if target {
		conn, err := v.getTiDBConn(ctx, tidbDB)
		if err != nil {
			zap.L().Warn("row diff NULL pre-scan skipped (conn)", zap.String("table", table), zap.Error(err))
			return false
		}
		defer conn.Close()
		q, quote = conn, v.tgtDialect.QuoteIdent
		if wm := v.wmFilterFor(ctx); wm != nil {
			wmFrag, wmA = v.tgtDialect.WmPredicateFragment(wm), wmArgs(wm)
		}
	} else {
		q, quote = pgDB, v.srcDialect.QuoteIdent
		if wm := v.wmFilterFor(ctx); wm != nil {
			wmFrag, wmA = v.srcDialect.WmPredicateFragment(wm), wmArgs(wm)
		}
	}
	qualified := quote(table)
	if !target {
		qualified = quote(schema) + "." + quote(table)
	}
	nullConds := make([]string, len(keyCols))
	for i, kc := range keyCols {
		nullConds[i] = quote(kc) + " IS NULL"
	}
	conds := make([]string, 0, 2)
	if wmFrag != "" {
		conds = append(conds, wmFrag)
	}
	conds = append(conds, "("+strings.Join(nullConds, " OR ")+")")
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		"SELECT 1 FROM %s WHERE %s LIMIT 1", qualified, strings.Join(conds, " AND ")), wmA...)
	if err != nil {
		zap.L().Warn("row diff NULL pre-scan skipped (query)", zap.String("table", table), zap.Error(err))
		return false
	}
	defer rows.Close()
	if rows.Next() {
		return true
	}
	return false
}

// rowDiffSeek renders the exclusive keyset-seek predicate chain:
//
//	k1 > $a OR (k1 = $a AND k2 > $b) OR (k1 = $a AND k2 = $b AND k3 > $c)
//
// The row-constructor form ((k1,k2) > (a,b)) is FORBIDDEN — MySQL's
// historical evaluation of tuple comparisons is unreliable — so the OR
// chain is spelled out. Placeholders: PG sources number $N CONTINUOUSLY
// from the watermark's last placeholder and REUSE one number per key
// column (every occurrence of key j binds the same value); MySQL/TiDB use
// positional ? per occurrence with the value repeated in args. Both forms
// bind the NATIVE driver values — never normalized strings.
type rowDiffSeek struct {
	pg     bool
	next   int
	tokens []string // per-key-column memoized $N (PG form only)
	args   []interface{}
}

func newRowDiffSeek(pgPlaceholders bool, wmArgCount int) *rowDiffSeek {
	return &rowDiffSeek{pg: pgPlaceholders, next: wmArgCount, tokens: make([]string, 8)}
}

// placeholder returns the bind token for key column i's value, allocating
// (and binding) at first use.
func (s *rowDiffSeek) placeholder(i int, val interface{}) string {
	if !s.pg {
		s.args = append(s.args, val)
		return "?"
	}
	if i >= len(s.tokens) {
		grow := make([]string, i+1)
		copy(grow, s.tokens)
		s.tokens = grow
	}
	if s.tokens[i] == "" {
		s.next++
		s.tokens[i] = fmt.Sprintf("$%d", s.next)
		s.args = append(s.args, val)
	}
	return s.tokens[i]
}

// chain renders the seek predicate (without the wrapping parens the caller
// adds) over the quoted key columns and the previous window's last native
// key values: k1 > $a OR (k1 = $a AND k2 > $b) OR … — each multi-equality
// group parenthesized, matching the approved OR-chain shape.
func (s *rowDiffSeek) chain(quotedKeys []string, vals []interface{}) string {
	groups := make([]string, 0, len(quotedKeys))
	for last := 0; last < len(quotedKeys); last++ {
		conds := make([]string, 0, last+1)
		for j := 0; j < last; j++ {
			conds = append(conds, fmt.Sprintf("%s = %s", quotedKeys[j], s.placeholder(j, vals[j])))
		}
		conds = append(conds, fmt.Sprintf("%s > %s", quotedKeys[last], s.placeholder(last, vals[last])))
		group := strings.Join(conds, " AND ")
		if last > 0 {
			group = "(" + group + ")"
		}
		groups = append(groups, group)
	}
	return strings.Join(groups, " OR ")
}

// sideLabel renders the fetch-side label for drill error messages.
func (v *Validator) sideLabel(target bool) string {
	if target {
		return "TiDB"
	}
	return v.srcLabel()
}

// compareKeyTuples compares two normalized key tuples element-wise bytewise
// (shorter tuple sorts first on full-prefix equality — cannot happen for a
// fixed key column list, kept for total order).
func compareKeyTuples(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// rowDiffSanitizeFileName reduces a table name to the rowdiff route's
// whitelist alphabet ([A-Za-z0-9._-]); the md8 suffix keeps two tables that
// sanitize to the same string distinct.
func rowDiffFileName(table string) string {
	var b strings.Builder
	for _, r := range table {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	sum := md5.Sum([]byte(table))
	return fmt.Sprintf("%s-%x.csv", b.String(), sum[:4])
}

// rowDiffCSVCell defuses spreadsheet formula injection (the mongo rowdiff
// family precedent): a cell starting with =, +, -, tab, CR or @ gets a '
// prefix BEFORE any quoting — csv.Writer still wraps cells that need it,
// and the prefix survives inside the quotes.
func rowDiffCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '\t', '\r', '@':
		return "'" + s
	}
	return s
}
