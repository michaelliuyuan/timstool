package webapi

// cdc_precheck_mysql.go — MS-11 pen 3: the MySQL route of the CDC precheck.
// Mirrors the PG six-item shape (source/version, replication enablement,
// privileges, target, base migration, no-PK warn) plus the binlog resume
// point (checkpoint file:pos vs SHOW MASTER STATUS). v1 hard requirements
// per ruling seq 953: floor ≥5.7, ROW format, FULL row image, REPLICATION
// SLAVE+CLIENT; DDL is not supported (hard-stop at runtime).

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/michaelliuyuan/timstool/internal/cdc"
	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// cdcMySQLProber abstracts the live MySQL probes so handler tests can mock
// them (separate from the PG cdcDBProber so existing stubs keep compiling).
type cdcMySQLProber interface {
	PingMySQL(cfg *config.Config) (version string, err error)
	MySQLVar(cfg *config.Config, name string) (string, error)
	MySQLReplPrivileges(cfg *config.Config) (slave, client bool, err error)
	MySQLNoPKTables(cfg *config.Config) ([]string, error)
	MySQLMasterStatus(cfg *config.Config) (file string, pos uint32, expireSeconds int64, err error)
	PingTarget(cfg *config.Config) error
}

type realCDCMySQLProber struct{}

// PingTarget reuses the PG prober's target reachability probe (same TiDB
// wire protocol and DSN shape).
func (realCDCMySQLProber) PingTarget(cfg *config.Config) error {
	return realCDCProber{}.PingTarget(cfg)
}

func myDB(cfg *config.Config) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?timeout=10s&readTimeout=30s",
		cfg.Source.User, cfg.Source.Password, cfg.Source.Host, cfg.Source.Port, cfg.Source.Database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (realCDCMySQLProber) PingMySQL(cfg *config.Config) (string, error) {
	db, err := myDB(cfg)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var version string
	if err := db.QueryRow("SELECT VERSION()").Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

// mysqlVarSQL renders SHOW GLOBAL VARIABLES with the variable name inlined.
// MS-11b: name is an internal whitelist constant (log_bin / binlog_format /
// binlog_row_image — see handleCDCPrecheckMySQL), never user input; some
// MySQL 8 builds reject the parameterized LIKE, so we mirror the literal
// form already proven at the binlog_expire_logs_seconds probe below.
func mysqlVarSQL(name string) string {
	return "SHOW GLOBAL VARIABLES LIKE '" + name + "'"
}

// mysqlNoPKSQL renders the no-PK table scan with the schema name inlined as
// an escaped string literal. MS-11b: information_schema refuses the `?`
// placeholder on some builds (schema is config-controlled, not operator
// free-typed at runtime); quoting is the same ”-doubling shape as
// incQuoteSQLLiteral.
// MS-11e pen 3: the NOT EXISTS subquery on KEY_COLUMN_USAGE misreports on
// some 8.0 builds (notably te's 8.0.26 — PK and no-PK tables both skew);
// the LEFT JOIN TABLE_CONSTRAINTS … IS NULL anti-join shape is the form
// verified correct on that build, so v1 switches to it.
func mysqlNoPKSQL(schema string) string {
	return fmt.Sprintf(`
		SELECT t.TABLE_NAME
		FROM information_schema.TABLES t
		LEFT JOIN information_schema.TABLE_CONSTRAINTS tc
		  ON tc.TABLE_SCHEMA = t.TABLE_SCHEMA AND tc.TABLE_NAME = t.TABLE_NAME
		 AND tc.CONSTRAINT_NAME = 'PRIMARY'
		WHERE t.TABLE_SCHEMA = %s AND t.TABLE_TYPE = 'BASE TABLE'
		  AND tc.CONSTRAINT_NAME IS NULL`, incQuoteSQLLiteral(schema))
}

func (realCDCMySQLProber) MySQLVar(cfg *config.Config, name string) (string, error) {
	db, err := myDB(cfg)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var key, val string
	if err := db.QueryRow(mysqlVarSQL(name)).Scan(&key, &val); err != nil {
		return "", err
	}
	return val, nil
}

func (realCDCMySQLProber) MySQLReplPrivileges(cfg *config.Config) (bool, bool, error) {
	db, err := myDB(cfg)
	if err != nil {
		return false, false, err
	}
	defer db.Close()
	rows, err := db.Query("SHOW GRANTS FOR CURRENT_USER()")
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	slave, client := false, false
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return false, false, err
		}
		up := strings.ToUpper(g)
		if strings.Contains(up, "REPLICATION SLAVE") || strings.Contains(up, "ALL PRIVILEGES") || strings.Contains(up, "SUPER") {
			slave = true
		}
		if strings.Contains(up, "REPLICATION CLIENT") || strings.Contains(up, "ALL PRIVILEGES") || strings.Contains(up, "SUPER") {
			client = true
		}
	}
	return slave, client, rows.Err()
}

func (realCDCMySQLProber) MySQLNoPKTables(cfg *config.Config) ([]string, error) {
	db, err := myDB(cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(mysqlNoPKSQL(cfg.Source.Database))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, cfg.Source.Database+"."+name)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// rowScanner abstracts *sql.Row so the master-status scan shapes are unit
// testable without a live server.
type rowScanner interface{ Scan(dest ...any) error }

// scanMasterStatus5 scans the modern 5-column SHOW MASTER STATUS row
// (file, position, binlog_do_db, binlog_ignore_db, executed_gtid_set) — the
// three trailing columns are discarded via *any sinks (MS-11b: nil dests
// panic on some drivers; new(any) is the discard form).
func scanMasterStatus5(row rowScanner) (file string, pos sql.NullInt64, err error) {
	var d1, d2, d3 any
	err = row.Scan(&file, &pos, &d1, &d2, &d3)
	return file, pos, err
}

// scanMasterStatus2 scans the legacy two-column form (file, position) — the
// final fallback for older column layouts.
func scanMasterStatus2(row rowScanner) (file string, pos sql.NullInt64, err error) {
	err = row.Scan(&file, &pos)
	return file, pos, err
}

// scanMasterStatus4 scans the MariaDB 4-column form (file, position,
// binlog_do_db, binlog_ignore_db — no executed_gtid_set column). MS-11e E4:
// MariaDB fails BOTH the 5- and 2-column scans, and the surfaced wording
// must name the layout problem instead of a raw column-count driver error.
func scanMasterStatus4(row rowScanner) (file string, pos sql.NullInt64, err error) {
	var d1, d2 any
	err = row.Scan(&file, &pos, &d1, &d2)
	return file, pos, err
}

func (realCDCMySQLProber) MySQLMasterStatus(cfg *config.Config) (string, uint32, int64, error) {
	db, err := myDB(cfg)
	if err != nil {
		return "", 0, 0, err
	}
	defer db.Close()
	file, pos, err := scanMasterStatus5(db.QueryRow("SHOW MASTER STATUS"))
	if err != nil {
		if err == sql.ErrNoRows {
			return "", 0, 0, fmt.Errorf("SHOW MASTER STATUS 无结果（log_bin 未开启？）")
		}
		// Column count varies across versions: MySQL 8 = 5, MariaDB = 4
		// (no executed_gtid_set), old MySQL = 2 (MS-11e E4 adds the 4-col
		// branch so MariaDB no longer falls through to a raw driver error).
		if f4, p4, err4 := scanMasterStatus4(db.QueryRow("SHOW MASTER STATUS")); err4 == nil {
			file, pos = f4, p4
		} else if f2, p2, err2 := scanMasterStatus2(db.QueryRow("SHOW MASTER STATUS")); err2 == nil {
			file, pos = f2, p2
		} else {
			return "", 0, 0, fmt.Errorf(
				"SHOW MASTER STATUS 列数不识别（5/4/2 列扫描均失败，非标布局或权限受限；原始错误：%v）", err)
		}
	}
	expire := int64(0)
	var evKey, ev string
	if err := db.QueryRow("SHOW GLOBAL VARIABLES LIKE 'binlog_expire_logs_seconds'").Scan(&evKey, &ev); err == nil {
		expire, _ = strconv.ParseInt(ev, 10, 64)
	}
	return file, uint32(pos.Int64), expire, nil
}

// cpBinlogCheckpointPath resolves the checkpoint file path the same way
// loadCheckpointInfo does.
func cpBinlogCheckpointPath(cfg *config.Config) string {
	if p := cfg.CDC.CheckpointFile; p != "" {
		return p
	}
	return ".cdc_checkpoint.json"
}

// mysqlVersionAt reports whether the server version meets the floor
// (major.minor). 8.0 recommended; 5.7 is the v1 floor.
func mysqlVersionAt(version string, floorMajor, floorMinor int) bool {
	var major, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err != nil {
		return false
	}
	return major > floorMajor || (major == floorMajor && minor >= floorMinor)
}

func (s *Server) cdcMySQLProber() cdcMySQLProber {
	if p, ok := s.cdcProbe.(cdcMySQLProber); ok {
		return p
	}
	return realCDCMySQLProber{}
}

// handleCDCPrecheckMySQL builds the MySQL six-item precheck + binlog resume
// point. Ruling seq 953 #2: DDL unsupported in v1 — surfaced in the
// conclusion, enforced by a runtime hard-stop.
func (s *Server) handleCDCPrecheckMySQL(w http.ResponseWriter, cfg *config.Config) {
	p := s.cdcMySQLProber()
	resp := cdcPrecheckResponse{CheckedAt: time.Now()}

	// ① source reachability + version (floor 5.7)
	if v, err := p.PingMySQL(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"source_conn", "源端连通性 / MySQL 版本", "fail",
			fmt.Sprintf("连接失败：%v", err)})
	} else if mysqlVersionAt(v, 5, 7) {
		note := ""
		if !strings.HasPrefix(v, "8.") {
			note = "（5.7 达 v1 地板，推荐 8.0+）"
		}
		resp.Items = append(resp.Items, cdcPrecheckItem{"source_conn", "源端连通性 / MySQL 版本", "ok",
			"MySQL " + v + note})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"source_conn", "源端连通性 / MySQL 版本", "fail",
			fmt.Sprintf("MySQL %s（需要 ≥5.7）", v)})
	}

	// ② log_bin enabled
	if v, err := p.MySQLVar(cfg, "log_bin"); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"log_bin", "binlog 开启", "fail",
			fmt.Sprintf("查询失败：%v", err)})
	} else if strings.EqualFold(v, "ON") {
		resp.Items = append(resp.Items, cdcPrecheckItem{"log_bin", "binlog 开启", "ok", "log_bin=ON"})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"log_bin", "binlog 开启", "fail",
			"log_bin=" + v + "（需 ON）"})
	}

	// ③ binlog_format=ROW + ④ binlog_row_image=FULL (v1 hard requirements)
	if v, err := p.MySQLVar(cfg, "binlog_format"); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"binlog_format", "binlog_format", "fail",
			fmt.Sprintf("查询失败：%v", err)})
	} else if strings.EqualFold(v, "ROW") {
		resp.Items = append(resp.Items, cdcPrecheckItem{"binlog_format", "binlog_format", "ok", "ROW"})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"binlog_format", "binlog_format", "fail",
			"当前 " + v + "（需 ROW，SET GLOBAL binlog_format=ROW 后重启会话）"})
	}
	if v, err := p.MySQLVar(cfg, "binlog_row_image"); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"binlog_row_image", "binlog_row_image", "fail",
			fmt.Sprintf("查询失败：%v", err)})
	} else if strings.EqualFold(v, "FULL") {
		resp.Items = append(resp.Items, cdcPrecheckItem{"binlog_row_image", "binlog_row_image", "ok", "FULL"})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"binlog_row_image", "binlog_row_image", "fail",
			"当前 " + v + "（需 FULL：UPDATE/DELETE 依赖完整旧像）"})
	}

	// ⑤ replication privileges
	if slave, client, err := p.MySQLReplPrivileges(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"repl_privs", "REPLICATION 权限", "fail",
			fmt.Sprintf("查询失败：%v", err)})
	} else if slave && client {
		resp.Items = append(resp.Items, cdcPrecheckItem{"repl_privs", "REPLICATION 权限", "ok",
			"具备 REPLICATION SLAVE + CLIENT（或 SUPER/ALL）"})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"repl_privs", "REPLICATION 权限", "fail",
			fmt.Sprintf("需 GRANT REPLICATION SLAVE, REPLICATION CLIENT（当前 SLAVE=%v CLIENT=%v）", slave, client)})
	}

	// ⑥ target reachability
	if err := p.PingTarget(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"target_conn", "目标 TiDB 连通性", "fail",
			fmt.Sprintf("连接失败：%v", err)})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"target_conn", "目标 TiDB 连通性", "ok",
			fmt.Sprintf("%s:%d/%s", cfg.Target.Host, cfg.Target.Port, cfg.Target.Database)})
	}

	// base migration (same semantics as PG)
	if s.hasCompletedMigration() {
		resp.Items = append(resp.Items, cdcPrecheckItem{"base_migration", "全量迁移基线", "ok",
			"已有成功的全量迁移任务"})
	} else {
		resp.Items = append(resp.Items, cdcPrecheckItem{"base_migration", "全量迁移基线", "warn",
			"没有已成功的全量迁移任务；incr_only 模式需要先具备目标端表结构"})
	}

	// no-PK warn (ROW 事件 UPDATE/DELETE 需主键定位)
	if tables, err := p.MySQLNoPKTables(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"no_pk_tables", "无主键表预警", "warn",
			fmt.Sprintf("无法查询（不影响启动）：%v", err)})
	} else if len(tables) == 0 {
		resp.Items = append(resp.Items, cdcPrecheckItem{"no_pk_tables", "无主键表预警", "ok",
			"范围内所有表均有主键"})
	} else {
		resp.NoPKTables = tables
		resp.Items = append(resp.Items, cdcPrecheckItem{"no_pk_tables", "无主键表预警", "warn",
			fmt.Sprintf("%d 张表无主键，UPDATE/DELETE 将无法定位行：%s", len(tables), joinLimit(tables, 10))})
	}

	// binlog resume point: checkpoint file:pos vs current master position.
	cpInfo := loadCheckpointInfo(cfg)
	// A MySQL checkpoint carries a binlog coordinate (not a WAL LSN) —
	// render it through the same LSN display field.
	if cp, err := cdc.NewCheckpointManager(cpBinlogCheckpointPath(cfg)).Load(); err == nil && cp != nil && cp.Binlog != nil {
		cpInfo.LSN = cp.Binlog.String()
	}
	resp.Checkpoint = cpInfo
	if file, pos, expire, err := p.MySQLMasterStatus(cfg); err != nil {
		resp.Items = append(resp.Items, cdcPrecheckItem{"master_status", "binlog 当前位点", "warn",
			fmt.Sprintf("无法查询（不影响启动）：%v", err)})
	} else {
		retention := ""
		if expire > 0 && expire < 2*86400 {
			retention = fmt.Sprintf("；⚠️ binlog 保留仅 %d 秒（建议 ≥2 天，防位点落后被清）", expire)
		}
		resp.Items = append(resp.Items, cdcPrecheckItem{"master_status", "binlog 当前位点", "ok",
			fmt.Sprintf("master %s:%d%s", file, pos, retention)})
	}

	if cpInfo.Exists && cpInfo.LSN != "" {
		resp.Conclusion = fmt.Sprintf("将从 checkpoint 位点 %s 续传（file:pos，v1）；注意：MySQL CDC v1 不支持在线 DDL——DDL 会使任务硬停并提示重跑全量。", cpInfo.LSN)
	} else {
		resp.Conclusion = "无 checkpoint，将从当前 master 位点新建链（v1 file:pos；GTID 为 v2 候选）；MySQL CDC v1 不支持在线 DDL——DDL 会使任务硬停并提示重跑全量。"
	}

	resp.WarnOnly = true
	for _, it := range resp.Items {
		if it.Level == "fail" {
			resp.WarnOnly = false
			break
		}
	}
	s.writeJSON(w, http.StatusOK, resp)
}
