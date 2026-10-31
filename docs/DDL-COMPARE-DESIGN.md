# DDL 对比校验（DDL Compare）— 设计文档

> 版本：v1.4（2026-10-31 用户 11:13 裁定「跨方言固有差异降提示级」§4.3；v1.3 = 2026-10-30 leader 裁定 V1-V4 + 刘源批准口径）
> 分支：`feat/ddl-compare`（自 `c383f10` / V3.19.38 切出；v1.4 亦落此分支）
> 需求线程：TiMS Loop 频道「支持 DDL 对比校验」线程（2026-10-30）

## 1. 目标与定位

源端（MySQL / PostgreSQL）与目标端（TiDB，经 MySQL 协议；亦支持 MySQL 目标）
的**结构对比校验**，输出详细对比报告，逐类列出两边对象及名称。纯只读校验，
不产生任何 DDL 修改。

对比面共**八类**（v1.3 口径，2026-10-30 用户三条调整 + leader V1-V4 裁定）：

| # | 类别 | 对比口径 |
|---|------|---------|
| 1 | 表 | 总数 + 两端清单；逐表比**主键、排序规则、字符集**；**字段级对比取消**（A1）、**索引不比较**（索引类整体移除） |
| 2 | 视图 | 仅存在性：个数 + 名称集合差（不比内容/定义） |
| 3 | 存储过程 | 仅存在性：个数 + 名称集合差 |
| 4 | 自定义函数 | 仅存在性：个数 + 名称集合差 |
| 5 | 触发器 | 仅存在性：个数 + 名称集合差（键 = 「表名.触发器名」限定名） |
| 6 | 序列（新增） | 仅存在性：个数 + 名称集合差 |
| 7 | 用户（新增） | 存在性：个数 + 清单差（MySQL/TiDB = User@Host；PG = rolname，过滤系统内置账号） |
| 8 | 权限（新增） | 每用户全局权限集合差，明细双侧列出（V1 分轨见 §4.1） |

每类独立结论 pass/warn/fail + 总体结论（fail > warn > pass）。

**报告/输出全量列名要求**（刘源 seq=29）：每类明细**全量列出两端所有对象
名称**——不仅差异项；仅源/仅目标/两侧都有 的完整名单逐行列出（左源右目标
两栏），表类逐表行含 表名+主键+排序规则+字符集 双侧值；JSON 报告同构输出
全量名单。

## 2. 包结构

```
internal/ddlcompare/            # 纯引擎 + 采集器
  types.go                    # Snapshot / TableMeta / UserMeta / Category / Result / Options
  normalize.go                # 标识符折叠 + 字符集/排序规则方向化折叠（V4）
  collect.go                  # MySQL 方言走查（MySQL 源与 TiDB 目标共用；含 V3 白名单）
  collect_pg.go               # PG 方言走查（pg_catalog / information_schema；序列/用户）
  diff.go                     # 纯函数对比引擎（八类；V1 权限分轨；V2 序列注解）
  ddlcompare_test.go          # 表驱动单测（含 V3/V4/V1 锚测）

internal/webapi/
  ddl_compare.go              # 请求/任务模型、数据源解析、四个端点
  ddl_compare_report.go       # HTML 报告渲染（format=json 直读 result.json）
  ddl_compare_test.go         # handler 测试（校验/类型门/失败持久化/报告双面/id 门）

web/frontend/src/views/DdlCompareView.vue   # /ddl-compare 页面
web/frontend/src/api/index.ts               # 四个 API + 报告 URL 助手
```

## 3. 归一化规则（"什么算一致"）

| 维度 | 规则 |
|------|------|
| 标识符 | IgnoreCase（默认开）：小写折叠（Users ≈ users） |
| 主键 | 列序列逐字（含顺序）折叠后比对；无主键 ↔ 无主键 = 一致 |
| 字符集（V4 方向化折叠） | `utf8`/`utf8mb3`/PG `UTF8` 折叠到 `utf8` 桶；**折叠仅限"目标为超集"方向**：源 utf8 → 目标 utf8mb4 = 一致（pass）；源 utf8mb4 → 目标 utf8 = **WARN**（4 字节字符丢失风险）；无关字符集（latin1 vs utf8mb4）= fail |
| 排序规则（V4） | 同实现同强度且字符集可折叠 → 一致；`_general` vs `_unicode` 同强度不同实现 → **WARN**；`_ci` vs `_bin` 强度不同 → **WARN**；PG locale（`en_US.UTF-8`/`C`）与 MySQL 命名词汇表不相交（跨方言固有）→ **HINT（v1.4 §4.3）** |
| 宽松类型 | 字段级对比已取消（A1），`loose_types` 仅保留为 v1.2 请求兼容，引擎忽略 |

## 4. 判定规则

- **表**：单侧存在 / 主键不同 → fail（结构性差异，与方言无关）；字符集方向反转 / 排序规则语义差异 → **同方言** warn / **跨方言** hint（§4.3）。
- **视图/存储过程/自定义函数/触发器/序列**：个数 + 集合差，任何差异 → fail。
- **用户**：个数 + 集合差，任何差异 → fail（跨方言可比：两端都是用户名清单）。
- **权限**：见 §4.1。

### 4.1 权限分轨（V1 裁定）

| 情形 | 处理 |
|------|------|
| 同方言（MySQL 源 ↔ TiDB/MySQL 目标） | 两端同词汇表（mysql.user 全局权限列），权限集合差**照常入判定、可 FAIL**，明细全量列出 |
| 跨方言（任一端为 PG） | 词汇表不相交（mysql ~30 priv 列 vs PG 4 布尔+角色成员），**不做 PASS/FAIL 判定，降级为 HINT（提示级，v1.4；原为 WARN）+ 双侧原始权限清单全量披露**，报告标注「跨方言权限词汇表不相交，不做结论」 |

用户存在性对比不受 V1 影响（两边都是用户名清单，可比，照 v1.3 口径 FAIL-able）。

### 4.3 跨方言提示级（v1.4，用户 10-31 11:13 裁定）

> 用户口径：「对于属于 pg→tidb 本身数据库的差异部分，不应该设置为对比
> 错误，可以加一个小的提示，比如字符集、权限这种对检查一致性不是很
> 重要的那些内容。」

新增第四档结论 **`hint`（提示）**，序位 pass < hint < warn < fail，**不参与
总体结论聚合**（总体只取 pass/warn/fail 的最坏值；hint 永不把总体拉离
pass）。适用面**仅限跨方言运行**（源 PG vs 目标 MySQL 协议端）：

| 差异面 | 同方言（v1.3 口径不变） | 跨方言（v1.4 降级） |
|------|------|------|
| 字符集取值差（如 UTF8 vs latin1） | fail/warn 照旧 | **hint**（注记「跨方言字符集差异仅提示（不判错）」） |
| 排序规则词汇表不相交（PG locale vs MySQL 命名） | —（两端词汇本就同系，走 V4 warn 档） | **hint**（不判错） |
| 权限词汇表不相交 | —（同方言可比） | **hint**（§4.1） |
| 主键差异 | fail | **fail（不降级：结构一致性核心信号）** |
| 单侧对象存在（任一类） | fail | **fail（不降级：用户明示排除「明确不存在」的情形）** |

一致性计数（matched）同步调整：跨方言排序规则词汇差**不再破坏 matched**
（孪生表按「实际一致」计入一致数，仅亮提示徽标）；字符集取值差为真实
差异，降级为提示但不计入 matched。报告/FE 渲染：新增 badge-hint 徽标
（蓝灰）、row-hint 行纹、结论卡「跨方言提示类（不判错）」计数；报告
版本 V3.20.0。

**提示级呈现（v3.20.2，用户 10-31 13:45 裁定）**：不再在每个属性单元
格下逐格注记差异原因（「跨方言字符集差异仅提示…」「排序规则词汇表不
相交…」），HTML 报告与 FE 整份结果各渲染**一条统一「跨方言说明」**
（「提示」= 两端固有差异，仅披露不判错；「失败」= 真实结构差异），
仅在存在 hint 时出现；逐格 cell-note/dc-note 渲染绝迹（负向测试锚）。

### 4.2 序列（V2/A3 裁定 → v1.3.1 采集升级，用户 10-31 裁定）

TiDB v7.1.9 实际支持 CREATE SEQUENCE（元数据：`information_schema.SEQUENCES`
含 SEQUENCE_SCHEMA/START/INCREMENT/MAX_VALUE/CYCLE 等属性，
`information_schema.TABLES` 以 `TABLE_TYPE='SEQUENCE'` 列出对象）；原版
MySQL 无此对象类型。采集器经 `TABLE_TYPE='SEQUENCE'` 清点 MySQL 协议端
序列：**TiDB 目标端与 PG 源做真实的存在性比对**（共有/单侧如实呈现，
集合相等即 PASS）；仅当目标端确无序列（MySQL 实例，或 TiDB 目标库未建
序列）时，落「目标端无序列对象…」**独立注解/徽章**，与真实差异 FAIL
视觉可区分。PG 源几乎必然带序列（serial/identity 即隐式序列），故
PG→MySQL 的序列类与总体结论预期仍为 FAIL（如实）。

## 5. 采集边界（已知降级，报告 Warnings 呈现）

- 结构类（表）查询失败 → 整次运行失败（任务记录 cause）；
  清单类（视图/例程/触发器/序列/**用户及权限**）查询失败 → 该类按空集对比 +
  顶部警告。特别地：
  - 不带 `information_schema.ROUTINES` 的 TiDB 构建 → 例程类空集 + 警告；
  - PG 10 以下无 `prokind` → 例程类空集 + 警告；
  - **无 mysql 库 SELECT 权限**（普通扫描账号常见）→ 用户/权限类降级 =
    WARNING + 空集（同 ROUTINES 政策），**不整次失败**。
- PG 表达式索引键部（indkey=0）不入签名（文档化边界；迁移校验语料中罕见）；
  部分索引谓词（WHERE）不参与签名。v1.3 索引类整体移除后此边界随索引比较
  一并不再适用。
- TiDB 目标端探测 `@@tidb_enable_procedure`：OFF 时报告头给 WARN 提示
  （不改变结论语义）——已建过程/函数可能被隐藏。
- 单侧对象上限默认 20000（对齐 ddlexport DefaultMaxObjects），超限 400
  并提示缩小范围。口径 = 顶层对象（表/视图/例程/触发器/**序列**）；用户
  账号不是 schema 对象，不计入上限。

### 5.1 mysql.user 只读红线（V3 门槛项）

- ① 采集只**显式 SELECT 权限列白名单**（`mysqlUserPrivColumns`：28 个
  `_priv` 列），先经 information_schema.COLUMNS 与 mysql.user 实际列求交，
  再 SELECT 交集 —— 杜绝 `SELECT *` 或整行落盘；
- ② task.json / result.json / 报告**永不含任何密码哈希**（白名单外永不被
  触碰：authentication_string / password_lifetime / global_priv 一律不入
  查询与输出）；
- ③ 无权限读 mysql.user → 降级 WARN + 空集，不整次失败；
- ④ PG 侧 pg_roles 无密码字段天然安全，pg_auth_members 只取成员名
  （`MEMBER:<rolename>` token）。
- 锚测：`TestCollectMySQLUsersWhitelistNoHash` 断言采集 SQL 与结果不含
  `authentication_string` / 密码哈希串；`TestCollectMySQLUsersDegradesOnNoPrivilege`
  断言降级路径。

## 6. API（/api/v1 前缀）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/ddl-compare/schemas` | `{side: source\|target, ref}` → `{schemas:[...]}`；源端门 = srcCapable(CapDDLExport)（mysql/postgres），目标端门 = source.IsMySQLWire（tidb/mysql） |
| POST | `/ddl-compare` | 同步执行；`{name?, source_ref, target_ref, source_schema, target_schema, options?}` → `{task_id, result}`；120s 组管 deadline（F-10 口径） |
| GET | `/ddl-compare/tasks` | 历史列表（新→旧） |
| GET | `/ddl-compare/tasks/{dcID}/report?format=html\|json` | HTML 家族报告（默认，附件下载）/ result.json 原字节 |

任务持久化：`dataDir/ddl-compare/{id}/`（task.json + result.json；id=uuid[:8]，
`^[0-9a-f]{8}$` 白名单门先行于一切文件系统触碰——compareIDRe 孪生）。
task.json 仅存免凭据回显（host/schema/类型/结论），密码与密码哈希永不落盘。

## 7. 类型分支纪律（five-pin #3）

webapi 层零新增裸类型分支：目标端资格与方言分派统一走
`source.IsMySQLWire` / `source.IsTiDB`（internal/source 注册表之家新增谓词，
IsMongoDB 孪生），frozen-baseline 守卫（TestTypeBranchFrozenBaseline）保持全绿。

## 8. 报告

- 服务端渲染 HTML（compare_data_report 家族样式）：banner + meta 栅格 +
  结论卡 + **八类汇总表**（类别含注解列）+ 表明细（表名/状态/主键/字符集/
  排序规则 双侧值 + V4 说明折叠区）+ 序列注解徽章 + 权限卡（V1 分轨：
  跨方言顶部警示 + 双侧原始清单） + 名称类三栏（仅源/仅目标/两边一致，
  逐名列出）+ 警告区 + 版本徽章 **V3.19.40**（dcmpReportVersion）。
- FE 顶栏徽章同步 bump：package.json `version` 单一事实源 → 3.19.40。
- 动态串一律 mcHTMLEsc（对象名含 `<script>` 转义有锚测）。

## 9. 前端（/ddl-compare）

两端 DataSourcePicker（源：postgres/mysql；目标：tidb/mysql）→ 获取
Schema/数据库（单选）→ 口径开关（v1.3 仅保留 忽略大小写；宽松类型与
索引含名称开关随 A1/索引类移除而撤销）→ 同步运行 → 八类汇总 + 表/权限
明细 + 名称类三栏 + 报告下载（HTML/JSON，读路由免 token，新窗口）。
历史卡含状态/结论/报告链接/失败原因 tooltip。

## 10. 测试账本

- 引擎：用例族含 V4 方向化（TestCharsetVerdict / TestCollationVerdict /
  TestDiffSameDialectTablesFold）、V1 分轨（TestDiffSameDialectPrivileges /
  TestPrivilegesBothEmptyPass）、V2 序列注解（TestDiffEqual 的 sequences
  断言 / TestSequencesMySQLToTiDB）、用户存在性（TestUsersExistenceDiff）、
  跨表同名触发器（D1）、PK join 契约（D6）、V3 白名单与降级锚测、
  排序规则/字符集采集契约。
- webapi：校验矩阵、双端类型门（含 schemas 探查）、失败运行持久化 +
  历史、报告双面（HTML 锚点断言含八类/序列注解/权限分轨 + JSON 逐字节 +
  XSS 转义）、{dcID} 白名穷举 404、对象上限/提示词纯函数。
- FE：ddlCompareView.spec.ts 结构锚（v3.19.40，含权限分轨/序列注解/无
  indexByName）+ ddlCompareViewBehavior.spec.ts 空对空 + 全量 vitest 绿。
- Go 全量 `go test ./internal/...` 绿；gofmt/vet 净；frozen-baseline 绿。

## 11. 联调状态

本地无 MySQL/PG/TiDB 实例、103 无 SSH 密钥、test_tidb root 密码此前即为
卡点——真实库联调（MySQL 源、PG 源 vs test_tidb）待凭据/环境就绪后补
（docker 本地双容器 e2e 见毕帖证据节）。PG 源含序列时总体结论预期为 FAIL
的端到端验证在 ④103 正式轮覆盖。
