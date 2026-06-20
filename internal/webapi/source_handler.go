package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/michaelliuyuan/timstool/internal/source"
)

// handleSources lists all registered source types for the Web selector (#t67 WSC).
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, source.RegisteredMeta())
}

// handleSourceConfigSchema returns the connection-form fields for a source type
// (the Web UI renders the dynamic form from this).
func (s *Server) handleSourceConfigSchema(w http.ResponseWriter, r *http.Request) {
	srcType := chi.URLParam(r, "type")
	fields := source.ConfigSchemaFor(srcType)
	if fields == nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("unknown or stub source %q", srcType))
		return
	}
	s.writeJSON(w, http.StatusOK, fields)
}

// handleSourceTest tests a source connection (#t67 WSC). Body = connection
// fields → source.Open(type,cfg) → Connect with timeout → success/failure.
func (s *Server) handleSourceTest(w http.ResponseWriter, r *http.Request) {
	srcType := chi.URLParam(r, "type")

	var fields map[string]string
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	cfg := source.SourceConfig{
		Kind:     srcType,
		Host:     fields["host"],
		User:     fields["user"],
		Password: fields["password"],
		Database: fields["database"],
		Schema:   fields["schema"],
		Options:  fields, // all fields available for source-specific parsing
	}
	if port := fields["port"]; port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}

	src, err := source.Open(srcType, cfg)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      false,
			"message": fmt.Sprintf("source %q not available: %v", srcType, err),
		})
		return
	}
	defer src.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := src.Connect(ctx); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": fmt.Sprintf("Connected to %s %s:%d", src.Name(), cfg.Host, cfg.Port),
	})
}
