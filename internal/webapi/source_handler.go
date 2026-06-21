package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/michaelliuyuan/timstool/internal/source"
)

// handleSources lists all registered sources' metadata for the Web selector +
// schema-driven form (doc multi-source-web-form-design §6.1). Does not open any
// connection, so stubs are described too.
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"sources": source.DescribeAll()})
}

// handleSourceConfigSchema returns the connection-form fields for a source type.
// (Standalone helper; the form usually gets fields from /sources in one fetch.)
func (s *Server) handleSourceConfigSchema(w http.ResponseWriter, r *http.Request) {
	srcType := chi.URLParam(r, "type")
	meta, err := source.Describe(srcType)
	if err != nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("unknown source %q", srcType))
		return
	}
	s.writeJSON(w, http.StatusOK, meta.Fields)
}

// testSourceRequest is the {source, fields} body for the multi-source connection
// test (doc §6.2). source defaults to "postgres" when empty (backward compat).
type testSourceRequest struct {
	Source string         `json:"source"`
	Fields map[string]any `json:"fields"`
}

// versioner is an optional Source capability: the server version string echoed
// in the connection-test response (doc §6.2). PG/MySQL implement it; stubs
// don't, and the field is simply left empty for them.
type versioner interface {
	Version(ctx context.Context) (string, error)
}

// testSource opens the named source, builds SourceConfig from the field map, and
// pings. Shared by /sources/{type}/test and /test-connection. Returns the
// {success, message, version} result map; stubs get a friendly "not implemented"
// without a real connection attempt.
func (s *Server) testSource(ctx context.Context, srcType string, fields map[string]any) map[string]interface{} {
	if srcType == "" {
		srcType = "postgres"
	}
	result := map[string]interface{}{"source": srcType}

	meta, err := source.Describe(srcType)
	if err != nil {
		result["success"] = false
		result["message"] = fmt.Sprintf("unknown source %q", srcType)
		return result
	}
	if !meta.Implemented {
		msg := meta.NotImplMsg
		if msg == "" {
			msg = fmt.Sprintf("source %q is not implemented yet", srcType)
		}
		result["success"] = false
		result["message"] = msg
		return result
	}

	cfg := source.SourceConfig{
		Kind:     srcType,
		Host:     stringField(fields, "host"),
		User:     stringField(fields, "user"),
		Password: stringField(fields, "password"),
		Database: stringField(fields, "database"),
		Schema:   stringField(fields, "schema"),
		Options:  stringifyMap(fields),
	}
	if port := stringField(fields, "port"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}

	src, err := source.Open(srcType, cfg)
	if err != nil {
		result["success"] = false
		result["message"] = fmt.Sprintf("source %q not available: %v", srcType, err)
		return result
	}
	defer src.Close()

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := src.Connect(cctx); err != nil {
		result["success"] = false
		result["message"] = err.Error()
		return result
	}

	result["success"] = true
	result["message"] = fmt.Sprintf("Connected to %s %s:%d", src.Name(), cfg.Host, cfg.Port)
	if v, ok := src.(versioner); ok {
		if ver, err := v.Version(cctx); err == nil {
			result["version"] = strings.TrimSpace(ver)
		}
	}
	return result
}

// handleSourceTest tests a source connection via the per-type URL (#t68 path).
// Body = field map.
func (s *Server) handleSourceTest(w http.ResponseWriter, r *http.Request) {
	srcType := chi.URLParam(r, "type")
	var fields map[string]any
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	s.writeJSON(w, http.StatusOK, s.testSource(r.Context(), srcType, fields))
}

// handleTestConnectionMulti is the canonical multi-source connection test
// (doc §6.2): body {source, fields}, source defaults to postgres. Response
// {success, message, version}.
func (s *Server) handleTestConnectionMulti(w http.ResponseWriter, r *http.Request) {
	var req testSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	s.writeJSON(w, http.StatusOK, s.testSource(r.Context(), req.Source, req.Fields))
}

// handleSourceTables lists a source's tables via the adapter's SchemaReader
// (multi-source; #t79 Phase 1). Body {source, fields}. This is the real fix for
// the MySQL "选择表" failure (the PG-only /config/list-tables sent pgx at the
// MySQL server). row_estimate is -1 (unknown) — CIR doesn't carry row counts;
// PG keeps its dedicated endpoint with reltuples estimates for zero-regression.
func (s *Server) handleSourceTables(w http.ResponseWriter, r *http.Request) {
	var req testSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	srcType := req.Source
	if srcType == "" {
		srcType = "postgres"
	}
	meta, err := source.Describe(srcType)
	if err != nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("unknown source %q", srcType))
		return
	}
	if !meta.Implemented {
		s.writeError(w, http.StatusBadRequest, meta.NotImplMsg)
		return
	}

	cfg := source.SourceConfig{
		Kind:     srcType,
		Host:     stringField(req.Fields, "host"),
		User:     stringField(req.Fields, "user"),
		Password: stringField(req.Fields, "password"),
		Database: stringField(req.Fields, "database"),
		Schema:   stringField(req.Fields, "schema"),
		Options:  stringifyMap(req.Fields),
	}
	if port := stringField(req.Fields, "port"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}

	src, err := source.Open(srcType, cfg)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "open source: "+err.Error())
		return
	}
	defer src.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := src.Connect(ctx); err != nil {
		s.writeError(w, http.StatusInternalServerError, "connect: "+err.Error())
		return
	}
	cir, err := src.SchemaReader().ReadSchema(ctx, source.Filter{})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "read schema: "+err.Error())
		return
	}

	type tableInfo struct {
		Name        string `json:"name"`
		RowEstimate int64  `json:"row_estimate"`
	}
	tables := make([]tableInfo, 0, len(cir.Tables))
	for _, t := range cir.Tables {
		tables = append(tables, tableInfo{Name: t.Name, RowEstimate: -1})
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"tables": tables, "count": len(tables)})
}

// stringField reads a string-valued field from the loose JSON map (numbers
// arrive as float64).
func stringField(fields map[string]any, key string) string {
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// stringifyMap converts the loose field map to map[string]string (for
// SourceConfig.Options) so source-specific DSN builders can read their keys
// (charset/sslmode/...).
func stringifyMap(fields map[string]any) map[string]string {
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		if v == nil {
			continue
		}
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}
