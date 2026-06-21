# timstool CIR→TiDB 执行引擎 — 架构设计（#t81）

> 状态：架构师设计完成，待 @刘源 确认 #t81 起止 + 灌数策略
> 上游：`docs/multi-source-architecture-design.md` §6（源无关 target）/ §7（orchestrator）
> 关联：#t79（路由，已交付 in_review）/ #t65（MySQL adapter，SchemaReader 就绪、DataReader 待确认）/ #t62
> 触发：#t79 交付后 dev+test grep 双实证 repo **无 CIR→TiDB 执行路径**（`migrator.go`/data migrator PG 直连 pgx+COPY→Lightning，不消费 CIR）——执行层须**新建**，非接线

---

## 1. 背景与 gap

#t79 打通了 source-type 端到端**路由** + list-tables 接 adapter（MySQL 能出表、test-connection 通、路由到非 PG 分支、路由日志可见）。但非 PG 分支**末端无执行引擎**——现有执行层 PG 直连（`sql.Open("pgx")+COPY→Lightning`），不消费 CIR。所以「开始迁移」对非 PG 仍 graceful 提示 #t81。

**#t81 建「源无关 target 执行引擎」**：消费 CIR → TiDB 建表 + 灌数 + 校验。这是真正跑通 MySQL→TiDB 全量迁移的核心 build，也是 §6/§7 设计的源无关 target 的落地、P1(#t64 2e) deferred 的「Source+CIR 流水线」。

## 2. 执行 pipeline（源无关）

```
source.Open(name, cfg)
  → SchemaReader.ReadSchema()  → CIR Schema  → target.ApplyDDL(CIR)        # 建表
  → DataReader.ReadTable()     → CIR RowIter  → target.LoadData(CIR rows)  # 灌数
  → validate(source, target, CIR)                                       # 校验
```

各 phase 只依赖 Source 接口 + CIR，**源无关**。PG 仍走原 COPY→Lightning 路径（#t79 dual-path，零回归）。

## 3. 关键决策：data 灌数策略 ⭐

| 方案 | 机制 | 优点 | 缺点 |
|------|------|------|------|
| **A. Lightning-from-CIR（推荐）** | CIR RowIterator → 序列化为 Lightning local backend 输入（CSV/SQL）→ SST import | 与 PG 路径**性能对齐**（生产级，大表/分库分表刚需）；复用现有 Lightning 导入逻辑 | 需 CIR→Lightning 输入格式适配；local backend 临时空间管理 |
| B. 批量 INSERT | CIR RowIterator → batch INSERT（事务/分批） | 实现最简 | 大表慢；与 PG 性能不对齐，生产不可用 |

**推荐 A（Lightning-from-CIR）**：PG 路径用 Lightning 是性能基线，非 PG 须对齐——否则生产 MySQL 迁移（国元 分库分表、大表）不可行。CIR RowIterator → Lightning CSV 序列化是**确定性**的（CIR Row 带列名 + TiDBType，按 TiDB 类型渲染值）。现有 Lightning 已消费 CSV（PG COPY 产 CSV），新增「CIR Row → CSV 行」适配即可。

> **决策点 @刘源**：确认 A。若想先用最简路径快速跑通 E2E 验证链路，可临时用 B（批量 INSERT）打通、再换 A 上性能；但**生产默认必须 A**。

## 4. CIR→DDL 建表（target.ApplyDDL）

- 遍历 CIR `Schema.Tables`，按 `Column.TiDBType` 渲染 `CREATE TABLE`：类型 / nullable / default / PK / index / auto_incr / comment。
- CIR `Column.TiDBType` 已由 TypeMapper 在 `ReadSchema` 时算好（§4 of multi-source-architecture），target **直接渲染、无需关心源类型**。
- **default 表达式**可能需源→TiDB 转换（MySQL `CURRENT_TIMESTAMP`→TiDB 同名；PG `now()`→`CURRENT_TIMESTAMP`）——在 TypeMapper 或 target 渲染层处理。
- `CREATE TABLE IF NOT EXISTS`（断点续传安全）。

## 5. 复用现有 lightning / validator（源无关化）

- `internal/lightning`（local backend SST 导入）已是 TiDB 侧；现输入是 PG COPY 产物。**新增 CIR→Lightning 输入适配**（CIR RowIterator → CSV/SQL 文件），导入逻辑复用。
- `internal/validator` 改为「经 Source 接口读源 + 读目标 → 对比 CIR」（§6），源无关。
- **不动 PG 路径**（dual-path：PG 仍 COPY→Lightning，零回归）。

## 6. orchestrator 非 PG 路径接入

- #t79 已加路由分支（非 PG→Source+CIR，当前 graceful「执行层构建中见 #t81」+ 路由日志 `source=mysql path=source-cir (pending #t81)`）。
- #t81 填该分支执行：按 §2 pipeline 跑 SchemaReader→ApplyDDL→DataReader→LoadData→validate。路由日志变为真执行。

## 7. 依赖与边界

- **依赖 #t65 adapter 完整**：SchemaReader 已就绪；**DataReader（读 MySQL→CIR RowIterator）须确认**——若未实现，是 #t81 前置（补 #t65 或并入 #t81 Step 0）。@开发 确认 DataReader 现状。
- **PG 零回归**硬约束（dual-path，PG 路径不动）。
- 分库分表汇聚（国元）：DataReader 多源 + 目标合并，复用 pg2tidb 分表思路（§5.2）；本期或后续。
- CIR 表达力：先覆盖 表/列/索引/PK；视图/触发器/存储过程按源逐步。

## 8. 验收（#t81）

1. `target.ApplyDDL(CIR)`：MySQL CIR schema → TiDB 建表（5 测试表结构对齐）
2. `target.LoadData`：CIR RowIterator → Lightning-from-CIR 灌数（行数对齐）
3. orchestrator 非 PG 路径**真执行**（路由日志 + 实际建表/灌数）
4. validator 源无关对比（结构/行数/类型映射/采样，FL-E2E-01）
5. MySQL→TiDB 全量迁移 E2E（t_users/t_orders/t_products/t_event_log/t_type_demo）
6. PG 零回归

## 9. 阶段建议（分步交付/验证）

- **Step 1**：`ApplyDDL`（CIR→TiDB 建表）——先验「MySQL schema 在 TiDB 建出来」。
- **Step 2**：`LoadData`（Lightning-from-CIR）——灌数、行数对齐。
- **Step 3**：validator 接入 + MySQL→TiDB 全量 E2E。
