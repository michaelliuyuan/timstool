# timstool 多源 Web 连接表单 — 源切换联动设计（§8 补充）

> 状态：架构师设计完成，待 @开发工程师 实现 / @测试工程师 验证
> 上游文档：`docs/multi-source-architecture-design.md`（§8「Web：配置向导加数据源类型选择→对应连接表单」的展开）
> 关联线程：#pg-tidb:871e8ac9　父任务：#t62
> 触发：@测试工程师 2026-06-21 报告「源选择器可用，切换后表单不联动（已知状态，等架构师重新设计）」，235 已回退 `6f403a9` 干净基线、17/17 绿

---

## 1. 问题与根因

现状：配置向导 Step 1 加了「数据源类型」下拉（postgres/mysql/oracle/mssql/db2），选择器本身可用；但下方连接表单仍是 **PG 专用硬编码表单**（`ConnectionForm.vue` 写死 Host/Port/User/Password/Database/Schema/SSL Mode）。

切源后表单不联动 = 表单字段不从「当前所选源」派生，导致两个具体危害：
1. **体验**：选 MySQL 仍显示 PG 的 `Schema`/`SSL Mode`，字段与源不符。
2. **正确性（更严重）**：切源不清旧值 → PG 的 `sslmode`/`schema` 等**源专属字段被静默带进 MySQL 的 SourceConfig**，污染连接配置、可能进 DSN。

根因：表单是**命令式硬编码**，而非**声明式 schema 驱动**；且前端没有「每源需要哪些字段」的真源。

## 2. 设计目标

| # | 目标 |
|---|------|
| G1 | 切换数据源类型 → 表单字段**自动重渲染**为该源的连接字段 |
| G2 | 切源时**保留通用字段**（host/user/password/database）、**清除源专属字段**（schema/sslmode/charset/…），杜绝旧值污染 |
| G3 | **单一真源**：字段定义来自后端 Source 注册表，新增 adapter（未来 Oracle）表单自动适配，前端零改 |
| G4 | **stub 源**（Oracle/MSSQL/DB2）在表单中以禁用态呈现并提示「暂未实现」，与「保留接口 stub」决策一致 |
| G5 | 与 **dual-path** 衔接：表单提交的 `source` 即驱动 orchestrator 双路径路由（postgres→现有 COPY→Lightning；非 PG→Source+CIR），无需额外接线 |
| G6 | 向后兼容：现有 PG 表单行为不回归（PG 仍是默认源） |

## 3. 总体方案

**schema 驱动 + 后端单一真源**：
- 每个 adapter（含 stub）向后端注册一份**连接表单元数据** `SourceMeta`（字段清单 + 能力 + 默认端口）。
- 后端 `GET /api/v1/sources` 暴露所有源的 `SourceMeta`，**不要求打开连接**（stub 也能描述自己）。
- 前端 `ConnectionForm.vue` 改为**从 `SourceMeta.fields` 动态渲染**；源切换 = 换一份 meta + 按规则 reconcile 表单模型。

```
Source 注册表 (Go)                         前端
┌──────────────────────┐   GET /api/v1/sources    ┌─────────────────────┐
│ postgres  SourceMeta │ ───────────────────────▶ │ useSourceSchema()   │
│ mysql     SourceMeta │                          │  (缓存 meta)         │
│ oracle    SourceMeta(stub)│                     │        │            │
│ mssql     SourceMeta(stub)│                     │        ▼            │
│ db2       SourceMeta(stub)│                     │ ConnectionForm.vue  │
└──────────────────────┘   POST test-connection   │  (按 fields 渲染)    │
        ▲                  ◀──────────────────────│  model = {field:val} │
        │                  {source, fields{...}}   └─────────────────────┘
   source.Open(cfg)+Ping
```

## 4. Source 元数据契约（Go）

新增 `internal/source/describe.go`，**不依赖 Connect**（stub 可用）：

```go
package source

// FieldSpec 描述连接表单的一个字段（前端渲染 + 后端校验共用）。
type FieldSpec struct {
    Key         string   `json:"key"`                    // "host"|"port"|"user"|"password"|"database"|"schema"|"sslmode"|...
    Label       string   `json:"label"`                  // 中文标签 "主机"
    Type        string   `json:"type"`                   // text|number|password|select|switch
    Required    bool     `json:"required"`
    Default     any      `json:"default,omitempty"`      // 端口/charset 等默认值
    Placeholder string   `json:"placeholder,omitempty"`
    Options     []Option `json:"options,omitempty"`      // type=select 时的候选项
    Help        string   `json:"help,omitempty"`
    Group       string   `json:"group"`                  // "common"|"source"|"advanced"（分组渲染）
}

type Option struct{ Label string `json:"label"`; Value string `json:"value"` }

type Capabilities struct {
    Schema bool `json:"schema"` // 能读 schema
    Data   bool `json:"data"`   // 能全量读数据
    CDC    bool `json:"cdc"`    // 能增量（PG=true，MySQL 本期 stub=false）
}

// SourceMeta 是一个源的完整连接描述，用于驱动表单 + 选择器。
type SourceMeta struct {
    Name         string       `json:"name"`                   // "postgres"
    DisplayName  string       `json:"displayName"`            // "PostgreSQL"
    Implemented  bool         `json:"implemented"`            // stub=false
    DefaultPort  int          `json:"defaultPort"`            // 5432
    Fields       []FieldSpec  `json:"fields"`
    Capabilities Capabilities `json:"capabilities"`
    NotImplMsg   string       `json:"notImplMsg,omitempty"`   // stub 提示语
}

// Describe 不打开连接，可描述 stub。
func Describe(name string) (SourceMeta, error)
func DescribeAll() []SourceMeta
```

注册方式（与现有 `Register` 并列）：每个 adapter 在 `init()` 里调 `RegisterMeta(name, meta)`。stub adapter 即使 `Open` 返回「未实现」，也注册完整 `meta`（Implemented=false），从而能在选择器/表单里展示。

## 5. 各源字段定义

> 字段按 `group` 分三组渲染：`common`（通用，所有源都有）→ `source`（源专属）→ `advanced`（高级，默认折叠）。

| 源 | Implemented | DefaultPort | common | source 专属 | advanced |
|----|-------------|-------------|--------|-------------|----------|
| **postgres** | ✅ true | 5432 | host / port / user / password / database | `schema`(默认 public) / `sslmode`(select: disable·prefer·require·verify-ca·verify-full) | `connect_timeout` |
| **mysql** | ✅ true | 3306 | host / port / user / password / database | `charset`(默认 utf8mb4) | `parseTime`(switch,默认 true) / `collation` / `tls`(switch，后续 advanced) |
| **oracle** | ❌ stub | 1521 | host / port / user / password | `serviceName` / `sid`(二选一) | — |
| **mssql** | ❌ stub | 1433 | host / port / user / password / database | `instance` | `encrypt`(switch) |
| **db2** | ❌ stub | 50000 | host / port / user / password / database | — | — |

要点：
- **MySQL 无 PG 意义的 `schema`**（MySQL schema==database），故 MySQL 字段不含 `schema`——这正是切源必须清旧值的原因。
- 端口默认随源变：切到 MySQL 默认填 3306，切到 PG 5432。
- stub 源字段照常列出（便于将来实现时直接复用），但 `Implemented=false` 触发禁用态。

## 6. 后端 API

### 6.1 新增：列出所有源（驱动选择器 + 表单）
```
GET /api/v1/sources
→ 200 { "sources": [ SourceMeta, ... ] }   // DescribeAll()
```
前端启动时拉一次并缓存；选择器与表单共用此缓存。

### 6.2 扩展：测试连接（接收 source + 字段 map）
现有 `POST /api/v1/test-connection` 是 PG 硬编码字段。改为：
```
POST /api/v1/test-connection
Request: { "source": "mysql", "fields": { "host": "...", "port": 3306, ... } }
Response 200: { "success": true, "message": "连接成功", "version": "MySQL 8.0.x", "schemas": [...], "tables_count": N }
Response 400: { "success": false, "message": "..." }
```
后端：`fields` → 构造 `SourceConfig` → `source.Open(name, cfg)` → `Ping()` → 关闭。stub 源直接返「该源暂未实现」友好错误（不真连）。

> 不破坏现有契约：`source` 缺省时按 `postgres` 处理（向后兼容旧前端调用 / CLI 默认）。

## 7. 前端联动机制

### 7.1 `ConnectionForm.vue` 改为 schema 驱动
- Props：`meta: SourceMeta`（所选源的元数据）、`modelValue: Record<string,any>`（连接字段值）、`disabled?: boolean`。
- 渲染：按 `meta.fields` 循环，按 `group` 分块；每个字段按 `type` 选控件（text/number/password/select/switch）。**不再写死任何 PG 字段。**
- stub（`meta.implemented===false`）：整体禁用 + 顶部 banner「该源暂未实现，敬请期待」，`测试连接`/后续`开始迁移`禁用。

### 7.2 切源 reconcile 规则（核心，修「不联动 + 旧值污染」）
选择器变化时（在向导页或 `useSourceSchema` composable 里 watch）：

1. 取新源 `newMeta`，旧表单模型 `oldModel`。
2. **保留通用字段**：`host / user / password / database`（这些 key 在新旧 `fields` 都存在）原样保留。
3. **端口 reconcile**：若 `oldModel.port` 等于旧源默认端口或为空 → 设为新源 `defaultPort`；否则（用户已手填非默认端口）保留用户值。
4. **清除源专属字段**：凡不在 `newMeta.fields` 的 key（如 PG→MySQL 时的 `schema`/`sslmode`）**从 model 删除**，绝不静默带入。
5. **补默认值**：新源 `fields` 中有 `Default` 且 model 缺该 key 的，填入默认（如 MySQL `charset=utf8mb4`）。
6. **重置连接测试态**：清掉上一次「连接成功 ✅」，因对旧源有效对新源无效。
7. 触发重渲染（由 `meta` 变更自然驱动）。

> 规则用纯函数 `reconcileModel(oldModel, oldMeta, newMeta)` 表达，便于单测覆盖（见 §10）。

### 7.3 `useSourceSchema` composable
- 启动 `GET /api/v1/sources` 一次，缓存 `Map<name, SourceMeta>` + 源列表（给选择器）。
- 暴露 `getSource(name)`、`listSources()`、`reconcileModel(...)`。

### 7.4 向导 Step 1 新结构
```
Step 1: 源端
  数据源类型: [ PostgreSQL ▾ ]          ← 选择器（来自 /sources；stub 灰显 + 角标「未实现」）
  ┌─ 动态连接表单（schema 驱动）─────────────┐
  │ 通用：host / port / user / password / database │
  │ 源专属：schema / sslmode …（随源变）            │
  │ 高级：▸（折叠）                                 │
  └────────────────────────────────────┘  [测试连接]
```

## 8. 与 dual-path / orchestrator 的衔接

- 表单提交的配置携带 `source` 字段 → 写入任务 `config.source` → orchestrator 双路径路由（P1 #t64 已就位）：
  - `postgres` → 现有 COPY→Lightning 流水线（零回归、保性能）
  - 非 PG（mysql 等）→ Source+CIR 流水线（#t65 填）
- 表单**无需感知 dual-path**：它只负责产出合法 `source + fields`，路径选择是 orchestrator 的事。这正是解耦带来的好处。

## 9. 向后兼容 / 回归护城

- `GET /sources` 新增端点，不动现有 API。
- `test-connection` 扩展为兼容旧 PG 调用（`source` 缺省=postgres）。
- 默认源仍 postgres，默认表单与旧 PG 表单字段一致（host/port/user/password/database/schema/sslmode）→ **PG 用户观感零变化**。
- 现有 PG Web 测试用例必须继续绿（#t66 回归项）。

## 10. 验收（给 @测试工程师 #t66）

1. 选 postgres：表单含 schema/sslmode，默认端口 5432；与旧版一致（回归）。
2. postgres → mysql：schema/sslmode **消失**，出现 charset，端口默认变 3306；host/user/password/database 保留。（`tls` 为后续 advanced 字段，**不在本期 §10 验收**）
3. 切换后 `test-connection` 请求体**不含**被清除的旧 key（抓包/日志验证无 sslmode 污染）。
4. mysql 连接实测可通（依赖 #t65 MySQL adapter；adapter 未就绪前可用 mock 校验字段裁剪）。
5. 选 oracle/mssql/db2（stub）：表单禁用 + 「暂未实现」提示，测试连接/开始迁移禁用。
6. 端口手填非默认值后切源：用户值保留（不被默认覆盖）。
7. `reconcileModel` 单测：覆盖 common 保留 / 专属清除 / 端口默认替换 / 默认值补齐 / stub 禁用。

## 11. 边界与风险

| 项 | 说明 |
|----|------|
| stub 字段前瞻性 | Oracle/MSSQL/DB2 字段是预填，将来实现 adapter 时若真实 DSN 需求不同，以实现时为准调整 meta（成本低，仅改注册） |
| 与 #t65 的依赖 | 表单 schema 化**不依赖** MySQL adapter 完成（字段裁剪/联动可先用 PG↔stub 验证）；真连 MySQL 需 #t65。两者可并行 |
| 字段 i18n | Label 中文优先；如需多语言后续在 meta 加 `labelI18n`，不影响结构 |
| 敏感字段 | password 用 `type=password`；不在前端日志/URL 明文打印 |
| tls 字段范围 | MySQL `tls` 列为**后续 advanced** 字段（安全连接），本期不实现/不验收；schema 驱动使将来新增零成本（仅改 meta 注册 + DSN），到时再补用例 |
