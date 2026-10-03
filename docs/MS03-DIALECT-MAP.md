# MS-03 Dialect Relocation Map（行号映射表 · 基线 d08e661）

纯搬迁契约（红队 seq 203-①）：commit② 实现体=下表「原行号」逻辑机械搬运，新文件行↔原行号一一对应；
任何看似等价的行为微调须独立 commit+标注，禁混入搬迁笔。

## 源侧（CompareDialect → postgres 实现）

| 接口方法 | 原位置（d08e661） | 备注 |
|---|---|---|
| QuoteIdent | validator.go:1173 quotePG | `"x"` |
| Qualify | checksum.go:213-218 / validator.go:401-404 的 `"s"."t"` 拼装 | 两段限定 |
| EstimateRows | quick.go:26-30（pg_stat_user_tables.n_live_tup） | err=估算不可用；fallback 决策留主流程 |
| CountExact | quick.go:35-36 / validator.go:404 | COUNT(*) |
| WmPredicateFragment | wmfilter.go:56-58 wmWherePG | `col <= $1`；wmOp/白名单留主流程 |
| ~~BuildSelect/BuildSelectAll~~ | 取数组合=**主流程 fmt.Sprintf+方言 QuoteIdent/WmPredicateFragment**（checksum.go:213-218/298-303、validator.go:508-516/889-893/967-970/1029-1032/1085-1088、nopk.go:170-173/507-510/626-631） | leader seq 214 裁定：零调用方死面裁撤，MS-08 真分叉再加回 |
| ValidateWatermarkColumn | wmfilter.go:68-84 checkWatermarkColumn 整函数 | information_schema 探测；**白名单 map（wmfilter.go:26）留主流程，其应用随函数搬运**（leader seq 209-③口径） |
| DetectTableKey | nopk.go:37-100（含 parseIndexColumns :102-128） | PK/唯一索引元数据 |
| AdjustDSN | wmfilter.go:97-103 appendPGDSNUTC | **P-INC-TZ 触点，段一逐字保留** |

## 目标侧（TargetDialect → TiDB 实现）

| 接口方法 | 原位置（d08e661） | 备注 |
|---|---|---|
| QuoteIdent | validator.go:1177 quoteMySQL | `` `x` `` |
| Qualify | checksum.go:298-303 的单段 `t` | 库=连接属性 |
| EstimateRows | quick.go:44-73（SHOW TABLE STATUS LIKE+escapeSQLLike+Sscanf；escapeSQLLike :90-96） | 解析失败=0+nil（保 :67-72 语义） |
| CountExact | quick.go:58-59 / validator.go:419 | COUNT(*) |
| WmPredicateFragment | wmfilter.go:61-63 wmWhereMySQL | `col` <= ? |
| ~~BuildSelect/BuildSelectAll~~ | 同源侧：主流程组合 | seq 214 裁撤 |
| SessionInit | validator.go getTiDBConn 的 SET time_zone（UTC） | **P-INC-TZ 触点，段一逐字保留（含 −8h 潜伏态）** |
| MatchHashColumns | nopk.go:646-662 | 小写列名匹配+approximate-float/json 跳过 |

## 留主流程（不进方言；复票首查）

- 四护栏本体：wmIdentRe（wmfilter.go:22）/wmAllowedColumnTypes（:26）/wmFilter（:37）/wmOp（:47）
- #t4 concSem 并发槽+死锁三不变量；方言实现零自带并发
- 三算法编排/阈值/per-table fail 语义/F-05 分支（validator.go 决策树）
- md5 行哈希/chunk 聚合/表哈希+truncate（checksum.go:273-283/:356-366）；
  **computeRowHash（PG，nopk.go:617 调用）与 computeTiDBRowHash（:679 调用）配对关系原样保留，禁统一**
- normalizeValue+PG 数组→JSON 解析（validator.go:1299 附近）——MS-03 单实现共用，MS-08 位预留
- 报告结构/Summary/Suggestion 文案
- quick fallback 决策（估算 err→COUNT 精确，同一触发条件/同一调用序，行为恒等迁出）
