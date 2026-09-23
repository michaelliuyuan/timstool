package webapi

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pglogrepl"
	"github.com/michaelliuyuan/timstool/internal/cdc"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/orchestrator"
)

// CDC chain (P1 task 1): "全量+增量衔接". When a migration task opts in via
// migration.cdc_chain, the server pre-creates the CDC publication + logical
// replication slot BEFORE the full migration runs — the slot then retains all
// WAL produced during the migration window, and after a successful migration
// the CDC supervisor is auto-started so replay begins exactly at the recorded
// consistent point. Zero-loss by construction (upsert/replace dedup handles
// the overlap between full data and replayed WAL).

// cdcChainProber abstracts the source-side DDL so handler tests can mock it.
type cdcChainProber interface {
	// EnsurePublication creates the publication (FOR ALL TABLES) if missing.
	// Returns whether it was created (false = reused an existing one).
	EnsurePublication(cfg *config.Config, name string) (created bool, err error)
	// EnsureSlot creates the logical replication slot if missing and returns
	// the consistent-point LSN plus whether the slot already existed.
	EnsureSlot(cfg *config.Config, name string) (lsn string, reused bool, err error)
}

// realCDCChainProber hits the actual source PG via the task's own connection.
type realCDCChainProber struct{}

func (realCDCChainProber) EnsurePublication(cfg *config.Config, name string) (bool, error) {
	if err := validateIdentName(name); err != nil {
		return false, err
	}
	db, err := pgDB(cfg)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_publication WHERE pubname=$1)`, name).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	_, err = db.Exec(fmt.Sprintf(`CREATE PUBLICATION %s FOR ALL TABLES`, name))
	return err == nil, err
}

func (realCDCChainProber) EnsureSlot(cfg *config.Config, name string) (string, bool, error) {
	if err := validateIdentName(name); err != nil {
		return "", false, err
	}
	db, err := pgDB(cfg)
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	var restartLSN string
	err = db.QueryRow(`SELECT restart_lsn::text FROM pg_replication_slots WHERE slot_name=$1`, name).Scan(&restartLSN)
	if err == nil {
		return restartLSN, true, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	var lsn string
	if err := db.QueryRow(`SELECT consistent_point::text FROM pg_create_logical_replication_slot($1, 'pgoutput')`, name).Scan(&lsn); err != nil {
		return "", false, err
	}
	return lsn, false, nil
}

// chainPublicationName / chainSlotName resolve names from the CDC config
// section with the same defaults the CDC child uses.
func chainPublicationName(cfg *config.Config) string {
	if cfg.CDC.Publication != "" {
		return cfg.CDC.Publication
	}
	return "pg2tidb_pub"
}

func chainSlotName(cfg *config.Config) string {
	if cfg.CDC.SlotName != "" {
		return cfg.CDC.SlotName
	}
	return "pg2tidb_cdc"
}

// prepareCDCChain runs before a chained migration starts: publication + slot
// are guaranteed to exist and the slot's consistent point is returned. Failure
// aborts the task (better not to run than to silently lose the window).
func (s *Server) prepareCDCChain(cfg *config.Config) (lsn string, reusedSlot bool, err error) {
	p := s.cdcChainProbe
	if p == nil {
		p = realCDCChainProber{}
	}
	if _, err := p.EnsurePublication(cfg, chainPublicationName(cfg)); err != nil {
		return "", false, fmt.Errorf("预建 publication 失败：%w", err)
	}
	lsn, reused, err := p.EnsureSlot(cfg, chainSlotName(cfg))
	if err != nil {
		return "", false, fmt.Errorf("预建 replication slot 失败：%w", err)
	}
	return lsn, reused, nil
}

// startCDCChainAfterSuccess auto-starts the CDC supervisor after a successful
// chained migration. Failure is loud (task log ERROR + broadcast) but does not
// flip the completed task to failed — the slot retains WAL, the operator can
// start CDC manually with zero loss.
//
// Before Start, the chain seeds the CDC checkpoint file with the recorded
// ChainStartLSN so the child replays the migration window deterministically:
// runner loads the checkpoint and passes that LSN to StartReplication. Under
// either PG semantics for the requested LSN (hint vs max(requested,
// confirmed_flush)), seeding makes the consistent point the effective start.
// An existing checkpoint (≥ our LSN by construction) is never clobbered.
func (s *Server) startCDCChainAfterSuccess(taskID string, cfg *config.Config) {
	msg := ""
	ok := true
	switch {
	case s.cdcSupervisor == nil:
		ok = false
		msg = "CDC 自动衔接失败：本服务未接入 CDC 控制（supervisor 未接线），请手动启动 CDC（slot 已保留 WAL，无数据丢失）"
	default:
		if st := s.cdcSupervisor.Status(); st.State == StateRunning || st.State == StateStarting || st.State == StateAdopted {
			msg = "CDC 已在运行，跳过自动衔接（现有 checkpoint/slot 点位优先）"
			break
		}
		if seedErr := s.seedChainCheckpoint(taskID, cfg); seedErr != nil {
			ok = false
			msg = fmt.Sprintf("CDC 自动衔接失败（预置 checkpoint 出错）：%v；slot 已保留 WAL，请手动启动 CDC 并核对起点", seedErr)
			break
		}
		if _, err := s.cdcSupervisor.Start(context.Background()); err != nil {
			ok = false
			msg = fmt.Sprintf("CDC 自动衔接失败：%v（slot 已保留 WAL，请手动启动 CDC，无数据丢失）", err)
		} else {
			lsn := cfg.Migration.ChainStartLSN
			if lsn == "" {
				lsn = "slot 创建点位（见任务日志）"
			}
			msg = fmt.Sprintf("增量已自动衔接：已按记录 LSN=%s 预置 checkpoint，CDC 将从该点位重放迁移窗口的变更（slot=%s，publication=%s；conflict_strategy=%s 幂等去重）",
				lsn, chainSlotName(cfg), chainPublicationName(cfg), chainConflictStrategy(cfg))
		}
	}
	if ok {
		s.logCollector.Append(taskID, "INFO", "CDC chain: "+msg, "")
	} else {
		s.logCollector.Append(taskID, "ERROR", "CDC chain: "+msg, "")
	}
	s.BroadcastProgress(taskID, map[string]interface{}{
		"phase":   "cdc_chain",
		"message": msg,
		"ok":      ok,
	})
}

// seedChainCheckpoint writes ChainStartLSN into the CDC checkpoint file the
// child will load — unless a checkpoint already exists (resume semantics: an
// existing checkpoint is always ≥ the chain point and wins).
func (s *Server) seedChainCheckpoint(taskID string, cfg *config.Config) error {
	if cfg.Migration.ChainStartLSN == "" {
		return nil // nothing recorded (e.g. slot reused pre-chain); child falls back to slot semantics
	}
	lsn, err := pglogrepl.ParseLSN(cfg.Migration.ChainStartLSN)
	if err != nil {
		return fmt.Errorf("解析 ChainStartLSN %q: %w", cfg.Migration.ChainStartLSN, err)
	}
	cdcCfg, err := func() (*config.Config, error) {
		cdcCfgMu.Lock()
		defer cdcCfgMu.Unlock()
		return s.loadCDCConfig()
	}()
	if err != nil {
		return fmt.Errorf("读取 CDC 配置: %w", err)
	}
	path := cdcCfg.CDC.CheckpointFile
	if path == "" {
		path = ".cdc_checkpoint.json"
	}
	mgr := cdc.NewCheckpointManager(path)
	if existing, lErr := mgr.Load(); lErr == nil && existing != nil {
		trusted := existing.SlotName == "" || existing.SlotName == chainSlotName(cfg)
		if trusted && existing.LSN >= lsn {
			// Same-slot checkpoint already at or past the chain point (a prior
			// CDC run advanced beyond it): keep the newer position, never rewind.
			s.logCollector.Append(taskID, "WARN",
				fmt.Sprintf("CDC chain: 现有 checkpoint LSN=%s ≥ 链点位 %s，沿用现有断点不倒退", existing.LSN.String(), lsn.String()), "")
			return nil
		}
		if !trusted {
			s.logCollector.Append(taskID, "INFO",
				fmt.Sprintf("CDC chain: 现有 checkpoint 属其它 slot（%q ≠ %q），其 LSN 不作为本链起点，预置为链点位",
					existing.SlotName, chainSlotName(cfg)), "")
		} else {
			s.logCollector.Append(taskID, "INFO",
				fmt.Sprintf("CDC chain: 现有 checkpoint LSN=%s 落后于链点位 %s，预置为链点位（覆盖旧值）", existing.LSN.String(), lsn.String()), "")
		}
	} else if lErr != nil {
		return fmt.Errorf("读取现有 checkpoint: %w", lErr)
	}
	mgr.SetSlotName(chainSlotName(cfg))
	mgr.Update(lsn)
	return mgr.Save()
}

func chainConflictStrategy(cfg *config.Config) string {
	if cfg.CDC.ConflictStrategy != "" {
		return cfg.CDC.ConflictStrategy
	}
	return "replace"
}

// onlyValidateFailed reports whether every failing pipeline result is the
// validate phase and chaining is enabled — the demotion precondition.
func onlyValidateFailed(results []orchestrator.PipelineResult, chain bool) bool {
	if !chain {
		return false
	}
	anyFailed := false
	for _, r := range results {
		if !r.Success {
			anyFailed = true
			if r.Phase != orchestrator.PhaseValidate {
				return false
			}
		}
	}
	return anyFailed
}

// validateIdentName defends the sprintf'd DDL: publication/slot names come
// from config.yaml (trusted operator input) but are still checked against the
// plain-identifier whitelist for defense in depth.
func validateIdentName(name string) error {
	if name == "" || !identRe.MatchString(name) {
		return fmt.Errorf("非法名称 %q（仅支持普通标识符）", name)
	}
	return nil
}
