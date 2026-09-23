package webapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/ddlexport"
)

// ddlExportRequest is the payload for both /ddl-export/schemas and
// /ddl-export (the schema/type selections only matter for the latter).
type ddlExportRequest struct {
	Host     string          `json:"host"`
	Port     int             `json:"port"`
	User     string          `json:"user"`
	Password string          `json:"password"`
	Database string          `json:"database"`
	SSLMode  string          `json:"sslmode"`
	Schemas  []string        `json:"schemas"`
	Types    *ddlExportTypes `json:"types"`
	TiDB     bool            `json:"tidb"`
}

type ddlExportTypes struct {
	Tables     *bool `json:"tables"`
	Indexes    *bool `json:"indexes"`
	Views      *bool `json:"views"`
	Sequences  *bool `json:"sequences"`
	Functions  *bool `json:"functions"`
	Procedures *bool `json:"procedures"`
	Triggers   *bool `json:"triggers"`
	Types      *bool `json:"types"`
}

func (req *ddlExportRequest) source() config.SourceConfig {
	port := req.Port
	if port == 0 {
		port = 5432
	}
	sslmode := req.SSLMode
	if sslmode == "" {
		sslmode = "disable"
	}
	return config.SourceConfig{
		Host: req.Host, Port: port, User: req.User,
		Password: req.Password, Database: req.Database, SSLMode: sslmode,
	}
}

// openDDLSource validates the request, opens a PG connection and pings it
// (reuses the test-connection semantics).
func (s *Server) openDDLSource(w http.ResponseWriter, req *ddlExportRequest) (*sql.DB, bool) {
	if req.Host == "" || req.Database == "" {
		s.writeError(w, http.StatusBadRequest, "host and database are required")
		return nil, false
	}
	db, err := openPGTestConn(req.source().DSN())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return nil, false
	}
	return db, true
}

// handleDDLSchemas lists non-system schemas for the schema checkbox step (D3).
func (s *Server) handleDDLSchemas(w http.ResponseWriter, r *http.Request) {
	var req ddlExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	db, ok := s.openDDLSource(w, &req)
	if !ok {
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	schemas, err := ddlexport.ListSchemas(ctx, db)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "list schemas failed: "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"schemas": schemas})
}

// handleDDLExport streams the selected DDL as a zip download.
func (s *Server) handleDDLExport(w http.ResponseWriter, r *http.Request) {
	var req ddlExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	db, ok := s.openDDLSource(w, &req)
	if !ok {
		return
	}
	defer db.Close()

	ts := ddlexport.DefaultTypes()
	if req.Types != nil {
		ts = applyTypeSelection(ts, req.Types)
	}
	for _, sc := range req.Schemas {
		if err := ddlexport.ValidateSchemaName(sc); err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	exporter := ddlexport.NewExporter(db, ddlexport.Options{
		Schemas:     req.Schemas,
		Types:       ts,
		IncludeTiDB: req.TiDB,
	})

	// Buffer the whole archive first: the catalog walk happens inside
	// ExportZip and may fail, and a failure after the first byte would
	// otherwise surface as a truncated (unopenable) zip that looks like a
	// successful download. Only send headers once the export succeeded.
	var buf bytes.Buffer
	if _, err := exporter.ExportZip(ctx, &buf); err != nil {
		s.writeError(w, http.StatusInternalServerError, "export ddl failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=ddl-export-%s.zip", time.Now().Format("20060102-150405")))
	w.Write(buf.Bytes())
}

func applyTypeSelection(ts ddlexport.TypeSet, sel *ddlExportTypes) ddlexport.TypeSet {
	if sel.Tables != nil {
		ts.Tables = *sel.Tables
	}
	if sel.Indexes != nil {
		ts.Indexes = *sel.Indexes
	}
	if sel.Views != nil {
		ts.Views = *sel.Views
	}
	if sel.Sequences != nil {
		ts.Sequences = *sel.Sequences
	}
	if sel.Functions != nil {
		ts.Functions = *sel.Functions
	}
	if sel.Procedures != nil {
		ts.Procedures = *sel.Procedures
	}
	if sel.Triggers != nil {
		ts.Triggers = *sel.Triggers
	}
	if sel.Types != nil {
		ts.Types = *sel.Types
	}
	return ts
}
