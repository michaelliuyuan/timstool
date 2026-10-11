# MS-05 · 水位建议 watermark_suggest 方言化 · 映射表

基线 d306c9e（V3.14 产线）。suggest 流源侧方言化：三件保名保位迁 wm_dialect.go+handler 路由+D4 守卫吸收。纯结构重构零行为变更。评审按表机械核对（行号=本单终态树）。

## 源侧迁移账（watermark_suggest.go → wm_dialect.go）

| 原位（基线 d306c9e） | 内容 | 新位（本单终态） | 口径 |
|---|---|---|---|
| watermark_suggest.go:44-47 | wmSystemSchemas 三库目录 | wm_dialect.go:125-129 | 保名保位；白名单头注（方言方法+白盒锚）；消费经 SystemSchemas() |
| watermark_suggest.go:49-53 | wmDefaultNowRe 正则 | wm_dialect.go:136（含原 RE2 前置守卫注释） | 保名保位；唯一合法消费=DefaultNowMatch+锚（禁双正则，ruling seq 310） |
| watermark_suggest.go:63-98 | wmCatalogSQL+queryWMCatalog | wm_dialect.go:146-165（const）+:254-276（QuerySuggestCatalog 方法） | SQL 逐字节恒等（脚本比对全等）；扫描体逐字唯一差异=DefaultNow 判定改 d.DefaultNowMatch(def)（单源裁定） |
| （新） | — | wm_dialect.go:247-249 SystemSchemas / :277-279 DefaultNowMatch | 接口 7→10（ ruling seq 310 定稿） |

## 消费面路由账（watermark_suggest.go 终态）

| 位 | 原 | 现 |
|---|---|---|
| :270（基线 :323） | `e.Type != "postgres"` | `!incSourceWatermarkCapable(e.Type)`（D4 吸收，ruling seq 82；空/未知→400 同文案） |
| :277（基线 :329） | `wmSystemSchemas[sc.Schema]` | `incSourceDialect.SystemSchemas()[sc.Schema]` |
| :291（基线 :343） | `queryWMCatalog(ctx, db, sc.Schema)` | `incSourceDialect.QuerySuggestCatalog(ctx, db, sc.Schema)`（薄委托已删） |

主流程/handler 对 wmSystemSchemas、wmDefaultNowRe、wmCatalogSQL 直读=归零（唯一残留 :44 迁移注记，注释非代码）。

## 双轨一致性（红队补点-B / leader 定稿）

- 内联双轨：wmSystemSchemas map ↔ wmCatalogSQL :159 内联 `NOT IN ('pg_catalog','information_schema','pg_toast')`。
- 锚 TestWMSuggestDialectDualTrack 双向钉死：正向=map 键渲染引号列表 Contains；反向=解析 NOT IN 字面集与键集相等（count+membership，防半漂移）。

## D4 吸收记录

- :270 守卫吸收后 watermark_suggest.go type 分支归零：A3 fixture `internal/source/capability_test.go` 条目 **1→0 删除**（红队补点-A）；TestTypeBranchFrozenBaseline PASS 亲证。fixture 现余 14 行（MS-12 空断言终点不变）。

## 零触碰清单

- scorer 六函数：wmNameNorm / wmStrongNames / wmCreatedNames / wmNameClass / wmTypeWeight / scoreWatermarkCandidates——零改动零改语义（行为锚保名）。
- wmCatalogColumn 结构：留 watermark_suggest.go（scorer 消费，同包零迁移成本）。
- openPGTestConn（dbconn.go:20）：webapi 全局连接器，七消费点（server×2 :516/:763，incremental×3 :705/:786/:876，ddl_export :65，suggest :333）——**非 suggest 专属，本单不动**；连接层接口化记 MS-09。

## 锚测账（实数）

- suggest 锚：基线 watermark_suggest_test.go 14 个 Test 函数+新增 TestWMSuggestDialectDualTrack=**15**（含 wmDefaultNowRe 直读锚保名零改动、wmCatalogSQL 结构锚保名引用）。
- A3 fixture：15→14 行（watermark_suggest.go 条目删）。
- MS-12 空断言终点不变。

## 分笔账

| commit | 件哈希（worktree 实测 linux-amd64 -trimpath -s -w） |
|---|---|
| 基线 d306c9e | 87DD95ACF0EC33F36016E149643EA2328F9C6B3D98865CC88576C9D00D32ECA1 / 23,965,858B |
| ① ab5b83d（接口+实现+迁移+双轨锚） | A8FD41AE5A5DC6358F7CFC35D27EEAE61ABC55CE16A3E8D15B58B0FB2B01AEF0 / 23,965,858B |
| ② 7780cb1（路由+D4+A3 剪枝） | 552FE34EF92EA546C7A7DDE5BEBBD247EC40A84C5C7802C816CEC93F1AECE7AA / 23,965,858B |
| ③ 本笔（doc-only） | 不变（铁律：doc-only 不进构建） |
