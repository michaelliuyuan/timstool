# MS-06 · server/assess 面源方言化 · 映射表

基线 2f21a5a（V3.15 产线）。本单定性=**纯守卫吸收单（D4 模式扩展）**：assess 流无独立方言位（assess.NewScanner 已在 internal/assess 独立包内聚，类比 validator CompareDialect 同源不同包禁统一）；handler 层方言位仅 DSN+连接两行=连接层，记 MS-09。评审按表机械核对。

## 守卫吸收账（server.go）

| 基线位 | 终态位 | 原 | 现 | 调用形态 |
|---|---|---|---|---|
| :2333（assess source_ref 守卫） | :2337 | `e.Type != "postgres"` | `!srcCapable(e.Type, source.CapAssess)` | **原始值直入**（e.Type 原样；空→helper 拒→400 同文案） |
| :1264（cdc_chain 守卫，M4 引入） | :1265 | `cfg.Source.SourceType() != "postgres"` | `!srcCapable(cfg.Source.SourceType(), source.CapCDC)` | **归一后入**（SourceType() 将空默认为 postgres→CapCDC true→放行） |

两处 400 文案原串不动（「兼容评估仅支持 PostgreSQL 数据源」/「cdc_chain 仅支持 PostgreSQL 源端」）。cdc_chain 守卫归属：M4 引入→**MS-06 吸收**（本表注记）。

## C1 空态对照表（两形态×五态；ruling seq 347 加钉）

| 输入 kind | 形态1 :2337（原始值直入） | 形态2 :1265（SourceType() 归一后入） |
|---|---|---|
| `""`（空） | **拒绝→400**（helper 空显式拒） | **放行**（归一为 postgres→CapCDC=true；原行为=SourceType()!=""postgres" 判定放行） |
| 未知（如 oracle） | 拒绝→400（err→false） | 拒绝→400（同） |
| mysql | 拒绝（CapAssess=false） | 拒绝（CapCDC=false） |
| tidb | 拒绝（CapAssess=false） | 拒绝（CapCDC=false） |
| postgres | 放行 | 放行 |

**两空态相反=原行为忠实保留**（基线 :2333 空 e.Type→"">"postgres" 比较=拒；:1264 空→SourceType()="postgres"=放行）。锚 TestSrcCapable 十态钉死（含 `SourceConfig{}.SourceType()=="postgres"` 前置钉）。

## helper 泛化账

| 基线 | 终态 |
|---|---|
| incSourceWatermarkCapable（MS-04，独立实现） | wm_dialect.go:65 薄委托 `return srcCapable(kind, source.CapWatermark)`（保名，既有锚零改动） |
| —（新） | wm_dialect.go:51 `srcCapable(kind string, cap source.Capability) bool`（空显式拒+NormalizeKind 前置拒空+err→false；C1 两形态注记在注释） |

## A3 账

- server.go fixture **6→4**（删 :2333/:1264 两吸收位）；总账 **14→12**。
- 保留 4 条：:478/:737 legacy PG 端点（**A 案裁定字面保留**——文案自白「mysql 请用 /test-connection」=端点路由约束非源能力；MS-09 端点翻新翻转点）+ :822/:832 tidb 侧（翻转点保留）。
- TestTypeBranchFrozenBaseline PASS 亲证。

## 零触碰/记档清单

- :478/:737：A 案保留+记档（MS-09 legacy 端点翻新单处理）。
- assess handler 连接层（BuildPGDSN+sql.Open pgx :2356-2362）：与 openPGTestConn 同类=webapi 连接层，**记 MS-09**。
- assess.Scanner（internal/assess 包）：独立包内聚，**不抽 AssessDialect**（强抽=空桩反模式，禁空桩先例）。
- A1 能力矩阵（postgres 7-true/mysql scoped/tidb all-false）零改动。

## 锚测账（实数）

- incremental_test.go：24→**25**（+TestSrcCapable）；TestIncSourceWatermarkCapable 保名零改动。

## 分笔账

| commit | 件哈希（worktree 实测 linux-amd64 -trimpath -s -w） |
|---|---|
| 基线 2f21a5a | 552FE34EF92EA546C7A7DDE5BEBBD247EC40A84C5C7802C816CEC93F1AECE7AA / 23,965,858B |
| ① e61b570（srcCapable+两守卫+A3+锚） | F0FBCB6D406CEEF16740ECFFC3AC36066BC1099BB5231BC085861E2E38D4A203 / 23,965,858B |
| ② 本笔（doc-only） | 不变（铁律） |
