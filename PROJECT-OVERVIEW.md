# PROJECT-OVERVIEW — TiMS (timstool) 项目总览

> **定位**：本文档面向**接手开发者**，与 [README.md](README.md)（面向使用者）互补：README 讲"怎么用"，本文讲"代码是怎么组织的、为什么这样设计、哪些坑已经踩过"。
>
> **基准**：origin/main @ `fd4ba57`（2026-11-10）。行号引用会随代码演进漂移，优先引用 文件路径 + 函数/类型名；提交号均经过 origin/main 可达性核验。
>
> **维护约定**：新增能力、重大修复、包结构调整时同步更新本文；与 README 的分工是——使用者视角进 README，开发者视角进本文。禁止引用不可达的 commit hash（历史教训见 §7.4）。

---

## 1. 项目定位与能力全景

**TiMS**（repo 名 `timstool`，Go module `github.com/michaelliuyuan/timstool`）是多源异构数据库 → TiDB 的迁移/同步工具。首个生产可用源为 PostgreSQL，架构上向多源扩展（MySQL 开发中，Oracle/SQL Server/DB2 为已注册 Stub）；另有 MongoDB↔MongoDB 独立比对、TiDB 作为 DDL 导出源（MS-12）两条支线。

命名提示：构建产物至今叫 `pg2tidb`（`build/pg2tidb`，`build-web.sh`），仓库根还躺着一个历史交付的 `pg2tidb-linux` 二进制——项目从 "PG→TiDB 单向迁移工具"（曾用名 pg2tidb）演进为多源 TiMS。**历史残留是二进制名 pg2tidb**；环境变量前缀早已是 `TIMSTOOL_`（如强制开启 CDC 用 `TIMSTOOL_CDC_FORCE=1`，`cmd/cdc.go:42`；`cmd/cdc.go:40` 注释曾滞后写 PG2TIDB_ 前缀——本文初稿即被其误导，已随手修正）。

六大核心能力 + 两条增量支线：

| 能力 | 入口 | 状态 |
|------|------|------|
| 兼容性评估 | `assess` CLI / Web 兼容评估页 | ✅ PG；MySQL 方言已接入（MS-10b） |
| 源端 DDL 导出 | `export-ddl` CLI / Web DDL 导出页 | ✅ postgres / mysql / tidb 三源 |
| Schema 迁移 | `schema` CLI / 向导 | ✅ PG→TiDB（类型映射 + DDL 转换） |
| 全量数据迁移 | `data` / `all` CLI / 向导 | ✅ PG（COPY+Lightning）；MySQL（Dumpling，开发中） |
| 数据校验 | `validate` CLI（迁移流水线第 4 阶段） | ✅ 三级（quick / sample / checksum） |
| 独立数据比对 | Web 数据比对页 / `compare` API | ✅ 任意源↔TiDB，含 A1 队列 / A2 多库批量 |
| CDC 增量同步（可选模块，默认关闭） | `cdc` CLI / Web CDC 仪表盘 | ✅ PG 逻辑复制；MySQL binlog（MS-11）已接入 |
| 时间戳水位补齐 | Web 数据同步组 | ✅ PG→TiDB 拉式批同步 |

数据源支持矩阵（§7.1 的待办池会回顾其中缺口）：

| 数据源 | 迁移 | 比对 | CDC | DDL 导出 | 兼容评估 |
|--------|------|------|-----|----------|----------|
| PostgreSQL | ✅ | ✅ | ✅ pgoutput | ✅ | ✅ |
| MySQL | 🔧 开发中（Dumpling） | ✅（MS-08） | ✅ binlog（MS-11） | ✅（MS-10c） | ✅（MS-10b） |
| TiDB（作为源） | — | ✅（比对目标端） | — | ✅（MS-12） | — |
| MongoDB | — | ✅（独立任务，mongocompare） | — | — | — |
| Oracle / SQL Server / DB2 | 📋 Stub（`internal/source/stubs.go`） | — | — | — | — |

使用方式：CLI（cobra，9 个子命令）与 Web 管理界面（`timstool web`，单二进制 go:embed 前端）双形态。

## 2. 技术架构总览

### 2.1 分层图

```
            CLI (cmd/*, cobra)                    Web 前端 (web/frontend, Vue3)
                 │                                       │ HTTP/WS (axios)
                 │                                       ▼
                 │                        internal/webapi ── chi 路由 /api/v1/*（60+ 端点 + WebSocket）
                 │                                       │
                 ▼                                       ▼
        internal/orchestrator（四阶段流水线编排 + 阶段生命周期状态机）
                 │
     ┌───────────┼────────────┬──────────────┬──────────────┐
     ▼           ▼            ▼              ▼              ▼
 internal/source   internal/schema  internal/data  internal/validator  internal/cdc
 (多源抽象+CIR)    (schema 迁移)    (导出/导入)     (校验/比对引擎)     (增量同步，可选)
     │                                  │              │
     │ pg COPY / MySQL Dumpling ──► internal/lightning（TiDB Lightning 集成）
     │                                  ▼              │
     └─────────────────────► internal/target（TiDB 侧 DDL/CSV/装载/校验执行）
                                        │
     internal/store（SQLite 任务库） + internal/common（config/checkpoint/logger/reporter/…）
```

两条关键"源中立"抽象贯穿全库：

- **CIR**（Common Intermediate Representation，`internal/source/cir.go`）：DB 无关的 schema + 行数据模型。SchemaReader/DataReader 把源库内容读成 CIR，target 侧再把 CIR 渲染成 TiDB DDL / Lightning CSV。多源化的本质就是"每个源实现 CIR 接口"（#t81 执行引擎）。
- **Dialect 族**：validator 侧的 `CompareDialect`（MS-03）与 `WatermarkDialect`（MS-04，实现落在 `internal/webapi/wm_dialect.go`）把比对/水位 SQL 按源方言分派；assess 的 `srcKind` 校准（MS-10b2）同理。

### 2.2 internal/ 全部 20 个包的职责

| 包 | 职责 | 关键文件 |
|----|------|----------|
| `source` | 多源抽象层：Source/Connector/MetaBrowser 接口 + CIR + registry + stubs（oracle/mssql/db2 占位） | `source.go`（Source 接口 :78）、`cir.go`、`registry.go`、`stubs.go` |
| `source/postgres` | PG adapter：schema_reader / data_reader（COPY 导出）/ incremental（逻辑复制采集） | `incremental.go` |
| `source/mysql` | MySQL adapter：schema_reader（information_schema）/ data_reader / defaults（默认值字面量 vs 表达式判定） | `defaults.go: quoteDefault` |
| `source/tidb` | TiDB 作为源：仅 DDL 导出面（MS-12 设计边界，SchemaReader/DataReader 显式未实现） | `tidb.go:95,101` |
| `source/mongo` | MongoDB 源：MetaBrowser（库/集合树）已实现，SchemaReader/DataReader 未实现（比对专用） | `source.go:143,148` |
| `orchestrator` | 四阶段流水线（precheck→schema→data→validate）编排、阶段生命周期状态机（P1 phase lifecycle）、PG 路径与多源 CIR 路径双实现 | `orchestrator.go`（886 行）、`pipeline.go`（Phase/Pipeline 定义） |
| `schema` | PG schema 采集 → 类型映射 → DDL AST 转换 → TiDB DDL 执行（支持 --dry-run） | `migrator.go`、`type_map.go`、`converter 逻辑` |
| `data` | 全量数据迁移编排：表发现/行数估算、分片导出、Lightning 导入（含 chunk 文件改名映射）、流式 INSERT 回退 | `migrator.go`（含原 scheduler 职责，见 §7.4 漂移项） |
| `lightning` | TiDB Lightning 集成：二进制内嵌（go:embed）+ 运行时释放 + 版本探测 + PATH 查找优先级 | `embed.go`、`version.go` |
| `dumpling` | MySQL Dumpling 导出封装（MySQL→TiDB 全量） | `dumpling.go` |
| `target` | TiDB 目标端：CIR 行 → Lightning CSV 渲染（对齐 PG 路径 TSV 格式）、DDL 执行、LoadData、目标端校验 | `csv.go`、`ddl.go`、`loaddata.go` |
| `validator` | 校验/比对引擎：quick/sample/checksum 三模式（checksum 即全量）+ no-PK 三策略 + row-hash spool（M1）+ keyset 分页 + 行级差异下钻 + 水位过滤 | `validator.go`、`nopk.go`、`rowhashspool.go`、`count_chunked.go`、`rowdiff*.go`、`dialect_*.go` |
| `cdc` | CDC 增量同步：Runner（source→filter→transform→apply）、PG pgoutput 源、MySQL binlog（canal）源、DDL 复制、checkpoint、Web supervisor 对接 | `runner.go`、`source.go`（PG）、`binlog_canal.go`（MySQL）、`ddl_tracker.go`、`checkpoint.go` |
| `canalpatch` | go-mysql canal 的内部补丁副本（vendored fork，带独立 LICENSE/README）：binlog 同步定制 + TiDB parser AST 引用 | `sync.go`、`master.go` |
| `ddlcompare` | DDL 对比校验：catalog inventory 采集（MySQL walk 兼作 TiDB 目标端）→ 8 类目 diff → 跨方言 hint 层 | `collect.go`、`diff.go`、`normalize.go` |
| `ddlexport` | 源端 DDL 导出：PG catalog walk / MySQL-wire SHOW CREATE walk（MS-10c）/ TiDB 源（MS-12），per-schema 自包含目录 | `exporter.go`、`exporter_mysql.go` |
| `mongocompare` | MongoDB 比对引擎：canonical BSON 序列化 + 分桶哈希 + 失配桶字段级下钻；checkpoint 带 `algoVersion` 语义版本戳 | `engine.go`、`canon/`、`diffjoin.go`、`plan.go`、`watermark.go` |
| `mongoconn` | MongoDB 版本自适应连接层（M5）：一个 Client 接口两个驱动实现（4.4+ 原生 / 3.x v1 桥），按连接逐实例分派 | `conn.go`、`v1.go`、`v2.go` |
| `assess` | 兼容性评估：scanner（PG / MySQL 双方言）+ checker（srcKind 校准措辞/映射）+ terminal/json/html 报告 | `scanner.go`、`scanner_mysql.go`、`checker.go` |
| `precheck` | 迁移前预检：连接/权限/磁盘/排序规则等检查器（磁盘检查分 posix/windows 实现） | `checker.go`、`disk_posix.go`、`disk_windows.go` |
| `webapi` | Web API 服务（chi）：全部 REST + WebSocket、统一数据源注册表、比对/DDL 导出/水位任务、CDC supervisor 控制、audit 环 | `server.go`（路由注册）、`datasource.go`、`compare*.go`、`ddl_export_handler.go`、`incremental.go`、`wm_dialect.go` |
| `store` | 任务持久化：SQLite（modernc.org 纯 Go 驱动，WAL + busy_timeout），`<dataDir>/timstool.db` | `store.go:57`（`tasks` 表） |
| `api` | CLI 监控面板 API（旧版兼容，`web.enable: true` 时 CLI 迁移顺带起面板） | `server.go` |
| `common` | 公共设施：config（yaml 全量配置）、checkpoint（表级/分片级/阶段记录）、logger（zap）、progress、reporter（报告类型）、errors、version、interfaces.go | `checkpoint/checkpoint.go`、`config/config.go` |

（注：`api`/`store` 亦在此表内，合计 20 包 + source 下 4 个源子包 + common 下 **7 个子包**（checkpoint/config/errors/logger/progress/reporter/version；`interfaces.go` 为文件非子包）。）

## 3. 后端实现细节

### 3.1 迁移链路

**CLI 命令树**（cobra，`cmd/`）：`all`（一键四阶段）、`precheck`、`schema`（--dry-run）、`data`、`validate`（L1/L2/L3 + --sample-ratio）、`assess`（--format terminal|json）、`export-ddl`、`web`（--port/--host/--data）、`cdc`（--enable-cdc / `TIMSTOOL_CDC_FORCE=1`）；全局 flag `--source`（postgres|mysql|oracle|mssql|db2）、`--config`、`--log-level/--log-format`。

**四阶段流水线**（`orchestrator/pipeline.go` 定义 `Phase` 枚举与 `PipelineConfig.Skip*`）：

1. **precheck**——源/目标连通、权限、磁盘、collation（PG 路径走 `internal/precheck`；多源路径在 orchestrator 内联 v1 探测，MS-10d 补齐 skip 语义）。
2. **schema**——`internal/schema` 采集 PG catalog → CIR → 类型映射（README §类型映射参考表）→ DDL 转换 → TiDB 执行；GIN/GiST 等不兼容索引仅告警不迁移。
3. **data**——两条路径：
   - **PG 路径**：`COPY TO STDOUT` 并行导出 CSV 到 `temp_dir` → 生成 `lightning.toml` → `tidb-lightning` local backend 物理导入（需 PD 可达、目标表空）→ 失败自动回退流式 INSERT。进度按阶段加权（导出 5% / 导入 47.5%×2），导入进度按 checkpoint 已完成表数展示；进度分母通过 `preRegisterTables`（`data/migrator.go:435`）开始即恒定。
   - **多源 CIR 路径**（#t81）：`DataReader` 流式产出 CIR 行 → `target/csv.go` 渲染为与 PG 路径字节对齐的 TSV CSV → 同一套 Lightning 管线。skip 四开关（SkipPrecheck/SkipSchema/...）与 PG 路径语义对齐（MS-10d）。**落地状态注意**：`cmd/all.go` 注释仍写 "until the engine lands"，但 `target.LoadData(source.Source,...)` 与 `orchestrator_cir_ms11n_*_test.go` 表明部分链路已落地——注释滞后，正确表述是"已落地部分 + #t81 剩余范围"，勿照抄注释。
   - **大表分片**：`exportTableChunked` 按行数把大表拆成 `{table}.{n}.csv` 多分片并行导出；`importViaLightning`（`data/migrator.go:565`）把分片改名为 Lightning 期望的 `{database}.{table}.{padded}.csv`。设计细节见 `docs/large-table-chunking-design.md`（背景：510 万行单 CSV 787MB、导出超时只完成 595K/5.1M 行的历史问题）。
   - **MySQL 源**：Dumpling 导出（`internal/dumpling`），快路径曾出 6 连修（§6 修复族 #t83）。
4. **validate**——见 §3.2。

**冲突策略** `target_policy`：`insert`（默认，冲突报错）/ `truncate` / `drop`（drop 重建）。**单表失败** `on_error`: `abort`（默认）/ `skip`。

**Checkpoint / 断点续传**：文件型，存 `migration.checkpoint_dir`（默认 `.checkpoint`）。`common/checkpoint/checkpoint.go` 定义 `TableCheckpoint`/`ChunkCheckpoint`（分片粒度断点）/`PhaseRecord`（P1 阶段生命周期：pending/running/completed/skipped，UI 首次轮询即有全量相位）。恢复流程：重跑同命令或 Web"重新执行"自动检测 checkpoint；Web 恢复提供 Plan B（强制 truncate 目标 + 清 checkpoint，杀 1062 重插债务，commit `bd8c956`）。

### 3.2 数据校验与比对引擎

**两条路径，命令名易混**：`cmd/data.go` 是全量数据迁移（COPY+Lightning）；校验/比对入口是 `cmd/validate.go` → `internal/validator`。迁移流水线第 4 阶段的抽样校验走 `target.ValidateMigration`，与独立比对（Web compare，任务数据存 `<dataDir>/compare/{id}/`，与迁移任务存储隔离）是**两条独立路径**；Web 独立比对复用 `internal/validator` 同一引擎。

**模式**：`quick`（行数估算）/ `sample`（行数 + 随机采样，默认，比例 0.01）/ `checksum`（行数 + 分块哈希）。**checksum 就是全量**——没有独立于 checksum 的"全量"引擎分支（validator.go:509-511 仅有 quick/sample/checksum 三个 case；README 的 full 描述对应 checksum 全量分块路径；另 `cmd/validate.go:61` 的 `--mode` help 声称支持 `full (all checks)` 属 help 漂移，已记入 §7.4 第 8 条）。且全库共**三套互不复用的比对引擎**：本节 SQL validator、§3.4 mongocompare、§3.4 ddlcompare——不存在统一比对框架，勿合并表述。引擎入口 `Validator.Run/RunWithDSNs`（`validator.go:336,377`）。

**并发与分块基准参数**（后文各处以这些为准）：比对并发默认 4、预算上限 8（`compare.concurrency`，统一全局预算信号量）；chunk 默认 50,000 行、下限 1,000；M1 spool 落盘阈值 65,536、归并 fan-in 32。

**资源口径是文档口径，不是运行时守护**：代码里没有任何 statfs/ulimit 探测；upfront 磁盘 preflight 曾有但**已移除**（`validator.go:65-73` 注释写明原因——毒化共享查询 deadline、高并发饿死连接池），唯一兜底是 spool 写失败 fail-loud（`rowhashspool.go:27`）。README/§8.3 的 fd 与磁盘数字是常量推导 + 实测的**规划口径**，接手者勿误认为存在运行时阈值守护。

**no-PK 表策略**（`compare.no_pk_strategy`）：`auto`（超 `no_pk_table_threshold` 行转 aggregate/bucket）/ `hash_group` / `bucket` / `aggregate`。aggregate 先算整体聚合哈希快判等，不等再降级 hash_group/bucket 取细节。

**M1 row-hash spool**（`validator/rowhashspool.go`，PR #2）——无 PK/降级路径的有界内存改造：哈希计数先进内存 map（与旧实现字节等价），越过 65,536 去重哈希阈值（约 5MB）后排序落盘成 run（每条 16 字节 md5 + uvarint 计数），结束时 k 路级联归并（fan-in 上限 32 保证 fd 有界）。红线：multiset 语义与旧实现 set-equal（oracle 等价测试钉死）；磁盘写失败显式 failed（错误含 "row-hash spool"），绝不静默退化为无界内存。资源口径（README 披露）：内存峰值恒定 ~50MB 与表大小无关；fd 上界随并发线性（规划口径 294@4 路 / 558@8 路）。

**keyset 分页**：checksum 深 OFFSET 分页有性能悬崖（BUG-COMPARE-STUCK #t94），现统一走 keyset cursor（`rowdiff_keyset_test.go`、`count_chunked.go`）；分片 COUNT 用 C2 估算路由 + C3 keyset 分窗（`count_chunked.go`），且**只有真主键**（拒绝 partial/expression unique index，`a3461dc`）才允许切 keyset 窗。

**可观测性**：checksum 模式有 per-chunk 进度回调 + `possibly_stuck` 心跳（`possibly_stuck_timeout`，clamp 语义见 `ee105d2`）；比对行级差异下钻三级：表级差异 → keyset 分页 rowdiff（`rowdiff/{file}` 路由）→ 单键内容左右对照（`rowdiff-content`，NULL 预扫 + 超大表 guardrail）。

**A1 任务队列**（PR #1）：同一时刻仅一个比对任务真正运行；遇忙不再 409，新任务 `202 + status=queued` 进 FIFO。队列状态就是 `task.json` 本身（重启不丢）；重启后遗留 running 自动标失败，带 `source_ref`/`target_ref` 的排队任务恢复调度，内联凭据者显式失败（密码永不落盘的直接推论）。排队任务可取消出队/直接删除。

**A2 多库批量**（#102）：`databases` 支持库名 + 通配（`*`/`?`，≤64 条，`exclude_databases` 扣除），运行期枚举解析（PG `pg_database` / MySQL `SHOW DATABASES` 自动剔系统库），解析结果落盘 `resolved_databases`；逐库**串行**外层编排（`compare_multidb.go:613`）复用单库流程，单库路径字节不变、不开新并发面（勿写成并行多库），任一库失败记 `status=error` 继续，收尾产出跨库汇总 `summary.json` + `summary.csv`（每库 PASS/FAIL 一行 + 合计）。

**水位过滤**（比对面）：`wmfilter`（含表级水位覆盖逻辑，无独立 wmoverride 源文件，覆盖实现并入 wmfilter.go）/`ignorefilter` 支持水位范围条件 `[start, end)`、忽略列排除值比较；水位缺失表回退全量比对（`01dd681`）。

### 3.3 CDC 增量同步（可选模块）

**可选化**（`docs/cdc-optional-module-design.md`）：默认 `cdc.enable: false`；开启三通道（配置文件 / `--enable-cdc` / 环境变量 `TIMSTOOL_CDC_FORCE=1`，`cmd/cdc.go:42`）。关闭时 `all` 行为不变、Web 隐藏 CDC 入口、`GET /api/v1/features` 返回 `cdc.enabled=false`。

**双源采集层**：
- **PG**：`cdc/source.go` 基于 pgoutput 逻辑复制（pglogrepl），publication + replication slot，流式解码 Relation/Row。
- **MySQL**：`cdc/binlog_canal.go`（MS-11）基于 go-mysql canal，但用仓库内的**补丁副本** `internal/canalpatch`（vendored fork，可引用 TiDB parser AST）。三条硬语义：位点真相 = 自己的 checkpoint（canal v1.11 master info 仅内存，绝不从 canal 回读）；gset/position 双支持；DDL 遇到默认 error 态（v1 仅 DML + file:pos）。

**Runner 管线**（`cdc/runner.go`）：`source → filter（TableFilter）→ transformer（类型/字符转换）→ applier（批量写 TiDB）`。DDL 轮询线程与 DML 流并行；setup 失败降级 DML-only，DDL 应用失败响亮 halt（`ddlErrCh`）。

**checkpoint-on-failure**（`cdc/checkpoint.go`）：LSN/file:pos 位点文件化，at-least-once 语义（重启重读不丢）；checkpoint 带源类型形状校验（PG hi/lo hex LSN vs MySQL file:pos，`pgLSNShape`/`checkpointMatchesSource`），错源位点拒绝加载。新表 DML 先于 DDL 到达时 applier 短退避重试，超限 halt 不静默丢。

**DDL 复制**（`sync_ddl: true` 默认开）：CREATE/ALTER/DROP TABLE + BTREE 索引复制到 TiDB（类型映射 + 去 PG schema 前缀 + `IF NOT EXISTS` 幂等 + `ddl_log` id checkpoint at-least-once）；视图/函数/触发器跳过 + 告警不阻断。能力缺口实例：`canalpatch/sync.go:91`（binlog 流内 DDL 未保存）与 `cdc/ddl_tracker.go:370`（transform 占位）两处 TODO——MySQL 面的 DDL 复制尚未闭环（§7.2）。

**运维面**：Web supervisor 一键启停（崩溃指数退避重启、SIGTERM→SIGKILL 优雅停、Web 重启领养在跑进程）；`/cdc/status` 返回 running/starting/stopping/failed/adopted + PID + 重启次数；in-process watchdog（`b2ed324`）复活领养死亡、耗尽上限、启动重建；destructive 操作过 audit 环（MS-11f，内存 ring + 异常守卫：非 audit 覆盖的 CDC stop 触发 ERROR 告警）。无主键表 CDC 预检预警 + 一键 `ALTER TABLE ... REPLICA IDENTITY FULL` 修复；1146 短路（表确认缺失后同表事件立即跳过，DDL poller 成功即清除，`389c28c`）。

**全量+增量衔接（cdc_chain）**：向导勾选后任务启动前预建 publication+slot（全量期间 WAL 由 slot 保留），全量成功自动拉起 CDC 并把 slot 点位预置进 checkpoint，重放整个迁移窗口变更（与全量重叠数据按 `conflict_strategy=replace` 幂等去重）。校验降级：仅校验阶段未过的链式任务仍算成功并衔接（全量期源端持续写入的预期差异由 CDC 收敛）。

### 3.4 周边能力

- **assess**（兼容性评估）：PG catalog scanner + MySQL scanner（MS-10b，"翻面≠拔闸"——每个 PG catalog 查询要么给 MySQL 等价要么记降级口径）；checker 按 `srcKind` 校准措辞/映射，PG 行为字节级不变；报告 terminal/json/html 三形态。
- **precheck**：连接/版本/schema 权限/磁盘（分 posix/windows 实现）/collation 检查，输出 PASS/WARN/FAIL。
- **ddlexport**：三源导出（PG catalog walk / MySQL-wire `SHOW CREATE` walk / TiDB 源 MS-12——注意 MS-12c 修订：企业版 TiDB 函数/过程/触发器真实存在，改"真导出 + 台账化降级"，原"TiDB 无存储程序"假设被 103 实机证伪）。每个 schema 一个自包含目录（tables/indexes/views/sequences/functions/procedures/triggers/types 八个 .sql + README.txt + manifest.json 计数与跳过清单）；不支持的语句逐条跳过（服务端 Warn + 响应头 `X-Tims-Skipped` 计数 + UI 提示）；非 tidb 源可选生成类型映射变体 `tidb-tables.sql`。
- **ddlcompare**：DDL 对比校验（独立任务类型）。采集层 MySQL walk 兼作 TiDB 目标端（同协议同谓词）；8 类目 diff（表 PK/charset/collation + views/routines/triggers/sequences/users 存在性）；跨方言固有差异降 hint 级（charset 值/collation 词表差异永不 fail，PK 差异与单侧缺失仍 fail）；历史修复见 BUG-DDLCMP-PSEUDO-PRI（MySQL 8.0 information_schema 伪主键污染，103 实测复现后改 STATISTICS 硬语义）与 mysqlTableMeta 指针别名 >16 表丢 PK（`d9e33e8`）。设计文档 `docs/DDL-COMPARE-DESIGN.md`（v1.4）。
- **mongocompare**：MongoDB↔MongoDB 独立比对（不经 orchestrator）。逐集合存在性/索引集/文档数/分桶哈希，失配桶自动下钻字段级差异（canonical BSON 序列化保证可比）；checkpoint 带 `algoVersion`（算法升级自动失效旧断点）；resume 语义含"数据未变前提 + 边界漂移自动重算"；结果三态 + `ttl_skip`；产物 `summary.json` + 逐集合差异 ndjson。版本自适应连接（`mongoconn`）：4.4+ 原生路径 / 3.x v1 桥（3.6+ 官方、3.2/3.4 best-effort）。已声明限制：大字段摘要未实现、跨实现字节一致 non-goal、v1 单库对单库（`docs/mongodb-compare.md`）。
- **时间戳水位补齐**（incremental）：基于更新时间列的拉式批同步（PG→TiDB）。表级水位列自动探测（timestamp/date/int/bigint 可比较类型 + 索引提示）、初始水位留空 = `MIN(col)` 全量补齐、默认 `≥` 模式重读边界时刻行保同秒迟到行不丢（严格 `>` 模式跳过边界重读但可能丢迟到行，UI 有提示）；表级状态/累计行数/run 级历史全记录，手动触发可重跑。源端 DELETE 不可捕获（比对兜底）——与 CDC 的取舍见 README FAQ 对比卡片。

### 3.5 Web API 层

- chi 路由（`webapi/server.go:273` 起 `/api/v1`），60+ REST 端点 + 1 WebSocket（`/api/v1/ws`，进度+日志推送）。分组：health/features、sources（类型化源注册 + config-schema + test/tables/meta）、test-connection、datasources CRUD（密码只写不读）、validate-lightning / validate-dumpling、migration-options、tasks（CRUD + start/pause/resume/cancel + progress/report/logs/phases）、mongocompare、ddl-compare、compare（A1/A2 语义见 §3.2）、cdc（status/stats/checkpoint/start/stop/config/slot/reset）、incremental（水位补齐任务 CRUD + run + 列探测 + 候选建议）、assess、ddl-export。完整端点表以 README「API 端点」节为单一真源。
- 中间件：Logger / Recoverer / CORS（`go-chi/cors`）；长耗时组用 60s Timeout（根级不挂全局 Timeout 以免截断长操作）。
- **鉴权现状**：无认证中间件（P3 已排期项）；destructive 路由（cdc/start|stop、mongocompare 创建等）挂 audit 中间件（`auditCompareOp`/`auditCDCOp`）；token gate 曾存在后有意移除（`a29af1e`，"internal use, destructive ops stay audited"）——与 §7 的鉴权缺口双向呼应，接手者不要误当回归。
- **审计环**（MS-11f）：内存 ring 记录 destructive 操作 + 异常守卫（未覆盖的 CDC stop 触发 ERROR 告警）。
- **持久化分域**：迁移任务 → SQLite `<dataDir>/timstool.db`（modernc 纯 Go 驱动 + WAL + busy_timeout 5s，首触 ping 有界防 wedge）；比对/DDL 对比任务 → 文件型 `<dataDir>/compare/{id}/`（task.json 即队列状态）；CDC → checkpoint 文件 + status 文件。
- webapi 是全库最大包（含测试 3.3 万行）；`wm_dialect.go`（619 行）承载水位/建议面的源方言分派（MS-04/05 产物）。

## 4. 前端实现细节

**技术栈**（`web/frontend/package.json`，版本 `3.23.6`，FE 版本号随提交递增并打进报告页 badge）：Vue `3.5.34` + TypeScript `~6.0.2` + Vite `8.0.12` + vue-router `4.6.4` + Element Plus `2.14.1` + ECharts `6.1.0`（vue-echarts）+ axios；测试 vitest `4.1.9` + @vue/test-utils + jsdom。

**目录约定**：`src/views`（页面 + co-located spec）、`src/components`（通用组件）、`src/composables`（`useDataSources`/`useSourceSchema`/`dataSourceTypes`/`sourceTypes`/`reconcileModel`）、`src/api/index.ts`（axios 封装，长操作超时 `LONG_TIMEOUT = 120000` 即 120s，`api/index.ts:15`）、`src/router/index.ts`。

**路由表**（14 条业务路由 + 404 catch-all；下表 13 行系 `/tasks` 与 `/tasks/:id` 合并一行）：

| 路径 | 视图 | 功能 |
|------|------|------|
| `/` | DashboardView | 总览仪表盘（五 gauge + 任务表统一模板 + 近期比对历史深链） |
| `/pipeline` | PipelineView | 迁移拓扑可视化（SVG 双侧画布 + 贝塞尔边按 DDL 结论着色；**只读红线**：仅 3 个 GET，真实边只来自 ddl-compare 的 source/target label，不虚构） |
| `/wizard` | WizardView | 5 步迁移向导（数据源引用选择 → 源端 → 目标端 → 选表 → 选项 → 确认） |
| `/mongocompare/new` | CompareCreateView | MongoDB 对比创建（集合选择 + 水位建议 + token 三态对话框） |
| `/tasks` `/tasks/:id` | TaskListView / TaskDetailView | 任务列表 / 详情（表级进度、比对报告入口、mongo 集合结果卡） |
| `/history` | HistoryView | 迁移历史 + 重新执行 |
| `/datasources` | DataSourcesView | 统一数据源管理（postgres/mysql/tidb，密码只写不读，Lightning 环境校验） |
| `/compare` | CompareView | 独立数据比对（A1 队列状态、chunk 进度、possibly_stuck、行级差异下钻、水位列） |
| `/assess` | AssessView | 兼容评估（数据源引用 + 测试连接） |
| `/ddl-export` | DDLExportView | DDL 导出（schema/对象类型勾选 + TiDB 变体 + 跳过提示） |
| `/ddl-compare` | DdlCompareView | DDL 对比校验（结果表 + 提示 badge） |
| `/incremental` | IncrementalView | 时间戳水位补齐（水位列探测 + 表级状态 + 运行历史 + "选哪个"对比卡） |
| `/cdc` | CDCView | CDC 仪表盘（一键启停 + LSN/吞吐/延迟 + 预检 + REPLICA IDENTITY 修复） |

**实现约定**：Vue 3 `<script setup>`；表单标准两列布局 + 数据源选择器优先（S1-UI 系列）；导航分组可折叠（localStorage `tims-nav-collapse`，损坏 JSON 回退全展开）；语义色系统（主操作 teal、红仅 danger）；WCAG AA 对比度；`prefers-reduced-motion` 适配拓扑动画。

**测试**：48 个 `.spec.ts` 与被测文件同目录共存（命名如 `compareMultiDb.spec.ts`、`wizardSourceAgnostic.spec.ts`），`npm test` = `vitest run`；构建 = `vue-tsc -b && vite build`。

**进后端**：`vite build` 产物由 `build-web.sh` 拷入 `cmd/static/`，Go 侧 `go:embed` 打进单二进制（资产卫生三步：prune→copy→exactly-2 校验，commit 信息习惯性附 assets sha256）。

## 5. 代码风格与工程约定

**提交与编号体系**（conventional commits）：`feat(scope):` / `fix(scope):` / `test:` / `docs:`；scope 用包/面名（compare/webapi/cdc/ddlexport/ui/web/wizard/incremental…）。行内引用的任务编号族谱：

| 前缀 | 含义 | 例 |
|------|------|----|
| `#tNN` / `#NN` | Loop 平台任务号（历史账）。注意：`#102` 这类无 t 前缀编号**是内部任务号，非 GitHub PR/issue**（仓库仅存在 PR#1/#2，git ls-remote refs/pull 可证） | #t94/#t95（比对可靠性）、#102（比对专项）、#t81（CIR 执行引擎）、#t109/#t110 |
| `BUG-*` | 带编号的缺陷修复 | BUG-CMP-COUNT-P0/P1、BUG-COMPARE-STUCK、BUG-DDLCMP-PSEUDO-PRI、BUG-WM-SUGGEST-CAP |
| `FEAT-*` | 特性批次 | FEAT-DASH-UNIFY、FEAT-CMP-TBL-DURATION |
| `MS-NN` | 多源化批次（多源 epic 的账册单元） | MS-03~MS-12，映射账册见 docs/MS*-DIALECT-MAP.md |
| `MC-NN` | MongoDB compare 批次 | MC-9~MC-19 |
| `UX-NN` / `S1-UI-NN` / `F-NN` | 前端打磨批 | UX 1-16、S1-UI-04~08、F-06/F-09/F-10 |
| `PR #N` | GitHub PR | #1（A1）、#2（M1） |

**Go 侧**：go 1.25；zap 结构化日志（阶段日志用中文，如 `Phase: Schema 迁移`）；注释文化极重——设计裁定（leader/用户 seq 号）、红线语义（"red line"/"绝不静默"）、资源口径公式直接写进注释（如 `rowhashspool.go` 头 25 行）；错误处理 fail-loud（写盘失败显式 failed、halt 不静默丢事件）；测试带 oracle 等价测试（新旧实现 set-equal 证明）与红队 seam 测试。

**前端侧**：co-located spec；viewBehavior 类行为测试与纯布局测试分开；破坏性操作必须 `ElMessageBox.confirm`；`native alert/confirm/prompt` 已清退换 element-plus。

**流程约定**（接手必读）：

1. **PG-GATE 硬门禁**（`docs/PG-GATE.md` + `scripts/pg-gate.ps1`）：从 MS-03 起所有多源化工单动代码前后各跑一遍 PG 回归门禁，红灯即停，"不许带红灯进评审/报批"。
2. **账册式重构**：多源化搬迁用"行号映射表"账册（docs/MS*.md）——纯搬迁零行为变更，红队按表机械核对；看似等价的微调必须独立 commit，禁混入搬迁笔。
3. **红队评审文化**：commit 注释常引 adversarial 票（seq 号）与 leader 裁定；发现 seam 缺口即补测试（#102 系列先补红队测试再修）。
4. **数据诚实红线**：前端不得虚构数据（PipelineView 只画有 label 的真实边；report 数字必须可溯源到引擎产物）。
5. **密码不落盘红线**：任何持久化（任务/队列/选项/报告）不保存数据源密码；重启恢复时内联凭据任务显式失败提示重提。
6. **103 环境实证**：关键修复要求 103 测试环境复现验证（BUG-DDLCMP-PSEUDO-PRI、A1/M1 部署验证、MS-12c 实机证伪皆是实例）。

## 6. 已修复问题清单（git 历史整理）

> **口径声明**：基准 origin/main @ `fd4ba57`，全量 **511 commits**（unshallow 后核验，评审A/B 双方独立一致）。fix 前缀计数随 grep 口径变化：宽松 `^fix` = 128、严格 `fix(scope)` = 119、`fix:` = 3——本文引用总数时用"宽松 128（其中 fix(scope) 119）"。**以上均为 subject 行口径**（按 `git log --format=%s` 统计）；用 `git log --grep="^fix"` 复算会得 132（body 行也参与匹配），勿用该方式复核。带编号 `BUG-*` 族共 **8 个**。本节为按缺陷严重度与任务线的**代表性归族节选**，非穷举；完整性对照以两位评审的独立基线为准（评审A `notes/` 深读报告、评审B `workspace/t8713-review-baseline.md`）。**旧半段（多源化批次之前的早期历史）修复密度不低**，浅克隆审查极易漏掉，故单列出——这批恰是接手开发者最容易踩的同类坑。所有提交号经本文作者本地 unshallow clone 逐一验证可达。

**带编号的重大缺陷修复（8 族，按时间分组）**：

近期（多源化/比对专项批次）：

| 编号 | 缺陷与修复 | commit |
|------|-----------|--------|
| BUG-CMP-COUNT-P0 | 30m count 超时默认 + backoff retry + 诚实 fail-row 计数 | `2e1d54c` |
| BUG-CMP-COUNT-P1 | 大表 chunked keyset COUNT（估算路由 + 分窗） | `850515d` |
| BUG-COMPARE-STUCK（#t94） | checksum 深 OFFSET 分页性能悬崖 → keyset cursor + query 级超时 | `d5f0be9` |
| BUG-DDLCMP-PSEUDO-PRI | MySQL 8.0 伪主键污染 information_schema 比对（103 实测复现）→ STATISTICS 硬语义 | `bbf8e98` |
| BUG-WM-SUGGEST-CAP | suggest-watermark 表上限 200→5000 | `0bf782a` |

旧半段（浅克隆易漏，接手高频同类坑）：

| 编号 | 缺陷与修复 | commit |
|------|-----------|--------|
| BUG-0930 | TEXT/BLOB/JSON 列 literal DEFAULT 剥离（TiDB 1101 报错）四连修：c1 剥离+warn+报告行、c2 向导显式排除开关、c3 CURRENT_TIMESTAMP(6) keep-anchor、c4 blocksColumnDefault family check 终稿 | `c39d364` 起 |
| BUG-0932 | PG unbounded numeric → DECIMAL(65,30) 保真默认 + 列级 warn；c2 抽共享 `isUnboundedNumeric` 谓词防 warn/mapping 漂移 | `947ddfd`/`1c51b13` |
| BUG-0934 | PG<11 prokind legacy 查询路径（assess scanner + ddlexport functions/procedures）+ 版本探测缓存；c3 去重复 ORDER BY、c4 探测失败 sentinel `MaxInt64`（leader 裁定，弃临时 999999） | `d45d3fe`/`d4f269c` |

**修复族（按任务线，含旧半段）**：

| 任务线 | 代表修复 |
|--------|----------|
| #102 比对专项（2026-11-10） | A1 FIFO 队列（409→202、单槽、重启恢复、fail-loud）`cd68710`；M1 无 PK row-hash spool `1867ee2`；A2 多库批量 + 红队修补 `980ecfa`/`a4a9c86` |
| #t95 比对可靠性 | 真 PK 判定拒绝 partial/expression unique `a3461dc`；query-timeout 披露+clamp `873923c`；CIR validate 互斥 `de4d2b1`；validate 并行化 `a1942c0` |
| #t83 dumpling 快路径（6 连修） | TAB 分隔真字节、consistency=lock、qualified 表名、stale lightning checkpoint 清理、真实行数回填 |
| #t81 CIR 链路 | DEFAULT 字面量引号（TiDB 1064）、target data policy 生效、CIR→CSV 对齐 Lightning TSV |
| MS-11 CDC 系列（最大族，40+ commits） | DDL 红线门硬化、respawn FAILED 终态、DML txn-control 修复 `e99913c`、chain 外源守卫 `49e825b`、report honesty |
| **P1 CDC hotfix（旧半段）** | 工具记账 DDL（`pg2tidb_*`）被复制到 TiDB 致 1101 停机循环等 3 修——DDL 复制白名单的直接由来 | `ef543a2` |
| **P-INC-TZ 时区漂移（旧半段）** | 增量写入 session 未钉 UTC，checksum 回读偏 -8h；`incTargetDSN` 强制 `time_zone=UTC` | `ff589c6` |
| **#t4 连接池死锁（旧半段）** | checksum ≥4 表并发死锁 → 单层全局并发预算信号量（count/chunk 各占 1 单元，不持连接等待） | `09aa421` |
| **checkpoint 双实例踩踏（旧半段）** | runData 双实例并发写 checkpoint → `FinishPhaseWithReload` 先重载再终存 | `8007503` |
| **nil Warnings 崩溃（旧半段）** | 零告警候选 auto-apply 把 nil 序列化为 JSON null 致崩溃 | `a776468` |
| DDL 对比 | 同值 charset/collation 判 Equal、指针别名 >16 表丢 PK `d9e33e8`、序列 inventory `9d73fd4`、跨方言 hint 层 `9b981dd` |
| mongocompare | MC-9/MC-14/MC-15 hardening、fail-loud summary、下载链接 absolutize |
| Web/UX/MC/F-10/#t1~#t3 批 | MC-13~19、UX 1-16、S1-UI 系列、nav collapse 自愈、pipeline 拓扑页；F-10 系列 3 修；#t1 水位 UX 与修复 `0d94d17`/`1058700`、#t2 AA 对比度 `f8f2dcb`、#t3 水位比对过滤组 `009b81e` |
| 流程级教训 | BUG-4/5/6/7（历史缺陷清单编号，非 commit 引用，git log 检索不可见）全部滑过 mock 单测只在真 PG 复现 → 建立 `cdc-integration` 真库 CI 门禁（`.github/workflows/cdc-integration.yml` 注释原文记载此动机） |

## 7. 未解决问题与路线图

### 7.1 README 待办池（逐项验证状态）

| 优先级 | 事项 | 验证状态 |
|--------|------|----------|
| P1 | #t110 fd/落盘口径文档（本地分支 `docs/t110-spool-disclosure` 已评审签发，**待推送 main**） | ✅ 属实：远程无 docs/* 分支，`ed9e17e` 不在 main |
| P1 | 103 环境验证期自建夹具表清理（`m1_smoke_nopk` 等 5 张，PG/TiDB 两侧） | 非 code 项，待 runtime 执行回执 |
| P2 | #t109 Version 回显：`-X main.Version` 注入实为空操作（无 Version 符号），二进制无法自报版本 | ✅ 属实：main.go/cmd/common 无 Version 符号（grep 核验） |
| P3 | Epic 候选池：① Web :8080 鉴权（当前无认证，`GET /api/v1/datasources` 无门枚举）② CDC 能力补齐（功能面最大缺口）③ CIR/PG 语义对齐批 ④ 比对重试韧性批 ⑤ PG zip 对称批 ⑥ 尾批小项（徽标单一真源、result Error 透出） | 方向性条目；①已核验属实（仅 CORS 头提及 Authorization，无 auth 中间件） |

### 7.2 代码硬标记（未解决能力的具体实例）

| 位置 | 内容 | 归属 |
|------|------|------|
| `internal/source/stubs.go` | oracle/mssql/db2 Stub（`SourceMeta.Implemented=false`） | 多源路线图 |
| `internal/source/mongo/source.go:143,148` | MongoDB SchemaReader/DataReader 未实现（仅比对） | mongo 能力边界 |
| `internal/canalpatch/sync.go:91` | TODO：binlog 流内遇 DDL 须保存 | CDC 能力补齐 epic |
| `internal/cdc/ddl_tracker.go:370` | TODO：transform objectType 占位 | CDC 能力补齐 epic |
| `internal/cdc/cdc_integration_test.go:19` | TODO(tier)：扩展完整 INSERT/UPDATE/DELETE 覆盖 | CDC 测试补齐 |
| `web/frontend/src/views/CompareView.vue:1438` | 比对源仅支持 postgres 与 mysql | 比对面源扩展 |
| `internal/validator/checksum.go:117-129` | **比对引擎无自动修复（auto-repair）**：只有三种降级重跑——rowdiff 下钻定位 / 无 PK 降 hash_group / 水位预检失败降全量；差异修复永远人工 | 比对重试韧性批（P3-④） |
| `web/frontend/src/views/TaskListView.vue` `HistoryView.vue` `DDLExportView.vue` | 三个视图**无同名 spec 且被全部 42 个功能域 spec 零 import**（逐视图 grep 验证）；另 `web/frontend/e2e/package.json:7` 的 `mc13:token-dialog` 脚本引用不存在的 `mc13-token-dialog.cjs`（`npm run mc13:token-dialog` 直接失败） | 前端测试补齐 |

### 7.3 设计边界（不要误记为"未解决缺陷"）

| 项 | 性质 | 说明 |
|----|------|------|
| TiDB 作为源仅 DDL 导出 | **设计边界**（MS-12） | `internal/source/tidb.go:95,101` 显式未实现迁移面 |
| CDC token gate 移除 | **有意决策**（`a29af1e`） | "internal use, destructive ops stay audited"；但与 P3 鉴权缺口呼应，接手者须双向知悉 |
| MongoDB 大字段摘要未实现 / v1 单库对单库 | **已声明限制** | `docs/mongodb-compare.md` 限制声明节 |
| PG CDC `Source.IncrementalCapture` 与增量引擎独立管线并存 | **架构选择** | 采集能力走 cdc 包独立管线，非 Source 接口强制路径 |

### 7.4 文档漂移与仓库卫生（本文档发布时应知晓的"README vs 代码"差异）

1. README「项目结构」树列有 `internal/data/scheduler.go`——实际不存在，调度/分片逻辑已并入 `data/migrator.go`。
2. README 结构树与多篇 docs 引用 `docs/cdc-architecture.md`、`docs/multi-source-architecture-design.md`——两文件不在 docs/ 目录（被引用但缺失，接手者勿按图索骥）。
3. README FAQ 说任务库为 `.timstool/tasks.db`——代码实际是 `<dataDir>/timstool.db`（`internal/store/store.go:57`）。
4. README 结构树未列 `canalpatch`/`ddlcompare`/`mongocompare`/`mongoconn` 四包（mongo 能力整体"README 未公告"）。
5. README 待办池引用的 CIR/PG 语义对齐分析（commit `0dd249e`）**不在 main 历史亦不在任何远程分支**——疑似仅存于刘源本地。本文档一律采用描述性引用；该分析如需纳档，待刘源推送或补发。
6. 仓库根 `pg2tidb-linux`（约 30MB）为历史交付的 linux 二进制入库残留；现行交付走 GitHub Releases（44.2MB，2026-11-10，SHA256 见 README）。建议后续移入 Release 附件或清理。
7. **安全卫生**：`scripts/pg-tunnel.ps1:22` 与 `scripts/deploy-verify.ps1:16` 的参数默认值内嵌 `root@<联调环境公网IP>`（SSH 用户名 + IP 一起入库，仓库为 public）。建议改为占位符/环境变量注入；与 §8.3 的环境信息脱敏策略一并处理（待仓库所有者拍板 IP 策略）。
8. **CLI help / 注释漂移**：`cmd/validate.go:61` 的 `--mode` flag help 曾声称支持 `full (all checks)`，但 validator 代码只有 quick/sample/checksum 三个分支（`validator.go:509-511`，无 default 分支）——help 与 `cmd/cdc.go:40` 的 `PG2TIDB_CDC_FORCE` 滞后注释已在本轮工作树中随手修正（随本文档同批交付，待推送）；README 特性节的"四种校验模式"为同一漂移的**上游描述，仍待修正**；`--mode full` 修复前的实际行为待 runtime 确认（低置信假设：落入空分支）。

### 7.5 路线图建议顺序

P1 收口（#t110 推送、103 夹具清理）→ P2 #t109 Version 回显 → P3 Epic 池按 README 优先级（鉴权 > CDC 补齐 > 语义对齐 > 韧性 > 对称批）。另外线程待办中"数据对比性能优化"是当前活跃方向，A1/M1/A2 已落地，后续候选见 P3-④ 比对重试韧性批。

## 8. 测试构建与部署

### 8.1 测试体系

- **Go 单测**：182 个 `_test.go`（全库 343 个 .go / 92,725 行，含测试）。风格：表格驱动 + oracle 等价测试（rowhashspool 新旧语义 set-equal）+ 红队 seam 测试（#102 先补穿透测试再修）。
- **集成 tier**：`internal/cdc/cdc_integration_test.go` 对真 PostgreSQL（wal_level=logical）跑真实逻辑复制握手；CI 守卫保证 PG 缺失即失败、绝不 silent skip。
- **前端**：vitest 48 spec（co-located），`npm run test`。覆盖缺口：TaskListView / HistoryView / DDLExportView 无专属 spec（§7.2）；无 e2e 常规门禁（e2e/ 为手工脚本集且已有坏引用）。
- **PG-GATE**：103 环境一键回归门禁（`scripts/pg-gate.ps1`），多源化工单硬门禁。
- **CI**：仅一条 workflow `cdc-integration.yml`——只在 `internal/cdc/**` 变更时触发（避免全量 PR 慢导致门禁被弃用），自起 PG16 docker（GHA services 无法改 wal_level，故手动 docker run + pgoutput 所需 flag）。

### 8.2 构建

Makefile targets：`build`（纯 CLI）/ `build-lightning` / `test` / `test-cover` / `lint` / `vet` / `fmt` / `clean` / `web-frontend` / `build-web`（=web-frontend+嵌入）/ `build-all` / `all`（fmt+vet+test+build）。

- 前端：`npm ci && npm run build`（vue-tsc 类型检查 + vite build）→ `build-web.sh` 拷贝 dist 到 `cmd/static/`。
- Lightning 内嵌：`LIGHTNING_BIN=/path/to/tidb-lightning bash build-web.sh` 把二进制放 `internal/lightning/tidb-lightning` 参与 go:embed（构建后 trap 恢复占位文件）；运行时优先级 = 系统 PATH > 内置释放（`embed.go: EnsureExtracted/FindBinary`）> 流式 INSERT 回退。
- 交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o build/pg2tidb .`（SQLite 用 modernc 纯 Go 驱动，无 CGO 依赖）。
- 已知坑：版本回显未接线（#t109，`-X main.Version` 注入目标符号不存在）。

### 8.3 Docker 与部署

- Dockerfile 三阶段：`node:20-alpine`（前端）→ `golang:1.22-alpine`（构建 + 下载 tidb-toolkit 内置 Lightning）→ `alpine:3.19`（运行时，仅 ca-certificates + tzdata）。`docker run -d -p 8080:8080` 即 Web 服务。
- 单机部署：单二进制 + `nohup ./timstool web -c config.yaml -p 8080` + `curl /api/v1/health` 验活；数据目录 `<dataDir>`（默认 `.timstool`）下含 `timstool.db`、`compare/`、`spool/`、checkpoint、CDC status/checkpoint 文件——**容量与 fd 数字是规划口径（文档推导 + 实测），运行时无 statfs/ulimit 守护**（§3.2），兜底仅 spool 写失败 fail-loud；`docs/deployment.md` 目前没有 fd 容量表（#t110 待合入正好补此缺口）。fd 规划 294@4 路 / 558@8 路；窄 ulimit 环境 4 路 ≥1024 / 8 路 ≥2048。
- Docker Compose：PG16 + TiDB v7.1.0 示例见 README。
- **联调测试环境**：团队内网联调环境一套（Web 入口 + PG/MySQL 测试源 + TiDB 目标源）。**地址、端口、库名、凭据一律不写入公开文档**（本仓库为 public，且 Web API 当前无认证中间件——§3.5/§7.1，环境细节入库等同攻击指引），以团队频道公告为权威源（密码轮换不影响拓扑描述）。
- 运维注意：链式任务（cdc_chain）全量期间源端 WAL 持续累积，`max_slot_wal_keep_size` 不要设过小（README 警示）。

---

*本文档由金山云glm53-开发基于 origin/main @ `fd4ba57` 深度阅读撰写（2026-11-10），经评审A（准确性）/ 评审B（问题清单完整性）双评审。发现描述与代码不符时，以代码为准并回改本文。*
