package webapi

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/michaelliuyuan/timstool/internal/assess"
	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/logger"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"github.com/michaelliuyuan/timstool/internal/lightning"
	"github.com/michaelliuyuan/timstool/internal/orchestrator"
	"github.com/michaelliuyuan/timstool/internal/store"
	"go.uber.org/zap"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	router  chi.Router
	store   *store.Store
	addr    string
	hub     *Hub
	dataDir string

	// CDC status provider (#t48 B): reads the CDC process's status file.
	cdcProvider cdcStatusProvider

	// cdcEnabled reflects cdc.enable (D3 #t53): when false the CDC dashboard is
	// hidden by the frontend and /cdc/status reports a stable disabled state.
	cdcEnabled bool

	// cdcSupervisor owns the CDC child lifecycle (CONTROL channel, #t55): spawn /
	// supervise / restart / stop / adopt. nil when CDC control is not wired.
	cdcSupervisor *CDCSupervisor

	// cdcCfgFilePath is the config.yaml the CDC child loads (-c). Wired by
	// cmd/web; falls back to the supervisor's copy (A1 connection card).
	cdcCfgFilePath string

	// cdcProbe overrides the live DB prober for /cdc/precheck (tests).
	cdcProbe cdcDBProber

	// running tasks
	runningTasks map[string]context.CancelFunc

	// standalone compare tasks (独立数据比对): single-run invariant + state
	compare compareState

	// log collection
	logCollector *LogCollector
	logCores     map[string]*TaskLogCore
}

type Hub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan []byte
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case conn := <-h.register:
			h.clients[conn] = true
		case conn := <-h.unregister:
			delete(h.clients, conn)
			conn.Close()
		case msg := <-h.broadcast:
			for conn := range h.clients {
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					delete(h.clients, conn)
					conn.Close()
				}
			}
		}
	}
}

func NewServer(store *store.Store, host string, port int, dataDir string, staticFS embed.FS, cdcSupervisor *CDCSupervisor, cdcStatusFile string, cdcStale time.Duration) *Server {
	cdcEnabled := false
	if cdcSupervisor != nil {
		cdcEnabled = cdcSupervisor.cfg.Enable
	}
	s := &Server{
		store:         store,
		addr:          fmt.Sprintf("%s:%d", host, port),
		hub:           newHub(),
		runningTasks:  make(map[string]context.CancelFunc),
		dataDir:       dataDir,
		logCollector:  NewLogCollector(),
		logCores:      make(map[string]*TaskLogCore),
		cdcEnabled:    cdcEnabled,
		cdcSupervisor: cdcSupervisor,
	}

	// Adopt any CDC already running before this web start (locked decision:
	// 领养监控 — detect via status file + PID, no zombie/duplicate).
	if cdcSupervisor != nil {
		cdcSupervisor.Adopt(cdcStatusFile, cdcStale, pidAlive)
	}

	// Compare tasks: a persisted "running" state means the previous process
	// died mid-run; fail them so the UI is not stuck (M3).
	s.recoverStaleCompares()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.handleHealth)
		r.Get("/features", s.handleFeatures)
		// Multi-source config endpoints (#t67 WSC)
		r.Get("/sources", s.handleSources)
		r.Get("/sources/{type}/config-schema", s.handleSourceConfigSchema)
		r.Post("/sources/{type}/test", s.handleSourceTest)
		r.Post("/sources/tables", s.handleSourceTables)
		r.Post("/test-connection", s.handleTestConnectionMulti)
		r.Post("/config/test-connection", s.handleTestConnection)
		r.Post("/config/list-tables", s.handleListTables)
		r.Post("/validate-lightning", s.handleValidateLightning)
		// Migration options persistence (迁移选项记忆): server-side single
		// source of truth so the wizard can prefill last-used Lightning path /
		// temp dir across sessions, browsers, and service restarts.
		r.Get("/migration-options", s.handleGetMigrationOptions)
		r.Put("/migration-options", s.handlePutMigrationOptions)
		r.Post("/tasks", s.handleCreateTask)
		r.Get("/tasks", s.handleListTasks)
		r.Route("/tasks/{taskID}", func(r chi.Router) {
			r.Get("/", s.handleGetTask)
			r.Post("/start", s.handleStartTask)
			r.Post("/pause", s.handlePauseTask)
			r.Post("/resume", s.handleResumeTask)
			r.Post("/cancel", s.handleCancelTask)
			r.Delete("/", s.handleDeleteTask)
			r.Get("/progress", s.handleTaskProgress)
			r.Get("/report", s.handleTaskReport)
			r.Get("/logs", s.handleTaskLogs)
			r.Get("/phases", s.handleTaskPhases)
		})
		r.Get("/ws", s.handleWebSocket)
		r.Post("/assess", s.handleAssess)
		// Standalone comparison (独立数据比对): run the validator against
		// explicit connections without migrating; own task namespace.
		r.Post("/compare/tasks", s.handleCreateCompare)
		r.Get("/compare/tasks", s.handleListCompares)
		r.Route("/compare/tasks/{compareID}", func(r chi.Router) {
			r.Get("/", s.handleGetCompare)
			r.Get("/report", s.handleCompareReport)
			r.Post("/cancel", s.handleCancelCompare)
			r.Delete("/", s.handleDeleteCompare)
		})
		// CDC endpoints (#t48 B: read CDC process status file)
		r.Get("/cdc/status", s.handleCDCStatus)
		r.Get("/cdc/stats", s.handleCDCStats)
		r.Get("/cdc/checkpoint", s.handleCDCCheckpoint)
		r.Post("/cdc/start", s.handleCDCStart)
		r.Post("/cdc/stop", s.handleCDCStop)
		// CDC connection config + precheck + resume inspection (A1/A2/A3)
		r.Get("/cdc/config", s.handleGetCDCConfig)
		r.Put("/cdc/config", s.handlePutCDCConfig)
		r.Post("/cdc/config/import", s.handleImportCDCConfig)
		r.Get("/cdc/precheck", s.handleCDCPrecheck)
		r.Get("/cdc/slot", s.handleCDCSlot)
		r.Post("/cdc/checkpoint/reset", s.handleCDCResetCheckpoint)
	})

	if staticFS != (embed.FS{}) {
		staticContent, err := fs.Sub(staticFS, "static")
		if err == nil {
			fileServer := http.FileServer(http.FS(staticContent))
			r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if path != "/" && !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/assets/") {
					if !fileExists(staticContent, path) {
						r.URL.Path = "/"
					}
				}
				fileServer.ServeHTTP(w, r)
			})
		}
	}

	s.router = r
	return s
}

func fileExists(fsys fs.FS, path string) bool {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return false
	}
	f, err := fsys.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func (s *Server) Start() error {
	go s.hub.Run()
	log := zap.L()
	log.Info("starting web server", zap.String("addr", s.addr))

	server := &http.Server{
		Addr:         s.addr,
		Handler:      s.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	return server.ListenAndServe()
}

func (s *Server) BroadcastProgress(taskID string, progress map[string]interface{}) {
	progress["task_id"] = taskID
	data, err := json.Marshal(progress)
	if err != nil {
		return
	}
	select {
	case s.hub.broadcast <- data:
	default:
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "ok",
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// handleFeatures exposes optional-module switches so the frontend can
// conditionally render modules (e.g. the CDC dashboard). It is never cached —
// the frontend must see the current toggle on every load (D3 #t53).
func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"cdc": map[string]bool{"enabled": s.cdcEnabled},
	})
}

type TestConnectionRequest struct {
	Type       string `json:"type"` // "source" or "target"
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Password   string `json:"password"`
	Database   string `json:"database"`
	Schema     string `json:"schema,omitempty"`
	SSLMode    string `json:"sslmode,omitempty"`
	PDAddr     string `json:"pd_addr,omitempty"`
	StatusPort int    `json:"status_port,omitempty"`
}

func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	var req TestConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var result map[string]interface{}
	switch req.Type {
	case "source":
		result = s.testPGConnection(r.Context(), &req)
	case "target":
		result = s.testTiDBConnection(r.Context(), &req)
	default:
		s.writeError(w, http.StatusBadRequest, "type must be 'source' or 'target'")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) testPGConnection(ctx context.Context, req *TestConnectionRequest) map[string]interface{} {
	cfg := config.SourceConfig{
		Host:     req.Host,
		Port:     req.Port,
		User:     req.User,
		Password: req.Password,
		Database: req.Database,
		Schema:   req.Schema,
		SSLMode:  req.SSLMode,
	}
	if cfg.Schema == "" {
		cfg.Schema = "public"
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}

	start := time.Now()
	dsn := cfg.DSN()
	pgConn, err := openPGTestConn(dsn)
	elapsed := time.Since(start)

	result := map[string]interface{}{
		"type":     "source",
		"host":     cfg.Host,
		"port":     cfg.Port,
		"database": cfg.Database,
		"elapsed":  elapsed.String(),
	}

	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}
	defer pgConn.Close()

	var version string
	pgConn.QueryRowContext(ctx, "SELECT version()").Scan(&version)
	result["ok"] = true
	result["version"] = version
	return result
}

func (s *Server) testTiDBConnection(ctx context.Context, req *TestConnectionRequest) map[string]interface{} {
	cfg := config.TargetConfig{
		Host:       req.Host,
		Port:       req.Port,
		User:       req.User,
		Password:   req.Password,
		Database:   req.Database,
		PDAddr:     req.PDAddr,
		StatusPort: req.StatusPort,
	}

	start := time.Now()
	mysqlConn, err := openMySQLTestConn(cfg.DSN())
	elapsed := time.Since(start)

	result := map[string]interface{}{
		"type":     "target",
		"host":     cfg.Host,
		"port":     cfg.Port,
		"database": cfg.Database,
		"elapsed":  elapsed.String(),
		"mysql_ok": false, // explicit: the field is always present in the response
	}

	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}
	defer mysqlConn.Close()

	var version string
	mysqlConn.QueryRowContext(ctx, "SELECT tidb_version()").Scan(&version)
	result["mysql_ok"] = true

	// PD / Status probes: only when the user provided the values — providing
	// them signals Lightning import, so a wrong value must fail the test HERE
	// (at config time) instead of surfacing at import time as
	// "[pd] failed to get cluster id". Probes run in parallel, 3s cap each.
	pd, st := s.probeTargetExtras(ctx, req.Host, req.PDAddr, req.StatusPort)
	if pd.provided {
		if pd.ok {
			result["pd_ok"] = true
			if pd.clusterID != "" {
				result["pd_cluster_id"] = pd.clusterID
			}
		} else {
			result["pd_ok"] = false
			result["pd_error"] = pd.errMsg
		}
	}
	if st.provided {
		if st.ok {
			result["status_ok"] = true
		} else {
			result["status_ok"] = false
			result["status_error"] = st.errMsg
		}
	}

	// Aggregate: MySQL ok + (provided ⇒ probe ok) for each of PD/Status.
	ok := true
	var reasons []string
	if pd.provided && !pd.ok {
		ok = false
		reasons = append(reasons, "PD 探活失败："+pd.errMsg)
	}
	if st.provided && !st.ok {
		ok = false
		reasons = append(reasons, "Status 端口探活失败："+st.errMsg)
	}
	result["ok"] = ok
	if len(reasons) > 0 {
		result["error"] = strings.Join(reasons, "；")
	}
	result["version"] = version
	return result
}

// probeOutcome describes one optional PD/Status probe result.
type probeOutcome struct {
	provided  bool
	ok        bool
	errMsg    string
	clusterID string
}

// probeTargetExtras runs the optional PD and TiDB-status probes in parallel
// (3s cap each) for the target connection test. Extracted from
// testTiDBConnection so the aggregation semantics are unit-testable without a
// live MySQL target.
func (s *Server) probeTargetExtras(ctx context.Context, host, pdAddr string, statusPort int) (pd, st probeOutcome) {
	var wg sync.WaitGroup
	if pdAddr != "" {
		pd.provided = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			cid, perr := probePD(ctx, pdAddr)
			pd.ok = perr == nil
			pd.clusterID = cid
			if perr != nil {
				pd.errMsg = perr.Error()
			}
		}()
	}
	if statusPort > 0 {
		st.provided = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			serr := probeTiDBStatus(ctx, host, statusPort)
			st.ok = serr == nil
			if serr != nil {
				st.errMsg = serr.Error()
			}
		}()
	}
	wg.Wait()
	return pd, st
}

// probePD checks PD reachability via its HTTP API and returns the cluster id
// so the user can confirm they pointed at the right cluster.
func probePD(ctx context.Context, pdAddr string) (string, error) {
	// Tolerate user input like "http://host:2379/" or "host:2379/": strip any
	// scheme and trailing slashes so the http:// prefix below is never doubled
	// and the path stays exact.
	pdAddr = strings.TrimRight(pdAddr, "/")
	if i := strings.Index(pdAddr, "://"); i >= 0 {
		pdAddr = pdAddr[i+3:]
		pdAddr = strings.TrimRight(pdAddr, "/")
	}
	if pdAddr == "" {
		return "", fmt.Errorf("PD 地址为空")
	}
	c := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+pdAddr+"/pd/api/v1/cluster", nil)
	if err != nil {
		return "", fmt.Errorf("构造 PD 请求失败: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w（确认 PD 地址 host:port 正确，经代理时填真实 PD 端口而非 SQL 端口）", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return "", fmt.Errorf("PD 返回 HTTP %d", resp.StatusCode)
	}
	var body struct {
		ClusterID uint64 `json:"cluster_id"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) == nil && body.ClusterID != 0 {
		return fmt.Sprintf("%d", body.ClusterID), nil
	}
	return "", nil // reachable; older PD may not return the field
}

// probeTiDBStatus checks the TiDB status port (default 10080) via /status.
func probeTiDBStatus(ctx context.Context, host string, port int) error {
	c := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/status", host, port), nil)
	if err != nil {
		return fmt.Errorf("构造 Status 请求失败: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w（确认 Status 端口正确，默认 10080，本环境可能为 10081）", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return fmt.Errorf("Status 返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) handleListTables(w http.ResponseWriter, r *http.Request) {
	var req TestConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Type != "source" {
		s.writeError(w, http.StatusBadRequest, "type must be 'source'")
		return
	}

	cfg := config.SourceConfig{
		Host:     req.Host,
		Port:     req.Port,
		User:     req.User,
		Password: req.Password,
		Database: req.Database,
		Schema:   req.Schema,
		SSLMode:  req.SSLMode,
	}
	if cfg.Schema == "" {
		cfg.Schema = "public"
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}

	pgConn, err := openPGTestConn(cfg.DSN())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "connect failed: "+err.Error())
		return
	}
	defer pgConn.Close()

	rows, err := pgConn.QueryContext(r.Context(), `
		SELECT table_name,
		       (SELECT reltuples::bigint FROM pg_class WHERE oid = (quote_ident($1)||'.'||quote_ident(t.table_name))::regclass) AS row_estimate
		FROM information_schema.tables t
		WHERE table_schema = $1 AND table_type = 'BASE TABLE'
		ORDER BY table_name`, cfg.Schema)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "query tables: "+err.Error())
		return
	}
	defer rows.Close()

	type TableInfo struct {
		Name        string `json:"name"`
		RowEstimate int64  `json:"row_estimate"`
	}
	var tables []TableInfo
	for rows.Next() {
		var ti TableInfo
		if err := rows.Scan(&ti.Name, &ti.RowEstimate); err != nil {
			continue
		}
		tables = append(tables, ti)
	}
	if tables == nil {
		tables = []TableInfo{}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"tables": tables,
		"count":  len(tables),
	})
}

type CreateTaskRequest struct {
	Name   string              `json:"name"`
	Source config.SourceConfig `json:"source"`
	Target config.TargetConfig `json:"target"`
	Opts   MigrationOptsBody   `json:"opts"`
}

type MigrationOptsBody struct {
	Parallel          int      `json:"parallel"`
	BatchSize         int      `json:"batch_size"`
	TempDir           string   `json:"temp_dir"`
	Tables            []string `json:"tables"`
	ExcludeTables     []string `json:"exclude_tables"`
	UseLightning      bool     `json:"use_lightning"`
	LightningPath     string   `json:"lightning_path"`
	SkipPrecheck      bool     `json:"skip_precheck"`
	SkipSchema        bool     `json:"skip_schema"`
	SkipData          bool     `json:"skip_data"`
	SkipValidate      bool     `json:"skip_validate"`
	TargetPolicy      string   `json:"target_policy"`
	CompareMode       string   `json:"compare_mode"`
	SampleRatio       float64  `json:"sample_ratio"`
	ChecksumChunkSize int64    `json:"checksum_chunk_size"`
	ChecksumParallel  int      `json:"checksum_parallel"`
}

// validateLightningRequest is the body for POST /api/validate-lightning.
// Lightning path gate for the web wizard: an empty path probes auto-discovery
// (PATH → embedded), a non-empty path must exist, be a regular file, and be
// executable (x-bit checked on unix; the deployment target is Linux — on
// Windows dev machines the x-bit is meaningless, so only existence applies).
type validateLightningRequest struct {
	Path string `json:"path"`
}

type validateLightningResponse struct {
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	ResolvedPath string `json:"resolved_path"`
}

func (s *Server) handleValidateLightning(w http.ResponseWriter, r *http.Request) {
	var req validateLightningRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if strings.TrimSpace(req.Path) == "" {
		resolved := lightning.FindBinary(s.dataDir)
		if resolved == "" {
			s.writeJSON(w, http.StatusOK, validateLightningResponse{
				Success: false,
				Message: "未配置 lightning 路径，且自动发现失败：PATH 中无 tidb-lightning，内嵌二进制不可用",
			})
			return
		}
		s.writeJSON(w, http.StatusOK, validateLightningResponse{
			Success:      true,
			Message:      fmt.Sprintf("未配置路径，将使用自动发现的 tidb-lightning：%s", resolved),
			ResolvedPath: resolved,
		})
		return
	}

	fi, err := os.Stat(req.Path)
	if err != nil {
		s.writeJSON(w, http.StatusOK, validateLightningResponse{Success: false, Message: fmt.Sprintf("路径不存在：%s", req.Path)})
		return
	}
	if fi.IsDir() {
		s.writeJSON(w, http.StatusOK, validateLightningResponse{Success: false, Message: "路径是目录，需要指向 tidb-lightning 可执行文件"})
		return
	}
	if runtime.GOOS != "windows" && fi.Mode()&0111 == 0 {
		s.writeJSON(w, http.StatusOK, validateLightningResponse{Success: false, Message: "文件存在但没有执行权限（需要 x 位，Linux 上 chmod +x）"})
		return
	}
	s.writeJSON(w, http.StatusOK, validateLightningResponse{
		Success:      true,
		Message:      "tidb-lightning 路径验证通过",
		ResolvedPath: req.Path,
	})
}

// migrationOptionsBody is the persisted shape of the wizard's migration
// options step (temp_dir / use_lightning / lightning_path, plus the
// target-cluster-only extras pd_addr / status_port — host/port/user/
// password stay per-task and are never persisted).
type migrationOptionsBody struct {
	TempDir       string `json:"temp_dir"`
	UseLightning  bool   `json:"use_lightning"`
	LightningPath string `json:"lightning_path"`
	PDAddr        string `json:"pd_addr,omitempty"`
	StatusPort    int    `json:"status_port,omitempty"`
}

func (s *Server) migrationOptionsFile() string {
	return filepath.Join(s.dataDir, "migration-options.json")
}

func (s *Server) handleGetMigrationOptions(w http.ResponseWriter, r *http.Request) {
	raw, err := os.ReadFile(s.migrationOptionsFile())
	if err != nil {
		// No saved options yet: return empty values; the frontend keeps its
		// defaults (temp_dir=/tmp/timstool, lightning off).
		s.writeJSON(w, http.StatusOK, migrationOptionsBody{})
		return
	}
	var opts migrationOptionsBody
	if err := json.Unmarshal(raw, &opts); err != nil {
		// Corrupted persistence file: log it, fall back to defaults rather
		// than breaking the wizard (the next successful PUT rewrites it).
		zap.L().Warn("migration-options.json corrupted, ignoring saved options",
			zap.String("file", s.migrationOptionsFile()), zap.Error(err))
		s.writeJSON(w, http.StatusOK, migrationOptionsBody{})
		return
	}
	s.writeJSON(w, http.StatusOK, opts)
}

func (s *Server) handlePutMigrationOptions(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var req migrationOptionsBody
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// R1 merge semantics: PUT is a partial update, not a whole-object
	// replace. Fields explicitly present in the body override the saved
	// values; fields absent from the body keep their saved values (so a
	// client sending only pd_addr cannot wipe the temp_dir/lightning memory).
	var presence map[string]json.RawMessage
	if err := json.Unmarshal(raw, &presence); err != nil {
		presence = nil
	}

	// Normalize once, then validate and persist the SAME trimmed values, so a
	// whitespace-only temp_dir cannot slip past validation and be stored raw.
	req.TempDir = strings.TrimSpace(req.TempDir)
	req.LightningPath = strings.TrimSpace(req.LightningPath)
	// Empty pd_addr means "clear the remembered address"; status_port must be
	// a valid port number (0 = not provided / skip probing).
	// A1: strip an optional scheme prefix and trailing slash so the stored
	// value matches what probePD accepts and the lightning toml consumes.
	req.PDAddr = normalizePDAddr(req.PDAddr)
	if req.StatusPort < 0 || req.StatusPort > 65535 {
		s.writeError(w, http.StatusBadRequest, "status_port 必须在 0-65535 之间（0 表示不探测）")
		return
	}

	// Validate before persisting: temp_dir must be creatable; an explicit
	// lightning path must exist, be a regular file and (on Linux) executable.
	// An empty lightning path is allowed and means auto-discovery at run time.
	// UNC paths (\\server\share) are rejected: this is a local tool and temp
	// dirs on remote shares are neither intended nor tested.
	if req.TempDir != "" {
		if strings.HasPrefix(req.TempDir, `\\`) || strings.HasPrefix(req.TempDir, "//") {
			s.writeError(w, http.StatusBadRequest, "临时目录不支持 UNC/网络共享路径，请使用本地目录")
			return
		}
		if err := os.MkdirAll(req.TempDir, 0o755); err != nil {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("临时目录不可用：%v", err))
			return
		}
	}
	if req.UseLightning && req.LightningPath != "" {
		fi, err := os.Stat(req.LightningPath)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("lightning 路径不存在：%s", req.LightningPath))
			return
		}
		if fi.IsDir() {
			s.writeError(w, http.StatusBadRequest, "lightning 路径是目录，需要指向 tidb-lightning 可执行文件")
			return
		}
		if runtime.GOOS != "windows" && fi.Mode()&0111 == 0 {
			s.writeError(w, http.StatusBadRequest, "lightning 文件存在但没有执行权限（需要 x 位，Linux 上 chmod +x）")
			return
		}
	}

	// Merge with the previously saved options: only fields explicitly present
	// in the request body are applied; absent fields keep their saved values.
	merged := s.loadMigrationOptions()
	if _, ok := presence["temp_dir"]; ok {
		merged.TempDir = req.TempDir
	}
	if _, ok := presence["use_lightning"]; ok {
		merged.UseLightning = req.UseLightning
	}
	if _, ok := presence["lightning_path"]; ok {
		merged.LightningPath = req.LightningPath
	}
	if _, ok := presence["pd_addr"]; ok {
		merged.PDAddr = req.PDAddr
	}
	if _, ok := presence["status_port"]; ok {
		merged.StatusPort = req.StatusPort
	}

	if err := s.writeMigrationOptions(&merged); err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("保存失败：%v", err))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// loadMigrationOptions returns the currently persisted options, or the zero
// value when nothing has been saved yet (or the file is corrupted — the next
// successful PUT rewrites it).
func (s *Server) loadMigrationOptions() migrationOptionsBody {
	raw, err := os.ReadFile(s.migrationOptionsFile())
	if err != nil {
		return migrationOptionsBody{}
	}
	var opts migrationOptionsBody
	if err := json.Unmarshal(raw, &opts); err != nil {
		return migrationOptionsBody{}
	}
	return opts
}

// normalizePDAddr strips an optional URL scheme and trailing slash so a
// remembered pd_addr is stored in the same form probePD accepts and the
// lightning toml consumes (e.g. "http://host:2389/" → "host:2389").
func normalizePDAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	return strings.TrimRight(addr, "/")
}

// writeMigrationOptions persists the options atomically (write temp file in
// dataDir, then rename over the target) so a crash mid-write can never leave a
// truncated JSON behind, and logs corruption instead of failing silently.
func (s *Server) writeMigrationOptions(opts *migrationOptionsBody) error {
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	raw, err := json.MarshalIndent(opts, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dataDir, "migration-options-*.tmp")
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
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.migrationOptionsFile()); err != nil {
		// Rename can fail (e.g. target locked on Windows); clean up the temp
		// file so failed saves never accumulate .tmp leftovers.
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Source.Host == "" || req.Target.Host == "" {
		s.writeError(w, http.StatusBadRequest, "source and target host are required")
		return
	}
	if req.Name == "" {
		req.Name = fmt.Sprintf("Migration %s", time.Now().Format("2006-01-02 15:04:05"))
	}

	task := &store.Task{
		ID:   uuid.New().String()[:8],
		Name: req.Name,
	}

	cfg := &config.Config{
		Source: req.Source,
		Target: req.Target,
		Migration: config.MigrationConfig{
			Parallel:      req.Opts.Parallel,
			BatchSize:     req.Opts.BatchSize,
			Tables:        req.Opts.Tables,
			ExcludeTables: req.Opts.ExcludeTables,
			UseLightning:  req.Opts.UseLightning,
			LightningPath: req.Opts.LightningPath,
			TempDir:       req.Opts.TempDir,
			CheckpointDir: fmt.Sprintf(".checkpoint/%s", task.ID),
			OnError:       "abort",
			TargetPolicy:  req.Opts.TargetPolicy,
			SkipPrecheck:  req.Opts.SkipPrecheck,
			SkipSchema:    req.Opts.SkipSchema,
			SkipData:      req.Opts.SkipData,
			SkipValidate:  req.Opts.SkipValidate,
		},
		Logging: config.LoggingConfig{Level: "info", Format: "console"},
		Compare: config.CompareConfig{
			CompareMode:       req.Opts.CompareMode,
			SampleRatio:       req.Opts.SampleRatio,
			ChecksumChunkSize: req.Opts.ChecksumChunkSize,
			ChecksumParallel:  req.Opts.ChecksumParallel,
		},
	}
	if cfg.Migration.Parallel <= 0 {
		cfg.Migration.Parallel = 4
	}
	if cfg.Migration.BatchSize <= 0 {
		cfg.Migration.BatchSize = 100000
	}
	if cfg.Migration.TempDir == "" {
		cfg.Migration.TempDir = "/tmp/timstool"
	}

	cfgBytes, _ := json.Marshal(cfg)
	task.ConfigJSON = string(cfgBytes)

	if err := s.store.CreateTask(task); err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}

	s.writeJSON(w, http.StatusCreated, task)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks(50, 0)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []*store.Task{}
	}
	s.writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}
	s.writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleStartTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.Status == store.TaskStatusRunning {
		s.writeError(w, http.StatusConflict, "task is already running")
		return
	}

	var cfg config.Config
	if err := json.Unmarshal([]byte(task.ConfigJSON), &cfg); err != nil {
		s.writeError(w, http.StatusInternalServerError, "invalid task config")
		return
	}

	if err := s.store.UpdateTaskStatus(taskID, store.TaskStatusRunning); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reset all progress fields for a fresh run
	_ = s.store.ResetTaskForRerun(taskID)

	// Clear old logs and checkpoint data
	s.logCollector.RemoveBuffer(taskID)
	os.RemoveAll(fmt.Sprintf(".checkpoint/%s", taskID))

	ctx, cancel := context.WithCancel(context.Background())
	s.runningTasks[taskID] = cancel

	go s.runMigration(ctx, taskID, cfg)

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "started", "task_id": taskID})
}

func (s *Server) runMigration(ctx context.Context, taskID string, cfg config.Config) {
	logCore := NewTaskLogCore(s.logCollector, taskID, nil)
	s.logCores[taskID] = logCore

	defer func() {
		logCore.Disable()
		delete(s.logCores, taskID)
		logger.UnregisterExtraCore()
	}()

	s.logCollector.GetBuffer(taskID)
	s.logCollector.Append(taskID, "INFO", "Migration task started", "")

	logger.RegisterExtraCore(logCore)

	progressCtx, progressCancel := context.WithCancel(ctx)
	defer progressCancel()
	go s.pollProgress(progressCtx, taskID, cfg.Migration.CheckpointDir)

	o := orchestrator.NewOrchestrator(cfg)
	pipeCfg := orchestrator.PipelineConfig{
		SkipPrecheck: cfg.Migration.SkipPrecheck,
		SkipSchema:   cfg.Migration.SkipSchema,
		SkipData:     cfg.Migration.SkipData,
		SkipValidate: cfg.Migration.SkipValidate,
	}

	results, err := o.Run(ctx, pipeCfg)

	progressCancel()

	if ctx.Err() == context.Canceled {
		s.logCollector.Append(taskID, "WARN", "Migration task cancelled", "")
		s.store.UpdateTaskStatus(taskID, store.TaskStatusCancelled)
		return
	}

	if err != nil {
		s.logCollector.Append(taskID, "ERROR", "Migration failed: "+err.Error(), "")
		s.store.SetTaskError(taskID, err.Error())
		return
	}

	// Final progress sync from checkpoint
	if cpMgr, cpErr := checkpoint.NewReadOnlyManager(cfg.Migration.CheckpointDir); cpErr == nil {
		cpPhase := cpMgr.GetPhase()
		if cpPhase == "data-migration" || cpPhase == "data-export" || cpPhase == "data-import" {
			cpPhase = "data"
		}
		cpTables := cpMgr.GetAllTables()
		var tDone, tTotal int
		var rDone, rTotal int64
		for _, tc := range cpTables {
			tTotal++
			rTotal += tc.RowsTotal
			rDone += tc.RowsDone
			if tc.State == checkpoint.StateCompleted || tc.State == checkpoint.StateFailed {
				tDone++
			}
		}
		zap.L().Info("[DEBUG] final progress sync",
			zap.String("phase", cpPhase),
			zap.Int("tables_total", tTotal),
			zap.Int("tables_done", tDone),
			zap.Int64("rows_total", rTotal),
			zap.Int64("rows_done", rDone))
		prog := computeTaskProgress(cpPhase, tDone, tTotal, rDone, rTotal, cpMgr.GetImportedTables(), cpMgr.GetImportMode())
		_ = s.store.UpdateTaskProgress(taskID, cpPhase, prog, tDone, tTotal, rDone, rTotal)
		s.BroadcastProgress(taskID, map[string]interface{}{
			"phase":        cpPhase,
			"progress":     prog,
			"tables_done":  tDone,
			"tables_total": tTotal,
			"rows_done":    rDone,
			"rows_total":   rTotal,
		})
	}

	resultData, _ := json.Marshal(results)
	s.store.SetTaskResult(taskID, string(resultData))

	allSuccess := true
	for _, r := range results {
		if !r.Success {
			allSuccess = false
			break
		}
	}
	if allSuccess {
		s.logCollector.Append(taskID, "INFO", "Migration completed successfully", "")
		s.store.UpdateTaskStatus(taskID, store.TaskStatusCompleted)
	} else {
		s.logCollector.Append(taskID, "WARN", "Migration completed with errors", "")
		s.store.UpdateTaskStatus(taskID, store.TaskStatusFailed)
	}
}

func (s *Server) pollProgress(ctx context.Context, taskID string, checkpointDir string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cpMgr, err := checkpoint.NewReadOnlyManager(checkpointDir)
			if err != nil {
				continue
			}

			rawPhase := cpMgr.GetPhase()
			phase := rawPhase
			if phase == "data-migration" || phase == "data-export" || phase == "data-import" {
				phase = "data"
			}
			tables := cpMgr.GetAllTables()

			var tablesDone, tablesTotal int
			var rowsDone, rowsTotal int64
			for _, tc := range tables {
				tablesTotal++
				rowsTotal += tc.RowsTotal
				rowsDone += tc.RowsDone
				if tc.State == checkpoint.StateCompleted || tc.State == checkpoint.StateFailed {
					tablesDone++
				}
			}

			importedTables := cpMgr.GetImportedTables()
			importMode := cpMgr.GetImportMode()
			progress := computeTaskProgress(rawPhase, tablesDone, tablesTotal, rowsDone, rowsTotal, importedTables, importMode)

			_ = s.store.UpdateTaskProgress(taskID, phase, progress, tablesDone, tablesTotal, rowsDone, rowsTotal)

			msg := map[string]interface{}{
				"phase":        phase,
				"progress":     progress,
				"tables_done":  tablesDone,
				"tables_total": tablesTotal,
				"rows_done":    rowsDone,
				"rows_total":   rowsTotal,
			}
			if rawPhase == "data-import" {
				msg["imported_tables"] = importedTables
			}
			s.BroadcastProgress(taskID, msg)
		}
	}
}

// weightedProgress combines the three migration stages into one total
// progress value.
//
// 权重定死，不做配置：schema 5%，导出 47.5%，导入 47.5%。
func weightedProgress(schemaDone, exportFrac, importFrac float64) float64 {
	const (
		wSchema = 0.05
		wExport = 0.475
		wImport = 0.475
	)
	p := wSchema*schemaDone + wExport*exportFrac + wImport*importFrac
	if p > 1.0 {
		p = 1.0
	}
	return p
}

// computeTaskProgress derives the weighted total progress for a checkpoint
// phase. validate and later stages report 100%; schema (and precheck) keep
// the coarse table-based ratio; data phases use the fixed 5/47.5/47.5
// weighting with the import fraction chosen by the recorded import mode:
//
//   - lightning: importFrac = importedTables/tablesTotal (0 while no table
//     has finished restoring — never the rows ratio), exportFrac = rows ratio.
//   - stream:    export counts as complete, importFrac = rows ratio.
//   - "" (historical checkpoint): imported_tables>0 selects the lightning
//     formula, otherwise the stream formula.
func computeTaskProgress(rawPhase string, tablesDone, tablesTotal int, rowsDone, rowsTotal int64, importedTables int, importMode string) float64 {
	rowsFrac := func() float64 {
		if rowsTotal <= 0 {
			return 0
		}
		return float64(rowsDone) / float64(rowsTotal)
	}
	tablesFrac := func() float64 {
		if tablesTotal <= 0 {
			return 0
		}
		return float64(importedTables) / float64(tablesTotal)
	}

	switch rawPhase {
	case "validate", "completed":
		return 1.0
	case "schema", "precheck", "":
		if tablesTotal > 0 {
			p := float64(tablesDone) / float64(tablesTotal)
			if p > 1.0 {
				p = 1.0
			}
			return p
		}
		return 0
	}

	// Data phases ("data", "data-migration", "data-export", "data-import").
	schemaDone := 1.0
	exportFrac := rowsFrac()
	importFrac := 0.0
	if rawPhase == "data-import" {
		mode := importMode
		if mode == "" && importedTables > 0 {
			// Historical-checkpoint compatibility only: checkpoints written
			// before import_mode existed. New checkpoints always carry a
			// mode — the migrator sets it before flipping the phase to
			// data-import, so the mode=="" && imported==0 window below is
			// unreachable for fresh runs.
			mode = checkpoint.ImportModeLightning
		}
		if mode == checkpoint.ImportModeLightning {
			exportFrac = rowsFrac()
			importFrac = tablesFrac()
		} else {
			// stream (explicit or inferred): rows double as the import
			// signal; the CSV export stage never ran (or is irrelevant).
			// With R1 the stream entry point zeroes RowsDone, so the rows
			// ratio starts at 0 and this branch reports 0.525 upward.
			exportFrac = 1.0
			importFrac = rowsFrac()
		}
	}
	return weightedProgress(schemaDone, exportFrac, importFrac)
}

func (s *Server) handlePauseTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, _ := s.store.GetTask(taskID)
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.Status != store.TaskStatusRunning {
		s.writeError(w, http.StatusConflict, "task is not running")
		return
	}
	s.store.UpdateTaskStatus(taskID, store.TaskStatusPaused)
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (s *Server) handleResumeTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, _ := s.store.GetTask(taskID)
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.Status != store.TaskStatusPaused {
		s.writeError(w, http.StatusConflict, "task is not paused")
		return
	}

	var cfg config.Config
	json.Unmarshal([]byte(task.ConfigJSON), &cfg)

	s.store.UpdateTaskStatus(taskID, store.TaskStatusRunning)
	ctx, cancel := context.WithCancel(context.Background())
	s.runningTasks[taskID] = cancel
	go s.runMigration(ctx, taskID, cfg)

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	if cancel, ok := s.runningTasks[taskID]; ok {
		cancel()
		delete(s.runningTasks, taskID)
	}
	s.store.UpdateTaskStatus(taskID, store.TaskStatusCancelled)
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, _ := s.store.GetTask(taskID)
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.Status == store.TaskStatusRunning {
		s.writeError(w, http.StatusConflict, "cannot delete running task, cancel first")
		return
	}
	s.store.DeleteTask(taskID)
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleTaskProgress(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}
	s.writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleTaskReport(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}

	format := r.URL.Query().Get("format")
	switch format {
	case "html":
		report := s.buildTaskReport(task)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=report-%s.html", taskID))
		w.Write([]byte(report.ToHTML()))
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=report-%s.json", taskID))
		w.Write([]byte(task.ResultJSON))
	default:
		report := s.buildTaskReport(task)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=report-%s.html", taskID))
		w.Write([]byte(report.ToHTML()))
	}
}

func (s *Server) buildTaskReport(task *store.Task) *reporter.Report {
	report := reporter.NewReport("Migration Report")
	report.Tool = "TiMS"
	report.Version = "1.0.0"

	if task.StartedAt != nil {
		report.StartTime = *task.StartedAt
	}
	if task.FinishedAt != nil {
		report.EndTime = *task.FinishedAt
	}
	report.Duration = reporter.FormatDuration(report.EndTime.Sub(report.StartTime))

	if task.Status == store.TaskStatusCompleted {
		report.Status = reporter.StatusPass
	} else if task.Status == store.TaskStatusFailed {
		report.Status = reporter.StatusFail
	} else {
		report.Status = reporter.StatusWarn
	}

	// Build table reports from checkpoint data
	cfg := config.Config{}
	if task.ConfigJSON != "" {
		json.Unmarshal([]byte(task.ConfigJSON), &cfg)
	}
	cpDir := cfg.Migration.CheckpointDir
	if cpDir == "" {
		cpDir = fmt.Sprintf(".checkpoint/%s", task.ID)
	}
	if cpMgr, err := checkpoint.NewReadOnlyManager(cpDir); err == nil {
		for _, tc := range cpMgr.GetAllTables() {
			tr := reporter.TableReport{
				TableName:  tc.TableName,
				SourceRows: tc.RowsTotal,
				TargetRows: tc.RowsDone,
				Duration:   "",
			}
			if !tc.FinishedAt.IsZero() && !tc.StartedAt.IsZero() {
				tr.Duration = reporter.FormatDuration(tc.FinishedAt.Sub(tc.StartedAt))
			}
			switch tc.State {
			case checkpoint.StateCompleted:
				tr.Status = reporter.StatusPass
			case checkpoint.StateFailed:
				tr.Status = reporter.StatusFail
				tr.Error = tc.Error
			default:
				tr.Status = reporter.StatusSkip
			}
			if tr.SourceRows > 0 && tr.TargetRows > 0 {
				tr.DiffRows = tr.SourceRows - tr.TargetRows
			}
			report.AddTableReport(tr)
		}
	}

	// Build summary
	var summaryParts []string
	summaryParts = append(summaryParts, fmt.Sprintf("Source: %s:%d/%s", cfg.Source.Host, cfg.Source.Port, cfg.Source.Database))
	summaryParts = append(summaryParts, fmt.Sprintf("Target: %s:%d/%s", cfg.Target.Host, cfg.Target.Port, cfg.Target.Database))
	summaryParts = append(summaryParts, fmt.Sprintf("Tables: %d/%d completed", task.TablesDone, task.TablesTotal))
	summaryParts = append(summaryParts, fmt.Sprintf("Rows: %d/%d", task.RowsDone, task.RowsTotal))
	if task.Error != "" {
		summaryParts = append(summaryParts, fmt.Sprintf("Error: %s", task.Error))
	}
	report.Summary = strings.Join(summaryParts, " | ")

	report.Finish(report.Status, report.Summary)
	// Restore the actual times after Finish overwrites them
	if task.StartedAt != nil {
		report.StartTime = *task.StartedAt
	}
	if task.FinishedAt != nil {
		report.EndTime = *task.FinishedAt
	}
	report.Duration = reporter.FormatDuration(report.EndTime.Sub(report.StartTime))

	return report
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.hub.register <- conn

	defer func() {
		s.hub.unregister <- conn
	}()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func (s *Server) handleTaskLogs(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}

	upgrade := r.URL.Query().Get("ws")
	if upgrade == "true" {
		s.handleLogStream(w, r, taskID)
		return
	}

	logs := s.logCollector.GetBuffer(taskID).GetAll()
	if logs == nil {
		logs = []TaskLogEntry{}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"task_id": taskID,
		"logs":    logs,
		"count":   len(logs),
	})
}

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request, taskID string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	buf := s.logCollector.GetBuffer(taskID)
	ch := buf.Subscribe()
	defer buf.Unsubscribe(ch)

	existingLogs := buf.GetAll()
	for _, entry := range existingLogs {
		data, _ := json.Marshal(entry)
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case entry, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(entry)
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

func (s *Server) handleTaskPhases(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := s.store.GetTask(taskID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}

	type PhaseInfo struct {
		Name       string                   `json:"name"`
		Label      string                   `json:"label"`
		Status     string                   `json:"status"`
		SubLabel   string                   `json:"sub_label,omitempty"`
		Tables     []map[string]interface{} `json:"tables,omitempty"`
		TableCount int                      `json:"table_count"`
		TablesDone int                      `json:"tables_done"`
		RowsTotal  int64                    `json:"rows_total"`
		RowsDone   int64                    `json:"rows_done"`
		Logs       []map[string]interface{} `json:"logs,omitempty"`
	}

	phaseNames := []struct{ name, label string }{
		{"precheck", "预检查"},
		{"schema", "Schema 迁移"},
		{"data", "数据迁移"},
		{"validate", "数据验证"},
	}

	var phases []PhaseInfo
	for i, p := range phaseNames {
		pi := PhaseInfo{
			Name:   p.name,
			Label:  p.label,
			Status: "pending",
		}

		if task.Status == "completed" {
			pi.Status = "completed"
		} else if task.Phase == p.name {
			if task.Status == "running" {
				pi.Status = "running"
			} else if task.Status == "failed" {
				pi.Status = "failed"
			}
		} else if task.Phase != "" {
			curIdx := -1
			for j, pp := range phaseNames {
				if pp.name == task.Phase {
					curIdx = j
					break
				}
			}
			if curIdx >= 0 && i < curIdx {
				pi.Status = "completed"
			}
		}

		if p.name == "data" {
			cpMgr, cpErr := checkpoint.NewReadOnlyManager(fmt.Sprintf(".checkpoint/%s", taskID))
			if cpErr == nil {
				cpPhase := cpMgr.GetPhase()
				switch cpPhase {
				case "data-export":
					pi.SubLabel = "数据导出"
				case "data-import":
					pi.SubLabel = "数据导入"
				}
				tables := cpMgr.GetAllTables()
				if len(tables) > 0 {
					for _, tc := range tables {
						tableInfo := map[string]interface{}{
							"name":       tc.TableName,
							"state":      string(tc.State),
							"rows_done":  tc.RowsDone,
							"rows_total": tc.RowsTotal,
						}
						pi.Tables = append(pi.Tables, tableInfo)
						pi.TableCount++
						pi.RowsTotal += tc.RowsTotal
						pi.RowsDone += tc.RowsDone
						if tc.State == checkpoint.StateCompleted || tc.State == checkpoint.StateFailed {
							pi.TablesDone++
						}
					}
				}
			}
		}

		logs := s.logCollector.GetBuffer(taskID).GetAll()
		phaseLabel := p.label
		inPhase := false
		for _, entry := range logs {
			if strings.Contains(entry.Message, "Phase: "+phaseLabel) {
				inPhase = true
				continue
			}
			if inPhase {
				nextPhaseIdx := -1
				for _, pp := range phaseNames {
					if pp.label != phaseLabel && strings.Contains(entry.Message, "Phase: "+pp.label) {
						nextPhaseIdx = 1
						break
					}
				}
				if nextPhaseIdx >= 0 || strings.Contains(entry.Message, "Migration task started") || strings.Contains(entry.Message, "migration pipeline completed") {
					break
				}
				pi.Logs = append(pi.Logs, map[string]interface{}{
					"level":     entry.Level,
					"message":   entry.Message,
					"timestamp": entry.Timestamp,
				})
			}
		}

		phases = append(phases, pi)
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"task_id": taskID,
		"phase":   task.Phase,
		"phases":  phases,
	})
}

// handleAssess runs a compatibility assessment and returns JSON for the frontend.
func (s *Server) handleAssess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
		Schema   string `json:"schema"`
		Format   string `json:"format"` // "json" (default) or "html"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Host == "" || req.Database == "" {
		s.writeError(w, http.StatusBadRequest, "host and database are required")
		return
	}
	if req.Schema == "" {
		req.Schema = "public"
	}
	if req.Port == 0 {
		req.Port = 5432
	}

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		req.User, req.Password, req.Host, req.Port, req.Database)

	pgDB, err := sql.Open("pgx", dsn)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "connect failed: "+err.Error())
		return
	}
	defer pgDB.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Scan and assess
	scanner := assess.NewScanner(pgDB, req.Schema)
	result, err := scanner.ScanAll(ctx)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "scan failed: "+err.Error())
		return
	}

	assessor := assess.NewAssessor()
	dims := assessor.Assess(result)

	rg := assess.NewReportGenerator(dims)

	// If HTML format requested, return HTML
	if req.Format == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		rg.WriteHTML(w)
		return
	}

	// Default: return JSON
	report := rg.Report()
	s.writeJSON(w, http.StatusOK, report)
}
