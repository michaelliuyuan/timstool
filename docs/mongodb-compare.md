# MongoDB 数据对比（MongoDB Compare）

MongoDB↔MongoDB 一致性对比：对源端与目标端的数据库逐集合做存在性、索引集、文档数、分桶哈希对比，失配桶自动下钻到字段级差异明细。实现为独立任务类型，与迁移流水线互不影响。

- 引擎：`internal/mongocompare`（M2：canonical BSON 序列化 + 分桶哈希 + 差异下钻；checkpoint 带 `algoVersion` 语义版本戳）
- 任务接线：`internal/webapi/compare_handler.go`（M3：独立任务类型 + 端点，不经过 orchestrator）
- 前端：Web 界面「MongoDB 对比」入口（M4：创建向导 + 任务详情结果卡）

## 使用（Web）

导航「MongoDB 对比」→ 填写源端/目标端连接（表单字段由 `/api/v1/sources` 元数据驱动）与数据库名 → 可选高级参数 → 创建并启动。任务详情页实时显示已完成集合心跳，结束后展示总体结果（equal/diff/error/ttl_skip）、四象限计数与产物路径；终态（已完成/失败）提供「下载对比报告」（HTML 详细报告）与「下载 JSON」（summary.json 原样工件）双按钮（MC-14）。

## 使用（API）

```
POST /api/v1/mongocompare/tasks
{
  "source": {"host": "10.0.0.1", "port": 27017, "user": "u", "password": "p", "auth_db": "admin", "database": "orders"},
  "target": {"host": "10.0.0.2", "port": 27017, "database": "orders"},
  "buckets": 64, "parallel_colls": 4, "parallel_buckets": 4, "batch_size": 1000,
  "levels": ["schema", "index", "count", "hash", "diff"]
}

POST /api/v1/tasks/{id}/start          # 全新对比（清空该任务的对比产物目录）
POST /api/v1/tasks/{id}/start  {"resume": true}   # 从引擎 checkpoint 断点续跑
POST /api/v1/tasks/{id}/pause          # 暂停（对比中断点续跑入口）
POST /api/v1/tasks/{id}/resume         # 恢复（等同 resume=true）
POST /api/v1/tasks/{id}/cancel         # 取消（再启动 = 全新对比）

GET  /api/v1/tasks/{id}/report                  # MC-14：HTML 对比报告（附件下载）
GET  /api/v1/tasks/{id}/report?format=json      # MC-14：summary.json 原样字节（附件下载）
```

- 每端可给 `uri`（完整连接串，转义由调用方负责）**或**离散字段 `host/port/user/password/auth_db`（服务端结构化构建凭证，口令含 `@ : / %` 等特殊字符安全）；两者同时给出时离散字段优先。
- `target.database` 省略时默认与源端同名；v1 语义为**单库对单库**（两端库名可不同）。
- 凭证存储与迁移任务同面：ConfigJSON 落库、API 序列化 `json:"-"` 不回显。
- **鉴权（MC-10）**：写操作端点在令牌门后——`POST /mongocompare/tasks`（创建）与 compare 类任务的 `start/pause/resume/cancel/delete`（按 ConfigJSON 的 kind 判别后插门，任何状态写之前）。语义与 CDC 门逐字一致：请求头 `X-Auth-Token` 对 **config.yaml `security.token` 的启动时快照**做恒时比较；服务端未配 token = 403 fail-closed（全部拒绝），token 缺失/错误 = 401（前端弹一次令牌框后重试）。读端点（GET list/get/report/progress/logs）沿 CDC 先例保持开放。同一路由上的**迁移类任务不受门**（回归锚）。审计：compare 写操作逐条落 `<dataDir>/audit/mc-ops.jsonl`（action 前缀 `mongocompare.create/start/pause/resume/cancel/delete`，含 denied 尝试；内存环与 CDC 共享，写失败降级语义同 CDC）；CDC 审计文件 `cdc-ops.jsonl` 不受影响。

## 结果与产物

`result_json`：`{kind, overall, counts{equal,diff,error,ttl_skip}, duration_sec, summary_path, diff_dir}`。

- `summary.json`：逐集合结果（含每桶 src/dst 计数与哈希、索引/计数检查、ttl 跳过标记）
- `diff/<db>__<coll>.ndjson`：字段级差异明细（modified/missing-in-target/missing-in-source + 叶子路径）
- `.checkpoints/*.json`：桶级 checkpoint（含边界 canonical 串与 `algo_version`）

产物目录：`<dataDir>/compare/<taskID>/`。下载走任务报告端点（MC-14）：`GET /tasks/{id}/report` 按 ConfigJSON kind 分流——compare 任务渲染 HTML 详细报告（横幅任务/两端库映射/耗时 + 四态汇总 + 逐集合五级明细表 + 失配桶下钻，失配桶每集合最多列 20 个；样式沿 reporter.ReportCSS 报告族；动态字段全量 HTML 转义，凭证与 URI 串永不渲染）；`format=json` 原样回传 `summary.json` 字节（attachment `compare-summary-<id>.json`）。summary.json 缺失（未跑完/连接期失败/重跑清空）= 404 并指明原因。迁移任务的 `/report` 行为零变化（回归锚）；该端点为只读，沿 MC-10 读开放口径不加令牌门。

## 语义与边界

- **resume 前提**：checkpoint 复用假设两次运行间数据未变。桶边界 canonical 串与重算计划不一致时该桶自动重算（数据漂移防护）；同一边界内的数据变更会被正常检出为 diff。取消/失败后重新 start 默认全新对比（产物目录清空）；断点续跑显式走 `start {"resume":true}` 或 pause→resume。
- **ttl_skip**：含 TTL 索引的集合跳过数据层对比（仅比较索引层）——TTL 后台删除会让哈希永不相等，js 参考实现同语义。**TTL + 索引差异语义（MC-9）**：ttl_skip 只能从 equal 降级而来；若索引层已检出差异，该集合判 **diff**（不被 TTL 跳过掩盖），`checks.skipped="ttl"` 标记仍保留（说明数据层为何缺席）。仅目标端有 TTL 索引时，该索引本身即索引差异（missing-in-source），且源端无 TTL 故数据层照常执行。
- **视图过滤（MC-9）**：对比按 `listCollections` 的 type 过滤，仅 `type="collection"` 参与对比；视图（view）不参与（两边视图定义/数据口径不可比）。**空 type 视为 collection**（旧服务端/驱动不回报 type 时的 M1 守卫，绝不静默丢集合）。被跳过的视图记入 `summary.json` 的 `stats.skipped_views` 名单（`db.view` 形式），不进入 counts 四态计数。
- **参数校验边界（MC-9）**：`buckets` 1..4096（缺省 64）、`parallel_colls` 1..32（缺省 4）、`parallel_buckets` 1..32（缺省 4）、`batch_size` 100..100000（缺省 1000）；`0` 表示未设置走缺省，非 0 越界一律 400 拒绝。`levels` 含 `diff` 但不含 `hash` 显式拒绝（400）：diff 只对哈希失配桶下钻，无 hash 层时 diff 永不触发，静默接受会伪装成"已对比"。
- **双向存在性（MC-10）**：schema 级开启时存在性检查是**双向**的——源端有、目标端无 → `missing-in-target`（既有语义）；**目标端有、源端无的集合** → 合成结果行 `{overall: diff, checks.schema: {"diff": ["missing-in-source"]}}`（迁移校验场景目标端多出集合同样是问题）。该行走 counts 四态的 `diff`（计数与整体 overall 自然翻 diff，四态形状不变），`db` 字段为目标端库名（集合实际所在侧）。对称过滤：`system.*` 与视图（view）在两侧同样排除——目标端多出的 **view 不报告**（与源端侧视图过滤对称；视图不参与对比）；schema 级关闭时不枚举目标端，双向检查整体不生效。
- **dst 同名视图修正（MC-10）**：目标端集合存在性只认 `type="collection"` 的条目——目标端**同名 view 不再能蒙混过存在性检查**（此前目标端只有同名 view 时源集合会被误判为存在）。
- **系统命名空间**：`system.*` 集合与 admin/config/local 库跳过。层次说明：MongoDB 的系统对象过滤在**对比引擎层**完成，MySQL/PG 的系统库过滤在**源适配层**完成——层次不同，用户可见行为一致（系统对象不参与对比）。
- **三态 + ttl_skip**：overall ∈ {equal, diff, error, ttl_skip}；任一集合 diff 即整体 diff，无 diff 但有集合 error 即整体 error。
- **存量任务参数复验（MC-10）**：`start`/`resume` 在写 Running 状态**之前**对存量任务存储的参数复跑 MC-9 边界校验——边界收紧前创建的老任务若带越界参数（如 `buckets=4097`），启动/恢复显式拒绝（400 带边界文案，任务状态零突变），**不做静默钳制**（fail-loud：让调用方看见并重建任务，避免带着越界参数跑出误导性结果）。

## 限制与声明（记录性）

1. **大字段摘要/截断未实现**（canonical-bson-spec §12）：参考实现同样未做；超大文档全量参与哈希。
2. **undefined vs null**：按 canonical-bson-spec v1 修订移除（现代驱动不产 undefined）。
3. **跨实现字节一致为 non-goal**：canonical 串的跨语言（Go/js）字节一致不承诺；一致性以「同型同值 ⇔ 同串」的注入性与 Go 版本内稳定性为准。
4. **v1 单库对单库**：全实例对比与库表映射留待后续版本。
5. **版本支持矩阵（M5 双驱动自适应）**：连接层按 `isMaster` 探测的 maxWireVersion **逐连接**自动选路——wire ≥ 9（MongoDB 4.4+）走 mongo-driver v2 原生路径；wire < 9 走 v1 桥（内部 `internal/mongoconn`，v1 仅作字节管道，全部解码统一 v2 bson，canonical 语义双路径逐字节一致，有桥接等价测试锚定）。桥的官方支持区间为 **MongoDB 3.6+（wire 6+，驱动 v1.12）**；wire 4–5（3.2/3.4）为 **best-effort**——通过放宽 v1 驱动下限实现，全管线（元数据/计数/哈希/钻取）已在真实 3.2.4 实例上实测通过，但超出驱动官方矩阵，以实测为准。两侧都连不上时报 `ProbeError`（含地址与双驱动错误，不静默选错路径）。

## 运维备忘

- 共享冒烟容器上，崩溃的活库测试进程可能遗留 `md_*_<pid>x<seq>` 后缀的孤儿夹具库（约 15MB/个）——harness 清扫刻意不碰带后缀命名（防误杀并发邻居），需**手动** drop。
