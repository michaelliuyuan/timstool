# MS-12 · DDL 导出 TiDB 源方言映射与边界总账

> 基线：TiDB v7.1.9-0.2 Enterprise（TE-104 live 实测，103 test_tidb）；产品宣称面 v6.5 LTS+（sequences 地板特性 v5.0+）。
> 实现：`internal/ddlexport/exporter_mysql.go`（MySQL-wire 共享 SHOW CREATE walk）+ `internal/source/tidb`（连接生命周期适配器）；
> 分派：`Options.SourceType="tidb"` 经 `source.IsMySQLWire` 谓词入共享 walk（`isMySQLWireDDL`）；PG 面 / MySQL 面零回归。
> 只读红线：全程 SHOW + information_schema SELECT，无任何 DDL 写操作；SHOW CREATE 输出 verbatim 一字节不改。

## 对象类型映射（TiDB 源）

| 文件 | 走法 | 边界 |
|---|---|---|
| tables.sql | `information_schema.TABLES` 点名（`TABLE_TYPE='BASE TABLE'`，有序）+ 逐对象 `SHOW CREATE TABLE`（DDL 列 idx 1） | **TABLE_TYPE 钉扎把 SEQUENCE 挡在 tables.sql 外**（序列入 sequences.sql）；TiDB 专有子句 verbatim（见注记 4 形状参考） |
| views.sql | `information_schema.VIEWS` 点名 + `SHOW CREATE VIEW`（DDL 列 idx 1） | DEFINER 原样导出（注记同 MS-10c ①） |
| sequences.sql | **真导出（裁 1）**：`information_schema.TABLES` `TABLE_TYPE='SEQUENCE'` 点名 + 逐对象 `SHOW CREATE SEQUENCE`（DDL 列 **idx 1**） | TE-104 live 双绿：列形同构（idx0=`Sequence` 对象名 / idx1=`Create Sequence` DDL 文本）+ information_schema 点名可靠；**读取器统一 idx1 取 DDL**，与 SHOW CREATE TABLE 读取器同构；DDL 全小写关键字形 verbatim |
| indexes.sql | **注记文件** | TiDB 索引无独立命名空间——全部在 SHOW CREATE TABLE 输出内 |
| functions.sql / procedures.sql / triggers.sql / types.sql | **注记文件** | TiDB 无存储程序 / 触发器 / 用户自定义类型 |
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

## 锚

- Go：TestTiDBDatabaseFilter（系统库过滤含 METRICS_SCHEMA，大小写不敏感）、
  TestTiDBSequencesRealExport（TABLE_TYPE='SEQUENCE' 点名 + idx1 取 DDL +
  列形突变响亮入台账）、TestTiDBTablesExcludeSequences（tables 点名钉 BASE TABLE）、
  TestTiDBNoteFilesAndLedger（四注记文件 + IncludeTiDB 忽略台账 + placement/RG 台账）、
  TestReadmeTiDBTemplate（模板正负锚）、TestDDLExportSrcTypeDispatch（tidb 分派 +
  4000 兜底 + UTC 钉扎）、TestSrcCapable / TestCapabilityMatrixSnapshot（tidb 翻转 +
  其余六位 false）。
- FE：DDLExportView spec——picker 含 tidb、tidb 源隐藏 IncludeTiDB 开关。

## 地板与投放面

- sequences 为 v5.0+ 特性（产品宣称面 v6.5 LTS+）；CLI 先于 Web 面板投放（v5.0 地板
  同 CLI 语义）；placement/RG 注记不做（v1 裁定）。
