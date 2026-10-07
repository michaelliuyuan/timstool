# MS-11 · MySQL CDC（binlog）口径册

> 基线 af906ab（V3.18.14）；笔① 823ea91（双源位点层）/笔①a 8fbcc6a（门禁确定性）/笔② 0668c8e（canal 采集层）/
> 笔③ 04e0887（runner 接电+precheck MySQL 路+互斥守卫）/笔④ 7f3a297（能力位翻面+chain 分派，+4a/4b）/
> 笔⑤ 本册。
> 审定 seq951/953：单批 MS-11 全量递交（不拆版）；v1=裁量 A（仅 DML+file:pos）；DDL→error 态硬语义。

## 双源 CDC 总架构

```
timstool cdc（cmd/cdc.go 单入口）
  ├─ SourceType()=="postgres"（含空 type legacy 默认）
  │    └─ PG 路：pglogrepl 流（slot+publication）——MS-11 零触碰（PG 零回归结构钉）
  └─ SourceType()=="mysql"
       └─ runBinlogCDC（cmd/binlog_cdc.go）
            └─ BinlogRunner（internal/cdc/binlog_runner.go，镜像 PG Runner 主循环）
                 canal 适配（binlog_canal.go，三硬条件封死）
                 → applier/transformer/冲突四态 ——与 PG 路**原样复用**（A2 复用面）
                 → checkpoint：UpdateBinlog one-behind（at-least-once 同 PG）
                 → status file Schema:2（binlog 族；PG=1 不混）
```

## 口径字典（PG vs MySQL CDC）

| 维度 | PostgreSQL | MySQL（MS-11 v1） |
|---|---|---|
| 定位机制 | replication slot + publication（logical, pgoutput） | binlog file:pos（**无 slot/pub 概念**——UI 段按 slot_name 存在性条件渲染） |
| 保留机制 | slot 保留 WAL（max_slot_wal_keep_size 护栏） | binlog 天然保留（expire_logs_days/binlog 空间护栏，precheck <2d warn） |
| 断点字段 | Checkpoint.LSN（legacy 字段） | Checkpoint.Binlog{File,Pos}（omitempty——PG marshal 零 binlog 键） |
| 位点渲染 | LSN 文本（`0/3D0000A0`） | `file:pos` 正典形（`mysql-bin.000003:157`，LastIndex 切分） |
| status file | Schema:1 | Schema:2（binlog 族隔） |
| DDL | DDL poller（pg2tidb_ddl_log，LastDDLID 断点） | **v1 硬 error 态**：OnDDL→ErrMySQLDDLUnsupported（话术：停链→双侧 DDL→重跑全量→重建链；禁静默跳过） |
| GTID | —（LSN 即序） | **v2 候选，刻意不载**（每 struct 一种定位语义） |
| server_id | — | cdc.server_id 必填唯一非零（复制拓扑内） |
| sync_ddl | cdc.sync_ddl 可开 | **前置拒**（config v1 规则；与 error 态一致） |
| 采集库 | pglogrepl（原生） | go-mysql/canal v1.11（master info 纯内存零持久化+OnPosSynced no-op=自持 checkpoint 真源） |
| precheck | 六项（wal_level/slot/…) | 六项镜像（source_conn 地板 **≥5.7**/8.0 推荐、log_bin、binlog_format=ROW、binlog_row_image=FULL、repl_privs、target+base+无主键 warn+master 保留期 warn+file:pos 续传结论含 DDL 通告） |
| Tables/ExcludeTables | 表名清单 | 同字段，**正则模式形**（canal IncludeTableRegex 语义——清单值需字面转义；明示非 glob） |
| 全量+增量衔接 | 预建 publication+slot，记 consistent point LSN | 零 provisioning，记 master file:pos（同 MySQLMasterStatus prober）；seed 不倒退镜像 PG |

## 互斥守卫（裁③：双增量互斥）

- 双向 409：`handleCDCStart`（增量 CDC 起时拦水位轮询）+ `handleRunIncrementalJob`（水位轮询活时拦 CDC 起）。
- 同源 key=`host:port:user:database`（**不含 password**——同端点同用户异密码按同源论，记档可接受，seq961 leader 观察②）。
- PG 永不拦（水位增量 PG 无 binlog CDC 冲突面）；CDC 未配置 fail-open（双锚钉）。

## at-least-once 语义与已知观察（seq961 leader 观察①）

- checkpoint 走 streamer one-behind：重启从 last-good 重放 ≤10s 窗（幂等由 conflict_strategy 兜底）。
- BinlogRunner ctx.Done 路径跳过末次 save/writeStatus——at-least-once 仍安全（resume 从 last-good 起）；**te 黑盒 kill-重启续传实测即证**（点单池在案）。

## FE 面（笔④）

- WizardView：cdc_chain 门=cdcChainCapable computed（postgres+mysql）；文案双保留模型（WAL 累积 vs binlog 保留期）。
- CDCView：数据源导入 postgres+mysql；REPLICA IDENTITY no-PK 助手对 MySQL 隐藏（PG-only 机制——MySQL 无主键 warn 自带指引：加 PK 方可行级 UPDATE/DELETE）。

## A3 记账（MS-11 累计）

- 笔③：config.go 2→3（server_id/sync_ddl v1 规则，sanctioned dispatch）；新目 cmd/cdc.go×1、cdc_precheck.go×1、cdc_mutex_guard.go×1。
- 笔④：cdc_config.go **2→1**（字面分派→CapCDC 能力读，only-decrease 正典形）；chain/seed 分派零新正则命中面。

## latent 总账

1. GTID 断点（v2 候选）：binlog file:pos 在源端 reset master/切主后失效——重建链（同 DDL 话术路径）。
2. DDL 同步（v2 候选）：error 态硬语义为 v1 边界，非缺陷。
3. 互斥 key 无 password（观察②记档）：边缘同源异密码按同源论。
4. ctx.Done 跳末次 save（观察①）：at-least-once 安全，黑盒 kill-重启实测在案。
5. SHOW MASTER STATUS 空结果=显式报错（cdc_precheck_mysql.go，log_bin 前置天然消解——空结果多因 log_bin off）。
