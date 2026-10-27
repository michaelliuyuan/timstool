package validator

// rowdiff_test.go anchors the row-diff drill-down semantics on a scripted
// fake driver: three-class classification, count-mismatch trigger +
// DiffRows口径修正, zero-diff disclosure, watermark WHERE passthrough, CSV
// formula-injection defense, truncation cap, the no-PK disclosure, the
// sortedness guard, the NULL-key abort, and the DiffDetailDir-empty
// feature-off red line.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// --- scripted fake driver ---

type scriptedRows struct {
	cols     []string
	colTypes []string
	rows     [][]driver.Value
	pos      int
}

func (r *scriptedRows) Columns() []string { return r.cols }
func (r *scriptedRows) Close() error      { return nil }
func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}
func (r *scriptedRows) ColumnTypeDatabaseTypeName(idx int) string { return r.colTypes[idx] }

// sideScript scripts ONE database side: counts, PK metadata (source side
// only) and the row dataset served for SELECT * queries (chunk windows and
// drill windows alike — chunkOverride optionally poisons only the small
// chunk windows to simulate data moving between the chunk phase and the
// drill). Drill windows may arrive in either paging shape: the legacy
// LIMIT/OFFSET chunk form and the v2 keyset-seek form (WHERE (k > ? …)
// ORDER BY k LIMIT N), whose seek tuple is evaluated against the scripted
// dataset so multi-window drills page without skip/dup. LIMIT 1 point
// queries (content probe) serve the key-equal row.
type sideScript struct {
	count         int64
	pkCols        []string
	uniqueIdxDef  string
	data          [][]driver.Value
	chunkOverride [][]driver.Value
	// probeErrKeys optionally fails content-probe point queries for the
	// given key (rendered "v1|v2") — per-entry error anchoring.
	probeErrKeys map[string]error
	// probeData optionally serves point queries from a DIFFERENT dataset
	// (simulating rows that moved between the merge and the probe).
	probeData [][]driver.Value

	mu       sync.Mutex
	queries  []string
	argLog   [][]driver.Value
	probeLog []string
}

var limitOffsetRe = regexp.MustCompile(`LIMIT (\d+) OFFSET (\d+)`)
var limitOnlyRe = regexp.MustCompile(`LIMIT (\d+)`)
var orderByRe = regexp.MustCompile("ORDER BY ((?:\"[A-Za-z0-9_]+\"|`[A-Za-z0-9_]+`)(?:, (?:\"[A-Za-z0-9_]+\"|`[A-Za-z0-9_]+`))*)")

func (s *sideScript) respond(query string, args []driver.NamedValue) (driver.Rows, error) {
	s.mu.Lock()
	s.queries = append(s.queries, query)
	logged := make([]driver.Value, len(args))
	for i, a := range args {
		logged[i] = a.Value
	}
	s.argLog = append(s.argLog, logged)
	s.mu.Unlock()

	switch {
	case strings.Contains(query, "COUNT(*)"):
		return &scriptedRows{cols: []string{"count"}, colTypes: []string{"int8"}, rows: [][]driver.Value{{s.count}}}, nil
	case strings.Contains(query, "information_schema.statistics"):
		// MySQL-source key detection: PRIMARY index rows
		// (index_name, column_name, non_unique).
		rows := make([][]driver.Value, 0, len(s.pkCols))
		for _, c := range s.pkCols {
			rows = append(rows, []driver.Value{"PRIMARY", c, int64(0)})
		}
		return &scriptedRows{cols: []string{"index_name", "column_name", "non_unique"}, colTypes: []string{"text", "text", "int8"}, rows: rows}, nil
	case strings.Contains(query, "table_constraints"):
		rows := make([][]driver.Value, 0, len(s.pkCols))
		for _, c := range s.pkCols {
			rows = append(rows, []driver.Value{c})
		}
		return &scriptedRows{cols: []string{"column_name"}, colTypes: []string{"text"}, rows: rows}, nil
	case strings.Contains(query, "pg_indexes"):
		var idxRows [][]driver.Value
		if s.uniqueIdxDef != "" {
			idxRows = [][]driver.Value{{s.uniqueIdxDef}}
		}
		return &scriptedRows{cols: []string{"indexdef"}, colTypes: []string{"text"}, rows: idxRows}, nil
	case strings.Contains(query, "IS NULL"):
		return s.respondNullScan(query)
	case strings.Contains(query, "SELECT *"):
		// LIMIT 1 alone = content-probe point query; a chunk window can be
		// "LIMIT 1 OFFSET n" (last short chunk) and must take the OFFSET path.
		if strings.Contains(query, "LIMIT 1") && !strings.Contains(query, "OFFSET") {
			return s.respondPointQuery(query, args)
		}
		if m := limitOffsetRe.FindStringSubmatch(query); m != nil {
			return s.respondOffsetWindow(m, query)
		}
		return s.respondKeysetWindow(query, args)
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

// respondOffsetWindow serves the legacy chunk-window shape (LIMIT n
// OFFSET m over the scripted dataset, chunkOverride honored).
func (s *sideScript) respondOffsetWindow(m []string, query string) (driver.Rows, error) {
	var limit, offset int
	fmt.Sscanf(m[1], "%d", &limit)
	fmt.Sscanf(m[2], "%d", &offset)
	src := s.data
	if limit < 50000 && s.chunkOverride != nil {
		src = s.chunkOverride
	}
	all := make([][]driver.Value, len(src))
	copy(all, src)
	if offset > len(all) {
		offset = len(all)
	}
	win := all[offset:]
	if limit < len(win) {
		win = win[:limit]
	}
	return &scriptedRows{cols: []string{"id", "name"}, colTypes: []string{"int8", "text"}, rows: win}, nil
}

// isNullColRe captures the columns of a NULL pre-scan's IS NULL chain in
// order ("name" IS NULL OR `name` IS NULL → [name]).
var isNullColRe = regexp.MustCompile("(?:\"([A-Za-z0-9_]+)\"|`([A-Za-z0-9_]+)`) IS NULL")

// respondNullScan serves the pre-paging NULL pre-scan (SELECT 1 … IS NULL
// … LIMIT 1): a one-row result iff any scripted row holds NULL in any of
// the scanned key columns.
func (s *sideScript) respondNullScan(query string) (driver.Rows, error) {
	var cols []string
	for _, m := range isNullColRe.FindAllStringSubmatch(query, -1) {
		if m[1] != "" {
			cols = append(cols, m[1])
		} else {
			cols = append(cols, m[2])
		}
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("null pre-scan without columns: %s", query)
	}
	scriptedCols := []string{"id", "name"}
	for _, row := range s.data {
		for _, c := range cols {
			for ci, sc := range scriptedCols {
				if strings.EqualFold(sc, c) && row[ci] == nil {
					return &scriptedRows{cols: []string{"1"}, colTypes: []string{"int8"}, rows: [][]driver.Value{{int64(1)}}}, nil
				}
			}
		}
	}
	return &scriptedRows{cols: []string{"1"}, colTypes: []string{"int8"}, rows: nil}, nil
}

// respondKeysetWindow serves the v2 drill window shape: SELECT * [… WHERE
// (seek chain)] ORDER BY <keys> LIMIT N. The seek tuple is recovered from
// the bound args — PG form ($N, one number per key column, watermark
// placeholders first) or positional ? (values repeated per OR-chain
// occurrence: v1 | v1 v2 | v1 v2 v3 …) — and rows are filtered to those
// whose key tuple is strictly greater, preserving script order.
func (s *sideScript) respondKeysetWindow(query string, args []driver.NamedValue) (driver.Rows, error) {
	m := limitOnlyRe.FindStringSubmatch(query)
	if m == nil {
		return nil, fmt.Errorf("unexpected SELECT * without LIMIT: %s", query)
	}
	var limit int
	fmt.Sscanf(m[1], "%d", &limit)

	ob := orderByRe.FindStringSubmatch(query)
	if ob == nil {
		return nil, fmt.Errorf("drill window without ORDER BY: %s", query)
	}
	keyNames := parseQuotedIdentList(ob[1])
	cols := []string{"id", "name"}
	keyIdxs := make([]int, len(keyNames))
	for i, k := range keyNames {
		idx := -1
		for ci, c := range cols {
			if strings.EqualFold(c, k) {
				idx = ci
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("unknown ORDER BY column %q", k)
		}
		keyIdxs[i] = idx
	}

	var seekTuple []driver.Value
	if strings.Contains(query, " > ") {
		n := len(keyIdxs)
		pgForm := strings.Contains(query, "$")
		chainArgs := n * (n + 1) / 2
		if pgForm {
			chainArgs = n
		}
		w := len(args) - chainArgs
		if w < 0 {
			return nil, fmt.Errorf("seek window with too few args: %s %v", query, args)
		}
		seekTuple = make([]driver.Value, n)
		for j := 1; j <= n; j++ {
			idx := w + j - 1 // PG form: one arg per key column, in order
			if !pgForm {
				// positional form: v_j first occurs at the end of group j
				idx = w + j*(j-1)/2 + (j - 1)
			}
			seekTuple[j-1] = args[idx].Value
		}
	}

	win := make([][]driver.Value, 0, limit)
	for _, row := range s.data {
		if seekTuple != nil && !keyTupleGreater(row, keyIdxs, seekTuple) {
			continue
		}
		win = append(win, row)
		if len(win) >= limit {
			break
		}
	}
	return &scriptedRows{cols: cols, colTypes: []string{"int8", "text"}, rows: win}, nil
}

// eqColRe captures the column names of a point query's equality chain in
// order ("id" = $1 AND `name` = ? → [id name]).
var eqColRe = regexp.MustCompile("(?:\"([A-Za-z0-9_]+)\"|`([A-Za-z0-9_]+)`) = ")

// respondPointQuery serves a content-probe point query (LIMIT 1): the
// first scripted row whose key equals the equality-bound values (the
// trailing args, one per equality, in the same column order).
func (s *sideScript) respondPointQuery(query string, args []driver.NamedValue) (driver.Rows, error) {
	var keyNames []string
	for _, m := range eqColRe.FindAllStringSubmatch(query, -1) {
		if m[1] != "" {
			keyNames = append(keyNames, m[1])
		} else {
			keyNames = append(keyNames, m[2])
		}
	}
	nk := len(keyNames)
	if nk == 0 || nk > len(args) {
		return nil, fmt.Errorf("point query without key equalities: %s", query)
	}
	vals := make([]driver.Value, nk)
	for i := 0; i < nk; i++ {
		vals[i] = args[len(args)-nk+i].Value
	}
	key := probeKeyString(vals)
	s.mu.Lock()
	s.probeLog = append(s.probeLog, key)
	s.mu.Unlock()
	if s.probeErrKeys != nil {
		if err, ok := s.probeErrKeys[key]; ok {
			return nil, err
		}
	}
	cols := []string{"id", "name"}
	keyIdxs := make([]int, nk)
	for i, k := range keyNames {
		keyIdxs[i] = -1
		for ci, c := range cols {
			if strings.EqualFold(c, k) {
				keyIdxs[i] = ci
				break
			}
		}
		if keyIdxs[i] < 0 {
			return nil, fmt.Errorf("point query on unknown column %q", k)
		}
	}
	data := s.data
	if s.probeData != nil {
		data = s.probeData
	}
	for _, row := range data {
		match := true
		for i := range vals {
			if compareDriverVals(row[keyIdxs[i]], vals[i]) != 0 {
				match = false
				break
			}
		}
		if match {
			return &scriptedRows{cols: cols, colTypes: []string{"int8", "text"}, rows: [][]driver.Value{row}}, nil
		}
	}
	return &scriptedRows{cols: cols, colTypes: []string{"int8", "text"}, rows: nil}, nil
}

// parseQuotedIdentList splits "a", "b" / `a`, `b` into bare names.
func parseQuotedIdentList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, "\"`")
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// keyTupleGreater reports row[keyIdxs] > tuple lexicographically
// (strictly — the seek is exclusive).
func keyTupleGreater(row []driver.Value, keyIdxs []int, tuple []driver.Value) bool {
	for i := range keyIdxs {
		c := compareDriverVals(row[keyIdxs[i]], tuple[i])
		if c != 0 {
			return c > 0
		}
	}
	return false
}

// compareDriverVals orders two driver values: numbers numerically,
// strings/[]byte bytewise, bool false<true (mixed types fall back to their
// string render — same-side comparisons never mix).
func compareDriverVals(a, b driver.Value) int {
	as, aok := byteSliceString(a)
	bs, bok := byteSliceString(b)
	if aok && bok {
		switch {
		case as < bs:
			return -1
		case as > bs:
			return 1
		}
		return 0
	}
	ai, aInt := a.(int64)
	bi, bInt := b.(int64)
	if aInt && bInt {
		switch {
		case ai < bi:
			return -1
		case ai > bi:
			return 1
		}
		return 0
	}
	ab, aBool := a.(bool)
	bb, bBool := b.(bool)
	if aBool && bBool {
		switch {
		case !ab && bb:
			return -1
		case ab && !bb:
			return 1
		}
		return 0
	}
	x, y := fmt.Sprint(a), fmt.Sprint(b)
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// byteSliceString renders a string-ish value for bytewise comparison.
func byteSliceString(v driver.Value) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case []byte:
		return string(t), true
	}
	return "", false
}

// probeKeyString renders a point-query key for logs and error maps.
func probeKeyString(vals []driver.Value) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		if b, ok := v.([]byte); ok {
			parts[i] = string(b)
			continue
		}
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, "|")
}

type drillConn struct {
	script *sideScript
}

func (c *drillConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *drillConn) Close() error                        { return nil }
func (c *drillConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *drillConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.script.respond(q, args)
}
func (c *drillConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

type drillConnector struct {
	script *sideScript
}

func (d *drillConnector) Connect(context.Context) (driver.Conn, error) {
	return &drillConn{script: d.script}, nil
}
func (d *drillConnector) Driver() driver.Driver { return fakeDriver{} }

func (s *sideScript) open() *sql.DB { return sql.OpenDB(&drillConnector{script: s}) }

func (s *sideScript) sawQueries(substr string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, q := range s.queries {
		if strings.Contains(q, substr) {
			out = append(out, q)
		}
	}
	return out
}

// argsFor returns the bound-argument lists of every recorded query
// containing substr (index-aligned with sawQueries).
func (s *sideScript) argsFor(substr string) [][]driver.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]driver.Value
	for i, q := range s.queries {
		if strings.Contains(q, substr) {
			out = append(out, s.argLog[i])
		}
	}
	return out
}

// drillWindowQueries returns the side's KEYSET drill windows in order:
// SELECT * … ORDER BY … LIMIT N — no OFFSET (chunk windows), no LIMIT 1
// (content probes), no metadata probes.
func (s *sideScript) drillWindowQueries() []string {
	var out []string
	for _, q := range s.sawQueries("SELECT *") {
		if strings.Contains(q, "ORDER BY") && !strings.Contains(q, "OFFSET") && !strings.Contains(q, "LIMIT 1") {
			out = append(out, q)
		}
	}
	return out
}

// pointQueries returns the side's content-probe point queries in order
// (a chunk window can legitimately be LIMIT 1 OFFSET n — excluded).
func (s *sideScript) pointQueries() []string {
	var out []string
	for _, q := range s.sawQueries("SELECT *") {
		if strings.Contains(q, "LIMIT 1") && !strings.Contains(q, "OFFSET") {
			out = append(out, q)
		}
	}
	return out
}

// probesSeen returns the rendered keys of every recorded point query.
func (s *sideScript) probesSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.probeLog...)
}

// newDrillValidator assembles a PG-dialect validator with a small chunk size
// (chunk windows are LIMIT-2, drill windows LIMIT-50000 — distinguishable in
// the script) and the drill enabled at dir.
func newDrillValidator(dir string) *Validator {
	v := NewValidator(config.Config{
		Compare: config.CompareConfig{ChecksumChunkSize: 2},
	})
	v.diffDetailDir = dir
	return v
}

func rowsOf(pairs ...[2]any) [][]driver.Value {
	out := make([][]driver.Value, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, []driver.Value{p[0], p[1]})
	}
	return out
}

// Test ①: three-class classification + CSV artifact + DiffRows口径修正
// (chunk-mismatch face: DiffRows was 0 with equal counts).
func TestRowDiffThreeClassClassification(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	src := &sideScript{count: 5, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 5,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(5), "a"}, [2]any{int64(6), "a"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail {
		t.Fatalf("status = %s, want fail", tr.Status)
	}
	if tr.RowDiffs == nil {
		t.Fatal("RowDiffs must be set for a failed drilled table")
	}
	rd := tr.RowDiffs
	if rd.ContentDiff != 1 || rd.SrcOnly != 1 || rd.DstOnly != 1 {
		t.Fatalf("counts = content %d / src %d / dst %d, want 1/1/1", rd.ContentDiff, rd.SrcOnly, rd.DstOnly)
	}
	if tr.DiffRows != 3 {
		t.Fatalf("DiffRows = %d, want three-class total 3", tr.DiffRows)
	}
	if !regexp.MustCompile(`^t1-[0-9a-f]{8}\.csv$`).MatchString(rd.File) {
		t.Fatalf("File = %q, want t1-<md8>.csv", rd.File)
	}
	if len(rd.SampleKeys) != 3 || rd.SampleKeys[0] != "3" {
		t.Fatalf("sample keys = %v, want first key 3 with 3 samples", rd.SampleKeys)
	}
	b, err := os.ReadFile(filepath.Join(dir, rd.File))
	if err != nil {
		t.Fatalf("detail artifact missing: %v", err)
	}
	text := string(b)
	if !strings.HasPrefix(text, "\uFEFF") {
		t.Fatal("CSV must start with the UTF-8 BOM")
	}
	for _, want := range []string{"content_diff,3", "src_only,4", "dst_only,6"} {
		if !strings.Contains(text, want) {
			t.Fatalf("CSV detail missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(tr.Suggestion, "行级下钻: 内容不一致 1 · 仅源端 1 · 仅目标端 1") {
		t.Fatalf("suggestion must disclose the drill counts, got %q", tr.Suggestion)
	}
}

// Test ②: count-mismatch face triggers the drill and REPLACES the coarse
// src-dst delta with the three-class total.
func TestRowDiffCountMismatchDrillAndDiffRowsFix(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	src := &sideScript{count: 5, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 4,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(4), "a"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail || tr.RowDiffs == nil {
		t.Fatalf("status=%s rowDiffs=%v, want fail+drill", tr.Status, tr.RowDiffs)
	}
	// old口径 DiffRows was 5-4=1; the honest total is content(1)+src_only(1)=2
	if tr.DiffRows != 2 || tr.RowDiffs.ContentDiff != 1 || tr.RowDiffs.SrcOnly != 1 || tr.RowDiffs.DstOnly != 0 {
		t.Fatalf("DiffRows=%d counts=%+v, want 2 and 1/1/0", tr.DiffRows, tr.RowDiffs)
	}
}

// Test ③: chunk mismatch but the drill aligns zero diffs (data moved between
// the phases) — explicit note, no artifact, verdict stays FAIL.
func TestRowDiffZeroDiffDisclosure(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "b"}, [2]any{int64(3), "c"}, [2]any{int64(4), "d"}, [2]any{int64(5), "e"})
	src := &sideScript{count: 5, pkCols: []string{"id"}, data: same}
	dst := &sideScript{count: 5, data: same,
		// poison ONLY the chunk windows (LIMIT 2) — the drill windows see
		// clean data → mismatch above, zero aligned diffs below.
		chunkOverride: rowsOf([2]any{int64(1), "X"}, [2]any{int64(2), "b"}, [2]any{int64(3), "c"}, [2]any{int64(4), "d"}, [2]any{int64(5), "e"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail {
		t.Fatal("chunk mismatch verdict must stand (FAIL)")
	}
	rd := tr.RowDiffs
	if rd == nil || rd.Note == "" || !strings.Contains(rd.Note, "行级对齐未发现差异") {
		t.Fatalf("zero-diff disclosure missing: %+v", rd)
	}
	if rd.File != "" {
		t.Fatalf("zero diffs must leave no artifact, got %q", rd.File)
	}
	if csvs, _ := filepath.Glob(filepath.Join(dir, "*.csv")); len(csvs) != 0 {
		t.Fatalf("no detail CSV expected, got %v", csvs)
	}
	if tr.DiffRows != 0 {
		t.Fatalf("aborted-drill keeps the original DiffRows, got %d", tr.DiffRows)
	}
}

// Test ④: the watermark predicate rides the drill windows (WHERE + quoted
// column), scoping the aligned universe exactly like the chunk hashers.
func TestRowDiffWatermarkPredicatePassthrough(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)
	v.cfg.Compare.Watermark = &config.WatermarkFilter{Column: "wm", Value: "100"}

	src := &sideScript{count: 5, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 5,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(5), "a"}, [2]any{int64(6), "a"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.RowDiffs == nil || tr.RowDiffs.ContentDiff != 1 {
		t.Fatalf("drill must run under the watermark scope: %+v", tr.RowDiffs)
	}
	drillQ := src.sawQueries("LIMIT 50000")
	if len(drillQ) == 0 {
		drillQ = dst.sawQueries("LIMIT 50000")
	}
	if len(drillQ) == 0 {
		t.Fatal("no drill window queries recorded")
	}
	if !strings.Contains(drillQ[0], "WHERE") || !strings.Contains(drillQ[0], `"wm"`) {
		t.Fatalf("drill window must carry the watermark predicate, got %s", drillQ[0])
	}
}

// Test ⑤: spreadsheet formula injection defense — keys starting with
// = + - etc. get the ' prefix in the CSV detail.
func TestRowDiffCSVInjectionDefense(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	src := &sideScript{count: 2, pkCols: []string{"id"},
		data: rowsOf([2]any{"+ADD", "x"}, [2]any{"=SUM(1,2)", "x"})}
	dst := &sideScript{count: 2,
		data: rowsOf([2]any{"-SUB", "x"}, [2]any{"=SUM(1,2)", "y"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.RowDiffs == nil {
		t.Fatal("drill must run")
	}
	b, err := os.ReadFile(filepath.Join(dir, tr.RowDiffs.File))
	if err != nil {
		t.Fatalf("detail artifact missing: %v", err)
	}
	text := string(b)
	for _, want := range []string{"'+ADD", `'=SUM(1,2)`, `'-SUB`} {
		if !strings.Contains(text, want) {
			t.Fatalf("injection defense missing %q in:\n%s", want, text)
		}
	}
}

// Test ⑥: DiffDetailDir empty = feature fully off (orchestrator/CLI red
// line): no RowDiffs, no artifact, no directory side effects.
func TestRowDiffDisabledWithoutDir(t *testing.T) {
	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})

	src := &sideScript{count: 5, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 4,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(4), "a"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.RowDiffs != nil {
		t.Fatalf("feature off must leave RowDiffs nil, got %+v", tr.RowDiffs)
	}
	if tr.DiffRows != 1 {
		t.Fatalf("feature off keeps the legacy src-dst delta, got %d", tr.DiffRows)
	}
}

// Test ⑦: truncation — the detail stops at the (test-lowered) cap while the
// counts keep counting.
func TestRowDiffTruncationCap(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)
	oldCap := rowDiffDetailCap
	rowDiffDetailCap = 3
	defer func() { rowDiffDetailCap = oldCap }()

	src := &sideScript{count: 5, pkCols: []string{"id"},
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 0}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	rd := tr.RowDiffs
	if rd == nil || rd.SrcOnly != 5 {
		t.Fatalf("counts must stay exact, got %+v", rd)
	}
	if !rd.Truncated {
		t.Fatal("Truncated must be set when the detail hits the cap")
	}
	b, err := os.ReadFile(filepath.Join(dir, rd.File))
	if err != nil {
		t.Fatalf("detail artifact missing: %v", err)
	}
	if got := strings.Count(string(b), "src_only"); got != 3 {
		t.Fatalf("detail rows = %d, want capped at 3", got)
	}
	if !strings.Contains(tr.Suggestion, "明细截断") {
		t.Fatalf("suggestion must disclose truncation, got %q", tr.Suggestion)
	}
}

// Test ⑧: no PK/unique key → explicit cannot-align note, no drill, no file.
func TestRowDiffNoKeyDisclosure(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	src := &sideScript{count: 5, pkCols: nil,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "c"}, [2]any{int64(4), "a"}, [2]any{int64(5), "a"})}
	dst := &sideScript{count: 4,
		data: rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"}, [2]any{int64(3), "d"}, [2]any{int64(4), "a"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	rd := tr.RowDiffs
	if rd == nil || !strings.Contains(rd.Note, "无主键/唯一索引") {
		t.Fatalf("no-key disclosure missing: %+v", rd)
	}
	if rd.File != "" {
		t.Fatalf("no drill without a key, got file %q", rd.File)
	}
}

// Test ⑨: sortedness guard — a side whose stream decreases in canonical
// order aborts the drill with the explicit collation note (no fabricated
// only-rows).
func TestRowDiffSortednessGuard(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	src := &sideScript{count: 2, pkCols: []string{"id"},
		// deliberately out of canonical order (3 after 1… then 3 again
		// would be decrease at the second 1): serve 3 then 1.
		data: rowsOf([2]any{int64(3), "a"}, [2]any{int64(1), "a"})}
	dst := &sideScript{count: 1, data: rowsOf([2]any{int64(3), "b"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	rd := tr.RowDiffs
	if rd == nil || !strings.Contains(rd.Note, "键序") {
		t.Fatalf("sortedness guard must abort with the collation note: %+v", rd)
	}
	if rd.File != "" {
		t.Fatalf("aborted drill must leave no artifact, got %q", rd.File)
	}
}

// Test ⑩: NULL key values under a nullable unique index abort the drill —
// all NULL keys normalize to one tuple and pairing same-key rows would be
// stream-order luck (fake content_diff). Honest note, no artifact, the
// chunk verdict and DiffRows stand.
func TestRowDiffNullKeyAborts(t *testing.T) {
	dir := t.TempDir()
	v := newDrillValidator(dir)

	uk := "CREATE UNIQUE INDEX uk_name ON t1 (name)"
	src := &sideScript{count: 3, uniqueIdxDef: uk,
		data: rowsOf([2]any{int64(1), nil}, [2]any{int64(2), nil}, [2]any{int64(3), "x"})}
	dst := &sideScript{count: 3, uniqueIdxDef: uk,
		data: rowsOf([2]any{int64(1), nil}, [2]any{int64(2), "y"}, [2]any{int64(3), "x"})}

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail {
		t.Fatal("chunk mismatch verdict must stand (FAIL)")
	}
	rd := tr.RowDiffs
	if rd == nil || !strings.Contains(rd.Note, "NULL") {
		t.Fatalf("NULL-key disclosure missing: %+v", rd)
	}
	if rd.File != "" {
		t.Fatalf("aborted drill must leave no artifact, got %q", rd.File)
	}
	if csvs, _ := filepath.Glob(filepath.Join(dir, "*.csv")); len(csvs) != 0 {
		t.Fatalf("no detail CSV expected, got %v", csvs)
	}
	if tr.DiffRows != 0 {
		t.Fatalf("aborted drill keeps the original DiffRows, got %d", tr.DiffRows)
	}
}
