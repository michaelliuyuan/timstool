# MS-04 WatermarkDialect 映射表（增量引擎源侧方言化）

基线 `ff589c6`（MS-03 段二终态）→ 本单链 9b88f53（①接口+实现+单点）→ db4ad97+1888450（②路由+D4 吸收+A3）→ ③docs 本文件。
口径：纯搬迁零行为变更；SQL/format 串逐字节恒等；行号=基线 ff589c6 行号（红队复票按表机械核对）。

## 源侧（incremental.go → wm_dialect.go）

| 基线位置 | 原体 | 去向 | 恒等口径 |
|---|---|---|---|
| incremental.go:141-142 | incQuotePG | pgWatermarkDialect.QuoteIdent | 逐字节（format 串+转义） |
| incremental.go:147-160 | incBuildSelectSQL | BuildSelectSQL | SQL 串逐字节（$1/$2 参数化不变） |
| incremental.go:162-172 | incBuildDrainSQL | BuildDrainSQL | 同上（`= $1` 无 LIMIT） |
| incremental.go:174-179 | incBuildNextWatermarkSQL | BuildNextWatermarkSQL | 同上（`MIN(%s)…> $1`） |
| incremental.go:129-137 | incWatermarkTypes var | wm_dialect.go 同名包级 var | 保名保位（同包锚零改动）；头注=消费白名单（方言方法+白盒锚，主流程禁直读） |
| incremental.go:671-702 | queryIncColumns | pgWatermarkDialect.QueryColumns | 目录 SQL 612B 脚本比对全等+扫描体逐字 |

## 消费面路由（②笔，全经包级 incSourceDialect 单点）

| 基线调用点 | 路由后 |
|---|---|
| incremental.go:1181 selSQL | BuildSelectSQL |
| incremental.go:1313 drain | BuildDrainSQL |
| incremental.go:1365 jump | BuildNextWatermarkSQL |
| incremental.go:1144 MIN 初始水位探针（incQuotePG×3：WatermarkColumn/sc.Schema/t.Table） | QuoteIdent×3 |
| incremental.go:787/:883 queryIncColumns×2（单表+批端点） | QueryColumns |
| incremental.go:1130 incWatermarkTypes 直读 | WatermarkEligible |
| incremental_logs.go:310-332 三渲染器（incQuotePG） | QuoteIdent（同源不同调用点禁再复制注记；恒等口径=引号片段，整体 SQL 与主流程非同串——logs 内联 wm/limit 供展示，锚 TestIncLogsRenderersQuoteFragments） |
| watermark_suggest.go:145 incWatermarkTypes 直读 | WatermarkEligible |

## 留主流程（不迁移，MS-09 位）

- incQuoteMySQL / incBuildInsertSQL / incExecShardedInsert / incTargetDSN：目标侧写路径（v1 目标恒 tidb；MySQL 目标接入时方言化）。
- queryIncTableKeys / incTableKeysSQL（:716-751，含 ANY($2)+pgx text[] 注）：键披露批查询——**MS-04 票面引证修正项**（红队 🟡①：ANY($2) 属此查询非 QueryColumns）。
- 水位游标状态机（incCursorStep/incJumpAfterDrain/incAdvanceWatermark）、占位符 shard、worker 池编排：通用逻辑无方言分叉。

## D4 守卫吸收（②笔）

- 四守卫（基线 :490/:563/:771/:852 `src/e.Type != "postgres"`）→ `!incSourceWatermarkCapable(kind)`：helper 显式拒空（NormalizeKind("") 默认 postgres 的语义反转风险防御）+未知 kind→false（400 同文案不升 500）。
- **形态偏离记档**：leader seq 271 定单 if 双条件 `Type==""||!Capable`——`.Type == ""` 命中 A3 冻结正则致 incremental.go 停 6、裁定 6→2 不落地；空检查移入 helper 后语义恒等且 6→2 落地（候追认）。
- 负锚：TestIncSourceWatermarkCapable（空/未知/mysql/tidb=false，postgres=true）。
- A3 fixture：internal/webapi/incremental.go 6→2（残留=target-tidb 守卫 :501/:574 同基线）。

## 源侧 UTC 会话（盘点记档，leader seq 269 ②裁定）

增量源读连接（openPGTestConn(sc.DSN())）无显式会话时区——水位/时间戳壁钟串按 PG 服务器 tz 渲染；合规前置 timezone='UTC'（docs/PG-GATE.md 环境前置）下全链自洽（水位串持久化+续跑跨会话同 tz）。**远期池券**：源侧会话时区显式化+存量水位串迁移方案（触发条件=非合规 timezone 环境部署；强钉会破坏存量水位断点续跑，需数据迁移配套，非纯搬迁范围）。

## 锚测账

- TestInc 锚（基线 22 个 TestInc 函数+2 新锚=24；旧「21」为既往口径计数，非本单引入）+keywarn 锚：渲染锚改靶 incSourceDialect（断言值零变化）；白名单/漂移锚同包保名零改动。
- 新增：TestIncSourceWatermarkCapable / TestIncLogsRenderersQuoteFragments。
