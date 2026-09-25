package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/store"
	"gopkg.in/yaml.v3"
)

// CDC connection-config endpoints (A1): the CDC child spawned by the
// supervisor reads source/target from config.yaml (-c cfgFile), NOT from the
// wizard's per-task connections — these endpoints make that live config
// visible (redacted), editable, and importable from the latest successful
// migration task, so the dashboard never starts CDC against the wrong DBs.

// cdcCfgMu serializes read→modify→write of config.yaml.
var cdcCfgMu sync.Mutex

// cdcCfgSummary is the GET /cdc/config response: a redacted view of the
// config the CDC child will actually use.
type cdcCfgSummary struct {
	CfgFile     string          `json:"cfg_file"`
	Source      sourceSummary   `json:"source"`
	Target      targetSummary   `json:"target"`
	CDC         cdcParamSummary `json:"cdc"`
	HasPassword bool            `json:"has_password"` // source password is set (never echoed)
}

type sourceSummary struct {
	Type     string `json:"type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Database string `json:"database"`
	Schema   string `json:"schema"`
	SSLMode  string `json:"sslmode"`
}

type targetSummary struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Database string `json:"database"`
}

type cdcParamSummary struct {
	Enable           bool     `json:"enable"`
	Mode             string   `json:"mode"`
	SlotName         string   `json:"slot_name"`
	Publication      string   `json:"publication"`
	SyncDDL          bool     `json:"sync_ddl"`
	ConflictStrategy string   `json:"conflict_strategy"`
	Parallel         int      `json:"parallel"`
	Tables           []string `json:"tables,omitempty"`
	ExcludeTables    []string `json:"exclude_tables,omitempty"`
	CheckpointFile   string   `json:"checkpoint_file"`
}

func summarizeSource(s config.SourceConfig) sourceSummary {
	return sourceSummary{
		Type: s.SourceType(), Host: s.Host, Port: s.Port, User: s.User,
		Database: s.Database, Schema: s.Schema, SSLMode: s.SSLMode,
	}
}

func summarizeTarget(t config.TargetConfig) targetSummary {
	return targetSummary{Host: t.Host, Port: t.Port, User: t.User, Database: t.Database}
}

func summarizeCDC(c config.CDCConfig) cdcParamSummary {
	return cdcParamSummary{
		Enable: c.Enable, Mode: c.Mode, SlotName: c.SlotName, Publication: c.Publication,
		SyncDDL: c.SyncDDL, ConflictStrategy: c.ConflictStrategy, Parallel: c.Parallel,
		Tables: c.Tables, ExcludeTables: c.ExcludeTables, CheckpointFile: c.CheckpointFile,
	}
}

// SetCDCConfigFile wires the config.yaml path the CDC child loads (A1).
func (s *Server) SetCDCConfigFile(path string) { s.cdcCfgFilePath = path }

// cdcCfgFile resolves the config file the CDC child loads: the explicit
// server wiring first, then the supervisor's copy.
func (s *Server) cdcCfgFile() string {
	if s.cdcCfgFilePath != "" {
		return s.cdcCfgFilePath
	}
	if s.cdcSupervisor != nil {
		return s.cdcSupervisor.cfgFile
	}
	return ""
}

// loadCDCConfig reads the live config.yaml. Missing file → zero config (the
// child would fall back to built-in defaults + localhost, which is exactly
// the misconnection risk this card exists to surface).
func (s *Server) loadCDCConfig() (*config.Config, error) {
	f := s.cdcCfgFile()
	if f == "" {
		return nil, fmt.Errorf("config file not wired on this server")
	}
	return config.Load(f)
}

// writeCDCConfig persists cfg back to config.yaml atomically. The write is a
// structured yaml round-trip that PRESERVES comments and untouched sections:
// only the source/target mapping values are edited in place on the parsed
// document node, so operator comments elsewhere in the file survive saves.
func (s *Server) writeCDCConfig(cfg *config.Config) error {
	f := s.cdcCfgFile()
	if f == "" {
		return fmt.Errorf("config file not wired on this server")
	}
	raw, err := os.ReadFile(f)
	if err != nil {
		// Missing file: fall back to a plain marshal of the full config —
		// still atomic (tmp+rename): a crash mid-write must never leave a
		// half file that would break the next server start.
		out, merr := yaml.Marshal(cfg)
		if merr != nil {
			return merr
		}
		tmp := f + ".tmp"
		if werr := os.WriteFile(tmp, out, 0o600); werr != nil {
			return werr
		}
		return os.Rename(tmp, f)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse config.yaml: %w", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config.yaml is not a yaml mapping")
	}
	srcMap := mappingValue(root, "source")
	tgtMap := mappingValue(root, "target")
	if srcMap == nil || tgtMap == nil {
		return fmt.Errorf("config.yaml missing source/target section")
	}
	setMapFields(srcMap, map[string]interface{}{
		"type": cfg.Source.Type, "host": cfg.Source.Host, "port": cfg.Source.Port,
		"user": cfg.Source.User, "password": cfg.Source.Password,
		"database": cfg.Source.Database, "schema": cfg.Source.Schema,
		"sslmode": cfg.Source.SSLMode,
	})
	setMapFields(tgtMap, map[string]interface{}{
		"host": cfg.Target.Host, "port": cfg.Target.Port, "user": cfg.Target.User,
		"password": cfg.Target.Password, "database": cfg.Target.Database,
	})
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f)
}

// mappingValue returns the mapping node stored under key in a yaml mapping
// (nil when absent or not a mapping).
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMapFields updates/creates scalar fields on a yaml mapping node in place,
// preserving neighbors, order, and comments. Numeric values are tagged !!int
// so ports stay ints in the emitted document.
func setMapFields(m *yaml.Node, fields map[string]interface{}) {
	for k, v := range fields {
		var tag string
		var val string
		switch n := v.(type) {
		case int:
			tag = "!!int"
			val = fmt.Sprintf("%d", n)
		default:
			val = fmt.Sprintf("%v", v)
		}
		replaced := false
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == k {
				node := m.Content[i+1]
				node.Value = val
				if tag != "" {
					node.Tag = tag
				} else {
					node.Tag = ""
				}
				node.Style = 0
				replaced = true
				break
			}
		}
		if !replaced {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
			valNode := &yaml.Node{Kind: yaml.ScalarNode, Value: val}
			if tag != "" {
				valNode.Tag = tag
			}
			m.Content = append(m.Content, keyNode, valNode)
		}
	}
}

func (s *Server) handleGetCDCConfig(w http.ResponseWriter, r *http.Request) {
	cdcCfgMu.Lock()
	defer cdcCfgMu.Unlock()
	cfg, err := s.loadCDCConfig()
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, cdcCfgSummary{
		CfgFile:     s.cdcCfgFile(),
		Source:      summarizeSource(cfg.Source),
		Target:      summarizeTarget(cfg.Target),
		CDC:         summarizeCDC(cfg.CDC),
		HasPassword: cfg.Source.Password != "" || cfg.Target.Password != "",
	})
}

// cdcConfigPutBody is the PUT /cdc/config body. Only source/target
// connection fields are editable here; the cdc section stays in config.yaml.
// Passwords: a non-empty value updates config.yaml (the CDC child needs real
// credentials); an empty/absent value keeps the stored one, and passwords are
// never returned by GET.
type cdcConfigPutBody struct {
	Source *cdcSourcePut `json:"source"`
	Target *cdcTargetPut `json:"target"`
}

type cdcSourcePut struct {
	Type     *string `json:"type"`
	Host     *string `json:"host"`
	Port     *int    `json:"port"`
	User     *string `json:"user"`
	Password *string `json:"password"`
	Database *string `json:"database"`
	Schema   *string `json:"schema"`
	SSLMode  *string `json:"sslmode"`
}

type cdcTargetPut struct {
	Host     *string `json:"host"`
	Port     *int    `json:"port"`
	User     *string `json:"user"`
	Password *string `json:"password"`
	Database *string `json:"database"`
}

func applySourcePut(dst *config.SourceConfig, p *cdcSourcePut) {
	if p == nil {
		return
	}
	if p.Type != nil {
		dst.Type = strings.TrimSpace(*p.Type)
	}
	if p.Host != nil {
		dst.Host = strings.TrimSpace(*p.Host)
	}
	if p.Port != nil {
		dst.Port = *p.Port
	}
	if p.User != nil {
		dst.User = strings.TrimSpace(*p.User)
	}
	// Empty password = "keep the stored one" (GET never echoes it back, so
	// the UI round-trips blanks; only an explicit new value rewrites it).
	if p.Password != nil && *p.Password != "" {
		dst.Password = *p.Password
	}
	if p.Database != nil {
		dst.Database = strings.TrimSpace(*p.Database)
	}
	if p.Schema != nil {
		dst.Schema = strings.TrimSpace(*p.Schema)
	}
	if p.SSLMode != nil {
		dst.SSLMode = strings.TrimSpace(*p.SSLMode)
	}
}

func applyTargetPut(dst *config.TargetConfig, p *cdcTargetPut) {
	if p == nil {
		return
	}
	if p.Host != nil {
		dst.Host = strings.TrimSpace(*p.Host)
	}
	if p.Port != nil {
		dst.Port = *p.Port
	}
	if p.User != nil {
		dst.User = strings.TrimSpace(*p.User)
	}
	if p.Password != nil && *p.Password != "" {
		dst.Password = *p.Password
	}
	if p.Database != nil {
		dst.Database = strings.TrimSpace(*p.Database)
	}
}

func (s *Server) handlePutCDCConfig(w http.ResponseWriter, r *http.Request) {
	var req cdcConfigPutBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Source == nil && req.Target == nil {
		s.writeError(w, http.StatusBadRequest, "nothing to update: source or target required")
		return
	}

	cdcCfgMu.Lock()
	defer cdcCfgMu.Unlock()
	cfg, err := s.loadCDCConfig()
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	applySourcePut(&cfg.Source, req.Source)
	applyTargetPut(&cfg.Target, req.Target)
	if cfg.Source.Host == "" || cfg.Target.Host == "" {
		s.writeError(w, http.StatusBadRequest, "source and target host cannot be empty")
		return
	}
	if err := s.writeCDCConfig(cfg); err != nil {
		s.writeError(w, http.StatusInternalServerError, "写入 config.yaml 失败："+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "message": "已更新 config.yaml（CDC 下次启动生效）",
	})
}

// handleImportCDCFromDataSource (F-02 D4): write a postgres datasource (source)
// + a tidb datasource (target) into config.yaml in one click. The supervisor /
// CDC child run chain is untouched — this only edits the same fields the
// connection card edits, then the operator starts/restarts CDC as usual.
func (s *Server) handleImportCDCFromDataSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceRef string `json:"source_ref"`
		TargetRef string `json:"target_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SourceRef == "" || req.TargetRef == "" {
		s.writeError(w, http.StatusBadRequest, "source_ref and target_ref are required")
		return
	}

	srcEntry, err := s.resolveDataSourceRef(req.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if srcEntry.Type != "postgres" {
		s.writeError(w, http.StatusBadRequest, "source_ref: CDC 仅支持 PostgreSQL 源端数据源")
		return
	}
	tgtEntry, err := s.resolveDataSourceRef(req.TargetRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "target_ref: "+err.Error())
		return
	}
	if tgtEntry.Type != "tidb" {
		s.writeError(w, http.StatusBadRequest, "target_ref: 目标端数据源类型必须是 tidb")
		return
	}

	cdcCfgMu.Lock()
	defer cdcCfgMu.Unlock()
	cfg, err := s.loadCDCConfig()
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	cfg.Source = dataSourceToSourceConfig(srcEntry)
	cfg.Target = dataSourceToTargetConfig(tgtEntry)
	if err := s.writeCDCConfig(cfg); err != nil {
		s.writeError(w, http.StatusInternalServerError, "写入 config.yaml 失败："+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": fmt.Sprintf("已从数据源导入连接到 config.yaml（%s → %s）", srcEntry.Name, tgtEntry.Name),
		"source":  summarizeSource(cfg.Source),
		"target":  summarizeTarget(cfg.Target),
	})
}

// handleImportCDCConfig copies the source/target of the latest completed
// migration task into config.yaml (the cdc section and everything else are
// left untouched). This closes the wizard↔CDC connection split (A1).
func (s *Server) handleImportCDCConfig(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks(50, 0)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var latest *store.Task
	for _, t := range tasks {
		if t.Status == store.TaskStatusCompleted {
			latest = t
			break // ListTasks is created_at DESC: first hit is the newest
		}
	}
	if latest == nil {
		s.writeError(w, http.StatusConflict, "没有已完成的迁移任务，无法导入连接")
		return
	}
	var taskCfg config.Config
	if err := json.Unmarshal([]byte(latest.ConfigJSON), &taskCfg); err != nil {
		s.writeError(w, http.StatusInternalServerError, "任务配置解析失败："+err.Error())
		return
	}

	cdcCfgMu.Lock()
	defer cdcCfgMu.Unlock()
	cfg, err := s.loadCDCConfig()
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	cfg.Source = taskCfg.Source
	cfg.Target = taskCfg.Target
	if err := s.writeCDCConfig(cfg); err != nil {
		s.writeError(w, http.StatusInternalServerError, "写入 config.yaml 失败："+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"message": fmt.Sprintf("已从任务 %s（%s）导入连接到 config.yaml", latest.ID, latest.Name),
		"task_id": latest.ID,
		"source":  summarizeSource(cfg.Source),
		"target":  summarizeTarget(cfg.Target),
	})
}
