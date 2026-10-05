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
| tidb-tables.sql | schema 采集+DDLBuilder 转换 | **manifest skip**（PG catalog 专用路径，MySQL 源不支持） | 不使导出失败；skip 面在 zip manifest+服务端 Warn 可见；**10c2 候选**：SHOW CREATE TABLE 结果经 mysql→TiDB 类型映射出 TiDB 转换版（候刘源点单；防「10c 完成」被误读为迁移主线 DDL 面闭环——导出面暂仅产原生形） |

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

## 5.7 地板

- `SHOW CREATE TRIGGER` 在 5.7 为 5 列（8.0 起改列序并增列）——DDL 列索引按 **8.0 列序**
  钉扎（idx 2）；产品 MySQL 面宣称 8.0.x（沿 MS-10b dialect-map 同项）。
- 函数式索引（TABLES 面间接涉及）为 8.0.13+，5.7 无此对象。
