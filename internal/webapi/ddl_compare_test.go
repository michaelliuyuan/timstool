package webapi

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

	"github.com/michaelliuyuan/timstool/internal/ddlcompare"
)

// DDL 对比 (ddlcompare) handler tests: request validation, datasource
// type gating (source: mysql/postgres; target: tidb/mysql), the failed-run
// persistence path, history listing, the id allow-list gate and both
// report formats. Live-DB collection paths are covered by the engine
// tests + 联调 (103), not here.

func dcSeedDataSource(t *testing.T, s *Server, name, dsType string) string {
	t.Helper()
	list := s.loadDataSources()
	for _, e := range list {
		if e.Name == name {
			return e.ID
		}
	}
	entry := dataSourceEntry{
		ID: newDDLCompareID(), Name: name, Type: dsType,
		Fields: map[string]any{
			"host": "127.0.0.1", "port": "1", "user": "u", "password": "p", "database": "db",
		},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	list = append(list, entry)
	if err := s.saveDataSources(list); err != nil {
		t.Fatalf("seed datasource: %v", err)
	}
	return entry.ID
}

func TestDDLCompareValidation(t *testing.T) {
	s, _ := newTestServer(t)
	srcID := dcSeedDataSource(t, s, "dc-src", "postgres")

	cases := []struct {
		name, body string
		wantCode   int
	}{
		{"missing refs", `{"source_schema":"public","target_schema":"db"}`, 400},
		{"missing schemas", fmt.Sprintf(`{"source_ref":%q,"target_ref":%q}`, srcID, srcID), 400},
		{"bad source schema", fmt.Sprintf(`{"source_ref":%q,"target_ref":%q,"source_schema":"../etc","target_schema":"db"}`, srcID, srcID), 400},
		{"bad target schema", fmt.Sprintf(`{"source_ref":%q,"target_ref":%q,"source_schema":"public","target_schema":"a..b"}`, srcID, srcID), 400},
		{"unknown source ref", `{"source_ref":"deadbeef","target_ref":"deadbeef","source_schema":"public","target_schema":"db"}`, 400},
		{"invalid body", `{`, 400},
	}
	for _, c := range cases {
		w, req := doReq("POST", "/api/v1/ddl-compare", c.body)
		s.handleDDLCompareRun(w, req)
		if w.Code != c.wantCode {
			t.Errorf("%s: code = %d (%s), want %d", c.name, w.Code, w.Body.String(), c.wantCode)
		}
	}
}

func TestDDLCompareTypeGates(t *testing.T) {
	s, _ := newTestServer(t)
	srcID := dcSeedDataSource(t, s, "dc-src-ok", "mysql")
	pgID := dcSeedDataSource(t, s, "dc-pg", "postgres")
	tidbID := dcSeedDataSource(t, s, "dc-tidb", "tidb")
	mongoID := dcSeedDataSource(t, s, "dc-mongo", "mongodb")

	// Run: tidb/mongo cannot be the source; pg/mongo cannot be the target.
	badRuns := []struct {
		name, src, tgt string
	}{
		{"tidb as source", tidbID, tidbID},
		{"mongo as source", mongoID, tidbID},
		{"pg as target", pgID, pgID},
		{"mongo as target", srcID, mongoID},
	}
	for _, c := range badRuns {
		body := fmt.Sprintf(`{"source_ref":%q,"target_ref":%q,"source_schema":"db","target_schema":"db"}`, c.src, c.tgt)
		w, req := doReq("POST", "/api/v1/ddl-compare", body)
		s.handleDDLCompareRun(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d (%s), want 400", c.name, w.Code, w.Body.String())
		}
	}

	// Schemas probe: same gates per side + the side discriminator.
	probeCases := []struct {
		name, side, ref string
		wantCode        int
	}{
		{"tidb as probe source", "source", tidbID, 400},
		{"mongo as probe source", "source", mongoID, 400},
		{"pg as probe target", "target", pgID, 400},
		{"bogus side", "middle", pgID, 400},
	}
	for _, c := range probeCases {
		body := fmt.Sprintf(`{"side":%q,"ref":%q}`, c.side, c.ref)
		w, req := doReq("POST", "/api/v1/ddl-compare/schemas", body)
		s.handleDDLCompareSchemas(w, req)
		if w.Code != c.wantCode {
			t.Errorf("%s: code = %d (%s), want %d", c.name, w.Code, w.Body.String(), c.wantCode)
		}
	}
	// A mysql source probe passes the gate and fails at the (unreachable)
	// connection — 502, not 400.
	w, req := doReq("POST", "/api/v1/ddl-compare/schemas", fmt.Sprintf(`{"side":"source","ref":%q}`, srcID))
	s.handleDDLCompareSchemas(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("mysql source probe: code = %d, want 502 (unreachable host)", w.Code)
	}
}

func TestDDLCompareFailedRunPersists(t *testing.T) {
	s, _ := newTestServer(t)
	srcID := dcSeedDataSource(t, s, "dc-src2", "postgres")
	tgtID := dcSeedDataSource(t, s, "dc-tgt2", "tidb")

	w, req := doReq("POST", "/api/v1/ddl-compare",
		fmt.Sprintf(`{"name":"失败联调","source_ref":%q,"target_ref":%q,"source_schema":"public","target_schema":"db"}`, srcID, tgtID))
	s.handleDDLCompareRun(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("unreachable run: code = %d (%s), want 502", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	// The failed task is visible in history with its cause.
	w, req = doReq("GET", "/api/v1/ddl-compare/tasks", "")
	s.handleListDDLCompares(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var list struct {
		Tasks []*ddlCompareTask `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Tasks) != 1 {
		t.Fatalf("history has %d tasks, want 1", len(list.Tasks))
	}
	task := list.Tasks[0]
	if task.Status != ddlCompareStatusFailed || task.Error == "" {
		t.Errorf("task status/error = %q/%q, want failed/non-empty", task.Status, task.Error)
	}
	if strings.Contains(mustJSON(t, task), "password") {
		// no password field exists on the echo type; belt-and-braces.
		t.Error("task echo must never carry credentials")
	}
	// Report for a failed run: task exists, result does not → 404 named.
	w, req = doReq("GET", "/api/v1/ddl-compare/tasks/"+task.ID+"/report", "")
	req = withChiParam(req, "dcID", task.ID)
	s.handleDDLCompareReport(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("failed-run report: code = %d, want 404", w.Code)
	}
}

func dcSeedResult(t *testing.T) *ddlcompare.Result {
	t.Helper()
	return &ddlcompare.Result{
		GeneratedAt: time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC),
		Duration:    "1.2s",
		Options:     ddlcompare.Options{IgnoreCase: true, LooseTypes: true},
		Source:      ddlcompare.Endpoint{Kind: "source", Type: "postgres", Schema: "public"},
		Target:      ddlcompare.Endpoint{Kind: "target", Type: "tidb", Schema: "db"},
		Categories: []ddlcompare.Category{
			{
				Key: "tables", Label: "表", SourceCount: 2, TargetCount: 1, Matched: 1, Verdict: ddlcompare.VerdictHint,
				Tables: []ddlcompare.TableDiff{
					{Table: "users", Verdict: ddlcompare.VerdictHint,
						PKDiff:        &ddlcompare.PKDiff{Source: []string{"id"}, Target: []string{"id"}, Equal: true},
						CharsetDiff:   &ddlcompare.AttrDiff{Source: "UTF8", Target: "utf8mb4", Equal: true},
						CollationDiff: &ddlcompare.AttrDiff{Source: "en_US.UTF-8", Target: "utf8mb4_bin", Equal: false, Note: "跨方言排序规则词汇表不相交，仅提示（不判错）"}},
					{Table: "orders<script>", SourceOnly: true, Verdict: ddlcompare.VerdictFail},
				},
			},
			{Key: "views", Label: "视图", SourceCount: 1, TargetCount: 2, Matched: 1, Verdict: ddlcompare.VerdictFail,
				Names: &ddlcompare.NameSetDiff{SourceOnly: []string{"v_old"}, TargetOnly: []string{"v_new"}, Common: []string{"v_users"}}},
			{Key: "procedures", Label: "存储过程", SourceCount: 1, TargetCount: 1, Matched: 1, Verdict: ddlcompare.VerdictPass,
				Names: &ddlcompare.NameSetDiff{Common: []string{"sp_recalc"}}},
			{Key: "functions", Label: "自定义函数", SourceCount: 2, TargetCount: 1, Matched: 1, Verdict: ddlcompare.VerdictFail,
				Names: &ddlcompare.NameSetDiff{SourceOnly: []string{"fn_greet"}, Common: []string{"fn_tax"}}},
			{Key: "triggers", Label: "触发器", SourceCount: 1, TargetCount: 1, Matched: 1, Verdict: ddlcompare.VerdictPass,
				Names: &ddlcompare.NameSetDiff{Common: []string{"tg_audit"}}},
			{Key: "sequences", Label: "序列", SourceCount: 2, TargetCount: 0, Matched: 0, Verdict: ddlcompare.VerdictFail,
				Names: &ddlcompare.NameSetDiff{SourceOnly: []string{"seq_users", "seq_orders"}},
				Note:  "目标端无此对象类型（TiDB/MySQL 无 SEQUENCE 对象）：PG 源含序列时总体结论预期为 FAIL"},
			{Key: "users", Label: "用户", SourceCount: 2, TargetCount: 2, Matched: 2, Verdict: ddlcompare.VerdictPass,
				Names: &ddlcompare.NameSetDiff{Common: []string{"appuser", "reporter"}}},
			{Key: "privileges", Label: "权限", SourceCount: 2, TargetCount: 2, Matched: 0, Verdict: ddlcompare.VerdictHint,
				Note: "跨方言权限词汇表不相交，不做结论",
				Privileges: &ddlcompare.PrivSetDiff{Comparable: false, CrossDialect: true, Users: []ddlcompare.PrivUserDiff{
					{User: "appuser", Source: []string{"SUPER", "CREATEDB"}, Target: []string{"Select_priv", "Insert_priv"}, Equal: false},
					{User: "reporter", Source: []string{"MEMBER:reader"}, Target: []string{"Select_priv"}, Equal: false},
				}}},
		},
		Overall:  ddlcompare.VerdictFail,
		Warnings: []string{"目标端 tidb_enable_procedure 为 OFF：若已建的存储过程/自定义函数未显示，请先开启该开关"},
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestDDLCompareReportFaces(t *testing.T) {
	s, _ := newTestServer(t)
	id := newDDLCompareID()
	task := &ddlCompareTask{
		ID: id, Name: "报表联调", Status: ddlCompareStatusCompleted,
		SourceType: "postgres", TargetType: "tidb",
		SourceSchema: "public", TargetSchema: "migration_test",
		Options:     ddlcompare.Options{IgnoreCase: true, LooseTypes: true},
		SourceLabel: "h1/public", TargetLabel: "h2/migration_test",
		Overall: "fail", Duration: "1.2s", CreatedAt: time.Now(),
	}
	if err := os.MkdirAll(s.dcDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.MarshalIndent(task, "", "  ")
	if err := os.WriteFile(filepath.Join(s.dcDir(id), "task.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	res := dcSeedResult(t)
	resRaw, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(s.dcDir(id), "result.json"), resRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	// HTML face: family banner, eight categories, both-sides names, the
	// escaped one-sided table, the version badge and escaped warnings.
	w, req := doReq("GET", "/api/v1/ddl-compare/tasks/"+id+"/report", "")
	req = withChiParam(req, "dcID", id)
	s.handleDDLCompareReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("report html: %d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{
		"TiMS DDL 对比报告",
		"八类汇总",
		"存储过程", "自定义函数", "触发器", "序列", "用户", "权限",
		"v_old", "v_new", "v_users", "fn_greet", "sp_recalc", "tg_audit",
		"seq_users", "seq_orders",
		"目标端无此对象类型",
		"跨方言权限词汇表不相交，不做结论",
		"badge-hint", "提示", "跨方言提示类（不判错）",
		"SUPER", "CREATEDB", "Select_priv",
		"V3.20.0",
		"tidb_enable_procedure",
		"orders&lt;script&gt;",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html report missing %q", want)
		}
	}
	if strings.Contains(html, "<script>alert") {
		t.Error("html report leaked unescaped object name")
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("content-disposition = %q", cd)
	}

	// JSON face: verbatim result.json bytes.
	w, req = doReq("GET", "/api/v1/ddl-compare/tasks/"+id+"/report?format=json", "")
	req = withChiParam(req, "dcID", id)
	s.handleDDLCompareReport(w, req)
	if w.Code != http.StatusOK || w.Body.String() != string(resRaw) {
		t.Errorf("report json: code=%d byte-identical=%v", w.Code, w.Body.String() == string(resRaw))
	}

	// History lists the seeded completed task first.
	w, req = doReq("GET", "/api/v1/ddl-compare/tasks", "")
	s.handleListDDLCompares(w, req)
	if !strings.Contains(w.Body.String(), `"overall": "fail"`) && !strings.Contains(w.Body.String(), `"overall":"fail"`) {
		t.Errorf("history body: %s", w.Body.String())
	}
}

func TestDDLCompareIDGate(t *testing.T) {
	s, _ := newTestServer(t)
	for _, bad := range []string{"DEADBEEF", "..%2F", "deadbeefx", "deadbee", ""} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ddl-compare/tasks/"+bad+"/report", nil)
		req = withChiParam(req, "dcID", bad)
		gated := s.requireValidDDLCompareID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("gate let %q through to the handler", bad)
		}))
		w := httptest.NewRecorder()
		gated.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("gate %q: code = %d, want 404", bad, w.Code)
		}
	}
	// Unknown but well-formed id: still 404 (task.json missing).
	w, req := doReq("GET", "/api/v1/ddl-compare/tasks/deadbeef/report", "")
	req = withChiParam(req, "dcID", "deadbeef")
	s.handleDDLCompareReport(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown id: code = %d, want 404", w.Code)
	}
}

func TestDDLCompareHelpers(t *testing.T) {
	// objectCapExceeded names the offending side.
	big := &ddlcompare.Snapshot{Views: make([]string, ddlcompare.DefaultMaxObjects+1)}
	if msg := objectCapExceeded(big, &ddlcompare.Snapshot{}, 0); !strings.Contains(msg, "源端") {
		t.Errorf("cap message = %q, want source-side naming", msg)
	}
	if msg := objectCapExceeded(&ddlcompare.Snapshot{}, big, 0); !strings.Contains(msg, "目标端") {
		t.Errorf("cap message = %q, want target-side naming", msg)
	}
	if msg := objectCapExceeded(&ddlcompare.Snapshot{}, &ddlcompare.Snapshot{}, 0); msg != "" {
		t.Errorf("under cap: %q", msg)
	}

	// dcmpVerdictCN + dcmpModeLabel + dcmpJoin render.
	if dcmpVerdictCN(ddlcompare.VerdictWarn) != "警告" {
		t.Error("verdict CN mapping")
	}
	if !strings.Contains(dcmpModeLabel(ddlcompare.Options{LooseTypes: true, IgnoreCase: true}), "宽松类型") {
		t.Error("mode label")
	}
	if dcmpJoin(nil) != "—" || dcmpJoin([]string{"a", "b"}) != "a, b" {
		t.Error("join rendering")
	}
	if dcmpPK(nil) != "无主键" || dcmpPK([]string{"a", "b"}) != "a, b" {
		t.Error("pk rendering")
	}
}

func TestDDLCompareEndpointHostPort(t *testing.T) {
	// D4: result.json's Endpoint carries Host/Port for the JSON consumer
	// (the HTML face always had them via the task label). Numeric ports
	// arrive as float64 through the loose field map; string ports parse;
	// garbage degrades to 0 (Label() falls back to host/schema).
	num := &dataSourceEntry{Type: "postgres", Fields: map[string]any{
		"host": "10.0.0.1", "port": 5432, "user": "u", "password": "s3cret",
	}}
	ep := dcmpEndpoint("source", "postgres", num, "public")
	if ep.Kind != "source" || ep.Type != "postgres" || ep.Host != "10.0.0.1" || ep.Port != 5432 || ep.Schema != "public" {
		t.Errorf("numeric-port endpoint = %+v", ep)
	}
	if ep.Label() != "10.0.0.1:5432/public" {
		t.Errorf("Label = %q", ep.Label())
	}
	if raw := mustJSON(t, ep); strings.Contains(raw, "password") || strings.Contains(raw, "s3cret") {
		t.Errorf("endpoint leaked credentials: %s", raw)
	}

	str := &dataSourceEntry{Type: "tidb", Fields: map[string]any{"host": "h", "port": "4000"}}
	if ep := dcmpEndpoint("target", "tidb", str, "db"); ep.Port != 4000 {
		t.Errorf("string-port endpoint = %+v, want port 4000", ep)
	}
	junk := &dataSourceEntry{Type: "tidb", Fields: map[string]any{"host": "h", "port": "80x"}}
	if ep := dcmpEndpoint("target", "tidb", junk, "db"); ep.Port != 0 || ep.Host != "h" {
		t.Errorf("garbage-port endpoint = %+v, want port 0 host h", ep)
	}
}
