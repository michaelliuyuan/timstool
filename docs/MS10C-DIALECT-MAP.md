# MS-10c · DDL 导出 MySQL 方言映射与边界总账

> 基线：MySQL 8.0.x（产品宣称面）；实现 `internal/ddlexport/exporter_mysql.go`；
> 走法=information_schema 点名（有序、不依赖会话默认库）+ 逐对象 `SHOW CREATE`。
> PG 面零回归：`Options.SourceType` 分派，空值/pg 走原 PG catalog walk（逐字节不变）。

## 对象类型映射（PG → MySQL）

| 文件 | PG 走法 | MySQL 走法 | 边界 |
|---|---|---|---|
| tables.sql | pg_class + 列级重建 | `SHOW CREATE TABLE db.tbl`（DDL 列 idx 1） | **FK 已含在表 DDL 内**（MySQL 无独立 ADD CONSTRAINT 导出面）；无 PG 的「FK 后置追加」段 |
| indexes.sql | pg_get_indexdef 逐索引 | **降级注记文件** | MySQL 索引无独立命名空间——全部在 SHOW CREATE TABLE 输出内（含函数式索引 8.0.13+，`KEY idx ((expr))` 表达式在表 DDL 内**天然保真**——与 seq746 P2 的 apply 面渲染缺口互补，apply 面缺口见 10c2/尾批池） |
| views.sql | pg_get_viewdef | `SHOW CREATE VIEW`（DDL 列 idx 1） | 输出含 `CREATE ALGORITHM=... DEFINER=...` 前缀（见 DEFINER 注记） |
| sequences.sql | pg_sequences 逐列 | **降级注记文件** | MySQL 无序列对象；AUTO_INCREMENT 语义在表 DDL 内 |
| functions.sql | pg_get_functiondef | `SHOW CREATE FUNCTION`（DDL 列 idx 2） | DEFINER 注记同下 |
| procedures.sql | pg_get_functiondef | `SHOW CREATE PROCEDURE`（DDL 列 idx 2） | 同上 |
| triggers.sql | pg_get_triggerdef | `SHOW CREATE TRIGGER`（DDL 列 idx 2） | 同上 |
| types.sql | enum/composite/domain 三查 | **降级注记文件** | MySQL 无用户自定义类型对象 |
| tidb-tables.sql | schema 采集+DDLBuilder 转换 | **CIR 转换版（MS-10c2 起）**：source adapter SchemaReader 走 information_schema 入 CIR（列级 mysqlTypeMapper 已出 TiDBType）→ target `RenderCreateTable` 渲染 | 转换版可信度=覆盖率明示：1:1 passthrough 列类型（未映射 verbatim/几何族）逐类型入 manifest skip 台账（禁静默吞）；函数式索引键部按 I_S EXPRESSION 原文 verbatim 重放（见注记 7）；锚：TestMySQLTiDBTablesConversion |

## 关键语义注记

1. **DEFINER 子句（刘源/leader 面签）**：`SHOW CREATE VIEW/FUNCTION/PROCEDURE/TRIGGER`
   输出自带 `DEFINER=\`user\`@\`host\``。**保留原样导出**——跨环境重放时若账号不存在会
   报错，这是 MySQL 生态的既有语义（与 mysqldump 默认行为一致）；需要跨账号重放的消费方
   自行改写（`SET sql_log_bin`/sed DEFINER 均为消费侧决策，导出器不做静默改写）。
2. **逐对象 SHOW 非原子快照**：每个 SHOW CREATE 是独立语句，无 PG 单事务快照等价物
   （information_schema 点名+逐对象取 DDL 之间存在并发 DDL 窗口）。导出失败的单对象走
   skip()（manifest+Warn），不使整包失败。
3. **UTC 会话钉扎**：连接走 `openSourceTestConn`→`DSNByType`（`time_zone='+00:00'`，
   MS-08d）——DDL 导出面继承同一钉扎会话（锚：TestDDLExportSrcTypeDispatch）。
4. **系统库过滤**：`SHOW DATABASES` 减 `mysql/information_schema/performance_schema/sys`
   四集（与 mysqlWatermarkDialect SystemSchemas 同口径，锚：TestMySQLDatabaseFilter）。
5. **schema 语义**：MySQL 的「schema」即数据库；`NewExporter` 的 `public` 兜底是 PG-only
   （MySQL 保留调用方清单原样，锚：TestMySQLSchemaDefaultKept）。
 6. **权限面**：SHOW CREATE VIEW 需 SHOW VIEW 权、FUNCTION/PROCEDURE 需对应创建权限——
    权限缺失走 skip() 面呈现（与 PG exporter 对象级失败同契约）。
 7. **函数式索引键部重放原文纪律（MS-10c2）**：apply/转换面 `target.renderIndexPart`
    对键部二态——合法标识符形（`^[A-Za-z0-9_$\x{0080}-\x{FFFF}]+$`）反引号包裹，
    其余（MySQL 8.0.13+ `information_schema.STATISTICS.EXPRESSION` 文本，如
    `(`a` + 1)`/`(lower(`b`))`）**verbatim 裸发：禁二次包裹、禁剥括号**（I_S 原文
    即合法 DDL）；不做 SQL parse（二态判据足够——表达式含反引号即标识符字符集外，
    裸发即 SHOW CREATE 原语义）。重放等价锚：渲染产物过真 ApplyDDL 执行链逐字节
    断言（TestApplyDDLReplaysFunctionalKeyPartsVerbatim，F-15 同源夹具）。
 8. **README 双版（MS-10c2）**：`readmeFile(schema, srcType)` 分派——PG 版（""/postgres）
    逐字节冻结（锚：TestReadmePGByteIdentity）；MySQL 版按本 map 撰（SHOW CREATE 应用
    序、DEFINER 原样、非原子快照、函数式索引保真、会话时区、5.7 地板、passthrough
    台账注记；零 PG 字样，负锚只扫模板字面段、DB 名等用户数据不误伤，
    锚：TestReadmeMySQLTemplate）。
 9. **TiDB 转换覆盖率台账（MS-10c2）**：MySQL 源 tidb-tables.sql 转换逐列经
    mysqlTypeMapper（35+ case）；判定 passthrough=TiDBType 与 SourceType 逐字节相等
    （mapper default 支 verbatim 回显=未映射）或几何族（GEOMETRY/POINT/…，拼写保留
    但无转换语义）→ 每 schema 每类型一条 manifest skip 台账（`type:<TYPE>`），原生 1:1
    整数族（BIGINT 等映射拼写）不入台账——大小写敏感比较区分「映射产出」与「原文回显」。

## 5.7 地板

- `SHOW CREATE TRIGGER` 在 5.7 为 5 列（8.0 起改列序并增列）——DDL 列索引按 **8.0 列序**
  钉扎（idx 2）；产品 MySQL 面宣称 8.0.x（沿 MS-10b dialect-map 同项）。
- 函数式索引（TABLES 面间接涉及）为 8.0.13+，5.7 无此对象。
