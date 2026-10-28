package webapi

// MC-16 data-compare report tests: the /compare/tasks/{id}/report family
// faces — HTML rendering anchors (banner/meta grid/两卡/# 列/中文徽章/页脚),
// the unit-11 表详情 face (8-column header, failure-first sort with
// original #, thousand separators, count pills, details/summary expand
// rows + print force-open, suggestion brief cut, G summary mapping with
// verbatim passthrough, V3.19.36 badge), verbatim JSON serving,
// default-format html, the 404 dual cause set, the corrupt-artifact 500
// (generic body, no path leak), the four mode-label mappings, XSS escaping
// of every attacker-controllable string, and the credential negative
// anchor. The migration and mongo report test sets are untouched and must
// stay green (family regression anchors).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// dcTaskFixture persists a standalone compare task (task.json) and
// optionally writes the engine-shaped report.json under
// <dataDir>/compare/<id>/. The fixture carries distinctive credentials and
// usernames — the negative anchor asserts none of them ever render.
func dcTaskFixture(t *testing.T, s *Server, rpt *reporter.Report, mutate func(*CompareTask)) string {
	t.Helper()
	now := time.Now()
	task := &CompareTask{
		ID:          "mc16rep",
		Name:        "MC-16 fixture",
		Status:      CompareStatusCompleted,
		Source:      config.SourceConfig{Host: "src.example", Port: 5432, User: "srcsecretuser", Password: "srcsecretpass", Database: "shop", Schema: "public"},
		Target:      config.TargetConfig{Host: "dst.example", Port: 4000, User: "tgtsecretuser", Password: "tgtsecretpass", Database: "shop2"},
		Mode:        "sample",
		SampleRatio: 0.01,
		Tables:      []string{"public.orders"},
		CreatedAt:   now,
		StartedAt:   &now,
	}
	if mutate != nil {
		mutate(task)
	}
	if err := s.persistCompareTask(task); err != nil {
		t.Fatal(err)
	}
	if rpt != nil {
		dir := s.compareDir(task.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := rpt.SaveJSON(filepath.Join(dir, "report.json")); err != nil {
			t.Fatal(err)
		}
	}
	return task.ID
}

// dcReport builds a representative engine report: three of the four table
// states, a diff row, an error, and a suggestion (the compare-specific
// column). Finish() computes Stats; stable times/duration are set after.
// The orders row counts (99,997/181,117) pin the unit-11 thousand-separator
// face end-to-end.
func dcReport() *reporter.Report {
	rpt := reporter.NewReport("validate")
	rpt.Tables = []reporter.TableReport{
		{TableName: "public.orders", Status: reporter.StatusPass, Duration: "10.200s", SourceRows: 99997, TargetRows: 181117},
		{TableName: "public.users", Status: reporter.StatusWarn, Duration: "5.000s", SourceRows: 50, TargetRows: 49, DiffRows: 1, Suggestion: "run a checksum compare on public.users"},
		{TableName: "public.logs", Status: reporter.StatusSkip},
		{TableName: "public.bad", Status: reporter.StatusFail, Error: "connect refused"},
	}
	rpt.Finish(reporter.StatusWarn, "采样比对完成：3/4 表通过")
	rpt.StartTime = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	rpt.EndTime = time.Date(2026, 1, 2, 15, 6, 5, 0, time.UTC)
	rpt.Duration = "2m0.000s"
	return rpt
}

func dcReportGet(t *testing.T, s *Server, id, format string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/compare/tasks/" + id + "/report"
	if format != "" {
		path += "?format=" + format
	}
	req := withChiParam(httptest.NewRequest(http.MethodGet, path, nil), "compareID", id)
	rec := httptest.NewRecorder()
	s.handleCompareReport(rec, req)
	return rec
}

// TestDataCompareReport_HTML pins the family tokens: TiMS title/banner,
// 起~止 subtitle, the six-cell meta grid (ID/name/endpoints/mode/duration),
// the 概览 + 统计 cards, the # column with Chinese badges, and the
// compare-specific 建议 column.
func TestDataCompareReport_HTML(t *testing.T) {
	s, _ := newTestServer(t)
	id := dcTaskFixture(t, s, dcReport(), nil)

	rec := dcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type=%q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "compare-report-mc16rep.html") {
		t.Errorf("content-disposition=%q", cd)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>TiMS 数据比对报告</title>",
		`<h1><span class="logo">Ti</span>MS 数据比对报告</h1>`,
		"2026-01-02 15:04:05 ~ 2026-01-02 15:06:05",
		`<div>任务 ID <span class="num">mc16rep</span></div>`,
		`<div>任务名 <span>MC-16 fixture</span></div>`,
		`<div>源端 <span>src.example:5432/shop</span></div>`,
		`<div>目标 <span>dst.example:4000/shop2</span></div>`,
		`<div>比对模式 <span>采样（比例 1%）</span></div>`,
		`<div>耗时 <span class="num">2m0.000s</span></div>`,
		`<div class="value overall-warn">警告</div>`, // 概览 status value
		"总表数", "源端行数", "目标行数",
		"采样比对完成：3/4 表通过", // summary line
		"<h2>统计</h2>", "通过", "失败", "警告", "跳过", "差异行数",
		"<h2>表详情</h2>",
		`<th>#</th><th>表名</th><th>状态</th><th class="colnum">源端行数</th><th class="colnum">目标行数</th><th class="colnum">差异</th><th>行级差异</th><th>备注</th>`, // unit-11 A: 8 columns
		`<td class="num">1</td><td>public.orders</td>`, // original # survives the failure-first sort
		`<td class="num">4</td><td>public.bad</td>`,    // fail row keeps engine # 4
		`<tr class="row-fail">`,                        // unit-11 D: fail tint
		`<span class="badge badge-warn">警告</span>`,     // Chinese badge
		`<span class="badge badge-skip">跳过</span>`,
		`<td class="num">99,997</td>`, // unit-11 E: thousand separator
		`<td class="num">181,117</td>`,
		`<td class="num diff-pos">1</td>`,                                         // diff>0 red class
		"run a checksum compare on public.users",                                  // 备注 cell (≤40 runes → whole)
		"connect refused",                                                         // 错误全文 in the expand area (C)
		`<tr class="row-detail"><td colspan="8"><details><summary>诊断详情</summary>`, // C expand row structure
		"tr.row-detail details > *:not(summary) { display: block !important; }",   // C print force-open
		"tr.row-detail details::details-content { display: block !important; content-visibility: visible !important; }",
		`<span class="tims-version">V3.19.36</span>`, // version badge
		"由 TiMS (TiDB Migration Suite) 生成 &middot; 数据比对 &middot; ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("html missing %q", want)
		}
	}
	// Failure-first order (unit-11 D): 失败(#4) → 警告(#2) → 通过(#1) → 跳过(#3)
	// in body order, original #s preserved for traceability.
	pos := func(sub string) int { return strings.Index(body, sub) }
	if !(pos(`<td class="num">4</td><td>public.bad</td>`) < pos(`<td class="num">2</td><td>public.users</td>`) &&
		pos(`<td class="num">2</td><td>public.users</td>`) < pos(`<td class="num">1</td><td>public.orders</td>`) &&
		pos(`<td class="num">1</td><td>public.orders</td>`) < pos(`<td class="num">3</td><td>public.logs</td>`)) {
		t.Error("表详情 must render failure-first (失败→警告→通过/跳过) with original # order")
	}
	// The dropped columns must be gone (unit-11 A).
	for _, gone := range []string{"<th>耗时</th>", "<th>错误</th>", "<th>建议</th>"} {
		if strings.Contains(body, gone) {
			t.Errorf("dropped column header still rendered: %q", gone)
		}
	}
	// Version badge: exactly one V3.19.36 stamp, no stale version.
	if n := strings.Count(body, "V3.19.36"); n != 1 {
		t.Errorf("V3.19.36 badge count=%d want 1", n)
	}
	if n := strings.Count(body, `class="tims-version"`); n != 1 {
		t.Errorf("tims-version badge count=%d want 1", n)
	}
	if strings.Contains(body, "V3.19.35") {
		t.Error("stale version V3.19.35 rendered")
	}
	// The pre-MC-16 browser-side title/footer vocabulary must be gone.
	if strings.Contains(body, "TimsTool") {
		t.Error("report must not render the legacy TimsTool footer")
	}
}

// TestDataCompareReport_DefaultHTMLAndEmptyName: no format param serves the
// html face (family default); an unnamed task renders the 任务 ID cell but
// no empty 任务名 cell (MC-15 semantics).
func TestDataCompareReport_DefaultHTMLAndEmptyName(t *testing.T) {
	s, _ := newTestServer(t)
	id := dcTaskFixture(t, s, dcReport(), func(task *CompareTask) { task.Name = "" })

	rec := dcReportGet(t, s, id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("default format must be html, content-type=%q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "compare-report-mc16rep.html") {
		t.Errorf("default content-disposition=%q", cd)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<div>任务 ID <span class="num">mc16rep</span></div>`) {
		t.Error("任务 ID cell missing for unnamed task")
	}
	if strings.Contains(body, "任务名") {
		t.Error("empty task name must not render a 任务名 cell")
	}
}

// TestDataCompareReport_JSONVerbatim: format=json serves the report.json
// bytes byte-identically with the json attachment name.
func TestDataCompareReport_JSONVerbatim(t *testing.T) {
	s, _ := newTestServer(t)
	id := dcTaskFixture(t, s, dcReport(), nil)

	rec := dcReportGet(t, s, id, "json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	raw, err := os.ReadFile(filepath.Join(s.dataDir, "compare", id, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != string(raw) {
		t.Error("format=json must be byte-identical to report.json")
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "compare-report-mc16rep.json") {
		t.Errorf("content-disposition=%q", cd)
	}
}

// TestDataCompareReport_TaskNotFound404: no task.json at all → 404 naming
// the task (not the report artifact).
func TestDataCompareReport_TaskNotFound404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := dcReportGet(t, s, "nosuch", "html")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "compare task not found") {
		t.Errorf("error must name the missing task: %s", rec.Body.String())
	}
}

// TestDataCompareReport_NoReport404: a task without report.json (unfinished,
// failed before the compare completed, or cleared) 404s on both formats with
// a message naming the causes — never a half-empty report.
func TestDataCompareReport_NoReport404(t *testing.T) {
	s, _ := newTestServer(t)
	id := dcTaskFixture(t, s, nil, nil)
	for _, f := range []string{"html", "json"} {
		rec := dcReportGet(t, s, id, f)
		if rec.Code != http.StatusNotFound {
			t.Errorf("format=%s status=%d want 404", f, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{"report.json", "unfinished", "cleared"} {
			if !strings.Contains(body, want) {
				t.Errorf("format=%s error must name the cause %q: %s", f, want, body)
			}
		}
	}
}

// TestDataCompareReport_CorruptReport500 (adversarial anchor): a report.json
// that exists but will not decode is a server-side artifact problem — the
// html face 500s with a generic body that leaks no server-side path, while
// the json face still serves the verbatim bytes (原样回传 contract holds
// regardless of decodability).
func TestDataCompareReport_CorruptReport500(t *testing.T) {
	s, _ := newTestServer(t)
	id := dcTaskFixture(t, s, dcReport(), nil)
	if err := os.WriteFile(filepath.Join(s.dataDir, "compare", id, "report.json"), []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := dcReportGet(t, s, id, "html")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("html status=%d want 500 (corrupt artifact ≠ missing artifact)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "read compare report") {
		t.Errorf("html error must name the read failure: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), s.dataDir) || strings.Contains(rec.Body.String(), filepath.Join("compare", id, "report.json")) {
		t.Errorf("html error leaks server-side path: %s", rec.Body.String())
	}
	rec = dcReportGet(t, s, id, "json")
	if rec.Code != http.StatusOK || rec.Body.String() != "{truncated" {
		t.Errorf("json status=%d body=%q want 200 verbatim bytes", rec.Code, rec.Body.String())
	}
}

// TestDataCompareReport_ModeLabels: the four family mode mappings, including
// the watermark dimension (水位过滤 · 基础模式中文) and the sample-ratio
// fallback derivation (<= 0 rides the 0.01 default).
func TestDataCompareReport_ModeLabels(t *testing.T) {
	cases := []struct {
		name  string
		task  CompareTask
		label string
	}{
		{"quick", CompareTask{Mode: "quick"}, "快速（行数）"},
		{"sample", CompareTask{Mode: "sample", SampleRatio: 0.015}, "采样（比例 1.5%）"},
		{"sample default ratio", CompareTask{Mode: "sample"}, "采样（比例 1%）"},
		{"sample stale >1 clamped", CompareTask{Mode: "sample", SampleRatio: 1.5}, "采样（比例 100%）"},
		{"sample stale huge clamped", CompareTask{Mode: "sample", SampleRatio: 150}, "采样（比例 100%）"},
		{"checksum", CompareTask{Mode: "checksum"}, "校验和"},
		{"watermark checksum base", CompareTask{Mode: "checksum", Watermark: &config.WatermarkFilter{Column: "updated_at", Value: "v", Op: "<=", BaseMode: "checksum"}}, "水位过滤 · 校验和（基础模式）"},
		{"watermark quick base", CompareTask{Mode: "quick", Watermark: &config.WatermarkFilter{Column: "updated_at", Value: "v", Op: "<=", BaseMode: "quick"}}, "水位过滤 · 快速（行数）（基础模式）"},
		{"empty mode", CompareTask{}, "—"},
	}
	for _, c := range cases {
		if got := dcModeLabel(&c.task); got != c.label {
			t.Errorf("%s: dcModeLabel=%q want %q", c.name, got, c.label)
		}
	}
}

// TestDataCompareReport_WatermarkRender: a watermark task renders the
// 水位过滤 mode label end-to-end through the handler.
func TestDataCompareReport_WatermarkRender(t *testing.T) {
	s, _ := newTestServer(t)
	id := dcTaskFixture(t, s, dcReport(), func(task *CompareTask) {
		task.Mode = "checksum"
		task.Watermark = &config.WatermarkFilter{Column: "updated_at", Value: "2026-01-01 00:00:00", Op: "<=", BaseMode: "checksum"}
	})
	rec := dcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<div>比对模式 <span>水位过滤 · 校验和（基础模式）</span></div>`) {
		t.Error("watermark mode label missing")
	}
}

// TestDataCompareReport_XSSAnchor: the task name, table names, errors and
// suggestions are attacker-controllable surfaces; every one of them must
// stay escaped (TaskNameXSSAnchor precedent).
func TestDataCompareReport_XSSAnchor(t *testing.T) {
	s, _ := newTestServer(t)
	rpt := dcReport()
	rpt.Tables[0].TableName = `<script>alert(1)</script>`
	rpt.Tables[3].Error = `"><img src=x onerror=alert(2)>`
	rpt.Tables[1].Suggestion = `<iframe src=evil></iframe>`
	id := dcTaskFixture(t, s, rpt, func(task *CompareTask) { task.Name = `<script>alert(3)</script>` })

	rec := dcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, raw := range []string{"<script>alert(1)</script>", "<img src=x", "<iframe src=evil", "<script>alert(3)</script>"} {
		if strings.Contains(body, raw) {
			t.Errorf("unescaped dynamic string rendered into report: %q", raw)
		}
	}
	if !strings.Contains(body, `<div>任务名 <span>&lt;script&gt;alert(3)&lt;/script&gt;</span></div>`) {
		t.Error("escaped task name cell missing")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("escaped table name missing")
	}
}

// TestDataCompareReport_StatusClassEscaped (MC-17 ①): the overview badge's
// class attribute interpolates rpt.Status — a status value carrying quotes
// must never break out of the attribute. The persisted report.json is
// engine-written, but a corrupt/hand-edited artifact reaches the same
// renderer, so the class string escapes like every other dynamic string
// (reporter.ToHTML twin anchor lives in reporter_test.go).
func TestDataCompareReport_StatusClassEscaped(t *testing.T) {
	s, _ := newTestServer(t)
	rpt := dcReport()
	rpt.Status = reporter.Status(`x" onload="alert(1)`)
	id := dcTaskFixture(t, s, rpt, nil)

	rec := dcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `overall-x" onload="alert(1)`) {
		t.Error("unescaped statusClass rendered into class attribute")
	}
	if !strings.Contains(body, `overall-x&quot; onload=&quot;alert(1)`) {
		t.Error("escaped statusClass missing")
	}
}

// TestDataCompareReport_TableLayoutCSSAnchor (MC-18 + unit-11 P2): the
// layout rules that keep the 表详情 table readable — headers/number
// cells/badges never wrap, long text cells break inside tokens, and the
// non-zero-diff cell actually renders red (rule presence, not just the
// class in HTML — review-P2 blind-spot closure) — must stay in the
// rendered style block (compare_data_report.go local style, ReportCSS
// untouched).
func TestDataCompareReport_TableLayoutCSSAnchor(t *testing.T) {
	s, _ := newTestServer(t)
	id := dcTaskFixture(t, s, dcReport(), nil)

	rec := dcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, rule := range []string{
		"th { white-space: nowrap; }",
		"td.num { white-space: nowrap; }",
		".badge { white-space: nowrap; }",
		"td:not(.num) { overflow-wrap: anywhere; }",
		"td.diff-pos { color: #E13C3C; font-weight: 600; }", // unit-11 E: diff-red rule must exist (review P2)
	} {
		if !strings.Contains(body, rule) {
			t.Errorf("layout rule missing from style block: %q", rule)
		}
	}
}

// TestDataCompareReport_CredentialsNeverRender: the fixture's task.json
// carries distinctive usernames (passwords are redacted on persist); the
// rendered report must contain neither (compareEndpointLabel policy — the
// label builder only ever sees host/port/database).
func TestDataCompareReport_CredentialsNeverRender(t *testing.T) {
	s, _ := newTestServer(t)
	id := dcTaskFixture(t, s, dcReport(), nil)
	rec := dcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"srcsecretuser", "tgtsecretuser", "srcsecretpass", "tgtsecretpass"} {
		if strings.Contains(body, secret) {
			t.Errorf("credential %q rendered into report", secret)
		}
	}
}

// TestDataCompareReport_NumberAndBriefFormat (unit-11 E + 备注): dcComma
// thousand-separator edges and dcBrief's first-segment / 40-rune cut.
func TestDataCompareReport_NumberAndBriefFormat(t *testing.T) {
	nums := []struct {
		in   int64
		want string
	}{
		{0, "0"}, {999, "999"}, {1000, "1,000"}, {99997, "99,997"},
		{181117, "181,117"}, {1234567, "1,234,567"}, {-1234, "-1,234"},
	}
	for _, c := range nums {
		if got := dcComma(c.in); got != c.want {
			t.Errorf("dcComma(%d)=%q want %q", c.in, got, c.want)
		}
	}
	briefs := []struct {
		in   string
		want string
	}{
		{"", "—"},
		{"短建议", "短建议"},
		{"run a checksum compare on public.users", "run a checksum compare on public.users"}, // ≤40, whole
		{"首段结论；checksum mode: 1 chunks × 1000 rows, 0 mismatches", "首段结论…"},                  // 首段 cut at the engine joiner
		{strings.Repeat("查", 41), strings.Repeat("查", 40) + "…"},                             // rune cut, multibyte safe
		{strings.Repeat("查", 40), strings.Repeat("查", 40)},                                   // exactly 40: no ellipsis
		{"第一段\n第二段", "第一段…"},
	}
	for _, c := range briefs {
		if got := dcBrief(c.in); got != c.want {
			t.Errorf("dcBrief(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestDataCompareReport_SummaryCN (unit-11 G): the canonical English
// validator summary maps to Chinese at the RENDER layer; everything
// unmatchable (or with a blank level) passes through verbatim — the honest
// face never swallows an unknown summary.
func TestDataCompareReport_SummaryCN(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"validated 13 tables at level data", "已校验 13 张表 · 级别：data"},
		{"validated 1 tables at level schema", "已校验 1 张表 · 级别：schema"},
		{"采样比对完成：3/4 表通过", "采样比对完成：3/4 表通过"},                               // 透传锚
		{"validated 13 tables", "validated 13 tables"},                     // malformed → passthrough
		{"validated 13 tables at level ", "validated 13 tables at level "}, // blank level → passthrough
	}
	for _, c := range cases {
		if got := dcSummaryCN(c.in); got != c.want {
			t.Errorf("dcSummaryCN(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestDataCompareReport_TableDetailPills (unit-11 B/C/D/E/G end-to-end):
// a failed checksum table with a drilled row-diff summary renders the three
// count pills, the fail tint, comma numbers, the truncated suggestion
// (brief in the cell, engine original byte-identical in the expand area),
// the CSV detail file ONLY in the expand area, the honest-abort note
// (brief in the cell, full text in the expand area, no pills), and the G
// summary mapping through the handler.
func TestDataCompareReport_TableDetailPills(t *testing.T) {
	s, _ := newTestServer(t)
	rpt := reporter.NewReport("validate")
	rpt.Tables = []reporter.TableReport{
		{TableName: "public.big", Status: reporter.StatusFail, SourceRows: 99997, TargetRows: 181117, DiffRows: 12,
			Error:      "checksum mismatch in 1/1 chunks",
			Suggestion: "首段结论；checksum mode: 1 chunks × 1000 rows, 0 mismatches",
			RowDiffs: &reporter.RowDiffSummary{KeyColumns: []string{"id"}, ContentDiff: 6, SrcOnly: 9, DstOnly: 0,
				File: "big-12345678.csv", Truncated: true}},
		{TableName: "public.nn", Status: reporter.StatusFail,
			RowDiffs: &reporter.RowDiffSummary{Note: "键列含 NULL，无法行级对齐"}},
		{TableName: "public.ok", Status: reporter.StatusPass, Suggestion: strings.Repeat("查", 80)},
	}
	rpt.Finish(reporter.StatusFail, "validated 13 tables at level data")
	id := dcTaskFixture(t, s, rpt, nil)

	rec := dcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<span class="rd-pill rd-content">内容 6</span>`, // B: three count pills
		`<span class="rd-pill rd-srconly">仅源 9</span>`,
		`<span class="rd-pill rd-dstonly">仅目标 0</span>`,
		`内容不一致 6 · 仅源端 9 · 仅目标端 0（明细截断）`,                 // readable sentence + truncation marker in the expand area
		`<span class="det-file">big-12345678.csv</span>`, // CSV file name, monospace, expand area only
		"checksum mismatch in 1/1 chunks",                // 错误全文 in the expand area
		"首段结论…",                                          // 备注 cell: first segment, ellipsized
		strings.Repeat("查", 40) + "…",                    // rune-cut brief cell for the 80-rune suggestion
		strings.Repeat("查", 80),                          // engine original byte-identical in the expand area
		"键列含 NULL，无法行级对齐",                                // honest-abort note
		`<span class="det-label">行级差异说明</span>`,
		`已校验 13 张表 · 级别：data`, // G mapping through the handler
		`<tr class="row-fail">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("html missing %q", want)
		}
	}
	// Exactly three pills — the note-aborted table renders no pills (the
	// count keys on the span class literal so the .rd-pill CSS rule text
	// does not skew it).
	if n := strings.Count(body, `class="rd-pill `); n != 3 {
		t.Errorf("pill span count=%d want 3 (counts table only, note table pill-free)", n)
	}
	// Exactly two fail tints (big + nn), pass table untinted.
	if n := strings.Count(body, `<tr class="row-fail">`); n != 2 {
		t.Errorf("row-fail count=%d want 2", n)
	}
	// The suggestion tail and the CSV file name live ONLY in the expand
	// area, never in the main row cells.
	if n := strings.Count(body, "checksum mode: 1 chunks × 1000 rows, 0 mismatches"); n != 1 {
		t.Errorf("suggestion tail count=%d want 1 (expand area only)", n)
	}
	if n := strings.Count(body, "big-12345678.csv"); n != 1 {
		t.Errorf("csv file count=%d want 1 (expand area only)", n)
	}
	if strings.Index(body, "big-12345678.csv") < strings.Index(body, "<details") {
		t.Error("csv file name must render inside the details expand area")
	}
	// The mapped summary replaces the English original on the html face.
	if strings.Contains(body, "validated 13 tables") {
		t.Error("unmapped English summary leaked past the G mapping")
	}
	// Brief cells stay whole-token-safe: 40查… appears once (cell), 80查 once (expand).
	if n := strings.Count(body, strings.Repeat("查", 40)+"…"); n != 1 {
		t.Errorf("brief ellipsis cell count=%d want 1", n)
	}
	if n := strings.Count(body, strings.Repeat("查", 80)); n != 1 {
		t.Errorf("full suggestion count=%d want 1 (expand area only)", n)
	}
}
