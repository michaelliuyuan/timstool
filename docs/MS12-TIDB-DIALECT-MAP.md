# MS-12 · DDL 导出 TiDB 源方言映射与边界总账

> 基线：TiDB v7.1.9-0.2 Enterprise（TE-104 live 实测，103 test_tidb）；产品宣称面 v6.5 LTS+（sequences 地板特性 v5.0+）。
> **MS-12c 修订（2026-11-03 te 五类对象实测 ddl_te_test）**：原「TiDB 无存储程序/触发器」假设**被实机证伪**——平凯企业版
> v7.1.9-0.2（`tidb_enable_procedure=ON`）函数/过程/触发器三类对象真实存在且完全可用，触发器更自社区版 v5.0+ 即支持；
> 函数/过程/触发器由**无条件注记文件**改为**真导出**（与 MySQL 面同构），确不支持时**台账化降级**（绝不静默）。
> 实现：`internal/ddlexport/exporter_mysql.go`（MySQL-wire 共享 SHOW CREATE walk）+ `internal/source/tidb`（连接生命周期适配器）；
> 分派：`Options.SourceType="tidb"` 经 `source.IsMySQLWire` 谓词入共享 walk（`isMySQLWireDDL`）；PG 面 / MySQL 面零回归。
> 只读红线：全程 SHOW + information_schema SELECT，无任何 DDL 写操作；SHOW CREATE 输出 verbatim 一字节不改。

## 对象类型映射（TiDB 源）

| 文件 | 走法 | 边界 |
|---|---|---|
| tables.sql | `information_schema.TABLES` 点名（`TABLE_TYPE='BASE TABLE'`，有序）+ 逐对象 `SHOW CREATE TABLE`（DDL 列 idx 1） | **TABLE_TYPE 钉扎把 SEQUENCE 挡在 tables.sql 外**（序列入 sequences.sql）；TiDB 专有子句 verbatim（见注记 4 形状参考） |
| views.sql | `information_schema.VIEWS` 点名 + `SHOW CREATE VIEW`（DDL 列 idx 1） | DEFINER 原样导出（注记同 MS-10c ①） |
| sequences.sql | **真导出（裁 1）**：`information_schema.TABLES` `TABLE_TYPE='SEQUENCE'` 点名 + 逐对象 `SHOW CREATE SEQUENCE`（DDL 列 **idx 1**） | TE-104 live 双绿：列形同构（idx0=`Sequence` 对象名 / idx1=`Create Sequence` DDL 文本）+ information_schema 点名可靠；**读取器统一 idx1 取 DDL**，与 SHOW CREATE TABLE 读取器同构；DDL 全小写关键字形 verbatim |
| functions.sql / procedures.sql | **真导出（MS-12c）**：`information_schema.ROUTINES` 点名（`ROUTINE_TYPE` 区分 FUNCTION/PROCEDURE，与 MySQL 面共用同一常量）+ 逐对象 `SHOW CREATE FUNCTION/PROCEDURE`（DDL 列 **idx 2**，与 MySQL 列形同构——te 实测 `SHOW CREATE` 全通） | 能力随发行面变化：平凯企业版 `tidb_enable_procedure=ON` 实测支持（te_fn_tax(100)=8.00 / CALL 落行）；社区版无存储程序——**点名失败→降级注记文件 + manifest skip 台账（带原始错误），绝不静默**；点名成功零行→「none found」注记文件（零对象是结果不是跳过，不进台账） |
| triggers.sql | **真导出（MS-12c）**：`information_schema.TRIGGERS` 点名 + 逐对象 `SHOW CREATE TRIGGER`（DDL 列 **idx 2**） | 触发器社区版 v5.0+ 即支持（平凯实测触发器联动生效）；同 MS-12c 降级契约（点名失败→注记+台账；零行→注记不进台账） |
| types.sql | **注记文件** | TiDB 无用户自定义类型（无可查询的目录面——与函数/过程/触发器不同，此处「无对象」是可靠的） |
| indexes.sql | **注记文件** | TiDB 索引无独立命名空间——全部在 SHOW CREATE TABLE 输出内 |
| tidb-tables.sql | **不生成** | 源已是 TiDB，转换参照无意义；`IncludeTiDB` 被忽略并记 manifest 台账（不静默） |

## 关键语义注记

1. **只读红线**：连接经 `DSNByType`（go-sql-driver，`charset=utf8mb4`、`time_zone='+00:00'`
   UTC 会话钉扎，同 MS-10c 注记 3）；采集面仅 SHOW + information_schema SELECT，
   无任何 DDL 写操作。
2. **PRE_SPLIT_REGIONS 不回显（裁 2a）**：一次性物理切分提示，`SHOW CREATE TABLE`
   天然不含该子句 → **tables.sql 不含 PRE_SPLIT_REGIONS 属如实导出、非缺陷**
   （verbatim 纪律的正面推论）。README 注明「重放如需预切分须手工补
   PRE_SPLIT_REGIONS」；导出**不承诺**该子句。
3. **VECTOR（裁 2b）**：列类型随 tables.sql verbatim 携带；**VECTOR 索引**为
   v7.1.9 特性未启用面（CREATE 即 1064，TE-104 实证），本版面无此对象；若源版本更高
   含 VECTOR 索引，重放目标须 ≥ 源且特性开启——并入注记 5「重放目标 ≥ 源」，
   不单开条款。
4. **形状参考三则（裁 3，防锚写错期望形）**：
   - SHARD_ROW_ID_BITS：表尾 `/*T! SHARD_ROW_ID_BITS=4 */` —— **`/*T!` 无方括号**
     （非 `/*T![...]`）；te 实测带 PK 建表报 8200 属**源端建表约束**，与导出无关。
   - AUTO_RANDOM：PK 列注记 `/*T![auto_rand] AUTO_RANDOM(5) */`。
   - TTL 三连子句（列名带反引号）：表尾
     `/*T![ttl] TTL=\`created_at\` + INTERVAL 1 DAY */ /*T![ttl] TTL_ENABLE='ON' */ /*T![ttl] TTL_JOB_INTERVAL='1h' */`。
   另：CLUSTERED 形 `/*T![clustered_index] CLUSTERED */`（PK 列尾）。
5. **重放目标地板**：重放目标 TiDB 版本 ≥ 源（TiDB 专有子句只在理解它的版本上可解析；
   VECTOR 索引情形见注记 3）。
6. **系统库过滤**：`SHOW DATABASES` 减共享集 `mysql.SystemDatabases`
   （mysql / information_schema / performance_schema / sys / **METRICS_SCHEMA**），
   大小写不敏感（TiDB 大写上报形），与 MS-10c 注记 4 同源同口径。
7. **placement policies / resource groups 不导出（v1 裁定）**：DBA 管理面；
   每 schema 两条 manifest skip 台账注记（信息性覆盖注记，非对象级失败）。
8. **README TiDB 版（MS-12）**：`readmeFileTiDB`——应用序 sequences → tables → views
   （表 DEFAULT 可引用序列）；verbatim 子句清单 + PRE_SPLIT_REGIONS 不回显注记 +
   重放目标地板（含 VECTOR 折叠）+ 注记文件清单 + 系统库排除；零 PG 字样
   （锚：TestReadmeTiDBTemplate）。
9. **逐对象 SHOW 非原子快照**：与 MS-10c 注记 2 同契约——单对象失败走 skip()
   （manifest + Warn），不使整包失败；sequences 列形突变（列数不符）由
   showCreate 列数守卫**响亮失败**入台账，绝不静默错解析。
10. **能力位配对纪律（five-pin）**：`tidb_meta.go` 仅 `DDLExport: true`，其余六位
     （schema/data/cdc/compare/watermark/assess）保持 false——六个非导出流的面走
     srcCapable 400；source_handler 两处 tidb 裸 guard 已随翻转移除（能力位接管），
     server.go 迁移任务的 tidb-as-source guard 保留（DDL 导出 Web-only 面）。
11. **重放须保留 `/*T!*/` 注释（te 🟡 注记裁定入档）**：mysql CLI 默认剥 `/*T!*/` 注释，
    重放需加 `--comments`（驱动直发原文天然 OK），否则 TTL / SHARD_ROW_ID_BITS /
    AUTO_RANDOM / CLUSTERED 等 TiDB 专属子句被客户端剥除；导出包正确，问题在重放工具侧。
12. **存储程序/触发器真导出 + 台账化降级（MS-12c，te 2026-11-03 五类实测驱动）**：
    - **证伪史**：MS-12 初版按「TiDB 无存储程序/触发器」写无条件注记文件（连查询都不发）；
      te 在平凯实机 `ddl_te_test` 种入 10 对象实测——三类对象真实存在且可用，导出却全为空占位，
      且**不进 manifest skipped 台账**（placement/RG 都有台账）→ 迁移重放端静默缺失全部业务逻辑，属产品功能缺口。
    - **真导出**：tidb 分支与 mysql 分支共用同一 information_schema 点名常量 + SHOW CREATE 走法
      （`tidbRenderStoredPrograms`；DDL 列 idx 2 钉扎，列形与 MySQL 同构由 showCreate 列数守卫响亮把守）。
    - **降级契约**（三分支各归其位，绝不静默）：①点名**失败**（如社区版无 `information_schema.ROUTINES`，
      Error 1109）→ 注记文件 + manifest skip 台账（reason 带原始错误）；②点名成功**零行** → 「none found」
      注记文件，不进台账（零对象是结果不是跳过）；③逐对象 SHOW 失败 → 该对象进台账，walk 继续。
    - **重放提示**：DEFINER 原样；BEGIN...END 体重放需接收端 delimiter 处理（mysql CLI `DELIMITER ;;`）。

## 锚

- Go：TestTiDBDatabaseFilter（系统库过滤含 METRICS_SCHEMA，大小写不敏感）、
  TestTiDBSequencesRealExport（TABLE_TYPE='SEQUENCE' 点名 + idx1 取 DDL +
  列形突变响亮入台账）、TestTiDBTablesExcludeSequences（tables 点名钉 BASE TABLE）、
  **TestTiDBStoredProgramsRealExport（MS-12c：函数/过程/触发器真导出 verbatim + 计数 +
  干净零台账 + 与 MySQL 共用点名常量）**、**TestTiDBStoredProgramsDegradeLoud（点名
  失败→注记文件 + 台账带原始错误，绝不静默空文件）**、**TestTiDBStoredProgramsEmptyListingNote
  （零行→none found 注记，不进台账）**、**TestTiDBStoredProgramShapeGuardLoud（SHOW
  CREATE 列形突变→逐对象响亮入台账，他类不受扰）**、
  TestTiDBNoteFilesAndLedger（types/indexes 注记 + **旧三类注记负锚（防静默丢失回归）** +
  IncludeTiDB 忽略台账 + placement/RG 台账）、
  TestReadmeTiDBTemplate（模板正负锚，含 MS-12c 应用序与真导出注记）、
  TestDDLExportSrcTypeDispatch（tidb 分派 + 4000 兜底 + UTC 钉扎）、
  TestSrcCapable / TestCapabilityMatrixSnapshot（tidb 翻转 + 其余六位 false）。
- FE：DDLExportView spec——picker 含 tidb、tidb 源隐藏 IncludeTiDB 开关
  （类型勾选清单三态通用，MS-12c 零 FE 改动）。

## 地板与投放面

- sequences 为 v5.0+ 特性（产品宣称面 v6.5 LTS+）；CLI 先于 Web 面板投放（v5.0 地板
  同 CLI 语义）；placement/RG 注记不做（v1 裁定）。
