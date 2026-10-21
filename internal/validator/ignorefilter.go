package validator

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// ignore-column exclusion: an OPTIONAL compare dimension that removes
// columns from value-level comparison without touching the SQL templates.
// The engine builds skipCols from column types (float/JSON precision and
// format differences); ignore columns ride the same channel, so every
// existing skip path (sample hashing, no-PK hash_group/aggregate/bucket,
// checksum chunk hashing, diff-row rendering) picks them up automatically.

// ignoreSet returns the normalized ignore-column set: TrimSpace + ToLower,
// so " created_at " and "Created_At" resolve to the same entry. The engine
// matches column names case-insensitively elsewhere (colIdxMap), so the
// ignore list follows the same convention. Empty list → empty set (zero
// regression for CLI / migration pipeline paths).
func ignoreSet(compare config.CompareConfig) map[string]bool {
	set := make(map[string]bool)
	for _, c := range compare.IgnoreColumns {
		n := strings.ToLower(strings.TrimSpace(c))
		if n != "" {
			set[n] = true
		}
	}
	return set
}

// ignoreConfigured reports whether the compare config carries any (normalized)
// ignore columns — the guard for the empty-comparable-set skip.
func ignoreConfigured(compare config.CompareConfig) bool {
	return len(ignoreSet(compare)) > 0
}

// ignoreDisplayList returns the configured ignore list for the report note
// (TrimSpace'd, deduplicated case-insensitively, first-occurrence order kept).
// nil when nothing is configured — the note line is then omitted entirely.
func ignoreDisplayList(compare config.CompareConfig) []string {
	cfg := compare.IgnoreColumns
	if len(cfg) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(cfg))
	var out []string
	for _, c := range cfg {
		t := strings.TrimSpace(c)
		if t == "" {
			continue
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}

// applyIgnoreSkips marks skipCols[i]=true for every configured ignore column
// PRESENT in cols, honoring PK protection: a column that is one of the
// table's primary-key columns is NOT skipped (the PK is the alignment key —
// excluding it would break row matching) and lands in pkProtected instead.
// Both returned lists are sorted and lower-cased; applied lists only columns
// actually present in the table. nil/nil when nothing is configured.
func applyIgnoreSkips(compare config.CompareConfig, cols []*sql.ColumnType, pkCols []string, skipCols map[int]bool) (applied, pkProtected []string) {
	set := ignoreSet(compare)
	if len(set) == 0 {
		return nil, nil
	}
	pkSet := make(map[string]bool)
	for _, p := range pkCols {
		pkSet[strings.ToLower(strings.TrimSpace(p))] = true
	}
	for i, c := range cols {
		name := strings.ToLower(c.Name())
		if !set[name] {
			continue
		}
		if pkSet[name] {
			pkProtected = append(pkProtected, name)
			continue
		}
		skipCols[i] = true
		applied = append(applied, name)
	}
	sort.Strings(applied)
	sort.Strings(pkProtected)
	return applied, pkProtected
}

// allColumnsSkipped reports whether every column of cols is marked in
// skipCols — the table then has NO comparable columns left.
func allColumnsSkipped(cols []*sql.ColumnType, skipCols map[int]bool) bool {
	for i := range cols {
		if !skipCols[i] {
			return false
		}
	}
	return true
}

// ignoreColsNote renders the per-table ignore disclosure: the applied
// columns plus any PK-protected entries that were declared but neutralized.
// Returns "" when there is nothing to disclose (zero noise).
func ignoreColsNote(applied, pkProtected []string) string {
	var parts []string
	if len(applied) > 0 {
		parts = append(parts, "排除字段: "+strings.Join(applied, ", "))
	}
	if len(pkProtected) > 0 {
		parts = append(parts, "主键列不可排除: "+strings.Join(pkProtected, ", "))
	}
	return strings.Join(parts, "；")
}

// prependAppliedNote stamps an already-computed ignore disclosure onto a
// table report's Suggestion (after the mode's own suggestion text) when
// there is anything to disclose.
func prependAppliedNote(tr reporter.TableReport, applied, pkProtected []string) reporter.TableReport {
	note := ignoreColsNote(applied, pkProtected)
	if note == "" {
		return tr
	}
	if tr.Suggestion == "" {
		tr.Suggestion = note
	} else {
		tr.Suggestion = note + "；" + tr.Suggestion
	}
	return tr
}

// prependPKProtectedNote stamps ONLY the PK-protection disclosure (design
// item 4) — the applied-column list is already stamped globally on every
// report, so per-table functions add just the "主键列不可排除" line.
func prependPKProtectedNote(tr reporter.TableReport, pkProtected []string) reporter.TableReport {
	if len(pkProtected) == 0 {
		return tr
	}
	note := "主键列不可排除: " + strings.Join(pkProtected, ", ")
	if tr.Suggestion == "" {
		tr.Suggestion = note
	} else {
		tr.Suggestion = note + "；" + tr.Suggestion
	}
	return tr
}

// errNoComparableCols is the sentinel a checksum chunk function returns when
// the ignore configuration leaves the table with no comparable columns — the
// chunk would otherwise hash the empty column set and PASS vacuously (both
// sides hash to the same empty string). The caller turns the sentinel into a
// per-table StatusSkip with a disclosure note instead of a FAIL.
var errNoComparableCols = errors.New("no comparable columns after ignore exclusion")

// ErrNoComparableCols is the exported view for tests.
var ErrNoComparableCols = errNoComparableCols

// applyIgnoreNoPK applies the ignore exclusion to a no-PK table's skipCols
// (no PK to protect here) and reports whether the table must be skipped
// (every column excluded — empty comparable set). When it returns true the
// caller returns tr as-is (already StatusSkip with the disclosure note).
func applyIgnoreNoPK(v *Validator, skipCols map[int]bool, pgCols []*sql.ColumnType, tr *reporter.TableReport) bool {
	applied, pkProtected := applyIgnoreSkips(v.compareCfg(), pgCols, nil, skipCols)
	if ignoreConfigured(v.compareCfg()) && allColumnsSkipped(pgCols, skipCols) {
		tr.Status = reporter.StatusSkip
		tr.Error = "无可比列（排除字段后无剩余可比列）"
		*tr = prependAppliedNote(*tr, applied, pkProtected)
		return true
	}
	return false
}

// ignoreSetMap converts a lower-cased applied-name slice into a set map for
// the checksum chunk hashers (which keyed skip decisions on column name).
func ignoreSetMap(applied []string) map[string]bool {
	if len(applied) == 0 {
		return nil
	}
	m := make(map[string]bool, len(applied))
	for _, n := range applied {
		m[n] = true
	}
	return m
}

// tableIgnoreColumns resolves the configured ignore set against a table's PK
// columns: PK columns are removed from the applied set (PK protection) and
// returned sorted. Both lists are lower-cased.
func tableIgnoreColumns(compare config.CompareConfig, pkCols []string) (applied, pkProtected []string) {
	set := ignoreSet(compare)
	if len(set) == 0 {
		return nil, nil
	}
	pkSet := make(map[string]bool)
	for _, p := range pkCols {
		pkSet[strings.ToLower(strings.TrimSpace(p))] = true
	}
	for name := range set {
		if pkSet[name] {
			pkProtected = append(pkProtected, name)
		} else {
			applied = append(applied, name)
		}
	}
	sort.Strings(applied)
	sort.Strings(pkProtected)
	return applied, pkProtected
}
