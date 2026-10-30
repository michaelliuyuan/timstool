// ddl_compare.go — the DDL comparison feature's webapi layer (DDL 对比校验):
// task lifecycle, datasource-ref resolution and the four endpoints. Runs
// are synchronous (structure queries are light; the chi 120s group
// governs the deadline — same F-10 ruling as ddl-export) and every run is
// persisted under dataDir/ddl-compare/{id}/ (task.json + result.json) so
// history, HTML report and JSON download survive the request.
//
// Passwords never persist: the task.json echo stores only the
// credential-free endpoint labels (compare-family policy); result.json is
// pure diff output.
package webapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/ddlcompare"
	"github.com/michaelliuyuan/timstool/internal/ddlexport"
	"github.com/michaelliuyuan/timstool/internal/source"
	"go.uber.org/zap"
)

// ddlCompareStatuses (synchronous run: only two terminal states exist).
const (
	ddlCompareStatusCompleted = "completed"
	ddlCompareStatusFailed    = "failed"
)

// ddlCompareRequest is the POST /ddl-compare body.
type ddlCompareRequest struct {
	Name         string             `json:"name"`
	SourceRef    string             `json:"source_ref"`
	TargetRef    string             `json:"target_ref"`
	SourceSchema string             `json:"source_schema"`
	TargetSchema string             `json:"target_schema"`
	Options      *ddlCompareOptions `json:"options"`
}

// ddlCompareOptions is the FE-facing options group (nil fields keep the
// defaults). v1.3 对比面收窄后 loose_types is accepted for v1.2 request
// compat but no longer influences any verdict (字段级对比取消, A1); the
// engine ignores it. index_by_name was dropped with the indexes class.
type ddlCompareOptions struct {
	IgnoreCase *bool `json:"ignore_case"`
	LooseTypes *bool `json:"loose_types"`
}

func (o *ddlCompareOptions) apply() ddlcompare.Options {
	opts := ddlcompare.Options{
		IgnoreCase: true,
		LooseTypes: true,
		MaxObjects: ddlcompare.DefaultMaxObjects,
	}
	if o == nil {
		return opts
	}
	if o.IgnoreCase != nil {
		opts.IgnoreCase = *o.IgnoreCase
	}
	if o.LooseTypes != nil {
		opts.LooseTypes = *o.LooseTypes
	}
	return opts
}

// ddlCompareTask is the persisted task record (credential-free).
type ddlCompareTask struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Status       string             `json:"status"`
	SourceType   string             `json:"source_type"`
	TargetType   string             `json:"target_type"`
	SourceSchema string             `json:"source_schema"`
	TargetSchema string             `json:"target_schema"`
	Options      ddlcompare.Options `json:"options"`
	SourceLabel  string             `json:"source_label"`
	TargetLabel  string             `json:"target_label"`
	Overall      string             `json:"overall,omitempty"`
	Error        string             `json:"error,omitempty"`
	Duration     string             `json:"duration,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
}

// ddlCompareSchemasRequest is the POST /ddl-compare/schemas body: one
// side's datasource ref (the UI probes source and target independently).
type ddlCompareSchemasRequest struct {
	Side string `json:"side"` // source | target
	Ref  string `json:"ref"`
}

// dcDir is one task's directory; dcIDRe is the {dcID} allow-list
// (compareIDRe twin: server-generated uuid[:8], 8 lowercase hex chars —
// every {dcID} route is gated before any filesystem touch).
func (s *Server) dcDir(id string) string { return filepath.Join(s.dataDir, "ddl-compare", id) }

var dcIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

func (s *Server) requireValidDDLCompareID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !dcIDRe.MatchString(chi.URLParam(r, "dcID")) {
			s.writeError(w, http.StatusNotFound, "ddl compare task not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loadDDLCompareTask reads one task.json.
func (s *Server) loadDDLCompareTask(id string) (*ddlCompareTask, error) {
	raw, err := os.ReadFile(filepath.Join(s.dcDir(id), "task.json"))
	if err != nil {
		return nil, err
	}
	var t ddlCompareTask
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("decode task.json: %w", err)
	}
	return &t, nil
}

// handleDDLCompareSchemas lists the comparable schemas/databases for one
// side's datasource ref.
func (s *Server) handleDDLCompareSchemas(w http.ResponseWriter, r *http.Request) {
	var req ddlCompareSchemasRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	e, err := s.resolveDataSourceRef(req.Ref)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("ref: %v", err))
		return
	}
	ctx := r.Context()
	switch req.Side {
	case "source":
		// Source eligibility mirrors DDL export (mysql+postgres): the same
		// metadata walk feeds both features.
		if !srcCapable(e.Type, source.CapDDLExport) {
			s.writeError(w, http.StatusBadRequest, "ref: 该数据源类型不能作为 DDL 对比的源端（支持 MySQL / PostgreSQL）")
			return
		}
		sc := dataSourceToSourceConfig(e)
		db, err := openSourceTestConn(sc)
		if err != nil {
			s.writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		defer db.Close()
		var schemas []string
		if source.IsMySQLWire(sc.SourceType()) {
			schemas, err = ddlcompare.ListMySQLSchemas(ctx, db)
		} else {
			schemas, err = ddlcompare.ListPGSchemas(ctx, db)
		}
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "list schemas failed: "+err.Error())
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"schemas": schemas})
	case "target":
		// Target eligibility = MySQL-wire kinds (tidb, mysql): the
		// registry predicate, no raw type branch (five-pin #3).
		if !source.IsMySQLWire(e.Type) {
			s.writeError(w, http.StatusBadRequest, "ref: 该数据源类型不能作为 DDL 对比的目标端（支持 TiDB / MySQL）")
			return
		}
		tc := dataSourceToTargetConfig(e)
		db, err := openDDLCompareTarget(tc)
		if err != nil {
			s.writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		defer db.Close()
		schemas, err := ddlcompare.ListMySQLSchemas(ctx, db)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "list databases failed: "+err.Error())
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"schemas": schemas})
	default:
		s.writeError(w, http.StatusBadRequest, "side must be source or target")
	}
}

// openDDLCompareTarget opens + pings a MySQL-protocol target (TiDB or
// MySQL) for the DDL compare walk (openSourceTestConn twin).
func openDDLCompareTarget(tc config.TargetConfig) (*sql.DB, error) {
	conn, err := sql.Open("mysql", tc.DSN())
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping failed: %w", err)
	}
	return conn, nil
}

// handleDDLCompareRun executes one comparison synchronously and persists
// the task + result. Structural collection errors fail the run (task
// persisted as failed — visible in history with the cause); per-class
// degradations surface as result.warnings.
func (s *Server) handleDDLCompareRun(w http.ResponseWriter, r *http.Request) {
	var req ddlCompareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SourceRef == "" || req.TargetRef == "" {
		s.writeError(w, http.StatusBadRequest, "source_ref and target_ref are required")
		return
	}
	if req.SourceSchema == "" || req.TargetSchema == "" {
		s.writeError(w, http.StatusBadRequest, "source_schema and target_schema are required")
		return
	}
	if err := ddlexport.ValidateSchemaName(req.SourceSchema); err != nil {
		s.writeError(w, http.StatusBadRequest, "source_schema: "+err.Error())
		return
	}
	if err := ddlexport.ValidateSchemaName(req.TargetSchema); err != nil {
		s.writeError(w, http.StatusBadRequest, "target_schema: "+err.Error())
		return
	}
	srcEntry, err := s.resolveDataSourceRef(req.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if !srcCapable(srcEntry.Type, source.CapDDLExport) {
		s.writeError(w, http.StatusBadRequest, "source_ref: 该数据源类型不能作为 DDL 对比的源端（支持 MySQL / PostgreSQL）")
		return
	}
	tgtEntry, err := s.resolveDataSourceRef(req.TargetRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "target_ref: "+err.Error())
		return
	}
	if !source.IsMySQLWire(tgtEntry.Type) {
		s.writeError(w, http.StatusBadRequest, "target_ref: 该数据源类型不能作为 DDL 对比的目标端（支持 TiDB / MySQL）")
		return
	}

	opts := req.Options.apply()
	start := time.Now()
	// uuid[:8] with collision regenerate (dataSource id twin).
	id := newDDLCompareID()
	for {
		if _, err := os.Stat(s.dcDir(id)); os.IsNotExist(err) {
			break
		}
		id = newDDLCompareID()
	}

	srcCfg := dataSourceToSourceConfig(srcEntry)
	if source.IsMySQLWire(srcCfg.SourceType()) {
		// MySQL: the compared schema IS the connection database.
		srcCfg.Database = req.SourceSchema
	} else {
		// PG connects per-database; the picked schema rides the Schema
		// field while Database stays the connection database.
		srcCfg.Schema = req.SourceSchema
		if srcCfg.Database == "" {
			srcCfg.Database = "postgres"
		}
	}
	tgtCfg := dataSourceToTargetConfig(tgtEntry)
	tgtCfg.Database = req.TargetSchema

	task := &ddlCompareTask{
		ID: id, Name: req.Name, Status: ddlCompareStatusFailed,
		SourceType: srcEntry.Type, TargetType: tgtEntry.Type,
		SourceSchema: req.SourceSchema, TargetSchema: req.TargetSchema,
		Options:     opts,
		SourceLabel: dcmpEndpointLabel(srcEntry, req.SourceSchema),
		TargetLabel: dcmpEndpointLabel(tgtEntry, req.TargetSchema),
		CreatedAt:   start,
	}
	s.persistDDLCompareTask(task)

	ctx := r.Context()
	srcDB, err := openSourceTestConn(srcCfg)
	if err != nil {
		s.failDDLCompare(w, task, start, err)
		return
	}
	defer srcDB.Close()
	tgtDB, err := openDDLCompareTarget(tgtCfg)
	if err != nil {
		s.failDDLCompare(w, task, start, err)
		return
	}
	defer tgtDB.Close()

	// Cap precheck (D2): cheap top-level COUNTs fail a monster catalog
	// BEFORE the heavy column/index walk. A precheck query the server
	// cannot answer (e.g. a TiDB build without information_schema.ROUTINES)
	// is non-fatal — the exact post-collection check below remains.
	if over := precheckObjectCap(ctx, source.IsMySQLWire(srcCfg.SourceType()), srcDB, tgtDB, req.SourceSchema, req.TargetSchema, opts.MaxObjects); over != "" {
		s.writeError(w, http.StatusBadRequest, over)
		os.RemoveAll(s.dcDir(id))
		return
	}

	var srcSnap, tgtSnap *ddlcompare.Snapshot
	var warnings []string
	if source.IsMySQLWire(srcCfg.SourceType()) {
		srcSnap, warnings, err = ddlcompare.CollectMySQL(ctx, srcDB, req.SourceSchema)
	} else {
		srcSnap, warnings, err = ddlcompare.CollectPG(ctx, srcDB, req.SourceSchema)
	}
	if err != nil {
		s.failDDLCompare(w, task, start, fmt.Errorf("源端采集失败: %w", err))
		return
	}
	var tgtWarns []string
	tgtSnap, tgtWarns, err = ddlcompare.CollectMySQL(ctx, tgtDB, req.TargetSchema)
	if err != nil {
		s.failDDLCompare(w, task, start, fmt.Errorf("目标端采集失败: %w", err))
		return
	}
	for _, wn := range tgtWarns {
		warnings = append(warnings, "目标端 "+wn)
	}
	// Operational hint (design v1.1): a TiDB target with the
	// procedure/function switch off may hide built objects.
	if source.IsTiDB(tgtEntry.Type) {
		if off, ok := tidbProcedureSwitchOff(ctx, tgtDB); ok && off {
			warnings = append(warnings, "目标端 tidb_enable_procedure 为 OFF：若已建的存储过程/自定义函数未显示，请先开启该开关")
		}
	}
	if over := objectCapExceeded(srcSnap, tgtSnap, opts.MaxObjects); over != "" {
		s.writeError(w, http.StatusBadRequest, over)
		os.RemoveAll(s.dcDir(id))
		return
	}

	res := ddlcompare.Diff(srcSnap, tgtSnap, opts)
	res.GeneratedAt = start
	res.Duration = durationStr(time.Since(start))
	res.Source = dcmpEndpoint("source", srcEntry.Type, srcEntry, req.SourceSchema)
	res.Target = dcmpEndpoint("target", tgtEntry.Type, tgtEntry, req.TargetSchema)
	res.Warnings = warnings

	// Persist the result BEFORE flipping the task to completed (D7): a
	// crash between the writes leaves the creation-default failed task
	// (report 404 names it honestly) instead of a "completed" history
	// entry whose report can never resolve.
	if err := s.persistDDLCompareResult(id, res); err != nil {
		// The run succeeded but persistence failed — fail honestly (the
		// report/history faces read from disk).
		s.failDDLCompare(w, task, start, fmt.Errorf("persist result: %w", err))
		return
	}
	task.Status = ddlCompareStatusCompleted
	task.Overall = string(res.Overall)
	task.Duration = res.Duration
	task.Error = ""
	s.persistDDLCompareTask(task)
	zap.L().Info("ddl compare completed", zap.String("id", id), zap.String("overall", string(res.Overall)))
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"task_id": id, "result": res})
}

// failDDLCompare marks the task failed (cause recorded) and answers 502 —
// collection/connect failures are upstream problems, not client ones.
func (s *Server) failDDLCompare(w http.ResponseWriter, task *ddlCompareTask, start time.Time, err error) {
	task.Status = ddlCompareStatusFailed
	task.Error = err.Error()
	task.Duration = durationStr(time.Since(start))
	s.persistDDLCompareTask(task)
	zap.L().Warn("ddl compare failed", zap.String("id", task.ID), zap.Error(err))
	s.writeError(w, http.StatusBadGateway, err.Error())
}

func (s *Server) persistDDLCompareTask(task *ddlCompareTask) {
	if err := os.MkdirAll(s.dcDir(task.ID), 0o755); err != nil {
		zap.L().Warn("ddl compare task persist failed", zap.String("id", task.ID), zap.Error(err))
		return
	}
	raw, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(s.dcDir(task.ID), "task.json"), raw, 0o600); err != nil {
		zap.L().Warn("ddl compare task persist failed", zap.String("id", task.ID), zap.Error(err))
	}
}

func (s *Server) persistDDLCompareResult(id string, res *ddlcompare.Result) error {
	if err := os.MkdirAll(s.dcDir(id), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dcDir(id), "result.json"), raw, 0o600)
}

// handleListDDLCompares returns history summaries, newest first.
func (s *Server) handleListDDLCompares(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "ddl-compare"))
	if err != nil {
		if os.IsNotExist(err) {
			s.writeJSON(w, http.StatusOK, map[string]interface{}{"tasks": []*ddlCompareTask{}})
			return
		}
		s.writeError(w, http.StatusInternalServerError, "list ddl compare tasks failed: "+err.Error())
		return
	}
	var tasks []*ddlCompareTask
	for _, e := range entries {
		if !e.IsDir() || !dcIDRe.MatchString(e.Name()) {
			continue
		}
		t, err := s.loadDDLCompareTask(e.Name())
		if err != nil {
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"tasks": tasks})
}

// handleDDLCompareReport serves one task's report: format=json streams
// the verbatim result.json; html (default) renders the family report.
func (s *Server) handleDDLCompareReport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "dcID")
	task, err := s.loadDDLCompareTask(id)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "ddl compare task not found")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "read ddl compare task failed")
		return
	}
	resultPath := filepath.Join(s.dcDir(id), "result.json")
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "result not found — the run failed before producing a result")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "read result failed")
		return
	}
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=ddl-compare-%s.json", id))
		w.Write(raw)
		return
	}
	var res ddlcompare.Result
	if err := json.Unmarshal(raw, &res); err != nil {
		zap.L().Warn("decode ddl compare result failed", zap.String("id", id), zap.Error(err))
		s.writeError(w, http.StatusInternalServerError, "result.json exists but is unreadable (corrupt artifact); cause logged server-side")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=ddl-compare-report-%s.html", id))
	w.Write([]byte(renderDDLCompareReportHTML(task, &res)))
}

// --- small helpers ---

func newDDLCompareID() string { return uuid.New().String()[:8] }

// dcmpEndpointLabel renders a credential-free side label for the task echo.
func dcmpEndpointLabel(e *dataSourceEntry, schema string) string {
	host := stringField(e.Fields, "host")
	if host == "" {
		return schema
	}
	return host + "/" + schema
}

// dcmpEndpoint builds the result.json side descriptor (D4): Host/Port are
// carried for the JSON consumer so both report faces see the connection
// target. Endpoint has no credential field by construction — passwords
// never enter result.json.
func dcmpEndpoint(kind, dsType string, e *dataSourceEntry, schema string) ddlcompare.Endpoint {
	ep := ddlcompare.Endpoint{Kind: kind, Type: dsType, Schema: schema}
	ep.Host = stringField(e.Fields, "host")
	if p := stringField(e.Fields, "port"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			ep.Port = n
		}
	}
	return ep
}

// precheckObjectCap fast-fails an over-cap catalog with one cheap COUNT
// per side before the heavy column/index walk (D2). Precheck failures are
// swallowed — the exact post-collection objectCapExceeded still guards.
func precheckObjectCap(ctx context.Context, srcIsMySQL bool, srcDB, tgtDB *sql.DB, srcSchema, tgtSchema string, max int) string {
	if max <= 0 {
		max = ddlcompare.DefaultMaxObjects
	}
	var srcN int
	var srcErr error
	if srcIsMySQL {
		srcN, srcErr = ddlcompare.CountMySQLTopLevel(ctx, srcDB, srcSchema)
	} else {
		srcN, srcErr = ddlcompare.CountPGTopLevel(ctx, srcDB, srcSchema)
	}
	if srcErr == nil && srcN > max {
		return fmt.Sprintf("源端对象数 %d 超过上限 %d，请缩小对比范围（选择更少的库/Schema）", srcN, max)
	}
	if tgtN, err := ddlcompare.CountMySQLTopLevel(ctx, tgtDB, tgtSchema); err == nil && tgtN > max {
		return fmt.Sprintf("目标端对象数 %d 超过上限 %d，请缩小对比范围（选择更少的库/Schema）", tgtN, max)
	}
	return ""
}

// tidbProcedureSwitchOff probes @@tidb_enable_procedure; ok=false means
// the variable is unknown (a plain MySQL target) — no hint then.
func tidbProcedureSwitchOff(ctx context.Context, db *sql.DB) (off, ok bool) {
	var v sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT @@tidb_enable_procedure").Scan(&v); err != nil || !v.Valid {
		return false, false
	}
	switch v.String {
	case "0", "OFF", "off", "false":
		return true, true
	case "1", "ON", "on", "true":
		return false, true
	}
	return false, false
}

// objectCapExceeded is the EXACT post-collection cap check (top-level
// objects, ddlexport口径); precheckObjectCap above is its cheap
// pre-walk fast-fail twin. The message names the offending side.
func objectCapExceeded(src, tgt *ddlcompare.Snapshot, max int) string {
	if max <= 0 {
		max = ddlcompare.DefaultMaxObjects
	}
	if n := src.Objects(); n > max {
		return fmt.Sprintf("源端对象数 %d 超过上限 %d，请缩小对比范围（选择更少的库/Schema）", n, max)
	}
	if n := tgt.Objects(); n > max {
		return fmt.Sprintf("目标端对象数 %d 超过上限 %d，请缩小对比范围（选择更少的库/Schema）", n, max)
	}
	return ""
}

func durationStr(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}
