package webapi

// MC-16 data-compare report tests: the /compare/tasks/{id}/report family
// faces — HTML rendering anchors (banner/meta grid/两卡/# 列/中文徽章/建议列/
// 页脚), verbatim JSON serving, default-format html, the 404 dual cause set,
// the corrupt-artifact 500 (generic body, no path leak), the four mode-label
// mappings, XSS escaping of every attacker-controllable string, and the
// credential negative anchor. The migration and mongo report test sets are
// untouched and must stay green (family regression anchors).

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
func dcReport() *reporter.Report {
	rpt := reporter.NewReport("validate")
	rpt.Tables = []reporter.TableReport{
		{TableName: "public.orders", Status: reporter.StatusPass, Duration: "10.200s", SourceRows: 1000, TargetRows: 1000},
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
		`<th>#</th><th>表名</th><th>状态</th><th>源端行数</th><th>目标行数</th><th>差异</th><th>耗时</th><th>错误</th><th>建议</th>`,
		`<td class="num">1</td><td>public.orders</td>`, // # column
		`<span class="badge badge-warn">警告</span>`,     // Chinese badge
		`<span class="badge badge-skip">跳过</span>`,
		"run a checksum compare on public.users", // 建议 column (D3)
		"connect refused",                        // error column
		"由 TiMS (TiDB Migration Suite) 生成 &middot; 数据比对 &middot; ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("html missing %q", want)
		}
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
