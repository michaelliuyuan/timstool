package webapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// CDC startup precheck (A2) + resume-point inspection (A3): every hard CDC
// requirement (wal_level, replication privileges, target reachability, base
// migration) is checked BEFORE Start is clicked, and the resume semantics
// (checkpoint LSN vs slot restart_lsn vs fresh slot) are made explicit.

// cdcPrecheckItem is one check result. Level: ok | warn | fail — warn items
// do not block start (e.g. tables without PK), fail items do.
type cdcPrecheckItem struct {
	Item   string `json:"item"`
	Label  string `json:"label"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

type cdcCheckpointInfo struct {
	Exists    bool      `json:"exists"`
	LSN       string    `json:"lsn,omitempty"`
	File      string    `json:"file,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type cdcSlotInfo struct {
	Exists     bool   `json:"exists"`
	Active     bool   `json:"active,omitempty"`
	RestartLSN string `json:"restart_lsn,omitempty"`
	LagBytes   int64  `json:"lag_bytes,omitempty"` // WAL retained behind current position
	CurrentLSN string `json:"current_lsn,omitempty"`
}

type cdcPrecheckResponse struct {
	Items      []cdcPrecheckItem `json:"items"`
	Checkpoint cdcCheckpointInfo `json:"checkpoint"`
	Slot       cdcSlotInfo       `json:"slot"`
	Conclusion string            `json:"conclusion"`
	WarnOnly   bool              `json:"warn_only"` // true when no item failed
	// NoPKTables is the structured no-PK table list (schema.table, annotation
	// stripped) so the UI can build REPLICA IDENTITY FULL fixes (P1 task 2).
	NoPKTables []string  `json:"no_pk_tables_list,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// cdcDBProber abstracts the live DB probes so handler tests can mock them.
type cdcDBProber interface {
	PingPG(cfg *config.Config) (version string, err error)
	WalLevel(cfg *config.Config) (string, error)
	HasReplicationRole(cfg *config.Config) (bool, error)
	NoPKTables(cfg *config.Config) ([]string, error)
	Slot(cfg *config.Config, slotName string) (restartLSN string, active bool, exists bool, err error)
	WalLag(cfg *config.Config, restartLSN string) (lagBytes int64, currentLSN string, err error)
	PingTarget(cfg *config.Config) error
}

// realCDCProber hits the actual databases via config.yaml connections.
type realCDCProber struct{}

func pgDB(cfg *config.Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.Source.DSN())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (realCDCProber) PingPG(cfg *config.Config) (string, error) {
	db, err := pgDB(cfg)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var version string
	if err := db.QueryRow("SHOW server_version").Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

func (realCDCProber) WalLevel(cfg *config.Config) (string, error) {
	db, err := pgDB(cfg)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var level string
	if err := db.QueryRow("SHOW wal_level").Scan(&level); err != nil {
		return "", err
	}
	return level, nil
}

func (realCDCProber) HasReplicationRole(cfg *config.Config) (bool, error) {
	db, err := pgDB(cfg)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var rep, super bool
	err = db.QueryRow(
		`SELECT rolreplication, rolsuper FROM pg_roles WHERE rolname = current_user`,
	).Scan(&rep, &super)
	if err != nil {
		return false, err
	}
	return rep || super, nil
}

func (realCDCProber) NoPKTables(cfg *config.Config) ([]string, error) {
	db, err := pgDB(cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	schema := cfg.Source.Schema
	if schema == "" {
		schema = "public"
	}
	rows, err := db.Query(`
		SELECT c.relname, c.relreplident FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r'
		  AND c.relname NOT LIKE 'pg2tidb_%'
		  AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)
		  AND c.relreplident NOT IN ('f', 'i')`,
		schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, ident string
		if err := rows.Scan(&name, &ident); err != nil {
			return nil, err
		}
		// Only tables that can neither locate old rows via PK nor via replica
		// identity remain (ident 'd' default / 'n' none): UPDATE/DELETE cannot
		// sync for them. FULL ('f') and INDEX ('i') tables are fine and stay
		// off this list.
		if ident == "n" {
			out = append(out, fmt.Sprintf("%s.%s (无主键且无 REPLICA IDENTITY)", schema, name))
		} else {
			out = append(out, fmt.Sprintf("%s.%s (无主键)", schema, name))
		}
	}
	return out, rows.Err()
}

func (realCDCProber) Slot(cfg *config.Config, slotName string) (string, bool, bool, error) {
	db, err := pgDB(cfg)
	if err != nil {
		return "", false, false, err
	}
	defer db.Close()
	var restartLSN string
	var active bool
	err = db.QueryRow(
		`SELECT restart_lsn::text, active FROM pg_replication_slots WHERE slot_name = $1`,
		slotName,
	).Scan(&restartLSN, &active)
	if err == sql.ErrNoRows {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return restartLSN, active, true, nil
}

func (realCDCProber) WalLag(cfg *config.Config, restartLSN string) (int64, string, error) {
	db, err := pgDB(cfg)
	if err != nil {
		return 0, "", err
	}
	defer db.Close()
	var current string
	var lag sql.NullFloat64
	if err := db.QueryRow(
		`SELECT pg_current_wal_lsn()::text, pg_wal_lsn_diff(pg_current_wal_lsn(), $1::pg_lsn)`,
		restartLSN,
	).Scan(&current, &lag); err != nil {
		return 0, "", err
	}
	return int64(lag.Float64), current, nil
}

func (realCDCProber) PingTarget(cfg *config.Config) error {
	db, err := sql.Open("mysql", cfg.Target.DSN())
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Ping()
}

// loadCheckpointInfo reads the CDC checkpoint file (A3). The path follows the
// cdc section (default .cdc_checkpoint.json relative to cwd, same resolution
// the child uses).
func loadCheckpointInfo(cfg *config.Config) cdcCheckpointInfo {
	path := cfg.CDC.CheckpointFile
	if path == "" {
		path = ".cdc_checkpoint.json"
	}
	info := cdcCheckpointInfo{File: path}
	cp, err := cdc.NewCheckpointManager(path).Load()
	if err != nil || cp == nil {
		return info
	}
	info.Exists = true
	info.LSN = cp.LSN.String()
	info.UpdatedAt = cp.Timestamp
	return info
}

// hasCompletedMigration reports whether a full migration already succeeded
// (full_incr mode expects a pre-migrated base).
func (s *Server) hasCompletedMigration() bool {
	tasks, err := s.store.ListTasks(50, 0)
	if err != nil {
		return false
	}
	for _, t := range tasks {
		if t.Status == store.TaskStatusCompleted {
			return true
		}
	}
	return false
}

func (s *Server) cdcProber() cdcDBProber {
	if s.cdcProbe != nil {
		return s.cdcProbe
	}
	return realCDCProber{}
}

func (s *Server) handleCDCPrecheck(w http.ResponseWriter, r *http.Request) {
	cfg, err := func() (*config.Config, error) {
		cdcCfgMu.Lock()
		defer cdcCfgMu.Unlock()
		return s.loadCDCConfig()
	}()
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	p := s.cdcProber()
	resp := cdcPrecheckResponse{CheckedAt: time.Now()}

	// ① source reachability + version
	if v, err := p.PingPG(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"source_conn", "源端连通性 / PG 版本", "fail",
			fmt.Sprintf("连接失败：%v", err)})
	} else {
		var major int
		fmt.Sscanf(v, "%d", &major)
		if major >= 10 {
			resp.Items = append(resp.Items, cdcPrecheckItem{"source_conn", "源端连通性 / PG 版本", "ok",
				"PostgreSQL " + v})
		} else {
			resp.Items = append(resp.Items, cdcPrecheckItem{"source_conn", "源端连通性 / PG 版本", "fail",
				fmt.Sprintf("PostgreSQL %s（需要 ≥10）", v)})
		}
	}

	// ② wal_level
	if lvl, err := p.WalLevel(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"wal_level", "wal_level", "fail",
			fmt.Sprintf("查询失败：%v", err)})
	} else if lvl == "logical" {
		resp.Items = append(resp.Items, cdcPrecheckItem{"wal_level", "wal_level", "ok", "logical"})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"wal_level", "wal_level", "fail",
			"当前 " + lvl + "（需 logical，改 postgresql.conf 后重启 PG）"})
	}

	// ③ replication privileges
	if ok, err := p.HasReplicationRole(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"repl_role", "REPLICATION 权限", "fail",
			fmt.Sprintf("查询失败：%v", err)})
	} else if ok {
		resp.Items = append(resp.Items, cdcPrecheckItem{"repl_role", "REPLICATION 权限", "ok",
			"当前用户具备 REPLICATION 或 SUPER 权限"})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"repl_role", "REPLICATION 权限", "fail",
			"当前用户无 REPLICATION 权限（ALTER ROLE xx REPLICATION）"})
	}

	// ④ target reachability
	if err := p.PingTarget(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"target_conn", "目标 TiDB 连通性", "fail",
			fmt.Sprintf("连接失败：%v", err)})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"target_conn", "目标 TiDB 连通性", "ok",
			fmt.Sprintf("%s:%d/%s", cfg.Target.Host, cfg.Target.Port, cfg.Target.Database)})
	}

	// ⑤ base migration completed (warn for incr_only, informational for full_incr)
	if s.hasCompletedMigration() {
		resp.Items = append(resp.Items, cdcPrecheckItem{"base_migration", "全量迁移基线", "ok",
			"已有成功的全量迁移任务"})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"base_migration", "全量迁移基线", "warn",
			"没有已成功的全量迁移任务；incr_only 模式需要先具备目标端表结构"})
	}

	// ⑥ tables without PK / replica identity (warn)
	if tables, err := p.NoPKTables(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"no_pk_tables", "无主键表预警", "warn",
			fmt.Sprintf("无法查询（不影响启动）：%v", err)})
	} else if len(tables) == 0 {
		resp.Items = append(resp.Items, cdcPrecheckItem{"no_pk_tables", "无主键表预警", "ok",
			"范围内所有表均有主键"})
	} else {
		resp.NoPKTables = stripNoPKAnnotations(tables)
		resp.Items = append(resp.Items, cdcPrecheckItem{"no_pk_tables", "无主键表预警", "warn",
			fmt.Sprintf("%d 张表无主键/REPLICA IDENTITY，UPDATE/DELETE 将无法同步：%s",
				len(tables), joinLimit(tables, 10))})
	}

	// A3: resume point
	resp.Checkpoint = loadCheckpointInfo(cfg)
	slotName := cfg.CDC.SlotName
	if slotName == "" {
		slotName = "pg2tidb_cdc"
	}
	restartLSN, active, exists, err := p.Slot(cfg, slotName)
	if err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"slot", "Replication Slot", "warn",
			fmt.Sprintf("无法查询 slot 状态：%v", err)})
	} else if exists {
		resp.Slot = cdcSlotInfo{Exists: true, Active: active, RestartLSN: restartLSN}
		if lag, cur, err := p.WalLag(cfg, restartLSN); err == nil {
			resp.Slot.LagBytes = lag
			resp.Slot.CurrentLSN = cur
		}
		resp.Items = append(resp.Items, cdcPrecheckItem{"slot", "Replication Slot", "ok",
			fmt.Sprintf("slot %s 已存在（active=%v, restart_lsn=%s, 滞留 WAL %s）",
				slotName, active, restartLSN, formatBytes(resp.Slot.LagBytes))})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"slot", "Replication Slot", "ok",
			"slot 不存在，启动时将从当前 LSN 新建 " + slotName})
	}

	// Conclusion: explicit resume semantics.
	resp.Conclusion = buildResumeConclusion(&resp, cfg, slotName)
	resp.WarnOnly = true
	for _, it := range resp.Items {
		if it.Level == "fail" {
			resp.WarnOnly = false
			break
		}
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// buildResumeConclusion states where CDC will start from and flags the
// full-migration vs fresh-slot gap (A3).
func buildResumeConclusion(resp *cdcPrecheckResponse, cfg *config.Config, slotName string) string {
	switch {
	case resp.Checkpoint.Exists:
		return fmt.Sprintf("将从 checkpoint 恢复：LSN %s（%s）",
			resp.Checkpoint.LSN, resp.Checkpoint.UpdatedAt.Format("2006-01-02 15:04:05"))
	case resp.Slot.Exists:
		return fmt.Sprintf("checkpoint 缺失，将从 slot %s 的 restart_lsn=%s 重放（宁重放不丢数据）",
			slotName, resp.Slot.RestartLSN)
	default:
		msg := fmt.Sprintf("将新建 slot %s 并从当前 LSN 开始（仅捕获启动之后的增量）", slotName)
		if sHasBase(resp) {
			// A base migration already ran: data changed between migration
			// and CDC start is NOT captured by a fresh slot.
			return msg + "；注意：若此前已完成全量迁移，全量完成点到 CDC 启动点之间的增量不会被捕获，需用「数据比对」核验后补差"
		}
		return msg
	}
}

func sHasBase(resp *cdcPrecheckResponse) bool {
	for _, it := range resp.Items {
		if it.Item == "base_migration" && it.Level == "ok" {
			return true
		}
	}
	return false
}

// stripNoPKAnnotations turns prober entries like "public.t1 (无主键)" /
// "public.t2 (REPLICA IDENTITY d)" into plain "public.t1" refs.
func stripNoPKAnnotations(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		for i := 0; i < len(e); i++ {
			if e[i] == ' ' {
				out = append(out, e[:i])
				break
			}
		}
	}
	return out
}

func joinLimit(items []string, n int) string {
	if len(items) <= n {
		out := ""
		for i, s := range items {
			if i > 0 {
				out += ", "
			}
			out += s
		}
		return out
	}
	return joinLimit(items[:n], n) + " …"
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// handleCDCSlot serves the live slot/checkpoint view for the running
// dashboard (A3): lag bytes grow while CDC is stopped (slot holds WAL).
func (s *Server) handleCDCSlot(w http.ResponseWriter, r *http.Request) {
	cfg, err := func() (*config.Config, error) {
		cdcCfgMu.Lock()
		defer cdcCfgMu.Unlock()
		return s.loadCDCConfig()
	}()
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	p := s.cdcProber()
	slotName := cfg.CDC.SlotName
	if slotName == "" {
		slotName = "pg2tidb_cdc"
	}
	resp := struct {
		Slot          cdcSlotInfo       `json:"slot"`
		Checkpoint    cdcCheckpointInfo `json:"checkpoint"`
		CheckpointAge float64           `json:"checkpoint_age_seconds,omitempty"`
	}{Checkpoint: loadCheckpointInfo(cfg)}
	if restartLSN, active, exists, err := p.Slot(cfg, slotName); err == nil && exists {
		resp.Slot = cdcSlotInfo{Exists: true, Active: active, RestartLSN: restartLSN}
		if lag, cur, err := p.WalLag(cfg, restartLSN); err == nil {
			resp.Slot.LagBytes = lag
			resp.Slot.CurrentLSN = cur
		}
	}
	if resp.Checkpoint.Exists {
		resp.CheckpointAge = time.Since(resp.Checkpoint.UpdatedAt).Seconds()
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleCDCResetCheckpoint deletes the checkpoint file (danger op, A3). The
// slot is deliberately left alone; the response spells out the consequences.
func (s *Server) handleCDCResetCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Confirm != "DELETE" {
		s.writeError(w, http.StatusBadRequest, `需要 {"confirm":"DELETE"} 确认`)
		return
	}
	if s.cdcSupervisor != nil {
		st := s.cdcSupervisor.Status()
		if st.State == StateRunning || st.State == StateAdopted || st.State == StateStarting {
			s.writeError(w, http.StatusConflict, "CDC 正在运行，请先停止再重置断点")
			return
		}
	}
	cfg, err := func() (*config.Config, error) {
		cdcCfgMu.Lock()
		defer cdcCfgMu.Unlock()
		return s.loadCDCConfig()
	}()
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	path := cfg.CDC.CheckpointFile
	if path == "" {
		path = ".cdc_checkpoint.json"
	}
	if _, err := os.Stat(path); err != nil {
		s.writeError(w, http.StatusNotFound, "checkpoint 文件不存在")
		return
	}
	// Rename instead of delete: the reset stays reversible (`.bak.<ts>`), the
	// operator can restore it manually if the reset was a mistake.
	bak := fmt.Sprintf("%s.bak.%d", path, time.Now().Unix())
	if err := os.Rename(path, bak); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
		"message": "checkpoint 已重置（备份为 " + bak + "，可手动恢复）。slot 未动：下次启动仍从 slot restart_lsn 重放；" +
			"如需彻底重来请手动删除 slot（SELECT pg_drop_replication_slot('" + cfgSlot(cfg) + "')）",
		"backup": bak,
	})
}

func cfgSlot(cfg *config.Config) string {
	if cfg.CDC.SlotName != "" {
		return cfg.CDC.SlotName
	}
	return "pg2tidb_cdc"
}
