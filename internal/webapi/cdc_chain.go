package webapi

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/common/config"
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
func (s *Server) startCDCChainAfterSuccess(taskID string, cfg *config.Config) {
	msg := ""
	ok := true
	switch {
	case s.cdcSupervisor == nil:
		ok = false
		msg = "CDC 自动衔接失败：本服务未接入 CDC 控制（supervisor 未接线），请手动启动 CDC（slot 已保留 WAL，无数据丢失）"
	default:
		if _, err := s.cdcSupervisor.Start(context.Background()); err != nil {
			ok = false
			msg = fmt.Sprintf("CDC 自动衔接失败：%v（slot 已保留 WAL，请手动启动 CDC，无数据丢失）", err)
		} else {
			msg = fmt.Sprintf("增量已自动衔接，起点 LSN=%s（slot=%s，publication=%s）；conflict_strategy=%s 幂等重放",
				cfg.Migration.ChainStartLSN, chainSlotName(cfg), chainPublicationName(cfg), chainConflictStrategy(cfg))
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

func chainConflictStrategy(cfg *config.Config) string {
	if cfg.CDC.ConflictStrategy != "" {
		return cfg.CDC.ConflictStrategy
	}
	return "replace"
}
