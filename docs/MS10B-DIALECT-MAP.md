# MS-10b · MySQL assess 方言映射与降级口径（Dialect Map）

> 基线 9f841d8（feat/ms10b-assess）；adversarial 票 seq714 面①②③⑥⑪ 记档归口此文件。
> 原则：**翻面≠拔闸**——每个 PG catalog 查询要么给出 MySQL 等价方案，要么显式记降级口径与影响面。

## 扫描器逐条映射（internal/assess/scanner_mysql.go）

| 维度 | PG 侧 | MySQL 侧 | 口径 | 备注 |
|---|---|---|---|---|
| tables | information_schema.tables | information_schema.TABLES | **等价** | TABLE_SCHEMA=? 占位 |
| columns | information_schema.columns + 约束 JOIN | information_schema.COLUMNS + PK EXISTS(INDEX_NAME='PRIMARY') 探针 | **等价** | PK 探针无 IS_VISIBLE（MySQL PRIMARY 不可 invisible，8.0 语法禁） |
| indexes | pg_indexes | information_schema.STATISTICS 聚合 | **等价** | GROUP_CONCAT 列单 ORDER BY SEQ_IN_INDEX；IS_VISIBLE='YES' 过滤沿 MS-10a 纪律（5.7 无此列=地板项）；**IsExpression=MAX(COLUMN_NAME IS NULL)** 捕获 8.0.13+ 函数式键部（P1-2 修）；IsPartial 恒 false=MySQL 本无 partial index（非降级） |
| views | views + pg_get_viewdef | information_schema.VIEWS | **等价（definition）/降级（DDL 空）** | 不付 per-view SHOW CREATE VIEW 往返；checker 空 DDL 有重建兜底 |
| functions | pg_proc | information_schema.ROUTINES | **降级** | Language 恒记 'sql'（MySQL routine 均 SQL-bodied）；DDL 空 |
| triggers | pg_trigger | information_schema.TRIGGERS | **等价 / DDL 降级空** | |
| enums | pg_type/pg_enum | — | **降级空集** | MySQL 无 schema 级枚举类型；列级 ENUM 经 data_type 维呈现 |
| extensions | pg_extension | — | **降级空集** | 无概念 |
| sequences | pg_class/pg_sequences | — | **降级空集** | AUTO_INCREMENT 列级、**未采集**——MySQL→TiDB 良性（TiDB 原生支持），反向复用成实缺口（adversarial P2-2） |

## 已知边界（记档，非阻断）

1. **评分语义（P1-1，候裁定）**：assess checker 为 PG→TiDB 定向——MySQL 原生类型（datetime/float/double/char/tinyint(1)/blob 族/enum/set/time/year/mediumint/binary/varbinary/text 族/geometry）无 checker 分支落 default「需手动评估」，实际 MySQL→TiDB 多为 1:1；近全兼容 schema 评分偏低失真（冒烟 87.3 convertible）。裁定选项：(a) v1 best-effort 复用+UI 标注口径；(b) MySQL 原生维度 map（尾批池）。
2. **降维满分 vs N/A**：enums/extensions/sequences 空维 checker 构造性满分——建议报告加 N/A 维态（候裁定）。
3. **GROUP_CONCAT 截断（P3）**：group_concat_max_len 默认 1024，超宽复合索引 Definition 可截断（仅报告展示，非评分面）。
4. **newMySQLScanner 空 schema 兜 "public"**：防御形（handler 已前置 database 兜底，webapi 不可达）；改进建议=该层不默认、交调用方（positive/adversarial 同见）。
5. **大库/无分页（P2）**：九扫描器串行全量 I_S 扫，挂 chi 120s 组单 ctx；九条查询非单事务（并发 DDL 下跨维非原子快照）。v1 典型规模护栏：建议 ≤ 数万列级。
6. **5.7 地板**：IS_VISIBLE 为 8.0+ 列（沿 MS-10a 尾批池同项）。
