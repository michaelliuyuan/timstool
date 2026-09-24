package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"context"
	"github.com/go-chi/chi/v5"
)

// Unified datasource registry (F-02): CRUD round-trip, write-only password
// semantics (never echoed; empty password on update keeps the stored one),
// 0600 file permissions, and source_ref resolution into task configs.

func dsBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(w.Body.String()), &m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

// withChiParam attaches a chi URL param to a test request (handler tests call
// the handlers directly, so the router never populates chi.URLParam).
func withChiParam(req *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return req
}

func TestDatasources_RoundTripAndPasswordRedaction(t *testing.T) {
	s, _ := newTestServer(t)

	// Empty list initially.
	w, req := doReq("GET", "/api/v1/datasources", "")
	s.handleListDataSources(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}

	// Create a postgres datasource with a password.
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "prod-pg", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "s3cret", "database": "app"}
	}`)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	created := dsBody(t, w)
	if created["name"] != "prod-pg" {
		t.Fatalf("unexpected name: %v", created["name"])
	}
	// The response must never contain the password (D2).
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("password leaked in create response: %s", w.Body.String())
	}
	if created["has_password"] != true {
		t.Fatalf("has_password missing: %s", w.Body.String())
	}
	fields := created["fields"].(map[string]any)
	if _, ok := fields["password"]; ok {
		t.Fatalf("fields contain a password key: %v", fields)
	}
	id := created["id"].(string)

	// GET list: redacted too.
	w, req = doReq("GET", "/api/v1/datasources", "")
	s.handleListDataSources(w, req)
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("password leaked in list response: %s", w.Body.String())
	}

	// PUT with an empty password keeps the stored one (write-only semantics).
	w, req = doReq("PUT", "/api/v1/datasources", `{"name": "prod-pg-2", "fields": {"host": "10.0.0.2"}}`)
	req = withChiParam(req, "id", id)
	s.handleUpdateDataSource(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("password leaked in update response: %s", w.Body.String())
	}

	// The stored entry still resolves WITH the password (server-side only).
	e, err := s.resolveDataSourceRef(id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if stringField(e.Fields, "password") != "s3cret" {
		t.Fatalf("stored password lost on empty-password update: %v", e.Fields)
	}
	if stringField(e.Fields, "host") != "10.0.0.2" {
		t.Fatalf("host not updated: %v", e.Fields)
	}
	if e.Name != "prod-pg-2" {
		t.Fatalf("name not updated: %v", e.Name)
	}

	// File permissions: 0600 on unix (Windows mode bits are advisory only).
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(s.dataDir, "datasources.json"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("datasources.json perms = %v, want 0600", fi.Mode().Perm())
		}
	}

	// Duplicate name is rejected.
	w, req = doReq("POST", "/api/v1/datasources", `{"name": "prod-pg-2", "type": "postgres", "fields": {"host": "x"}}`)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate name: %d", w.Code)
	}

	// Unknown type / missing host are rejected.
	w, req = doReq("POST", "/api/v1/datasources", `{"name": "bad", "type": "oracle", "fields": {"host": "x"}}`)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oracle type: %d", w.Code)
	}

	// DELETE removes it.
	w, req = doReq("DELETE", "/api/v1/datasources", "")
	req = withChiParam(req, "id", id)
	s.handleDeleteDataSource(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := s.resolveDataSourceRef(id); err == nil {
		t.Fatalf("deleted datasource still resolvable")
	}
}

func TestDatasources_TaskRefSnapshot(t *testing.T) {
	s, st := newTestServer(t)

	// Create source + target datasources.
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw1", "database": "db1"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw2", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)

	// Create a migration task by refs.
	body := fmt.Sprintf(`{"name": "ref-task", "source_ref": %q, "target_ref": %q, "opts": {}}`, srcID, tgtID)
	w, req = doReq("POST", "/api/v1/tasks", body)
	s.handleCreateTask(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create task: %d %s", w.Code, w.Body.String())
	}
	var task struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(w.Body.String()), &task)
	stored, err := st.GetTask(task.ID)
	if err != nil || stored == nil {
		t.Fatalf("task not stored: %v", err)
	}

	var cfg struct {
		Source struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			User     string `json:"user"`
			Password string `json:"password"`
			Type     string `json:"type"`
			Schema   string `json:"schema"`
			SSLMode  string `json:"sslmode"`
		} `json:"source"`
		Target struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Password string `json:"password"`
		} `json:"target"`
	}
	if err := json.Unmarshal([]byte(stored.ConfigJSON), &cfg); err != nil {
		t.Fatalf("task config: %v", err)
	}
	if cfg.Source.Host != "10.0.0.1" || cfg.Source.Password != "pw1" || cfg.Source.Schema != "public" || cfg.Source.SSLMode != "disable" {
		t.Fatalf("source snapshot wrong: %+v", cfg.Source)
	}
	if cfg.Target.Host != "10.0.0.9" || cfg.Target.Port != 4000 || cfg.Target.Password != "pw2" {
		t.Fatalf("target snapshot wrong: %+v", cfg.Target)
	}

	// Deleting the datasource must not affect the already-created task
	// (connection snapshot lives in the task config).
	w, req = doReq("DELETE", "/api/v1/datasources", "")
	req = withChiParam(req, "id", srcID)
	s.handleDeleteDataSource(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete source: %d", w.Code)
	}
	stored2, err := st.GetTask(task.ID)
	if err != nil || stored2 == nil {
		t.Fatalf("task gone after datasource delete: %v", err)
	}
	if !strings.Contains(stored2.ConfigJSON, "10.0.0.1") {
		t.Fatalf("task config lost the snapshot: %s", stored2.ConfigJSON)
	}

	// A dangling ref is rejected with a clear error, not an empty-config task.
	w, req = doReq("POST", "/api/v1/tasks", `{"name": "dangling", "source_ref": "nope", "target_ref": "nope", "opts": {}}`)
	s.handleCreateTask(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("dangling ref: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "source_ref") {
		t.Fatalf("dangling ref error should mention source_ref: %s", w.Body.String())
	}

	// Type mismatches: tidb as source, postgres as target.
	w, req = doReq("POST", "/api/v1/tasks", fmt.Sprintf(`{"name": "bad1", "source_ref": %q, "opts": {}}`, tgtID))
	s.handleCreateTask(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "tidb") {
		t.Fatalf("tidb-as-source: %d %s", w.Code, w.Body.String())
	}
}

func TestDatasources_DDLAndAssessRefTypeGate(t *testing.T) {
	s, _ := newTestServer(t)

	// A mysql datasource cannot drive DDL export / assess (PG-only flows).
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "my", "type": "mysql",
		"fields": {"host": "10.0.0.3", "port": 3306, "user": "root", "password": "pw", "database": "d"}
	}`)
	s.handleCreateDataSource(w, req)
	myID := dsBody(t, w)["id"].(string)

	w, req = doReq("POST", "/api/v1/ddl-export/schemas", fmt.Sprintf(`{"source_ref": %q}`, myID))
	s.handleDDLSchemas(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "PostgreSQL") {
		t.Fatalf("ddl mysql ref: %d %s", w.Code, w.Body.String())
	}

	w, req = doReq("POST", "/api/v1/assess", fmt.Sprintf(`{"source_ref": %q}`, myID))
	s.handleAssess(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "PostgreSQL") {
		t.Fatalf("assess mysql ref: %d %s", w.Code, w.Body.String())
	}
}

// P2-2: a non-string password value must NOT be stringified into garbage and
// overwrite the stored password — it is dropped and the keep-old rule applies.
func TestDatasources_NonStringPasswordKeepsStored(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "np", "type": "postgres",
		"fields": {"host": "h1", "user": "u", "password": "good-pw", "database": "d"}
	}`)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := dsBody(t, w)["id"].(string)

	// Object password → dropped, stored password preserved.
	w, req = doReq("PUT", "/api/v1/datasources", `{"fields": {"host": "h2", "password": {"deep": 1}}}`)
	req = withChiParam(req, "id", id)
	s.handleUpdateDataSource(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update object password: %d %s", w.Code, w.Body.String())
	}
	e, err := s.resolveDataSourceRef(id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if stringField(e.Fields, "password") != "good-pw" {
		t.Fatalf("non-string password clobbered stored one: %v", e.Fields["password"])
	}

	// A genuine new string password DOES replace the stored one (write-only).
	w, req = doReq("PUT", "/api/v1/datasources", `{"fields": {"host": "h2", "password": "new-pw"}}`)
	req = withChiParam(req, "id", id)
	s.handleUpdateDataSource(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update new password: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "new-pw") {
		t.Fatalf("password leaked in update response: %s", w.Body.String())
	}
	if v, ok := dsBody(t, w)["has_password"]; !ok || v != true {
		t.Fatalf("has_password must stay true: %s", w.Body.String())
	}
	e, err = s.resolveDataSourceRef(id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if stringField(e.Fields, "password") != "new-pw" {
		t.Fatalf("new password not stored: %v", e.Fields["password"])
	}
}

// P2-3: compare rejects non-postgres source refs (the validator speaks the PG
// wire protocol only) and rejects non-PG inline source types.
func TestDatasources_CompareSourceTypeGate(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "my2", "type": "mysql",
		"fields": {"host": "10.0.0.4", "port": 3306, "user": "root", "password": "pw", "database": "d"}
	}`)
	s.handleCreateDataSource(w, req)
	myID := dsBody(t, w)["id"].(string)

	w, req = doReq("POST", "/api/v1/compare", fmt.Sprintf(`{"source_ref": %q, "target": {"host": "t", "port": 4000}}`, myID))
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "postgres") {
		t.Fatalf("compare mysql ref: %d %s", w.Code, w.Body.String())
	}

	// Inline mysql source type is equally rejected.
	w, req = doReq("POST", "/api/v1/compare", `{"source": {"host": "s", "type": "mysql"}, "target": {"host": "t", "port": 4000}}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "postgres") {
		t.Fatalf("compare inline mysql: %d %s", w.Code, w.Body.String())
	}
}

// P2-5: the documented source_ref on the connection-test endpoints must be
// consumed server-side; a dangling ref 400s instead of silently testing empty
// fields.
func TestDatasources_TestConnectionSourceRef(t *testing.T) {
	s, _ := newTestServer(t)

	// Dangling ref on /test-connection (multi-source).
	w, req := doReq("POST", "/api/v1/test-connection", `{"source_ref": "nope"}`)
	s.handleTestConnectionMulti(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "source_ref") {
		t.Fatalf("dangling multi test ref: %d %s", w.Code, w.Body.String())
	}

	// Dangling ref on /config/test-connection.
	w, req = doReq("POST", "/api/v1/config/test-connection", `{"type": "source", "source_ref": "nope"}`)
	s.handleTestConnection(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "source_ref") {
		t.Fatalf("dangling config test ref: %d %s", w.Code, w.Body.String())
	}

	// A mysql ref on the PG-only /config/test-connection is rejected with a
	// pointer to the multi-source endpoint.
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "my3", "type": "mysql",
		"fields": {"host": "10.0.0.5", "port": 3306, "user": "root", "password": "pw", "database": "d"}
	}`)
	s.handleCreateDataSource(w, req)
	myID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/config/test-connection", fmt.Sprintf(`{"type": "source", "source_ref": %q}`, myID))
	s.handleTestConnection(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "postgres") {
		t.Fatalf("mysql ref on PG test endpoint: %d %s", w.Code, w.Body.String())
	}
}

// P2-6: crash-orphaned datasources-*.tmp files are removed at server start.
func TestDatasources_TempFileCleanup(t *testing.T) {
	s, _ := newTestServer(t)
	// A registry must exist so we can prove cleanup only touches tmp files.
	if err := os.WriteFile(s.datasourcesFile(), []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	orphan := filepath.Join(s.dataDir, "datasources-123456.tmp")
	if err := os.WriteFile(orphan, []byte(`[{"fields":{"password":"x"}}]`), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	s.cleanupDataSourceTempFiles()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned tmp file not removed (err=%v)", err)
	}
	// The registry itself is untouched.
	if _, err := os.Stat(s.datasourcesFile()); err != nil {
		t.Fatalf("datasources.json must survive cleanup: %v", err)
	}
}

// F-02c: tidb datasources accept optional pd_addr / status_port — they are
// echoed back on create, flow into target snapshots (trimmed / parsed), stay
// zero for legacy entries, and bad values are rejected at validation time.
func TestDatasources_TiDBPDAndStatusPort(t *testing.T) {
	s, _ := newTestServer(t)

	// ① Create a tidb source carrying both optional fields.
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "tgt-extras", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db",
			"pd_addr": " 10.0.0.9:2379 ", "status_port": 10080}
	}`)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	created := dsBody(t, w)
	fields := created["fields"].(map[string]any)
	if fields["pd_addr"] == nil || fields["status_port"] == nil {
		t.Fatalf("extras not echoed in view: %v", fields)
	}

	// ② The target snapshot carries PDAddr (trimmed) and StatusPort.
	e, err := s.resolveDataSourceRef(created["id"].(string))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	tc := dataSourceToTargetConfig(e)
	if tc.PDAddr != "10.0.0.9:2379" {
		t.Fatalf("PDAddr = %q, want trimmed 10.0.0.9:2379", tc.PDAddr)
	}
	if tc.StatusPort != 10080 {
		t.Fatalf("StatusPort = %d, want 10080", tc.StatusPort)
	}

	// ③ Legacy entries without the fields resolve to zero values.
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "tgt-legacy", "type": "tidb",
		"fields": {"host": "10.0.0.8", "port": 4000, "user": "root", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create legacy: %d %s", w.Code, w.Body.String())
	}
	legacy, _ := s.resolveDataSourceRef(dsBody(t, w)["id"].(string))
	tcl := dataSourceToTargetConfig(legacy)
	if tcl.PDAddr != "" || tcl.StatusPort != 0 {
		t.Fatalf("legacy entry must keep zero extras: %+v", tcl)
	}

	// ④ Validation: out-of-range / non-numeric status_port and malformed
	// pd_addr are rejected with 400.
	for _, body := range []string{
		`{"name": "bad1", "type": "tidb", "fields": {"host": "h", "port": 4000, "status_port": 70000}}`,
		`{"name": "bad2", "type": "tidb", "fields": {"host": "h", "port": 4000, "status_port": "abc"}}`,
		`{"name": "bad3", "type": "tidb", "fields": {"host": "h", "port": 4000, "pd_addr": "no-port-here"}}`,
	} {
		w, req = doReq("POST", "/api/v1/datasources", body)
		s.handleCreateDataSource(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid body accepted: %d %s (%s)", w.Code, w.Body.String(), body)
		}
	}
}

// Missing test ①: GET /tasks must never expose the task ConfigJSON (which
// holds plaintext passwords at rest) — the json:"-" tag is the only guard, so
// anchor it with an assertion a regression cannot silently pass.
func TestTasks_ListRedactsPasswords(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "src-x", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "task-pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "tgt-x", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "tpw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)

	w, req = doReq("POST", "/api/v1/tasks", fmt.Sprintf(`{"name": "t", "source_ref": %q, "target_ref": %q, "opts": {}}`, srcID, tgtID))
	s.handleCreateTask(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create task: %d %s", w.Code, w.Body.String())
	}

	w, req = doReq("GET", "/api/v1/tasks", "")
	s.handleListTasks(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list tasks: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "task-pw") || strings.Contains(w.Body.String(), "tpw") {
		t.Fatalf("password leaked in task list: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "config_json") || strings.Contains(w.Body.String(), "ConfigJSON") {
		t.Fatalf("config_json exposed in task list: %s", w.Body.String())
	}
}

// Missing test ⑤: CDC import-from-datasource — happy path writes config.yaml
// from refs and echoes a redacted summary; wrong types and dangling refs 400.
func TestDatasources_CDCImportFromDataSource(t *testing.T) {
	s, _ := newTestServer(t)
	// Wire a config.yaml target (missing file → zero config server-side).
	s.cdcCfgFilePath = filepath.Join(t.TempDir(), "config.yaml")

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "cdc-src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "cdc-pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "cdc-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "ctpw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)

	// Wrong types are rejected.
	w, req = doReq("POST", "/api/v1/cdc/import-from-datasource", fmt.Sprintf(`{"source_ref": %q, "target_ref": %q}`, tgtID, tgtID))
	s.handleImportCDCFromDataSource(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "PostgreSQL") {
		t.Fatalf("cdc import wrong source type: %d %s", w.Code, w.Body.String())
	}
	w, req = doReq("POST", "/api/v1/cdc/import-from-datasource", fmt.Sprintf(`{"source_ref": %q, "target_ref": %q}`, srcID, srcID))
	s.handleImportCDCFromDataSource(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "tidb") {
		t.Fatalf("cdc import wrong target type: %d %s", w.Code, w.Body.String())
	}
	// Dangling ref.
	w, req = doReq("POST", "/api/v1/cdc/import-from-datasource", `{"source_ref": "nope", "target_ref": "nope"}`)
	s.handleImportCDCFromDataSource(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "source_ref") {
		t.Fatalf("cdc import dangling ref: %d %s", w.Code, w.Body.String())
	}

	// Happy path: 200, redacted summary, and the password landed in config.yaml.
	w, req = doReq("POST", "/api/v1/cdc/import-from-datasource", fmt.Sprintf(`{"source_ref": %q, "target_ref": %q}`, srcID, tgtID))
	s.handleImportCDCFromDataSource(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cdc import: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "cdc-pw") || strings.Contains(w.Body.String(), "ctpw") {
		t.Fatalf("password leaked in cdc import response: %s", w.Body.String())
	}
	raw, err := os.ReadFile(s.cdcCfgFilePath)
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	if !strings.Contains(string(raw), "cdc-pw") || !strings.Contains(string(raw), "ctpw") {
		t.Fatalf("config.yaml missing imported credentials: %s", string(raw))
	}
}
