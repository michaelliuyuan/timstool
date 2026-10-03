# MS-07 · DDL export 面源方言化（守卫吸收账）

> 基线 72090b2（V3.16，件 F0FBCB6D）。单 commit 链：`b8e2369`（①守卫+锚+A3）→ docs → config 锚。
> 裁定：leader 清单票 seq373 + v1.1（seq375，positive ② 文案前瞻记档采纳）；红队预核 seq376。

## 吸收点

| 位置 | legacy | 新 | 形态 |
| --- | --- | --- | --- |
| internal/webapi/ddl_export_handler.go:84（applySourceRef，两端点 /ddl-export/schemas 与 /ddl-export 共享） | `if e.Type != "postgres"` | `if !srcCapable(e.Type, source.CapDDLExport)` | **C1 shape 1：原始值直入**（e.Type 为 datasource 实体原始存储值，非 SourceType() 归一） |

恒等论证（与 legacy `!= "postgres"` 全等，无反转）：

| e.Type | srcCapable(·, CapDDLExport) | legacy | 结果 |
| --- | --- | --- | --- |
| `""`（空） | false（helper 显式拒） | `!= "postgres"` → 拒 | 400 ✅ |
| `postgres` | true（pg DDLExport=true，postgres/source.go:41） | 放 | 放行 ✅ |
| `mysql` / `tidb` | false（位未置） | 拒 | 400 ✅ |
| unknown（`oracle` 等） | false（未知 kind 解析 false） | 拒 | 400 ✅ |

400 文案「source_ref: DDL 导出仅支持 PostgreSQL 数据源」字面保留不动。

## C1 空态对照（DDLExport 属 shape 1）

| 调用形态 | 空值行为 | 本单 |
| --- | --- | --- |
| 原始值直入 `srcCapable(e.Type, CapDDLExport)` | 空 → 拒（400） | **本单采用**（同 MS-06 :2333 assess） |
| SourceType() 归一后入 `srcCapable(cfg.Source.SourceType(), CapCDC)` | 空 → 默认 postgres → 放行 | 不适用（e.Type 无归一包装） |

锚：`TestSrcCapable`（incremental_test.go）增 CapDDLExport 四态——空/unknown 拒、mysql/tidb false、postgres true。

## A3 账（typeBranchFixture）

- `ddl_export_handler.go: 1` 行+注释行删除 → **总账 12→11**；MS-12 目标清零不变。
- 不在范围：openDDLSource:65 openPGTestConn 连接层（MS-09）；:28 ddlExportRequest.TiDB 导出器选项与 :29 注记非源类型分支；两端点路由不动。

## 文案前瞻记档（v1.1 ②，positive 提出/leader 采纳）

400 文案「仅支持 PostgreSQL 数据源」为 legacy 字面保留；**MS-09+ 若有 kind 增 DDLExport 位需同步文案**（同 openPGTestConn 连接层 MS-09 并列记档）。

## 分笔哈希账

| commit | 内容 | 件哈希 |
| --- | --- | --- |
| b8e2369 | ①守卫+四态锚+A3 12→11 | 见 config 锚笔实测 |
| （docs） | ②本文件 | doc-only，件哈希不动（铁律） |
| （config） | ③ scripts/pg-gate.config.json 锚 bump | 逐笔实测贴 |
