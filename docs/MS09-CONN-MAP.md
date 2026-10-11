# MS-09 连接层泛化账册（MS09-CONN-MAP）

范围：`openPGTestConn` 七消费点 → `openSourceTestConn(sc)` 类型分派。**只动连接层**——水位/增量/建议/评估的功能开闸（能力位翻位）属 MS-10/11；本单完成态=这些面对 MySQL「连通测试通、功能仍 400」即正确形态。

## 消费点处置表

| # | 位置 | 面 | 处置 |
|---|---|---|---|
| 1 | server.go `testPGConnection`（/config/test-connection） | 连通测试 | source_ref 闸移除+按 ref 类型真分派（`refType` 参数线程化 `cfg.Type`）；inline 无 type 维持 PG-only 端点语义 |
| 2 | server.go `handleListTables`（/config/list-tables） | 向导表清单 | **守卫保留**（迁移源架构 PG-only，MySQL 不在任何已批 MS 范围；FE 已分派多源端点，放行=零用户价值休眠面）；底层换 `openSourceTestConn`+`cfg.Type=sc.Type` 前瞻化（MS-10/11 解守卫日分派即真） |
| 3 | ddl_export_handler.go `openDDLSource` | DDL 导出 | 守卫（CapDDLExport）不动；底层换装备翻面 |
| 4 | watermark_suggest.go | 水位建议 | 守卫（WatermarkDialect capability）不动；底层换装 |
| 5–6 | incremental.go 列清单（单表/批量） | 增量元数据 | 守卫不动；底层换装 |
| 7 | incremental.go `runIncrementalJob` | 增量执行引擎 | 原无守卫直开 pgx（创建/更新双闸后深防缺口）→ 换分派，历史 job/改挂 ref 不再出「连接源端失败」pgx 误导错 |

## 分派设计（dbconn.go）

`sourceConnSpec(sc)`：`SourceType()=="mysql"` → `("mysql", sc.DSNByType())`（**MS-08d `time_zone='+00:00'` UTC pin 随 DSN 继承**）；其余（postgres/空·legacy/未知）→ `("pgx", sc.DSN())` 逐字节恒等于旧 `openPGTestConn(sc.DSN())` 调用形。`openSourceTestConn`=open+ping 15s 同旧语义。能力闸与连接层解耦：闸在 handler（什么类型**能做什么**），分派在连接层（怎么**连**）——MS-10/11 翻面时零改守卫。

## 锚

- `TestSourceConnSpecDispatch`：三态分派+DSNByType 逐位等+pin 存在负锚（`%27%2B00%3A00%27`）+sslmode 不入 mysql DSN 负锚。
- `TestSourceConnDirectCallTripwire`：`openPGTestConn` webapi 非测试直呼白名单={dbconn.go}；`openMySQLTestConn` 白名单={dbconn.go, server.go（target 测试）, incremental.go（UTC 写会话）}——新源侧消费必走 `openSourceTestConn`，防绕分派丢 pin。
- `TestDatasources_TestConnectionSourceRef`（改判）：mysql ref→200/ok=false+mysql 驱动 dial 错（`10.0.0.5:3306`）+禁 pgx 签名（`failed to connect to ``）——分派真伪证明（只删闸不穿 type 即红）。

## A3 记账

笔① dbconn.go +1（routing 非 gating，#t79/DSNByType 先例同型）13；笔② server.go 闸移除 −1（only-decrease prune）→ **净 12**（config 2 / validator 1 / orchestrator 3 / cdc_config 2 / compare 1 / incremental 2 / server 3 / source_handler 2 / datasource 2 / dbconn 1）。

## 兄弟点记档（MS-10/11 翻面统一换装清单，勿静默）

- `cdc_precheck.go:71 pgDB()`：6 个 CDC prober 共用直开 pgx（CapCDC 闸后，今日 MySQL 不可达）。
- `server.go handleAssess` 引擎直开 pgx（CapAssess 闸后同上）。

随能力翻位一并 `openSourceTestConn` 化，届时配套 prober 的 MySQL 方言（WalLevel/Slot 等 PG 专有探针需 per-dialect 实现）另立清单票。

## 前端

零触及（FE 调用面 grep 实证在册：legacy `/config/test-connection` 仅 target 面+PG-only inline 分支；MySQL 双路均走多源端点）——无 FE 产物笔。

## 笔账

| 笔 | hash | 面 |
|---|---|---|
| ①芯 | （本笔）分派函数+七点换装+双 tripwire+A3+1 |
| ② | 闸移除+type 线程化+锚翻转+A3 −1 |
| ③ | 本 docs（doc-only，件哈希不动） |
| ④ | config 锚 bump（件哈希申报） |
