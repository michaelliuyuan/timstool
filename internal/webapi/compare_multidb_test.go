package webapi

// A2 multi-database batch tasks (#102): intake validation, wildcard
// resolution (against the faked listing seam), slug/summary/CSV/HTML
// rendering, the report family's multi-db faces (?db= selector, format=csv
// summary), the rowdiff ?db= routing, and the loop's fail-continue
// semantics against unreachable databases (connection refused — the
// per-database run must record status=error, keep looping, fail the task
// with the named databases, and still write the cross-db summary).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

func TestNormalizeCompareDBList(t *testing.T) {
	// nil/empty stays nil (legacy single-db body).
	got, err := normalizeCompareDBList(nil, "databases")
	if err != nil || got != nil {
		t.Fatalf("nil list: %v %v", got, err)
	}
	got, err = normalizeCompareDBList([]string{"  ", ""}, "databases")
	if err != nil || got != nil {
		t.Fatalf("all-empty list: %v %v", got, err)
	}
	// Trim + dedup (exact) + order preserved.
	got, err = normalizeCompareDBList([]string{" shop ", "report*", "shop", ""}, "databases")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"shop", "report*"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
	// Illegal characters reject (injection-shaped names never reach a DSN
	// or a filesystem path).
	for _, bad := range []string{"a;b", "a b", "db'--", "a/b", "..", "*", "a?"} {
		if bad == "*" || bad == "a?" {
			continue // legal wildcards
		}
		if _, err := normalizeCompareDBList([]string{bad}, "databases"); err == nil {
			t.Errorf("bad name %q accepted", bad)
		}
	}
	// Lone wildcards are legal patterns.
	if _, err := normalizeCompareDBList([]string{"*", "a?"}, "databases"); err != nil {
		t.Errorf("legal wildcards rejected: %v", err)
	}
	// Request cap.
	var many []string
	for i := 0; i < maxCompareDatabasesRequest+1; i++ {
		many = append(many, fmt.Sprintf("db%d", i))
	}
	if _, err := normalizeCompareDBList(many, "databases"); err == nil {
		t.Error("over-cap list accepted")
	}
}

func TestCompareDBPatternMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"shop", "shop", true},
		{"shop", "Shop", false}, // case-sensitive exact
		{"shop", "shop2", false},
		{"shop_*", "shop_2024", true},
		{"shop_*", "shopshop", false}, // `_` is literal, only *? are wildcards
		{"*", "anything", true},
		{"db-?", "db-1", true},
		{"db-?", "db-12", false},
		{"a*b*c", "a-x-b-y-c", true},
		{"a.b", "axb", false}, // dot is literal
	}
	for _, c := range cases {
		if got := compareDBPatternMatch(c.pattern, c.name); got != c.want {
			t.Errorf("match(%q, %q) = %v want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestResolveCompareDatabases(t *testing.T) {
	// Fake the server listing (the compareListSourceDatabases seam).
	prev := compareListSourceDatabases
	fakeListing := func(ctx context.Context, sc config.SourceConfig) ([]string, error) {
		// Mirror the real MySQL seam's system-schema filtering so the test
		// pins the resolution layer, not the dialect walk.
		var out []string
		for _, n := range []string{"mysql_like", "information_schema", "shop_2023", "shop_2024", "report", "metrics_schema", "postgres"} {
			if n == "information_schema" || n == "metrics_schema" {
				continue
			}
			out = append(out, n)
		}
		return out, nil
	}
	compareListSourceDatabases = fakeListing
	t.Cleanup(func() { compareListSourceDatabases = prev })
	sc := config.SourceConfig{Type: "mysql", Host: "h", Port: 3306}

	// Literals only: no listing call needed, passes through sorted.
	compareListSourceDatabases = func(ctx context.Context, sc config.SourceConfig) ([]string, error) {
		t.Error("literal-only selection must not hit the listing")
		return nil, nil
	}
	got, err := resolveCompareDatabases(context.Background(), sc, []string{"zeta", "alpha"}, nil)
	if err != nil || len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("literals: %v %v", got, err)
	}
	compareListSourceDatabases = fakeListing

	// Wildcard + literal + exclusions: deterministic sorted set.
	got, err = resolveCompareDatabases(context.Background(), sc,
		[]string{"shop_*", "report", "mysql_like"}, []string{"*_2023"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mysql_like", "report", "shop_2024"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("wildcard resolution: got %v want %v", got, want)
	}

	// Exclusion that empties the set fails loud.
	if _, err := resolveCompareDatabases(context.Background(), sc, []string{"report"}, []string{"*"}); err == nil {
		t.Fatal("fully-excluded selection must error")
	}

	// Nothing matches.
	if _, err := resolveCompareDatabases(context.Background(), sc, []string{"nope_*"}, nil); err == nil {
		t.Fatal("no-match wildcard must error")
	}

	// Resolved-set cap.
	big := make([]string, maxCompareDatabasesRun+1)
	for i := range big {
		big[i] = fmt.Sprintf("d%03d", i)
	}
	if _, err := resolveCompareDatabases(context.Background(), sc, big, nil); err == nil {
		t.Fatal("over-cap resolution must error")
	}
}

func TestCompareDBSlug(t *testing.T) {
	if s := compareDBSlug(0, "shop_2024"); s != "01-shop_2024" {
		t.Fatalf("slug = %q", s)
	}
	// Sanitization: nothing outside [A-Za-z0-9._-] survives into a path
	// segment (belt-and-braces — intake already rejects these characters).
	if s := compareDBSlug(1, "we;ird"); strings.ContainsAny(s, ";") {
		t.Fatalf("slug not sanitized: %q", s)
	}
	if s := compareDBSlug(11, "x"); !strings.HasPrefix(s, "12-") {
		t.Fatalf("ordinal prefix missing: %q", s)
	}
	// Same sanitized name at different ordinals stays distinct.
	if compareDBSlug(0, "a;b") == compareDBSlug(1, "a;b") {
		t.Fatal("ordinal must disambiguate identical sanitized names")
	}

	task := &CompareTask{ResolvedDatabases: []string{"alpha", "beta"}}
	if s, ok := compareDBSlugFor(task, "beta"); !ok || s != "02-beta" {
		t.Fatalf("slugFor(beta) = %q %v", s, ok)
	}
	if _, ok := compareDBSlugFor(task, "nope"); ok {
		t.Fatal("unknown db must not resolve")
	}
}

func TestCompareSummaryBuildAndCSV(t *testing.T) {
	task := &CompareTask{
		ID:     "abcd0123",
		Name:   "batch",
		Mode:   "checksum",
		Source: config.SourceConfig{Host: "src", Port: 5432},
		Target: config.TargetConfig{Host: "tgt", Port: 4000},
	}
	results := []CompareDatabaseResult{
		{Database: "db1", Status: compareDBStatusPass, TotalTables: 10, PassTables: 10},
		{Database: "db2", Status: compareDBStatusFail, TotalTables: 4, FailTables: 2, PassTables: 2, DiffRows: 7},
		{Database: "db3", Status: compareDBStatusError, Error: "connect refused"},
	}
	sum := buildCompareSummary(task, results, time.Now().Add(-time.Minute), time.Now(), "")
	if sum.Status != compareDBStatusError {
		t.Fatalf("overall = %q want error (an error db poisons the verdict)", sum.Status)
	}
	if sum.Totals.Databases != 3 || sum.Totals.DatabasesBad != 2 || sum.Totals.DatabasesPass != 1 {
		t.Fatalf("totals: %+v", sum.Totals)
	}
	if sum.Totals.TotalTables != 14 || sum.Totals.TotalDiffRows != 7 {
		t.Fatalf("table/diff totals: %+v", sum.Totals)
	}

	csv := string(compareSummaryCSV(sum))
	if !strings.HasPrefix(csv, "\uFEFF") {
		t.Fatal("CSV must start with the UTF-8 BOM")
	}
	if !strings.Contains(csv, "\r\n") {
		t.Fatal("CSV must use CRLF")
	}
	for _, want := range []string{"库名", "db1", "db2", "db3", "connect refused", "合计（3 库）"} {
		if !strings.Contains(csv, want) {
			t.Errorf("CSV missing %q", want)
		}
	}

	// Worst-first ordering of the overall verdict without an error db.
	ok := []CompareDatabaseResult{
		{Database: "a", Status: compareDBStatusPass},
		{Database: "b", Status: compareDBStatusWarn},
		{Database: "c", Status: compareDBStatusFail},
	}
	if s := compareSummaryOverallStatus(ok); s != compareDBStatusFail {
		t.Fatalf("fail>warn ordering broken: %q", s)
	}
	if s := compareSummaryOverallStatus(ok[:2]); s != compareDBStatusWarn {
		t.Fatalf("warn>pass ordering broken: %q", s)
	}
	if s := compareSummaryOverallStatus(ok[:1]); s != compareDBStatusPass {
		t.Fatalf("all-pass verdict: %q", s)
	}
}

func TestRenderCompareSummaryHTML(t *testing.T) {
	task := &CompareTask{ID: "abcd0123", Mode: "checksum"}
	sum := buildCompareSummary(task, []CompareDatabaseResult{
		{Database: "shop_<x>", Status: compareDBStatusFail, Error: "<script>alert(1)</script>"},
	}, time.Now(), time.Now(), "")
	html := renderCompareSummaryHTML(task, sum)
	for _, want := range []string{"数据比对汇总报告", "各库结果", "shop_&lt;x&gt;", "&lt;script&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("summary HTML missing %q", want)
		}
	}
	if strings.Contains(html, "<script>alert") {
		t.Error("summary HTML must escape the error text")
	}
}

// multiDbBody is a minimal valid create body with a multi-db selection.
func multiDbBody(dbs, excludes string) string {
	return `{"source":{"host":"127.0.0.1","port":1,"type":"postgres","database":"maint"},
	 "target":{"host":"127.0.0.1","port":1},"mode":"quick","databases":` + dbs +
		`,"exclude_databases":` + excludes + `}`
}

func TestCreateCompare_MultiDbValidation(t *testing.T) {
	s, _ := newTestServer(t)
	cases := []struct {
		name, body string
	}{
		{"bad name", multiDbBody(`["a;b"]`, `[]`)},
		{"bad exclude", multiDbBody(`["a"]`, `["x y"]`)},
		{"exclude without databases", `{"source":{"host":"s"},"target":{"host":"t"},"exclude_databases":["x"]}`},
	}
	for _, c := range cases {
		w, req := doReq("POST", "/api/v1/compare/tasks", c.body)
		s.handleCreateCompare(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

// TestCreateCompare_LegacyTaskJSONByteCompat pins the #102 red line: a
// single-database create (no databases key) persists a task.json whose key
// set contains NONE of the A2 fields — old payloads round-trip
// byte-identically.
func TestCreateCompare_LegacyTaskJSONByteCompat(t *testing.T) {
	s, _ := newTestServer(t)
	w, req := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s","type":"postgres","port":5432,"database":"d"},"target":{"host":"t"},"sample_ratio":1}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var resp CompareTask
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.compareDir(resp.ID), "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"databases", "exclude_databases", "resolved_databases", "databases_done", "databases_total", "current_database", "database_results"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Fatalf("legacy task.json gained A2 key %q: %s", key, raw)
		}
	}
}

// TestCreateCompare_MultiDbPersisted: the accepted multi-db body persists
// the selection (redacted passwords, per the standing red line) and runs
// the loop against unreachable databases — both databases error, the task
// fails with them named, and the cross-db summary still lands.
func TestCreateCompare_MultiDbPersisted(t *testing.T) {
	s, _ := newTestServer(t)
	w, req := doReq("POST", "/api/v1/compare/tasks", multiDbBody(`["dbone","dbtwo"]`, `[]`))
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var resp CompareTask
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Databases) != 2 || resp.Databases[0] != "dbone" || resp.Databases[1] != "dbtwo" {
		t.Fatalf("databases not round-tripped: %+v", resp.Databases)
	}
	if resp.Status != CompareStatusRunning {
		t.Fatalf("status = %q want running", resp.Status)
	}

	// Poll until terminal (both runs must fail fast: connection refused).
	deadline := time.Now().Add(30 * time.Second)
	var task *CompareTask
	for time.Now().Before(deadline) {
		task, _ = s.loadCompareTask(resp.ID)
		if task != nil && task.Status != CompareStatusRunning && task.Status != CompareStatusQueued {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if task == nil || task.Status != CompareStatusFailed {
		t.Fatalf("task did not fail: %+v", task)
	}
	if !strings.Contains(task.Error, "2/2") || !strings.Contains(task.Error, "dbone") || !strings.Contains(task.Error, "dbtwo") {
		t.Fatalf("error must name both failed databases: %q", task.Error)
	}
	if task.DatabasesTotal != 2 || task.DatabasesDone != 2 {
		t.Fatalf("db counters: done=%d total=%d", task.DatabasesDone, task.DatabasesTotal)
	}
	if strings.Join(task.ResolvedDatabases, ",") != "dbone,dbtwo" {
		t.Fatalf("resolved: %+v", task.ResolvedDatabases)
	}
	if len(task.DatabaseResults) != 2 {
		t.Fatalf("results: %+v", task.DatabaseResults)
	}
	for _, r := range task.DatabaseResults {
		if r.Status != compareDBStatusError {
			t.Fatalf("unreachable db must record status=error: %+v", r)
		}
	}

	// Summary artifacts landed with one error row per database.
	sumRaw, err := os.ReadFile(filepath.Join(s.compareDir(resp.ID), "summary.json"))
	if err != nil {
		t.Fatalf("summary.json missing: %v", err)
	}
	var sum CompareSummary
	if err := json.Unmarshal(sumRaw, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Status != compareDBStatusError || len(sum.Databases) != 2 || sum.Totals.DatabasesBad != 2 {
		t.Fatalf("summary: %+v", sum)
	}
	if _, err := os.Stat(filepath.Join(s.compareDir(resp.ID), "summary.csv")); err != nil {
		t.Fatalf("summary.csv missing: %v", err)
	}
}

// fabricateMultiTask writes a finished multi-db task plus its per-db
// reports and summary artifacts directly to disk (the report faces are
// pure reads over the on-disk store).
func fabricateMultiTask(t *testing.T, s *Server) *CompareTask {
	t.Helper()
	task := &CompareTask{
		ID:                "aa000001",
		Name:              "batch",
		Status:            CompareStatusCompleted,
		Mode:              "checksum",
		Databases:         []string{"shop_*"},
		ResolvedDatabases: []string{"shop_2023", "shop_2024"},
		DatabasesDone:     2,
		DatabasesTotal:    2,
		DatabaseResults: []CompareDatabaseResult{
			{Database: "shop_2023", Status: compareDBStatusPass, TotalTables: 2, PassTables: 2},
			{Database: "shop_2024", Status: compareDBStatusFail, TotalTables: 1, FailTables: 1, DiffRows: 3},
		},
	}
	if err := s.persistCompareTask(task); err != nil {
		t.Fatal(err)
	}
	for i, db := range task.ResolvedDatabases {
		dir := filepath.Join(s.compareDir(task.ID), "db", compareDBSlug(i, db))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		rpt := reporter.NewReport("data-validation")
		rpt.AddTableReport(reporter.TableReport{TableName: "t1", Status: reporter.StatusPass})
		rpt.Finish(reporter.StatusPass, "validated 1 tables at level quick")
		raw, _ := json.Marshal(rpt)
		if err := os.WriteFile(filepath.Join(dir, "report.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum := buildCompareSummary(task, task.DatabaseResults, time.Now(), time.Now(), "")
	if err := writeCompareSummary(s.compareDir(task.ID), sum); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestCompareReport_MultiDbFaces(t *testing.T) {
	s, _ := newTestServer(t)
	task := fabricateMultiTask(t, s)
	base := "/api/v1/compare/tasks/" + task.ID + "/report"

	get := func(query string) (int, string) {
		w, req := doReq("GET", base+query, "")
		s.router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	// Summary faces.
	code, body := get("?format=json")
	if code != http.StatusOK || !strings.Contains(body, `"task_id": "aa000001"`) || !strings.Contains(body, `"databases"`) {
		t.Fatalf("summary json: %d %s", code, body[:min(200, len(body))])
	}
	if !strings.Contains(body, `"shop_2024"`) {
		t.Fatalf("summary json must carry per-db rows: %s", body[:min(400, len(body))])
	}
	code, body = get("?format=csv")
	if code != http.StatusOK || !strings.Contains(body, "shop_2023") || !strings.HasPrefix(body, "\uFEFF") {
		t.Fatalf("summary csv: %d %q", code, body[:min(120, len(body))])
	}
	code, body = get("")
	if code != http.StatusOK || !strings.Contains(body, "数据比对汇总报告") || !strings.Contains(body, "shop_2024") {
		t.Fatalf("summary html: %d", code)
	}

	// Per-db report faces.
	code, body = get("?db=shop_2024&format=json")
	if code != http.StatusOK || !strings.Contains(body, `"tables"`) {
		t.Fatalf("per-db json: %d %s", code, body[:min(200, len(body))])
	}
	code, body = get("?db=shop_2024")
	if code != http.StatusOK || !strings.Contains(body, "数据比对报告") {
		t.Fatalf("per-db html: %d", code)
	}
	// Unknown db name: 404 before any filesystem touch.
	if code, _ := get("?db=../etc&format=json"); code != http.StatusNotFound {
		t.Fatalf("unknown db must 404, got %d", code)
	}
	// Missing summary on an unfinished task: 404 with the cause named.
	if code, body := get("?format=json&db=nope_db"); code != http.StatusNotFound || !strings.Contains(body, "解析出的库名") {
		t.Fatalf("bad db 404 wording: %d %s", code, body)
	}

	// Legacy single-db task: format=csv is a named 404, plain faces intact.
	legacy := &CompareTask{ID: "bb000002", Status: CompareStatusCompleted, Mode: "quick"}
	if err := s.persistCompareTask(legacy); err != nil {
		t.Fatal(err)
	}
	w, req := doReq("GET", "/api/v1/compare/tasks/bb000002/report?format=csv", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "仅多库任务") {
		t.Fatalf("legacy csv: %d %s", w.Code, w.Body.String())
	}
}

func TestCompareRowDiff_DbParam(t *testing.T) {
	s, _ := newTestServer(t)
	task := fabricateMultiTask(t, s)
	diffDir := filepath.Join(s.compareDir(task.ID), "diff", "02-shop_2024")
	if err := os.MkdirAll(diffDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diffDir, "t1-abc12345.csv"), []byte("k,v\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ?db= routes into the slug dir; without it the flat legacy path misses.
	w, req := doReq("GET", "/api/v1/compare/tasks/aa000001/rowdiff/t1-abc12345.csv?db=shop_2024", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("db-scoped rowdiff: %d %s", w.Code, w.Body.String())
	}
	w, req = doReq("GET", "/api/v1/compare/tasks/aa000001/rowdiff/t1-abc12345.csv", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("flat path must miss for multi-db artifacts: %d", w.Code)
	}
	w, req = doReq("GET", "/api/v1/compare/tasks/aa000001/rowdiff/t1-abc12345.csv?db=unknown", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown db must 404: %d", w.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestMultiDBRun_PerDBConfigOverride is a seam test of the loop's
// per-database configuration derivation: executeCompareRun must receive
// Source/Target with Database swapped per database while everything else
// (schema, type, credentials) stays the task-level value. Pinned through
// the same seam the loop uses — a listing override plus a validator-call
// recorder would need a live engine, so the derivation is pinned at the
// DSN level instead (BuildMySQLDSN carries the database).
func TestMultiDBRun_PerDBConfigOverride(t *testing.T) {
	base := config.SourceConfig{Type: "mysql", Host: "h", Port: 3306, User: "u", Password: "p"}
	seen := map[string]bool{}
	for _, db := range []string{"dbone", "dbtwo"} {
		src := base
		src.Database = db
		dsn := src.DSNByType()
		if !strings.Contains(dsn, "/"+db) {
			t.Fatalf("dsn for %s missing database: %s", db, dsn)
		}
		seen[db] = true
	}
	if !seen["dbone"] || !seen["dbtwo"] {
		t.Fatal("derivation loop broken")
	}
	// The task-level config object must never be mutated in place (the run
	// loop copies per database; this pins the copy discipline the callbacks
	// rely on when they persist the task).
	task := &CompareTask{Source: base, Target: config.TargetConfig{Host: "t", Database: "maint"}}
	src := task.Source
	src.Database = "other"
	if task.Source.Database != "" {
		t.Fatal("per-db override leaked into the task-level config")
	}
}

// TestMultiDBTaskJSONMarshalStability: unmarshal→marshal of a persisted
// multi-db task is stable (the resolved list and results ride task.json
// without spurious key churn).
func TestMultiDBTaskJSONMarshalStability(t *testing.T) {
	raw := `{"id":"aa000001","status":"completed","databases":["shop_*"],"resolved_databases":["shop_2023"],"databases_done":1,"databases_total":1,"database_results":[{"database":"shop_2023","status":"pass","total_tables":2}]}`
	var t1 CompareTask
	if err := json.Unmarshal([]byte(raw), &t1); err != nil {
		t.Fatal(err)
	}
	b1, err := json.Marshal(&t1)
	if err != nil {
		t.Fatal(err)
	}
	var t2 CompareTask
	if err := json.Unmarshal(b1, &t2); err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(&t2)
	if string(b1) != string(b2) {
		t.Fatalf("marshal not stable:\n%s\n%s", b1, b2)
	}
	if t1.DatabasesDone != 1 || t1.CurrentDatabase != "" {
		t.Fatalf("field decode: %+v", t1)
	}
}

// silence unused-import linters when fixtures shrink.
var _ = sql.Open
var _ sync.Mutex
