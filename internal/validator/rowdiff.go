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
//   - Windows page by OFFSET on each side independently — the merge aligns
//     by KEY, not by position, so insert/delete shifts that misalign chunk
//     offsets cannot fabricate diffs here.
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
// size.
const rowDiffWindow = 50000

// rowDiffDetailCap is the per-table CSV detail record cap. Beyond it the
// detail stops writing (Truncated=true) while the summary counts keep
// counting — the overview stays exact for arbitrarily diverged tables.
// A var (not const) so tests can anchor the truncation semantics without
// materializing a million fake rows; never configurable outside the
// package.
var rowDiffDetailCap int64 = 1000000

// rowDiffSampleKeys is the sample-keys budget carried in the summary.
const rowDiffSampleKeys = 20

// rowDiffCategories are the CSV category labels (also the disclosure
// vocabulary in suggestions/reports).
const (
	rowDiffCatContent = "content_diff"
	rowDiffCatSrcOnly = "src_only"
	rowDiffCatDstOnly = "dst_only"
)

// diffRow is one aligned (key, hash) observation from either side.
type diffRow struct {
	key  []string // normalized key tuple, one element per key column
	hash string   // rowMD5 over the shared hash plan
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

	sum := v.drillRowDiffs(ctx, pgDB, tidbDB, sem, schema, table, orderByCols, keyCols, igEff)
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
			tr.Suggestion += fmt.Sprintf("（明细截断至 %d 万行，计数仍完整）", rowDiffDetailCap/10000)
		}
	}
}

// drillRowDiffs runs the windowed merge alignment and writes the CSV
// detail. It never fails the table harder: every error folds into
// RowDiffSummary.Note (the chunk/count verdict already stands).
func (v *Validator) drillRowDiffs(ctx context.Context, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table, orderByCols string, keyCols []string, ig map[string]bool) *reporter.RowDiffSummary {
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

	record := func(category string, key []string) error {
		if err := beginDetail(); err != nil {
			return err
		}
		if len(sum.SampleKeys) < rowDiffSampleKeys {
			sum.SampleKeys = append(sum.SampleKeys, strings.Join(key, "|"))
		}
		if detailRows >= rowDiffDetailCap {
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
					return fail("键列含 NULL（可空唯一索引下同键多行无法确定配对），无法行级对齐")
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
					return fail("键列含 NULL（可空唯一索引下同键多行无法确定配对），无法行级对齐")
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
				if err := record(rowDiffCatDstOnly, r.key); err != nil {
					return fail("行级下钻失败: 写差异明细: %v", err)
				}
			}
			continue
		}
		if dst.done && dst.empty() {
			for !src.empty() {
				r := src.pop()
				sum.SrcOnly++
				if err := record(rowDiffCatSrcOnly, r.key); err != nil {
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
			if err := record(rowDiffCatSrcOnly, r.key); err != nil {
				return fail("行级下钻失败: 写差异明细: %v", err)
			}
		case c > 0:
			r := dst.pop()
			sum.DstOnly++
			if err := record(rowDiffCatDstOnly, r.key); err != nil {
				return fail("行级下钻失败: 写差异明细: %v", err)
			}
		default:
			s, d := src.pop(), dst.pop()
			if s.hash != d.hash {
				sum.ContentDiff++
				if err := record(rowDiffCatContent, s.key); err != nil {
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
// time, consumed by the merge loop. fetched drives the per-side OFFSET
// paging; lastKey carries the sortedness guard across window resets.
type diffStream struct {
	rows    []diffRow
	pos     int
	fetched int64
	done    bool // side exhausted (window fetched fewer than the window size)
	// hasLast/lastKey survive the rows-reset in pop() so the guard also
	// covers the first row of every new window (cross-window decrease).
	hasLast bool
	lastKey []string
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
// the merge would fabricate only-rows.
func (s *diffStream) push(r diffRow) bool {
	if s.hasLast && compareKeyTuples(s.lastKey, r.key) > 0 {
		return false
	}
	s.rows = append(s.rows, r)
	s.hasLast, s.lastKey = true, r.key
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
// stream. The watermark predicate rides every query exactly like the chunk
// hashers' (the drill compares the same filtered universe).
func (v *Validator) fetchDiffWindow(ctx context.Context, target bool, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table, orderByCols string, ig map[string]bool, stream *diffStream) error {
	release, aerr := sem.acquire(ctx)
	if aerr != nil {
		return aerr
	}
	defer release()

	var query func(ctx context.Context, limit, offset int64) (*sql.Rows, error)
	if target {
		conn, err := v.getTiDBConn(ctx, tidbDB)
		if err != nil {
			return fmt.Errorf("get TiDB conn for row-diff window: %w", err)
		}
		defer conn.Close()
		query = func(ctx context.Context, limit, offset int64) (*sql.Rows, error) {
			if wm := v.wmFilterFor(ctx); wm != nil {
				return conn.QueryContext(ctx, fmt.Sprintf(
					"SELECT * FROM %s WHERE %s ORDER BY %s LIMIT %d OFFSET %d",
					v.tgtDialect.QuoteIdent(table), v.tgtDialect.WmPredicateFragment(wm),
					quoteOrderByCols(orderByCols, v.tgtDialect.QuoteIdent), limit, offset), wmArgs(wm)...)
			}
			return conn.QueryContext(ctx, fmt.Sprintf(
				"SELECT * FROM %s ORDER BY %s LIMIT %d OFFSET %d",
				v.tgtDialect.QuoteIdent(table), quoteOrderByCols(orderByCols, v.tgtDialect.QuoteIdent), limit, offset))
		}
	} else {
		query = func(ctx context.Context, limit, offset int64) (*sql.Rows, error) {
			if wm := v.wmFilterFor(ctx); wm != nil {
				return pgDB.QueryContext(ctx, fmt.Sprintf(
					"SELECT * FROM %s.%s WHERE %s ORDER BY %s LIMIT %d OFFSET %d",
					v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table), v.srcDialect.WmPredicateFragment(wm),
					quoteOrderByCols(orderByCols, v.srcDialect.QuoteIdent), limit, offset), wmArgs(wm)...)
			}
			return pgDB.QueryContext(ctx, fmt.Sprintf(
				"SELECT * FROM %s.%s ORDER BY %s LIMIT %d OFFSET %d",
				v.srcDialect.QuoteIdent(schema), v.srcDialect.QuoteIdent(table),
				quoteOrderByCols(orderByCols, v.srcDialect.QuoteIdent), limit, offset))
		}
	}

	rows, err := query(ctx, rowDiffWindow, stream.offset())
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
	keyIdxs := make([]int, 0, len(strings.Split(orderByCols, ",")))
	for _, kc := range strings.Split(orderByCols, ",") {
		idx, ok := lowerName[strings.ToLower(strings.TrimSpace(kc))]
		if !ok {
			return fmt.Errorf("key column %q missing on %s side", strings.TrimSpace(kc), v.sideLabel(target))
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
		for i, idx := range keyIdxs {
			if values[idx] == nil {
				return errNullKey
			}
			key[i] = normalizeValue(values[idx])
		}
		if !stream.push(diffRow{key: key, hash: rowMD5(values, hashIdxs)}) {
			return fmt.Errorf("键序与对齐序不一致（%s 端排序规则或键值规范化差异），无法行级对齐", v.sideLabel(target))
		}
		fetched++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate row-diff rows: %w", err)
	}
	stream.fetched += int64(fetched)
	if fetched < rowDiffWindow {
		return errDiffStreamDone
	}
	return nil
}

// offset/advance track per-side OFFSET paging (each side advances by exactly
// the rows it yielded).
func (s *diffStream) offset() int64 { return s.fetched }

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
