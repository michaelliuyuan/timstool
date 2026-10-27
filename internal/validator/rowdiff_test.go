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
// drill).
type sideScript struct {
	count         int64
	pkCols        []string
	uniqueIdxDef  string
	data          [][]driver.Value
	chunkOverride [][]driver.Value

	mu      sync.Mutex
	queries []string
}

var limitOffsetRe = regexp.MustCompile(`LIMIT (\d+) OFFSET (\d+)`)

func (s *sideScript) respond(query string) (driver.Rows, error) {
	s.mu.Lock()
	s.queries = append(s.queries, query)
	s.mu.Unlock()

	switch {
	case strings.Contains(query, "COUNT(*)"):
		return &scriptedRows{cols: []string{"count"}, colTypes: []string{"int8"}, rows: [][]driver.Value{{s.count}}}, nil
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
	case strings.Contains(query, "SELECT *"):
		m := limitOffsetRe.FindStringSubmatch(query)
		if m == nil {
			return nil, fmt.Errorf("unexpected SELECT * without LIMIT/OFFSET: %s", query)
		}
		var limit, offset int
		fmt.Sscanf(m[1], "%d", &limit)
		fmt.Sscanf(m[2], "%d", &offset)
		src := s.data
		if limit < rowDiffWindow && s.chunkOverride != nil {
			src = s.chunkOverride
		}
		// serve the windowed slice of the scripted dataset
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
	return nil, fmt.Errorf("unexpected query: %s", query)
}

type drillConn struct {
	script *sideScript
}

func (c *drillConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *drillConn) Close() error                        { return nil }
func (c *drillConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *drillConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return c.script.respond(q)
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
