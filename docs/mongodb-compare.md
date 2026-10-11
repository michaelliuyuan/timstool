# MongoDB 数据对比（MongoDB Compare）

MongoDB↔MongoDB 一致性对比：对源端与目标端的数据库逐集合做存在性、索引集、文档数、分桶哈希对比，失配桶自动下钻到字段级差异明细。实现为独立任务类型，与迁移流水线互不影响。

- 引擎：`internal/mongocompare`（M2：canonical BSON 序列化 + 分桶哈希 + 差异下钻；checkpoint 带 `algoVersion` 语义版本戳）
- 任务接线：`internal/webapi/compare_handler.go`（M3：独立任务类型 + 端点，不经过 orchestrator）
- 前端：Web 界面「MongoDB 对比」入口（M4：创建向导 + 任务详情结果卡）

## 使用（Web）

导航「MongoDB 对比」→ 填写源端/目标端连接（表单字段由 `/api/v1/sources` 元数据驱动）与数据库名 → 可选高级参数 → 创建并启动。任务详情页实时显示已完成集合心跳，结束后展示总体结果（equal/diff/error/ttl_skip）、四象限计数与产物路径。

## 使用（API）

```
POST /api/v1/compare/tasks
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
```

- 每端可给 `uri`（完整连接串，转义由调用方负责）**或**离散字段 `host/port/user/password/auth_db`（服务端结构化构建凭证，口令含 `@ : / %` 等特殊字符安全）；两者同时给出时离散字段优先。
- `target.database` 省略时默认与源端同名；v1 语义为**单库对单库**（两端库名可不同）。
- 凭证存储与迁移任务同面：ConfigJSON 落库、API 序列化 `json:"-"` 不回显。

## 结果与产物

`result_json`：`{kind, overall, counts{equal,diff,error,ttl_skip}, duration_sec, summary_path, diff_dir}`。

- `summary.json`：逐集合结果（含每桶 src/dst 计数与哈希、索引/计数检查、ttl 跳过标记）
- `diff/<db>__<coll>.ndjson`：字段级差异明细（modified/missing-in-target/missing-in-source + 叶子路径）
- `.checkpoints/*.json`：桶级 checkpoint（含边界 canonical 串与 `algo_version`）

产物目录：`<dataDir>/compare/<taskID>/`。v1 不提供下载端点，按 `summary_path`/`diff_dir` 路径取用（服务端文件）。

## 语义与边界

- **resume 前提**：checkpoint 复用假设两次运行间数据未变。桶边界 canonical 串与重算计划不一致时该桶自动重算（数据漂移防护）；同一边界内的数据变更会被正常检出为 diff。取消/失败后重新 start 默认全新对比（产物目录清空）；断点续跑显式走 `start {"resume":true}` 或 pause→resume。
- **ttl_skip**：含 TTL 索引的集合跳过数据层对比（仅比较索引层）——TTL 后台删除会让哈希永不相等，js 参考实现同语义。
- **系统命名空间**：`system.*` 集合与 admin/config/local 库跳过。层次说明：MongoDB 的系统对象过滤在**对比引擎层**完成，MySQL/PG 的系统库过滤在**源适配层**完成——层次不同，用户可见行为一致（系统对象不参与对比）。
- **三态 + ttl_skip**：overall ∈ {equal, diff, error, ttl_skip}；任一集合 diff 即整体 diff，无 diff 但有集合 error 即整体 error。

## 限制与声明（记录性）

1. **大字段摘要/截断未实现**（canonical-bson-spec §12）：参考实现同样未做；超大文档全量参与哈希。
2. **undefined vs null**：按 canonical-bson-spec v1 修订移除（现代驱动不产 undefined）。
3. **跨实现字节一致为 non-goal**：canonical 串的跨语言（Go/js）字节一致不承诺；一致性以「同型同值 ⇔ 同串」的注入性与 Go 版本内稳定性为准。
4. **v1 单库对单库**：全实例对比与库表映射留待后续版本。

## 运维备忘

- 共享冒烟容器上，崩溃的活库测试进程可能遗留 `md_*_<pid>x<seq>` 后缀的孤儿夹具库（约 15MB/个）——harness 清扫刻意不碰带后缀命名（防误杀并发邻居），需**手动** drop。
