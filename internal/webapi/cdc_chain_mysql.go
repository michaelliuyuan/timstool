package webapi

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/cdc"
	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// MS-11 pen 4: the MySQL side of the 全量+增量衔接. PG provisions a
// publication + logical slot so the migration window's WAL is retained; MySQL
// needs no provisioning — the binlog itself is the retention — so chain prep
// is just recording the current master file:pos (the same probe precheck
// uses). The recorded position is stored in Migration.ChainStartLSN in the
// canonical "file:pos" rendering and seeded into the checkpoint file after a
// successful migration, mirroring the PG consistent-point semantics.

// prepareCDCChainFor is the single chain-prep dispatch (capability guard
// already passed at the handler; PG path untouched).
func (s *Server) prepareCDCChainFor(cfg *config.Config) (startPos string, reused bool, err error) {
	if sourceIsMySQL(cfg.Source) {
		return s.prepareCDCChainMySQL(cfg)
	}
	return s.prepareCDCChain(cfg)
}

// prepareCDCChainMySQL records the source's current master binlog position.
// reused is always false (nothing is provisioned).
func (s *Server) prepareCDCChainMySQL(cfg *config.Config) (string, bool, error) {
	file, pos, _, err := s.cdcMySQLProber().MySQLMasterStatus(cfg)
	if err != nil {
		return "", false, fmt.Errorf("读取 MySQL master 位点失败：%w", err)
	}
	return fmt.Sprintf("%s:%d", file, pos), false, nil
}

// parseChainBinlogPos parses the "file:pos" ChainStartLSN rendering.
func parseChainBinlogPos(s string) (cdc.BinlogPosition, error) {
	i := strings.LastIndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return cdc.BinlogPosition{}, fmt.Errorf("非法 binlog 位点 %q（期望 file:pos）", s)
	}
	pos, err := strconv.ParseUint(s[i+1:], 10, 32)
	if err != nil {
		return cdc.BinlogPosition{}, fmt.Errorf("解析 binlog 位点 %q: %w", s, err)
	}
	return cdc.BinlogPosition{File: s[:i], Pos: uint32(pos)}, nil
}

// seedChainCheckpointMySQL seeds the recorded chain file:pos into the CDC
// checkpoint file — unless an existing checkpoint is already at or past it
// (resume semantics: never rewind, mirroring the PG seed).
func (s *Server) seedChainCheckpointMySQL(taskID string, cfg *config.Config) error {
	if cfg.Migration.ChainStartLSN == "" {
		return nil // nothing recorded; child starts from the current master position
	}
	bp, err := parseChainBinlogPos(cfg.Migration.ChainStartLSN)
	if err != nil {
		return err
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
	if existing, lErr := mgr.Load(); lErr == nil && existing != nil && existing.Binlog != nil {
		if !binlogBefore(*existing.Binlog, bp) {
			s.logCollector.Append(taskID, "WARN",
				fmt.Sprintf("CDC chain: 现有 checkpoint 位点 %s ≥ 链点位 %s，沿用现有断点不倒退",
					existing.Binlog.String(), bp.String()), "")
			return nil
		}
		s.logCollector.Append(taskID, "INFO",
			fmt.Sprintf("CDC chain: 现有 checkpoint 位点 %s 落后于链点位 %s，预置为链点位（覆盖旧值）",
				existing.Binlog.String(), bp.String()), "")
	} else if lErr != nil {
		return fmt.Errorf("读取现有 checkpoint: %w", lErr)
	}
	mgr.UpdateBinlog(bp)
	return mgr.Save()
}

// binlogBefore reports whether a is strictly before b (same file: offset;
// different file: lexicographic file order — binlog file names are ordered).
func binlogBefore(a, b cdc.BinlogPosition) bool {
	if a.File != b.File {
		return a.File < b.File
	}
	return a.Pos < b.Pos
}
