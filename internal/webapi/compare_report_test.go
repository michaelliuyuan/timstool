package webapi

// MC-14 compare report tests: the /tasks/{id}/report branch for mongocompare
// tasks — HTML rendering anchors, verbatim JSON serving, 404 semantics for
// summary-less tasks, and the migration-path regression anchor (legacy
// configs must keep rendering the migration report byte-for-byte via the
// untouched branch).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/mongocompare"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// mcReportFixture creates a compare task in the store and (optionally) writes
// a fabricated summary.json under <dataDir>/compare/<id>/.
func mcReportFixture(t *testing.T, s *Server, sum *mongocompare.Summary) string {
	t.Helper()
	return mcReportFixtureNamed(t, s, sum, "MC-14 fixture")
}

// mcReportFixtureNamed is mcReportFixture with an explicit task name (MC-15:
// the banner renders the 任务名 cell only for non-empty names).
func mcReportFixtureNamed(t *testing.T, s *Server, sum *mongocompare.Summary, name string) string {
	t.Helper()
	task := &store.Task{ID: "mc14rep", Name: name}
	env := compareTaskEnvelope{Kind: compareTaskKind, Compare: compareTaskSpec{
		Source:  compareEndpoint{Host: "src.example", Port: 27017, Database: "shop"},
		Target:  compareEndpoint{Host: "dst.example", Port: 27018, Database: "shop2"},
		Buckets: 64,
	}}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	task.ConfigJSON = string(b)
	if err := s.store.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if sum != nil {
		dir := filepath.Join(s.dataDir, "compare", task.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		sb, err := json.MarshalIndent(sum, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "summary.json"), sb, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return task.ID
}

func mcReportSummary() *mongocompare.Summary {
	now := time.Now()
	sum := &mongocompare.Summary{
		StartedAt:   now.Add(-90 * time.Second),
		FinishedAt:  now,
		DurationSec: 62.5,
		Counts:      map[string]int{"equal": 1, "diff": 1, "error": 1, "ttl_skip": 1},
		Overall:     mongocompare.OverallDiff,
		Stats:       map[string]any{"buckets": 64, "parallel_colls": 4, "parallel_buckets": 4, "batch_size": 1000, "resume": false},
	}
	var equalColl mongocompare.CollectionResult
	equalColl.DB, equalColl.Coll, equalColl.Overall = "shop", "users", mongocompare.OverallEqual
	equalColl.Checks.Schema = map[string]any{"ok": true}
	equalColl.Checks.Index = map[string]any{"ok": true}
	equalColl.Checks.Count = map[string]any{"src": float64(100), "dst": float64(100), "ok": true}
	equalColl.Checks.Hash = []mongocompare.BucketResult{{Bucket: 0, Match: true, SrcCount: 50, DstCount: 50}, {Bucket: 1, Match: true, SrcCount: 50, DstCount: 50}}
	equalColl.Checks.Diff = map[string]any{"count": float64(0)}

	var diffColl mongocompare.CollectionResult
	diffColl.DB, diffColl.Coll, diffColl.Overall = "shop", "orders", mongocompare.OverallDiff
	diffColl.Checks.Schema = map[string]any{"ok": true}
	diffColl.Checks.Count = map[string]any{"src": float64(120), "dst": float64(80), "ok": false}
	diffColl.Checks.Hash = []mongocompare.BucketResult{
		{Bucket: 0, Match: true, SrcCount: 60, DstCount: 60},
		{Bucket: 1, Match: false, SrcCount: 60, DstCount: 20},
	}
	diffColl.Checks.Diff = map[string]any{"count": float64(37)}

	var errColl mongocompare.CollectionResult
	errColl.DB, errColl.Coll, errColl.Overall = "shop", "logs", mongocompare.OverallError
	errColl.Checks.Error = "count documents: socket timeout"

	var ttlColl mongocompare.CollectionResult
	ttlColl.DB, ttlColl.Coll, ttlColl.Overall = "shop", "sessions", mongocompare.OverallTTLSkip
	ttlColl.Checks.Index = map[string]any{"ok": true}
	ttlColl.Checks.Skipped = "ttl"

	sum.Collections = []mongocompare.CollectionResult{equalColl, diffColl, errColl, ttlColl}
	return sum
}

func mcReportGet(t *testing.T, s *Server, id, format string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/tasks/" + id + "/report"
	if format != "" {
		path += "?format=" + format
	}
	req := withChiParam(httptest.NewRequest(http.MethodGet, path, nil), "taskID", id)
	rec := httptest.NewRecorder()
	s.handleTaskReport(rec, req)
	return rec
}

func TestCompareReport_HTML(t *testing.T) {
	s, _ := newTestServer(t)
	id := mcReportFixture(t, s, mcReportSummary())

	rec := mcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type=%q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "compare-report-mc14rep.html") {
		t.Errorf("content-disposition=%q", cd)
	}
	body := rec.Body.String()
	// Family + banner anchors (MC-15: migration-report-style split cells).
	for _, want := range []string{
		"MongoDB 对比报告",
		`<div>任务 ID <span class="num">mc14rep</span></div>`,
		`<div>任务名 <span>MC-14 fixture</span></div>`,
		`<div>源端 <span>src.example:27017/shop</span></div>`, // one cell per endpoint
		`<div>目标端 <span>dst.example:27018/shop2</span></div>`,
		"62.50s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("html missing %q", want)
		}
	}
	// The pre-MC-15 composite "源端 … → 目标端 …" single cell must be gone.
	if strings.Contains(body, "→ 目标端") {
		t.Error("banner must not render the composite 源端 → 目标端 cell")
	}
	// Four-state counts + per-collection rows.
	for _, want := range []string{
		"集合总数", ">4<", // len(Collections)
		"shop.users", "shop.orders", "shop.logs", "shop.sessions",
		"存在差异", "对比出错", "TTL 跳过", "TTL 数据跳过",
		"目标端缺失", // not in this fixture — sanity guard below instead
		"120 → 80",
		"1/2", // orders hash matched/total
		"socket timeout",
		"引擎参数",
	} {
		if want == "目标端缺失" {
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("html missing %q", want)
		}
	}
	// Drill-down card: mismatched bucket + diff doc count.
	if !strings.Contains(body, "字段级差异文档") || !strings.Contains(body, "37") {
		t.Error("drill-down diff count missing")
	}
	if !strings.Contains(body, "桶 <span class=\"num\">#1</span>：源 <span class=\"num\">60</span> / 目标 <span class=\"num\">20</span>") {
		t.Error("mismatched bucket row missing")
	}
	// Credentials must never render (fixture carries none; the guard is the
	// URI-mode label below). Default format == html.
	rec2 := mcReportGet(t, s, id, "")
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "MongoDB 对比报告") {
		t.Errorf("default format must render html: %d", rec2.Code)
	}
}

// TestCompareReport_EmptyNameNoNameCell (MC-15): an unnamed task renders the
// 任务 ID cell but no empty 任务名 cell.
func TestCompareReport_EmptyNameNoNameCell(t *testing.T) {
	s, _ := newTestServer(t)
	id := mcReportFixtureNamed(t, s, mcReportSummary(), "")

	rec := mcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<div>任务 ID <span class="num">mc14rep</span></div>`) {
		t.Error("任务 ID cell missing for unnamed task")
	}
	if strings.Contains(body, "任务名") {
		t.Error("empty task name must not render a 任务名 cell")
	}
}

// TestCompareReport_TaskNameXSSAnchor (MC-15): the attacker-controllable task
// name reaches the banner 任务名 cell and must stay escaped.
func TestCompareReport_TaskNameXSSAnchor(t *testing.T) {
	s, _ := newTestServer(t)
	id := mcReportFixtureNamed(t, s, mcReportSummary(), `<script>alert(3)</script>`)

	rec := mcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(3)</script>") {
		t.Error("unescaped task name rendered into banner")
	}
	if !strings.Contains(body, `<div>任务名 <span>&lt;script&gt;alert(3)&lt;/script&gt;</span></div>`) {
		t.Error("escaped task name cell missing")
	}
}

// TestCompareReport_JSON serves the verbatim engine artifact bytes.
func TestCompareReport_JSON(t *testing.T) {
	s, _ := newTestServer(t)
	sum := mcReportSummary()
	id := mcReportFixture(t, s, sum)

	rec := mcReportGet(t, s, id, "json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	raw, err := os.ReadFile(filepath.Join(s.dataDir, "compare", id, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != string(raw) {
		t.Error("format=json must be byte-identical to summary.json")
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "compare-summary-mc14rep.json") {
		t.Errorf("content-disposition=%q", cd)
	}
}

// TestCompareReport_NoSummary404: a compare task without summary.json (never
// ran, failed at connect, or wiped by rerun) 404s on both formats with a
// message naming the cause — never an empty report.
func TestCompareReport_NoSummary404(t *testing.T) {
	s, _ := newTestServer(t)
	id := mcReportFixture(t, s, nil)
	for _, f := range []string{"html", "json"} {
		rec := mcReportGet(t, s, id, f)
		if rec.Code != http.StatusNotFound {
			t.Errorf("format=%s status=%d want 404", f, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "summary.json") {
			t.Errorf("format=%s error must name the missing artifact: %s", f, rec.Body.String())
		}
	}
}

// TestCompareReport_CorruptSummary500 (adversarial MC-14 Level-2): an
// existing but undecodable summary.json is a server-side artifact problem —
// the html face 500s (never a "not produced" 404 misattributing the cause),
// while the json face still serves the verbatim bytes (原样回传 contract:
// byte-identity holds regardless of decodability).
func TestCompareReport_CorruptSummary500(t *testing.T) {
	s, _ := newTestServer(t)
	sum := mcReportSummary()
	id := mcReportFixture(t, s, sum)
	dir := filepath.Join(s.dataDir, "compare", id)
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := mcReportGet(t, s, id, "html")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("html status=%d want 500 (corrupt artifact ≠ missing artifact)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "read compare summary") {
		t.Errorf("html error must name the read failure: %s", rec.Body.String())
	}
	rec = mcReportGet(t, s, id, "json")
	if rec.Code != http.StatusOK || rec.Body.String() != "{truncated" {
		t.Errorf("json status=%d body=%q want 200 verbatim bytes", rec.Code, rec.Body.String())
	}
}

// TestCompareReport_MigrationRegressionAnchor: a migration config must keep
// rendering the migration report (the compare branch must stay invisible to
// legacy tasks — same red line as parseCompareTask).
func TestCompareReport_MigrationRegressionAnchor(t *testing.T) {
	s, _ := newTestServer(t)
	task := &store.Task{
		ID:         "mc14mig",
		Name:       "legacy migration",
		ConfigJSON: `{"source":{"host":"a"},"target":{"host":"b"},"migration":{"parallel":4}}`,
		ResultJSON: `{}`,
	}
	if err := s.store.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	rec := mcReportGet(t, s, task.ID, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "迁移报告") {
		t.Error("migration task must render the migration report")
	}
	if strings.Contains(body, "MongoDB 对比报告") {
		t.Error("migration task must not render the compare report")
	}
}

// TestCompareReport_XSSAnchor: collection names and error strings are
// attacker-controllable surfaces (DB/collection names); the report must
// escape them.
func TestCompareReport_XSSAnchor(t *testing.T) {
	s, _ := newTestServer(t)
	sum := mcReportSummary()
	sum.Collections[0].Coll = `<script>alert(1)</script>`
	sum.Collections[0].Checks.Error = `"><img src=x onerror=alert(2)>`
	id := mcReportFixture(t, s, sum)

	rec := mcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, "<img src=x") {
		t.Error("unescaped dynamic string rendered into report")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("escaped collection name missing")
	}
}

// TestCompareReport_BucketCap: an adversarial all-mismatch collection lists
// at most 20 buckets per drill-down.
func TestCompareReport_BucketCap(t *testing.T) {
	s, _ := newTestServer(t)
	sum := mcReportSummary()
	var coll mongocompare.CollectionResult
	coll.DB, coll.Coll, coll.Overall = "shop", "wide", mongocompare.OverallDiff
	coll.Checks.Count = map[string]any{"src": float64(2500), "dst": float64(0), "ok": false}
	buckets := make([]mongocompare.BucketResult, 25)
	for i := range buckets {
		buckets[i] = mongocompare.BucketResult{Bucket: i, Match: false, SrcCount: 100, DstCount: 0}
	}
	coll.Checks.Hash = buckets
	sum.Collections = []mongocompare.CollectionResult{coll}
	sum.Counts = map[string]int{"equal": 0, "diff": 1, "error": 0, "ttl_skip": 0}
	id := mcReportFixture(t, s, sum)

	rec := mcReportGet(t, s, id, "html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "共 25 个失配桶（仅列前 20）") {
		t.Error("bucket cap notice missing")
	}
	if n := strings.Count(body, "桶 <span class=\"num\">#"); n != 20 {
		t.Errorf("rendered %d bucket rows, want 20", n)
	}
}

// TestCompareReport_EndpointLabel: URI-mode endpoints render a credential-
// free label — the URI string itself may embed user:password.
func TestCompareReport_EndpointLabel(t *testing.T) {
	e := compareEndpoint{URI: "mongodb://u:secret@h/", Database: "d"}
	if got := compareEndpointLabel(e); strings.Contains(got, "secret") || !strings.Contains(got, "URI 连接/d") {
		t.Errorf("uri label leaks or malformed: %q", got)
	}
	if got := compareEndpointLabel(compareEndpoint{Host: "h", Port: 1, Database: "d"}); got != "h:1/d" {
		t.Errorf("host label = %q", got)
	}
	if got := compareEndpointLabel(compareEndpoint{Host: "h", Database: "d"}); got != "h/d" {
		t.Errorf("portless host label = %q", got)
	}
}

// TestCompareReport_SchemaMissingLabels: the schema diff vocabulary renders
// in Chinese (missing-in-target / missing-in-source).
func TestCompareReport_SchemaMissingLabels(t *testing.T) {
	if got := schemaCheckLabel(map[string]any{"diff": []string{"missing-in-target"}}); got != "目标端缺失" {
		t.Errorf("missing-in-target label = %q", got)
	}
	if got := schemaCheckLabel(map[string]any{"diff": []string{"missing-in-source"}}); got != "源端缺失" {
		t.Errorf("missing-in-source label = %q", got)
	}
	if got := schemaCheckLabel(nil); got != "—" {
		t.Errorf("nil schema = %q", got)
	}
	if got := schemaCheckLabel(map[string]any{"ok": true}); got != "✓" {
		t.Errorf("ok schema = %q", got)
	}
	// []any shape (json-decoded summary from disk).
	if got := schemaCheckLabel(map[string]any{"diff": []any{"missing-in-target"}}); got != "目标端缺失" {
		t.Errorf("[]any diff label = %q", got)
	}
	if got := indexCheckLabel(map[string]any{"diff": []any{"a", "b"}}); got != fmt.Sprintf("差异 %d 项", 2) {
		t.Errorf("[]any index label = %q", got)
	}
}
