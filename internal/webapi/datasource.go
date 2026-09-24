package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"go.uber.org/zap"
)

// Unified datasource registry (F-02): named connection profiles persisted
// server-side in dataDir/datasources.json (0600). Passwords live ONLY here on
// the server — every API response redacts them (write-only semantics, same
// contract as /cdc/config): create/update accept a password, GET/PUT responses
// never return one, and an empty/absent password on update means "keep the
// stored value". Task/compare/DDL/assess/CDC flows reference a datasource by
// id; the server resolves the ref into a connection SNAPSHOT at creation time,
// so deleting a datasource never affects already-created or running tasks.

// dsMu serializes all read→modify→write cycles on datasources.json.
var dsMu sync.Mutex

// dataSourceEntry is the persisted shape (fields INCLUDE the password).
type dataSourceEntry struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"` // postgres | mysql | tidb
	Fields     map[string]any `json:"fields"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	LastTested *time.Time     `json:"last_tested,omitempty"`
	LastTestOK bool           `json:"last_test_ok"`
}

// dataSourceView is the API shape: identical to the entry but the password
// field is stripped and reported as has_password instead.
type dataSourceView struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Fields      map[string]any `json:"fields"`
	HasPassword bool           `json:"has_password"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	LastTested  *time.Time     `json:"last_tested,omitempty"`
	LastTestOK  bool           `json:"last_test_ok"`
}

func viewDataSource(e *dataSourceEntry) dataSourceView {
	fields := make(map[string]any, len(e.Fields))
	for k, v := range e.Fields {
		if k == "password" {
			continue
		}
		fields[k] = v
	}
	_, hasPwd := e.Fields["password"]
	return dataSourceView{
		ID: e.ID, Name: e.Name, Type: e.Type, Fields: fields,
		HasPassword: hasPwd,
		CreatedAt:   e.CreatedAt, UpdatedAt: e.UpdatedAt,
		LastTested: e.LastTested, LastTestOK: e.LastTestOK,
	}
}

// dataSourceTypes are the allowed datasource types (D3: postgres+tidb first,
// mysql released in the same batch).
var dataSourceTypes = map[string]int{
	"postgres": 5432,
	"mysql":    3306,
	"tidb":     4000,
}

func (s *Server) datasourcesFile() string { return s.dataDir + "/datasources.json" }

// cleanupDataSourceTempFiles removes crash-orphaned "datasources-*.tmp" files
// at startup: they contain the full plaintext registry and would otherwise
// linger forever (P2-6).
func (s *Server) cleanupDataSourceTempFiles() {
	matches, err := filepath.Glob(s.dataDir + "/datasources-*.tmp")
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			zap.L().Warn("failed to remove orphaned datasource temp file", zap.String("file", m), zap.Error(err))
		} else {
			zap.L().Info("removed orphaned datasource temp file", zap.String("file", m))
		}
	}
}

// loadDataSources reads the registry; missing/corrupt file → empty list (the
// next successful save rewrites it — same self-heal as migration-options).
func (s *Server) loadDataSources() []dataSourceEntry {
	raw, err := os.ReadFile(s.datasourcesFile())
	if err != nil {
		return nil
	}
	var list []dataSourceEntry
	if err := json.Unmarshal(raw, &list); err != nil {
		zap.L().Warn("datasources.json corrupted, ignoring saved datasources",
			zap.String("file", s.datasourcesFile()), zap.Error(err))
		return nil
	}
	return list
}

// saveDataSources persists the registry atomically with 0600 permissions (D2):
// temp file in dataDir → chmod 0600 → rename. The file holds plaintext
// passwords, so group/other read bits must never be set (on Windows the mode
// bits are advisory only; the deployment target is Linux).
func (s *Server) saveDataSources(list []dataSourceEntry) error {
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dataDir, "datasources-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.datasourcesFile()); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// resolveDataSourceRef returns a copy of the entry for a datasource id.
func (s *Server) resolveDataSourceRef(ref string) (*dataSourceEntry, error) {
	dsMu.Lock()
	list := s.loadDataSources()
	dsMu.Unlock()
	for i := range list {
		if list[i].ID == ref {
			e := list[i]
			return &e, nil
		}
	}
	return nil, fmt.Errorf("datasource %q not found", ref)
}

// dsRequestBody is the POST/PUT /datasources body.
type dsRequestBody struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Fields map[string]any `json:"fields"`
}

func validateDataSourceBody(name, dsType string, fields map[string]any) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required")
	}
	defPort, ok := dataSourceTypes[dsType]
	if !ok {
		return fmt.Errorf("type must be one of postgres/mysql/tidb")
	}
	if stringField(fields, "host") == "" {
		return fmt.Errorf("host is required")
	}
	if p := stringField(fields, "port"); p != "" {
		var port int
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("port must be 1-65535")
		}
	}
	_ = defPort
	return nil
}

func (s *Server) handleListDataSources(w http.ResponseWriter, r *http.Request) {
	dsMu.Lock()
	list := s.loadDataSources()
	dsMu.Unlock()
	views := make([]dataSourceView, 0, len(list))
	for i := range list {
		views = append(views, viewDataSource(&list[i]))
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"datasources": views})
}

func (s *Server) handleCreateDataSource(w http.ResponseWriter, r *http.Request) {
	var req dsRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Type = strings.TrimSpace(req.Type)
	if err := validateDataSourceBody(req.Name, req.Type, req.Fields); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	entry := dataSourceEntry{
		Name: strings.TrimSpace(req.Name), Type: req.Type,
		Fields: normalizeDSFields(req.Fields, nil), CreatedAt: now, UpdatedAt: now,
	}

	dsMu.Lock()
	list := s.loadDataSources()
	for _, e := range list {
		if e.Name == entry.Name {
			dsMu.Unlock()
			s.writeError(w, http.StatusConflict, fmt.Sprintf("数据源名称 %q 已存在", entry.Name))
			return
		}
	}
	// P3-8: uuid[:8] is only 32 bits — regenerate on an id collision instead
	// of writing a duplicate id (update/delete/resolve would hit the first).
	for {
		entry.ID = uuid.New().String()[:8]
		dup := false
		for _, e := range list {
			if e.ID == entry.ID {
				dup = true
				break
			}
		}
		if !dup {
			break
		}
	}
	list = append(list, entry)
	if err := s.saveDataSources(list); err != nil {
		dsMu.Unlock()
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	dsMu.Unlock()
	s.writeJSON(w, http.StatusCreated, viewDataSource(&entry))
}

// handleUpdateDataSource: PUT with write-only password semantics — a password
// that is empty or absent keeps the stored one; all other fields are replaced
// wholesale from the request body (absent ones become zero values), so clients
// must send the full object (the web UI always does).
func (s *Server) handleUpdateDataSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req dsRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	dsMu.Lock()
	defer dsMu.Unlock()
	list := s.loadDataSources()
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.writeError(w, http.StatusNotFound, "datasource not found")
		return
	}
	e := &list[idx]
	if strings.TrimSpace(req.Name) != "" {
		newName := strings.TrimSpace(req.Name)
		for i := range list {
			if i != idx && list[i].Name == newName {
				s.writeError(w, http.StatusConflict, fmt.Sprintf("数据源名称 %q 已存在", newName))
				return
			}
		}
		e.Name = newName
	}
	if req.Type != "" && req.Type != e.Type {
		s.writeError(w, http.StatusBadRequest, "type cannot be changed (create a new datasource instead)")
		return
	}
	if req.Fields != nil {
		e.Fields = normalizeDSFields(req.Fields, e.Fields)
	}
	if err := validateDataSourceBody(e.Name, e.Type, e.Fields); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	e.UpdatedAt = time.Now()
	if err := s.saveDataSources(list); err != nil {
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, viewDataSource(e))
}

// normalizeDSFields stringifies the loose JSON field map (numbers arrive as
// float64) and applies the write-only password rule: an empty, absent or
// non-string password keeps the previously stored one (old may be nil for
// create) — a non-string password value is never stringified into garbage.
func normalizeDSFields(fields, old map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if v == nil {
			continue
		}
		if k == "password" {
			if s, ok := v.(string); ok {
				out[k] = s
			}
			// non-string password: drop, falls through to the keep-old rule below
			continue
		}
		if s, ok := v.(string); ok {
			out[k] = s
		} else {
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	if pwd, ok := out["password"].(string); !ok || pwd == "" {
		if oldPwd, ok := old["password"].(string); ok && oldPwd != "" {
			out["password"] = oldPwd
		} else {
			delete(out, "password")
		}
	}
	return out
}

func (s *Server) handleDeleteDataSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	dsMu.Lock()
	defer dsMu.Unlock()
	list := s.loadDataSources()
	out := list[:0]
	found := false
	for _, e := range list {
		if e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "datasource not found")
		return
	}
	if err := s.saveDataSources(out); err != nil {
		s.writeError(w, http.StatusInternalServerError, "删除失败："+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// handleTestDataSource tests the stored credentials (server-side resolution:
// the password never travels to the browser) and records the outcome.
func (s *Server) handleTestDataSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var entry *dataSourceEntry
	dsMu.Lock()
	list := s.loadDataSources()
	for i := range list {
		if list[i].ID == id {
			entry = &list[i]
			break
		}
	}
	if entry == nil {
		dsMu.Unlock()
		s.writeError(w, http.StatusNotFound, "datasource not found")
		return
	}

	fields := make(map[string]any, len(entry.Fields))
	for k, v := range entry.Fields {
		fields[k] = v
	}
	dsMu.Unlock()

	var result map[string]interface{}
	if entry.Type == "tidb" {
		port := 4000
		if p := stringField(fields, "port"); p != "" {
			fmt.Sscanf(p, "%d", &port)
		}
		req := &TestConnectionRequest{
			Type: "target", Host: stringField(fields, "host"), Port: port,
			User: stringField(fields, "user"), Password: stringField(fields, "password"),
			Database: stringField(fields, "database"),
		}
		raw := s.testTiDBConnection(r.Context(), req)
		result = map[string]interface{}{
			"source":  "tidb",
			"success": raw["ok"] == true,
			"message": dsTestMessage(raw),
		}
		if v, ok := raw["version"].(string); ok && v != "" {
			result["version"] = v
		}
	} else {
		result = s.testSource(r.Context(), entry.Type, fields)
	}

	// Record the test outcome (success updates last_tested + ok flag).
	now := time.Now()
	dsMu.Lock()
	updated := s.loadDataSources()
	for i := range updated {
		if updated[i].ID == id {
			updated[i].LastTested = &now
			updated[i].LastTestOK = result["success"] == true
			if err := s.saveDataSources(updated); err != nil {
				zap.L().Warn("failed to persist datasource test outcome", zap.Error(err))
			}
			result["datasource"] = viewDataSource(&updated[i])
			break
		}
	}
	dsMu.Unlock()
	s.writeJSON(w, http.StatusOK, result)
}

func dsTestMessage(raw map[string]interface{}) string {
	if raw["ok"] == true {
		if v, ok := raw["version"].(string); ok && v != "" {
			return v
		}
		return "Connected"
	}
	if e, ok := raw["error"].(string); ok && e != "" {
		return e
	}
	return "connection failed"
}

// --- Ref resolution into connection configs (snapshots) ---

// dsDefaultPort returns the canonical default port for a datasource type.
func dsDefaultPort(dsType string) int {
	if p, ok := dataSourceTypes[dsType]; ok {
		return p
	}
	return 5432
}

// dataSourceToSourceConfig converts a datasource entry into a migration
// source config snapshot (defaults applied: port/schema/sslmode).
func dataSourceToSourceConfig(e *dataSourceEntry) (sc config.SourceConfig) {
	port := dsDefaultPort(e.Type)
	if p := stringField(e.Fields, "port"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	schema := stringField(e.Fields, "schema")
	if schema == "" {
		schema = "public"
	}
	sslmode := stringField(e.Fields, "sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	sc.Type = e.Type
	sc.Host = stringField(e.Fields, "host")
	sc.Port = port
	sc.User = stringField(e.Fields, "user")
	sc.Password = stringField(e.Fields, "password")
	sc.Database = stringField(e.Fields, "database")
	sc.Schema = schema
	sc.SSLMode = sslmode
	return sc
}

// dataSourceToTargetConfig converts a tidb datasource entry into a target
// config snapshot (used by wizard/compare/CDC import).
func dataSourceToTargetConfig(e *dataSourceEntry) (tc config.TargetConfig) {
	port := 4000
	if p := stringField(e.Fields, "port"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	tc.Host = stringField(e.Fields, "host")
	tc.Port = port
	tc.User = stringField(e.Fields, "user")
	tc.Password = stringField(e.Fields, "password")
	tc.Database = stringField(e.Fields, "database")
	return tc
}

// sortDataSourcesByName orders views for stable list rendering.
func sortDataSourcesByName(views []dataSourceView) {
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
}
