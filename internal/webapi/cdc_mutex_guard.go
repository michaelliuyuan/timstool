package webapi

// cdc_mutex_guard.go — MS-11 pen 3, ruling #3 (seq 953): for a MySQL source,
// the watermark-polling incremental engine and the binlog CDC v1 stream are
// MUTUALLY EXCLUSIVE on the same source. Running both would interleave two
// writers into TiDB with no shared ordering — double-writes and lost-update
// hazards. Both directions are refused server-side with an explicit
// choose-one message (the FE surfaces the same rule, pen 4).

import (
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// dualIncrementMsg is the shared remediation message (backend face).
const dualIncrementMsg = "MySQL 源的「时间戳水位增量」与「binlog CDC」互斥（v1）：同一源端双增量并存会双写乱序。请二选一：停止 CDC 链后再跑水位增量，或改用 CDC 并停用水位增量任务。"

// sourceIsMySQL is the single kind branch of this guard file (A3 hygiene:
// one sanctioned dispatch point, everything else routes through it).
func sourceIsMySQL(sc config.SourceConfig) bool {
	return sc.SourceType() == "mysql"
}

// mysqlDualIncrementKey renders the mutex identity of a MySQL source.
func mysqlDualIncrementKey(sc config.SourceConfig) string {
	return fmt.Sprintf("%s:%d:%s:%s", sc.Host, sc.Port, sc.User, sc.Database)
}

// cdcBinlogRunningKey reports the mutex key when the CDC child is live AND
// its configured source is MySQL; empty string otherwise (PG CDC and a dead
// child never block the polling engine).
func (s *Server) cdcBinlogRunningKey() string {
	cdcCfgMu.Lock()
	cfg, err := s.loadCDCConfig()
	cdcCfgMu.Unlock()
	if err != nil || !sourceIsMySQL(cfg.Source) {
		return ""
	}
	if !s.cdcStatus().Running {
		return ""
	}
	return mysqlDualIncrementKey(cfg.Source)
}

// incrementalRunningKeys lists mutex keys of sources with a live polling
// run (history stub status=running). Unresolvable refs are skipped — the
// engine itself will fail those runs loudly on their own.
func (s *Server) incrementalRunningKeys() map[string]bool {
	keys := make(map[string]bool)
	incMu.Lock()
	list := s.loadIncrementalJobs()
	incMu.Unlock()
	for i := range list {
		running := false
		for j := range list[i].History {
			if list[i].History[j].Status == incRunStatusRunning {
				running = true
				break
			}
		}
		if !running {
			continue
		}
		src, err := s.resolveDataSourceRef(list[i].SourceRef)
		if err != nil || !sourceIsMySQL(dataSourceToSourceConfig(src)) {
			continue
		}
		keys[mysqlDualIncrementKey(dataSourceToSourceConfig(src))] = true
	}
	return keys
}

// blockIncrementalRunForCDC returns true when a polling run on sc must be
// refused because binlog CDC is live on the same MySQL source.
func (s *Server) blockIncrementalRunForCDC(sc config.SourceConfig) bool {
	if !sourceIsMySQL(sc) {
		return false
	}
	key := s.cdcBinlogRunningKey()
	return key != "" && key == mysqlDualIncrementKey(sc)
}

// blockCDCStartForIncremental returns true when starting the binlog CDC
// child must be refused because a polling run is live on the same MySQL
// source.
func (s *Server) blockCDCStartForIncremental() bool {
	cfg, err := func() (*config.Config, error) {
		cdcCfgMu.Lock()
		defer cdcCfgMu.Unlock()
		return s.loadCDCConfig()
	}()
	if err != nil || !sourceIsMySQL(cfg.Source) {
		return false
	}
	return s.incrementalRunningKeys()[mysqlDualIncrementKey(cfg.Source)]
}
