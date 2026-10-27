// rowdiff_content.go implements the B-line content probe of the row-diff
// drill: after the merge alignment completes (bounded memory — the merge
// itself never holds full rows), the first rowDiffContentBudget diff keys
// in key order are re-fetched as whole rows via indexed point queries and
// rendered into compare/{id}/diff/{table}-{hash8}.content.json — one
// {key, category, src_row, dst_row} entry per probed key with a
// left/right value table the FE expands lazily.
//
// Discipline:
//   - Each point query is ONE #t4 budget unit (acquire → open/query/close
//     → release); no connection is held while waiting for a slot.
//   - The point queries carry the SAME watermark predicate as the drill
//     windows — the probe reads the same filtered universe.
//   - A per-entry failure (query error, row vanished mid-compare) becomes
//     an error field on that entry — the CSV detail, the counts and the
//     table verdict are untouched.
//   - The artifact is auxiliary: a marshal/write failure logs a warning
//     and never demotes the drill.
package validator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// rowDiffContentBudget is the per-table content-probe key budget (first N
// diff keys in key order). A var (not const) so tests can anchor the
// budget-stop semantics cheaply; never configurable outside the package.
var rowDiffContentBudget = 200

// rowDiffContentValueRunes caps one rendered cell value (characters); the
// "…" suffix marks the cut. Var for anchor injection only.
var rowDiffContentValueRunes = 256

// rowDiffContentRowBytes caps one rendered row (bytes over column names +
// values); cells past the cap are dropped and Truncated is set. Var for
// anchor injection only.
var rowDiffContentRowBytes = 8192

// rowDiffTruncMark is the cut marker appended to truncated values.
const rowDiffTruncMark = "…"

// rowDiffProbe is one captured diff key awaiting the post-merge point
// queries. srcRaw/dstRaw hold the row's sanitized native key values (nil
// for the side the key was absent from).
type rowDiffProbe struct {
	key    string // display form: normalized values joined with "|"
	cat    string
	srcRaw []interface{}
	dstRaw []interface{}
}

// rowDiffContentRow is one side's probed row: column names and rendered
// (display-normalized, per-value capped) values.
type rowDiffContentRow struct {
	Columns   []string `json:"columns"`
	Values    []string `json:"values"`
	Truncated bool     `json:"truncated,omitempty"`
}

// rowDiffContentEntry is one probed diff key. Exactly one of
// src_row/dst_row is set for *_only keys; both for content_diff. Error
// discloses a per-entry probe failure (the key stays listed).
type rowDiffContentEntry struct {
	Key      string             `json:"key"`
	Category string             `json:"category"`
	SrcRow   *rowDiffContentRow `json:"src_row,omitempty"`
	DstRow   *rowDiffContentRow `json:"dst_row,omitempty"`
	Error    string             `json:"error,omitempty"`
}

// writeRowDiffContent probes the captured keys and writes the content
// artifact. No-op when nothing was captured (zero diffs / aborted drill —
// the caller only reaches here after a committed CSV). Failure logs and
// returns: the drill summary is already final.
func (v *Validator) writeRowDiffContent(ctx context.Context, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table, file string, keyCols []string, probes []rowDiffProbe) {
	if len(probes) == 0 || file == "" {
		return
	}
	entries := make([]rowDiffContentEntry, 0, len(probes))
	for _, p := range probes {
		if err := ctx.Err(); err != nil {
			entries = append(entries, rowDiffContentEntry{Key: p.key, Category: p.cat, Error: fmt.Sprintf("未点查: %v", err)})
			continue
		}
		e := rowDiffContentEntry{Key: p.key, Category: p.cat}
		if p.srcRaw != nil {
			row, perr := v.probeRowDiffRow(ctx, false, pgDB, tidbDB, sem, schema, table, keyCols, p.srcRaw)
			if perr != nil {
				e.Error = perr.Error()
			} else {
				e.SrcRow = row
			}
		}
		if p.dstRaw != nil {
			row, perr := v.probeRowDiffRow(ctx, true, pgDB, tidbDB, sem, schema, table, keyCols, p.dstRaw)
			if perr != nil {
				if e.Error != "" {
					e.Error += "；"
				}
				e.Error += perr.Error()
			} else {
				e.DstRow = row
			}
		}
		entries = append(entries, e)
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		zap.L().Warn("row diff content marshal failed", zap.String("table", table), zap.Error(err))
		return
	}
	if err := os.MkdirAll(v.diffDetailDir, 0o755); err != nil {
		zap.L().Warn("row diff content dir failed", zap.String("table", table), zap.Error(err))
		return
	}
	// temp+rename — the CSV twin's discipline (red team ⑥, leader final
	// ruling): an interrupted write must never leave a half artifact under
	// the served name; the route's whitelist does not match the tmp name,
	// so a crashed probe is invisible to the FE, not half-readable.
	tmp, err := os.CreateTemp(v.diffDetailDir, ".rowdiff-*.content.json.tmp")
	if err != nil {
		zap.L().Warn("row diff content write failed", zap.String("table", table), zap.Error(err))
		return
	}
	tmpName := tmp.Name()
	if _, werr := tmp.Write(append(b, '\n')); werr != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		zap.L().Warn("row diff content write failed", zap.String("table", table), zap.Error(werr))
		return
	}
	// Close BEFORE rename: Windows refuses to rename an open file.
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpName)
		zap.L().Warn("row diff content write failed", zap.String("table", table), zap.Error(cerr))
		return
	}
	if rerr := os.Rename(tmpName, filepath.Join(v.diffDetailDir, file)); rerr != nil {
		_ = os.Remove(tmpName)
		zap.L().Warn("row diff content rename failed", zap.String("table", table), zap.Error(rerr))
	}
}

// errRowDiffGone marks a point query whose key no longer exists on that
// side (data moved between the merge and the probe) — disclosed as a
// per-entry error, never a drill failure.
var errRowDiffGone = fmt.Errorf("行已不存在（比对期间数据变动）")

// probeRowDiffRow runs ONE indexed point query for one side —
// SELECT * WHERE <key equality> AND <watermark> LIMIT 1 — as one #t4
// budget unit, and renders the row (bounded). target=true → TiDB via a
// dedicated UTC session conn, mirroring the drill windows.
func (v *Validator) probeRowDiffRow(ctx context.Context, target bool, pgDB, tidbDB *sql.DB, sem concSem,
	schema, table string, keyCols []string, keyVals []interface{}) (*rowDiffContentRow, error) {
	release, aerr := sem.acquire(ctx)
	if aerr != nil {
		return nil, aerr
	}
	defer release()

	var q dialectQueryer
	var quote func(string) string
	var wmFrag string
	var wmA []interface{}
	pgPlaceholders := false
	if target {
		conn, err := v.getTiDBConn(ctx, tidbDB)
		if err != nil {
			return nil, fmt.Errorf("点查失败: %v", err)
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
		_, pgPlaceholders = v.srcDialect.(postgresDialect)
	}

	qualified := quote(table)
	if !target {
		qualified = quote(schema) + "." + quote(table)
	}
	conds := make([]string, 0, len(keyCols)+1)
	if wmFrag != "" {
		conds = append(conds, wmFrag)
	}
	args := append([]interface{}{}, wmA...)
	for j, kc := range keyCols {
		ph := "?"
		if pgPlaceholders {
			ph = fmt.Sprintf("$%d", len(wmA)+j+1)
		}
		conds = append(conds, fmt.Sprintf("%s = %s", quote(kc), ph))
		args = append(args, keyVals[j])
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s WHERE %s LIMIT 1", qualified, strings.Join(conds, " AND ")), args...)
	if err != nil {
		return nil, fmt.Errorf("点查失败: %v", err)
	}
	defer rows.Close()

	cols, _ := rows.ColumnTypes()
	if cols == nil {
		return nil, fmt.Errorf("点查失败: no column types")
	}
	values := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("点查失败: %v", err)
		}
		return nil, errRowDiffGone
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("点查失败: %v", err)
	}
	return renderRowDiffContentRow(cols, values), nil
}

// renderRowDiffContentRow renders one probed row: display-normalized
// values, each capped at rowDiffContentValueRunes characters (cut marked
// with "…"), the row capped at rowDiffContentRowBytes bytes over names +
// values (Truncated marks the cut; the first cell is always included so
// an entry never renders empty).
func renderRowDiffContentRow(cols []*sql.ColumnType, values []interface{}) *rowDiffContentRow {
	out := &rowDiffContentRow{Columns: make([]string, 0, len(cols)), Values: make([]string, 0, len(cols))}
	used := 0
	for i, c := range cols {
		cell := displayRowValue(values[i])
		cell = truncateRunesMarked(cell, rowDiffContentValueRunes)
		cost := len(c.Name()) + len(cell) + 2
		if i > 0 && used+cost > rowDiffContentRowBytes {
			out.Truncated = true
			break
		}
		if i == 0 && cost > rowDiffContentRowBytes {
			out.Truncated = true
		}
		out.Columns = append(out.Columns, c.Name())
		out.Values = append(out.Values, cell)
		used += cost
	}
	return out
}

// displayRowValue renders one cell for the compare table: NULL stays the
// readable "NULL"; everything else goes through the shared normalizeValue
// (one normalization implementation — the hash family's precedent).
func displayRowValue(v interface{}) string {
	if v == nil {
		return "NULL"
	}
	return normalizeValue(v)
}

// truncateRunesMarked caps s at max runes, appending the cut mark when a
// cut happened.
func truncateRunesMarked(s string, max int) string {
	if max <= 0 {
		return rowDiffTruncMark
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + rowDiffTruncMark
}
