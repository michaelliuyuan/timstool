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

	"github.com/go-chi/chi/v5"
	"context"
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
