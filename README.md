# timstool

**TiDB 多源异构数据同步工具**（TiDB Multi-source Integrated Tool）—— 多源异构数据库 → TiDB 迁移/同步工具。当前以 PostgreSQL → TiDB 全量迁移（含可选 CDC 增量同步 + DDL 复制）为首个已实现源，架构上扩展为多源（MySQL/Oracle/SqlServer/DB2 … → TiDB）。

覆盖兼容性评估、Schema 迁移、全量数据迁移、数据校验四大能力，并含**可选的 CDC 增量同步**（实时增量 + DDL 复制，默认关闭）。支持 CLI 和 Web 管理界面两种使用方式。

## 数据源支持状态

| 数据源 | 状态 | 说明 |
|--------|------|------|
| PostgreSQL | ✅ 生产可用 | PG COPY 导出 + Lightning 导入 + CDC 增量 |
| MySQL | 🔧 开发中 | Dumpling 导出 → Lightning 导入 |
| Oracle | 📋 Stub | 接口已注册，实现待规划 |
| SQL Server | 📋 Stub | 接口已注册，实现待规划 |
| DB2 | 📋 Stub | 接口已注册，实现待规划 |

## 近期更新

- **独立数据比对（数据对比）**（`0067a13`）— Web 新增「数据比对」页面：独立配置源端/目标端连接（不依赖迁移任务），支持 quick / sample / checksum 三种比对模式、表级选择（含预估行数与搜索）、实时进度与 WebSocket 推送、比对历史与表级差异报告（源/目标行数、差异行数、耗时）；连接信息可「保存连接 / 加载连接」复用（密码永不落盘，需重新输入）。
- **迁移进度优化**（`35358f1`）— 数据迁移进度按阶段加权（导出 5% / Lightning 导入 47.5% / 流式导入 47.5%），Lightning 导入阶段显示"数据导入中（x/N 表）"；导入失败自动回退流式写入时进度从 50% 平滑续走，不再异常跳变。
- **目标库配置记忆**（`35358f1`）— 向导中的 PD 地址（自动归一化，去掉 `http://` 前缀与尾部 `/`）与 TiDB 状态端口在任务间记忆复用，新建任务自动回填；"状态端口"文案更正为"TiDB 状态端口"；增量更新不会误清已有的临时目录 / Lightning 配置记忆。
- **进度分母恒定**（`b0706ca`）— 迁移开始即预登记全部表，进度分母全程恒定，修复进度长期挂在 100% 附近不动或回退跳变的问题；恢复任务时已完成表进度归零重算，避免虚高。
- **Lightning 日志增强**（`bf14b57`）— 每张表的 Lightning 导入结果单独记录，Lightning 日志落独立文件，排障更直观。
- **迁移选项记忆**（`f48a516`）— 临时目录、是否使用 Lightning、Lightning 路径在任务间记忆，新建任务自动预填。

## 特性

- **多源异构架构** — 统一 Source 抽象层，支持 PostgreSQL（生产可用）、MySQL（开发中）、Oracle/SqlServer/DB2（Stub 规划中）→ TiDB
- **兼容性评估** — 迁移前扫描不兼容对象（Trigger、Stored Function、特殊索引等），输出风险报告（`assess` 命令，支持 terminal/json 输出）
- **Schema Migration** — 自动采集 PG schema，类型映射 + DDL AST 转换，生成 TiDB 兼容 DDL，支持 `--dry-run` 预览
- **Data Migration** — 基于 PG COPY 协议导出 + TiDB Lightning CLI 导入，实现高性能并行数据迁移；MySQL 源使用 Dumpling 导出；支持大表分片拆分
- **Data Validation** — 四种校验模式（quick 行数对比 / sample 抽样验证 / checksum 分块哈希 / full 全量校验）确保数据一致性
- **独立数据比对** — Web「数据比对」页面，脱离迁移流水线手工比对任意源/目标：三种模式（quick / sample / checksum）、表级选择、连接配置保存/加载（密码不落盘）、比对历史与差异报告
- **Web 管理界面** — 可视化配置向导（含数据源选择器）、独立数据比对、交互式表选择、实时进度监控、日志流查看、CDC 仪表盘、迁移历史、HTML 报告下载
- **断点续传** — Checkpoint 机制支持中断后从断点恢复
- **单二进制部署** — 前端通过 `go:embed` 嵌入，零外部依赖，Docker 一键启动

## 系统要求

| 组件 | 要求 |
|------|------|
| Go | 1.25+ |
| PostgreSQL | 10+ |
| TiDB | 7.1+ |
| MySQL Client + Dumpling | MySQL→TiDB 迁移时需要 `mysql` 客户端和 `dumpling` 二进制 |
| TiDB Lightning | 使用 Lightning 导入时需要安装 `tidb-lightning` 二进制 |
| Node.js | 20+（仅前端开发构建时需要） |

## 安装

> 📌 **如果你是第一次使用，推荐从「方式一：下载二进制」开始，最简单。**

### 方式一：下载预编译二进制（推荐，最简单）

从 [GitHub Releases](https://github.com/michaelliuyuan/timstool/releases) 下载对应平台的二进制文件。

**Linux（最常见的服务器环境）：**

```bash
# 1. 下载（以 Linux amd64 为例）
wget https://github.com/michaelliuyuan/timstool/releases/latest/download/timstool-linux-amd64 -O timstool

# 2. 添加执行权限
chmod +x timstool

# 3. 移到系统路径（可选）
sudo mv timstool /usr/local/bin/

# 4. 验证安装
./timstool --help
```

**Windows：**

```powershell
# 下载 timstool-windows-amd64.exe，重命名
Rename-Item timstool-windows-amd64.exe timstool.exe

# 验证安装
.\timstool.exe --help
```

### 方式二：从源码构建（适合开发者）

**前置条件：** 安装 [Go 1.25+](https://go.dev/dl/)，如果需要 Web UI 还需要 [Node.js 20+](https://nodejs.org/)

```bash
# 克隆仓库
git clone https://github.com/michaelliuyuan/timstool.git
cd timstool

# 仅 CLI 版本（不含 Web UI）
make build

# 含 Web UI 版本（需要 Node.js 20+）
make build-web

# 构建产物在 build/ 目录
ls build/timstool
```

**交叉编译（在 Mac/Windows 上编译 Linux 版本）：**

```bash
# Linux amd64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o timstool .

# Linux arm64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o timstool .

# Linux arm64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o timstool .
```

### 方式三：Docker（适合容器化部署）

```bash
# 构建镜像（自动包含前端 + 内置 tidb-lightning）
docker build -t timstool .

# 启动 Web 服务
docker run -d -p 8080:8080 --name timstool timstool

# 浏览器访问 http://localhost:8080
```

---

## 快速开始

### Web 界面模式（推荐）

```bash
# 启动 Web 管理界面
./timstool web

# 自定义端口和数据目录
./timstool web --port 8080 --data /data/timstool
```

浏览器访问 `http://localhost:8080`，通过向导完成：
1. **配置源端** — 填写 PostgreSQL 连接信息，点击「测试连接」
2. **配置目标端** — 填写 TiDB 连接信息（含 PD 地址），点击「测试连接」
3. **选择表** — 查看所有表及预估行数，勾选待迁移表（支持搜索）
4. **迁移选项** — 并发数、批量大小、Lightning 开关、冲突策略
5. **确认执行** — 实时监控迁移进度，查看日志流，下载 HTML 报告

### CLI 命令行模式

#### 1. 准备配置文件

```bash
cp configs/config.yaml config.yaml
```

编辑 `config.yaml`，填入 PG 和 TiDB 连接信息：

```yaml
source:
  type: "postgres"             # postgres (默认) | mysql | oracle | mssql | db2
  host: "pg-host"
  port: 5432
  user: "postgres"
  password: ""
  database: "mydb"
  schema: "public"
  sslmode: "disable"

target:
  host: "tidb-host"
  port: 4000
  user: "root"
  password: ""
  database: "mydb"
  pd_addr: "tidb-host:2379"    # Lightning 导入时必填
```

#### 2. 一键迁移

```bash
./timstool all --config config.yaml
```

执行流程：Pre-check → Schema Migration → Data Migration → Data Validation → Summary Report

#### 3. 分步执行

```bash
# 预检（不执行迁移）
./timstool precheck --config config.yaml

# 仅迁移 Schema（支持 --dry-run 预览）
./timstool schema --config config.yaml
./timstool schema --config config.yaml --dry-run --output schema.sql

# 仅迁移数据
./timstool data --config config.yaml

# 仅数据校验
./timstool validate --config config.yaml
./timstool validate --config config.yaml --level L2 --sample-ratio 0.05  # L2 抽样
./timstool validate --config config.yaml --level L3                       # L3 全量 Checksum

# 兼容性评估（迁移前风险评估）
./timstool assess --config config.yaml                    # 终端报告
./timstool assess --config config.yaml --format json -o report.json  # JSON 报告
```

---

## 命令参考

```
timstool [command]

Commands:
  all                一键执行完整迁移流程（precheck → schema → data → validate）
  assess             兼容性评估（迁移前扫描不兼容对象，输出风险报告）
  precheck           预检与兼容性评估
  schema             Schema 迁移（--dry-run 仅预览 DDL）
  data               全量数据迁移
  validate           数据校验（L1/L2/L3）
  web                启动 Web 管理界面
  cdc                CDC 增量同步（可选模块，默认关闭；见下文「CDC 增量同步」节）

Global Flags:
  -c, --config string     配置文件路径 (默认 "configs/config.yaml")
      --source string     数据源类型: postgres (默认) | mysql | oracle | mssql | db2
      --log-level string  日志级别: debug, info, warn, error (默认 "info")
      --log-format string 日志格式: console, json (默认 "console")

Web Command Flags:
  -p, --port int     Web 服务端口 (默认 8080)
      --host string  Web 服务监听地址 (默认 "0.0.0.0")
      --data string  数据存储目录 (默认 ".timstool")
```

---

## 配置说明

### 完整配置示例

```yaml
# 数据源端配置
source:
  type: "postgres"             # postgres (默认) | mysql | oracle | mssql | db2
  host: "localhost"
  port: 5432
  user: "postgres"
  password: ""
  database: "mydb"
  schema: "public"
  sslmode: "disable"           # disable | require | verify-ca | verify-full

# TiDB 目标端配置
target:
  host: "localhost"
  port: 4000
  user: "root"
  password: ""
  database: "mydb"
  pd_addr: "localhost:2379"    # PD 地址，Lightning 导入时必填

# 迁移配置
migration:
  parallel: 4                  # 同时迁移的表数量
  batch_size: 100000           # 批量大小（行数）
  temp_dir: "/tmp/timstool"     # CSV 临时文件目录
  tables: []                   # 空=全部表，支持正则匹配
  exclude_tables: []           # 排除表，支持正则匹配
  use_lightning: true          # 使用 TiDB Lightning 加速导入
  on_error: "abort"            # 单表失败策略: skip | abort
  target_policy: "insert"     # 目标表冲突策略: insert | truncate | drop
  checkpoint_dir: ".checkpoint"  # Checkpoint 目录
  read_timeout: "30m"          # PG 读超时
  write_timeout: "30m"         # TiDB 写超时

# 日志配置
logging:
  level: "info"                # debug | info | warn | error
  format: "console"            # console | json
  output: ""                   # 空=stderr，或指定文件路径

# 数据校验配置
compare:
  no_pk_strategy: "auto"       # 无PK表策略: auto | hash_group | bucket | aggregate
  no_pk_bucket_count: 100      # bucket 策略桶数
  no_pk_table_threshold: 1000000  # auto 模式下超过此行数使用 aggregate/bucket
  compare_mode: "sample"       # quick(行数) | sample(抽样,默认) | checksum(分块哈希) | full(全量)
  checksum_chunk_size: 50000   # checksum 模式每块行数
  checksum_parallel: 4         # checksum 模式并发数

# Web 监控面板（CLI 模式可选）
web:
  enable: false                # CLI 模式下是否启动监控面板
  port: 8080                   # 监听端口
  host: "0.0.0.0"              # 监听地址

# CDC 增量同步（可选模块，默认关闭）
cdc:
  enable: false                # 可选模块，默认关闭。需要零停机增量同步时设为 true
  mode: "full_incr"            # full_incr(全量+增量) | incr_only(仅增量，需已有全量基线)
  slot_name: "pg2tidb_cdc"     # PG 逻辑复制 slot
  publication_name: "pg2tidb_pub"  # PG publication
  batch_size: 1000             # 每批最大事件数
  parallel: 1                  # 1=串行(正确性优先)；>1 按表并行(不保证跨表 FK/多表事务序)
  conflict_strategy: "replace" # replace | insert_ignore | upsert | skip
  sync_ddl: true               # 复制源端 DDL 到 TiDB（CREATE/ALTER/DROP TABLE + BTREE 索引；VIEW/FN/TRIGGER 跳过）；false=仅 DML
  tables: []                   # CDC 同步表清单（空=全部，独立于 migration.tables）
  exclude_tables: []
  checkpoint_file: ".cdc_checkpoint.json"
```

### 关键配置项说明

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `compare.compare_mode` | 校验模式: `quick`(行数) / `sample`(抽样,默认) / `checksum`(分块哈希) | sample |
| `compare.sample_ratio` | sample 模式抽样比例 (0.0-1.0) | 0.01 |
| `migration.parallel` | 并行导出 worker 数 | 4 |
| `migration.batch_size` | 每批次迁移行数 | 100000 |
| `migration.use_lightning` | 使用 TiDB Lightning CLI 物理导入（需安装 tidb-lightning） | true |
| `target.pd_addr` | PD 地址（Lightning local backend 必填） | host:2379 |
| `target.status_port` | TiDB Status 端口（Lightning 使用） | 10080 |
| `migration.target_policy` | 目标表冲突策略: `insert` / `truncate` / `drop` | insert |
| `migration.on_error` | 单表失败时 `skip` 继续或 `abort` 中止 | abort |
| `migration.temp_dir` | CSV 导出临时目录 | /tmp/timstool |
| `logging.level` | 日志级别 | info |
| `web.enable` | CLI 模式下启用 Web 监控面板 | false |
| `cdc.enable` | CDC 增量同步（可选模块）总开关 | false |

#### target_policy 说明

| 策略 | 行为 |
|------|------|
| `insert` | 直接 INSERT，主键冲突时报错（默认） |
| `truncate` | 先 TRUNCATE 目标表再导入 |
| `drop` | 先 DROP TABLE 再重建并导入 |

---

## CDC 增量同步（可选模块）

CDC（Change Data Capture，PostgreSQL logical replication → TiDB 实时增量同步）是**可选模块，默认关闭**。timstool 出厂形态是纯全量迁移工具；仅当需要零停机增量同步时才显式开启。

**开启方式（任选其一）：**
- 配置文件：`cdc.enable: true`（持久）
- CLI 一次性：`timstool cdc --enable-cdc`（本次运行）
- 环境变量：`PG2TIDB_CDC_FORCE=1`（本次运行，适合容器化）

默认关闭（`cdc.enable: false`）时：
- `timstool all`（全量流水线）行为**完全不变**，不含 CDC。
- `timstool cdc` 打印开启提示并退出（非硬阻断），加 `--enable-cdc` 或 `PG2TIDB_CDC_FORCE=1` 即放行。
- Web 界面隐藏 CDC 导航入口；`GET /api/v1/features` 返回 `{"cdc":{"enabled":false}}`，`/cdc/status` 返回稳定禁用态。

**Web 一键启停（`cdc.enable: true` 后）**：CDC 仪表盘点「启动 CDC / 停止 CDC」按钮即可启停增量同步，无需 SSH/CLI——CDC 进程由 Web supervisor 管理（崩溃指数退避自动重启、SIGTERM→SIGKILL 优雅停止、Web 重启领养在跑的 CDC）。`/cdc/status` 返回 supervisor 控制态（running/starting/stopping/failed/adopted + PID/重启次数）。设计见 [docs/cdc-web-control-design.md](docs/cdc-web-control-design.md)。

**DDL 复制（`cdc.sync_ddl`，默认 `true`）**：启用 CDC 后，源端的 `CREATE/ALTER/DROP TABLE`（及 BTREE 索引）会自动复制到 TiDB（PG→TiDB 类型映射 + PG schema 前缀去除 + `IF NOT EXISTS` 幂等 + `ddl_log` id checkpoint 的 at-least-once）。视图/函数/触发器等不兼容 DDL 跳过+告警，不阻断。`sync_ddl: false` 时仅复制 DML（面向已预先全量迁移的 schema）。新表的 DML 若先于其 DDL 到达，applier 会短退避重试等 DDL 落地，超限 halt（不静默丢）。

> ⚠️ **升级提示（行为变更）**：CDC 的同步表清单现从 `cdc.tables` 读取（原从 `migration.tables`）。若你此前依赖 `migration.tables` 过滤 CDC 同步范围，请将其复制到 `cdc.tables`。`cdc.tables` 为空表示同步全部表。

CDC 功能细节（INSERT/UPDATE/DELETE、事务顺序性、checkpoint-on-failure、无 PK 表 halt、Web 一键启停 + 只读监控）见 [docs/cdc-web-monitoring-contract.md](docs/cdc-web-monitoring-contract.md)；可选化设计见 [docs/cdc-optional-module-design.md](docs/cdc-optional-module-design.md)，一键启停设计见 [docs/cdc-web-control-design.md](docs/cdc-web-control-design.md)，DDL 复制设计见 [docs/cdc-ddl-replication-design.md](docs/cdc-ddl-replication-design.md)。

---

## TiDB Lightning 集成

timstool 使用 TiDB Lightning 作为数据导入引擎，提供两种后端模式：

| 模式 | 配置 | 速度 | 要求 |
|------|------|------|------|
| **Local Backend**（推荐） | `backend = "local"` | 100–500 GiB/h | 需要访问 PD 地址，目标表必须为空 |
| **TiDB Backend** | `backend = "tidb"` | 10–50 GiB/h | 通过 SQL 执行，目标表可非空 |

### Lightning 使用前提

timstool 支持两种方式获取 tidb-lightning：

1. **内置模式（推荐）**：构建时将 tidb-lightning 二进制嵌入 timstool，运行时自动释放到数据目录，无需手动安装
   ```bash
   # 构建 Web UI + 内置 Lightning
   LIGHTNING_BIN=/path/to/tidb-lightning bash build-web.sh

   # 或使用 Make
   make build-all LIGHTNING_BIN=/path/to/tidb-lightning
   ```

2. **系统 PATH**：如果 timstool 未内置 tidb-lightning，运行时会从系统 PATH 查找。如果都找不到，自动回退到流式 INSERT 模式

> **优先级**：系统 PATH tidb-lightning > 内置 tidb-lightning > 流式 INSERT 回退

### Lightning 导入流程

```
PG COPY 导出 CSV → 生成 lightning.toml → 调用 tidb-lightning CLI → 清理 CSV
```

如果 Lightning 导入失败，工具会自动回退到流式 INSERT 方式完成导入。

### 数据迁移流程

```
PostgreSQL                    timstool                     TiDB
─────────                    ───────                     ─────
    │                            │                           │
    │◄─── PG COPY TO STDOUT ────┤                           │
    │      (并行 N workers)      │                           │
    │──── CSV 数据 ────────────►│                           │
    │                            │  写入 temp_dir/*.csv      │
    │                            │                           │
    │                            │── tidb-lightning ────────►│
    │                            │  (local backend 物理导入)  │
    │                            │                           │
    │                            │◄── 导入完成 ──────────────┤
    │                            │                           │
    │                            │── (回退: 流式 INSERT) ────►│
    │                            │                           │
```

---

## 类型映射参考

| PostgreSQL | TiDB/MySQL | 备注 |
|---|---|---|
| serial / bigserial | BIGINT AUTO_INCREMENT | 自增主键 |
| integer / int | INT | |
| bigint | BIGINT | |
| real / float4 | FLOAT | |
| double precision / float8 | DOUBLE | |
| numeric(p,s) | DECIMAL(p,s) | |
| varchar(n) | VARCHAR(n) | |
| text | TEXT | |
| char(n) | CHAR(n) | |
| boolean | TINYINT(1) | t/f → 1/0 |
| date | DATE | |
| timestamp / timestamptz | DATETIME / TIMESTAMP(3) | 保留毫秒精度 |
| time / timetz | TIME | |
| interval | VARCHAR(64) | 转为字符串 |
| uuid | CHAR(36) | 转为字符串 |
| json / jsonb | JSON | |
| bytea | BLOB | |
| bit / varbit | BIT / VARCHAR | |
| enum | TEXT | 转为 TEXT（非 MySQL ENUM） |
| money | DECIMAL(19,2) | |
| array | JSON | 序列化为 JSON 数组 |
| composite type | JSON | 扁平化为 JSON |
| inet / cidr | VARCHAR(45) | |
| macaddr | VARCHAR(17) | |
| point / line / polygon | VARCHAR | 几何类型存为 WKT |

> **注意**：GIN、GiST 等不兼容索引类型不会自动迁移，仅在 precheck 阶段记录告警日志。

---

## Web 管理界面

### 启动

```bash
# 默认端口 8080
./timstool web

# 自定义配置
./timstool web --port 9090 --host 0.0.0.0 --data /data/timstool

# Docker
docker run -p 8080:8080 -v /data/timstool:/data timstool
```

### 功能页面

| 页面 | 功能说明 |
|------|----------|
| **配置向导** | 5 步向导：数据源选择（PG/MySQL/...）→ 源端配置 → 目标端配置 → 选择表（支持搜索/全选） → 迁移选项 → 确认执行 |
| **数据比对** | 独立数据比对（不执行迁移）：配置源/目标连接并测试，三种模式（快速行数 / 采样 / 分块校验）+ 表级选择；「保存连接 / 加载连接」复用连接配置（密码永不保存）；实时进度（WebSocket + 轮询）、取消、比对历史与表级差异报告（源/目标行数、差异行数、耗时、错误建议） |
| **兼容性评估** | 迁移前风险评估，展示不兼容对象清单和迁移建议 |
| **任务监控** | 实时进度、表级详情（导出/导入行数、百分比）、吞吐量指标 |
| **CDC 仪表盘** | CDC 增量同步监控（LSN、吞吐、延迟），一键启停 |
| **日志查看** | 实时日志流，按级别过滤（info/warn/error） |
| **迁移历史** | 历史任务列表、详情查看、重新执行 |
| **报告下载** | 生成 HTML 格式完整迁移报告，包含各阶段执行结果 |

### API 端点

#### 任务管理

```
POST /api/v1/config/test-connection      # 测试 PG/TiDB 连接
POST /api/v1/config/list-tables          # 列出源端表及预估行数
POST /api/v1/tasks                       # 创建迁移任务
GET  /api/v1/tasks                       # 列出所有任务（分页）
GET  /api/v1/tasks/{id}                  # 获取任务详情
POST /api/v1/tasks/{id}/start            # 启动任务
POST /api/v1/tasks/{id}/pause            # 暂停任务
POST /api/v1/tasks/{id}/resume           # 恢复任务
POST /api/v1/tasks/{id}/cancel           # 取消任务
DELETE /api/v1/tasks/{id}                # 删除任务
```

#### 进度与监控

```
GET  /api/v1/tasks/{id}/progress         # 获取实时进度
GET  /api/v1/tasks/{id}/phases           # 获取各阶段状态
GET  /api/v1/tasks/{id}/logs             # 获取任务日志
GET  /api/v1/tasks/{id}/report           # 获取迁移报告
GET  /api/v1/ws                          # WebSocket 实时推送（进度+日志）
```

#### 独立数据比对

```
POST   /api/v1/compare/tasks                   # 创建并启动比对任务（源/目标连接 + 模式 + 表清单）
GET    /api/v1/compare/tasks                   # 比对历史列表
GET    /api/v1/compare/tasks/{compareID}       # 比对任务详情（状态、表进度、当前表）
GET    /api/v1/compare/tasks/{compareID}/report  # 比对报告（表级 pass/fail、行数、差异行数）
POST   /api/v1/compare/tasks/{compareID}/cancel  # 取消进行中的比对
DELETE /api/v1/compare/tasks/{compareID}       # 删除比对记录
GET    /api/v1/compare/options                 # 读取已保存的连接配置（密码恒为空）
PUT    /api/v1/compare/options                 # 保存连接配置（部分合并；密码永不落盘）
```

> 比对模式：`quick`（行数估算，最快）/ `sample`（行数+随机采样，推荐）/ `checksum`（行数+分块哈希校验，支持分块大小与并行数）。表清单留空表示比对全部表。任务数据持久化在 `<dataDir>/compare/{id}/`，与迁移任务存储隔离；同一时刻仅允许一个比对任务运行，进程重启后遗留的 running 任务自动标记为失败。

#### 健康检查

```
GET  /api/v1/health                      # 健康检查
```

### CLI 监控模式（兼容）

在配置文件中启用 `web.enable: true` 后，CLI 命令执行迁移时也会启动监控面板。API 端点：

```
GET /api/v1/status       # 全局迁移状态
GET /api/v1/tables       # 各表进度
GET /api/v1/validation   # 校验结果
GET /api/v1/report       # 最终报告
```

---

## 断点续传

迁移过程中断后，重新执行相同命令即可从上次断点继续。Checkpoint 信息保存在 `migration.checkpoint_dir`（默认 `.checkpoint`）目录中。

Web 模式下，可在迁移历史页面点击「重新执行」，工具会自动检测并利用已有 checkpoint。

---

## 部署指南

> 📌 **首次部署建议：先看完「方式一：Web 模式单机部署」，5 分钟即可上手。**

### 方式一：Web 模式单机部署（推荐，最简单）

这是最简单的部署方式，只需要一个二进制文件和一台 Linux 服务器。

**第一步：准备配置文件**

创建 `config.yaml`：

```yaml
source:
  host: "你的PG地址"
  port: 5432
  user: "postgres"
  password: "你的密码"
  database: "要迁移的数据库"
  schema: "public"
  sslmode: "disable"

target:
  host: "你的TiDB地址"
  port: 4000
  user: "root"
  password: "你的密码"
  database: "目标数据库"
  pd_addr: "TiDB-PD地址:2379"

web:
  enable: true
  port: 8080
  host: "0.0.0.0"
```

**第二步：启动服务**

```bash
# 上传 timstool 二进制和 config.yaml 到服务器
scp timstool config.yaml user@your-server:/home/user/timstool/

# SSH 登录服务器
ssh user@your-server

# 启动 Web 服务
cd /home/user/timstool
chmod +x timstool
nohup ./timstool web -c config.yaml -p 8080 > timstool.log 2>&1 &
```

**第三步：浏览器操作**

1. 打开浏览器访问 `http://服务器IP:8080`
2. 按照页面向导操作：配置源端 → 配置目标端 → 选择表 → 设置选项 → 开始迁移
3. 实时查看迁移进度和日志

**验证服务是否正常：**

```bash
# 检查进程是否在运行
ps aux | grep timstool

# 检查 HTTP 服务是否响应
curl http://localhost:8080/api/v1/health
```

**停止服务：**

```bash
# 找到进程并停止
kill $(lsof -ti:8080)
```

### 方式二：CLI 命令行模式

适合自动化脚本或 CI/CD 环境使用。

```bash
# 一键完整迁移（precheck → schema → data → validate）
./timstool all --config config.yaml

# 分步执行
./timstool precheck --config config.yaml   # 第1步：预检
./timstool schema --config config.yaml     # 第2步：Schema 迁移
./timstool data --config config.yaml       # 第3步：数据迁移
./timstool validate --config config.yaml   # 第4步：数据校验
```

### 方式三：Docker 部署

```bash
# 构建镜像
docker build -t timstool .

# 运行（Web 模式，默认启动 8080 端口）
docker run -d \
  --name timstool \
  -p 8080:8080 \
  -v /data/timstool:/data \
  timstool

# 运行（CLI 模式）
docker run --rm \
  -v $(pwd)/config.yaml:/etc/timstool/config.yaml \
  timstool all --config /etc/timstool/config.yaml
```

### Docker Compose（含 PG + TiDB 测试环境）

```yaml
version: '3.8'
services:
  timstool:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - timstool-data:/data
    depends_on:
      - postgres
      - tidb

  postgres:
    image: postgres:16
    environment:
      POSTGRES_DB: testdb
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: postgres
    ports:
      - "5432:5432"

  tidb:
    image: pingcap/tidb:v7.1.0
    ports:
      - "4000:4000"
      - "10080:10080"

volumes:
  timstool-data:
```

---

## 项目结构

```
timstool/
├── cmd/                        # CLI 命令
│   ├── root.go                 # 根命令 + 全局 flags（含 --source 数据源选择）
│   ├── all.go                  # all — 一键完整流程
│   ├── assess.go               # assess — 兼容性评估
│   ├── precheck.go             # precheck — 预检
│   ├── schema.go               # schema — Schema 迁移
│   ├── data.go                 # data — 数据迁移
│   ├── validate.go             # validate — 数据校验
│   ├── web.go                  # web — Web 管理界面
│   ├── cdc.go                  # cdc — CDC 增量同步
│   └── static/                 # 嵌入式前端资源（go:embed）
├── web/
│   └── frontend/               # Vue 3 前端源码
│       ├── src/
│       │   ├── views/          # 页面组件
│       │   ├── components/     # 通用组件
│       │   └── ...
│       ├── package.json
│       └── vite.config.ts
├── internal/
│   ├── source/                 # 多源抽象层（Source interface）
│   │   ├── registry.go         # 数据源注册（postgres/mysql/oracle/mssql/db2）
│   │   ├── postgres/           # PostgreSQL adapter
│   │   │   ├── schema_reader.go
│   │   │   ├── data_reader.go
│   │   │   └── incremental/    # PG 逻辑复制（CDC）
│   │   └── mysql/              # MySQL adapter（开发中）
│   │       ├── schema_reader.go
│   │       ├── data_reader.go
│   │       └── defaults.go
│   ├── target/                 # 目标端抽象（TiDB adapter）
│   ├── assess/                 # 兼容性评估模块（增强版 precheck）
│   ├── schema/                 # Schema 迁移模块
│   │   ├── reader.go           # PG schema 读取
│   │   ├── converter.go        # DDL 转换
│   │   ├── type_map.go         # 类型映射
│   │   └── applier.go          # DDL 执行
│   ├── data/                   # 数据迁移模块
│   │   ├── exporter.go         # PG COPY 导出 CSV
│   │   ├── migrator.go         # 迁移编排
│   │   └── scheduler.go        # 并发调度器（含大表分片）
│   ├── lightning/              # TiDB Lightning 集成（独立模块）
│   ├── dumpling/               # MySQL Dumpling 导出（MySQL→TiDB）
│   ├── cdc/                    # CDC 增量同步模块
│   ├── validator/              # 数据校验模块
│   │   ├── validator.go        # L1/L2/L3/full 校验引擎
│   │   └── reporter.go         # 报告生成
│   ├── precheck/               # 兼容性预检
│   │   └── checker.go          # 预检检查器
│   ├── orchestrator/           # 任务编排器
│   │   └── orchestrator.go     # 4 阶段管道编排
│   ├── webapi/                 # Web API 服务
│   │   ├── server.go           # HTTP + WebSocket 服务（chi 路由）
│   │   ├── compare.go          # 独立数据比对任务（/api/v1/compare/*）
│   │   ├── dbconn.go           # 数据库连接测试
│   │   ├── logbuffer.go        # 日志缓冲（支持实时订阅）
│   │   └── logcore.go          # Zap 日志采集 Core
│   ├── store/                  # 任务持久化
│   │   ├── store.go            # SQLite 存储（modernc.org/sqlite）
│   │   └── store_test.go
│   ├── api/                    # CLI 监控面板 API（旧版兼容）
│   │   └── server.go
│   └── common/                 # 公共模块
│       ├── config/             # 配置加载
│       ├── logger/             # 日志初始化
│       ├── checkpoint/         # Checkpoint 管理
│       ├── reporter/           # 报告类型定义
│       └── errors/             # 错误类型
├── docs/                       # 设计文档
│   ├── cdc-architecture.md
│   ├── cdc-optional-module-design.md
│   ├── cdc-web-monitoring-contract.md
│   ├── cdc-web-control-design.md
│   ├── cdc-ddl-replication-design.md
│   ├── multi-source-execution-engine-design.md
│   ├── multi-source-web-form-design.md
│   └── large-table-chunking-design.md
├── configs/
│   └── config.yaml             # 配置文件模板
├── main.go                     # 程序入口
├── Makefile                    # 构建脚本
├── Dockerfile                  # 多阶段 Docker 构建
├── go.mod
└── go.sum
```

---

## 测试验证

### 测试环境

| 项目 | 配置 |
|------|------|
| 构建平台 | Windows/amd64, Go 1.26.3 |
| PostgreSQL | 16.13 |
| TiDB | v7.1.9 |
| 测试数据库 | mydb (20 张表 + 1 视图) |
| Lightning | tidb-lightning (local backend) |

### Pre-check 结果

```
5 PASS, 3 WARN, 0 FAIL
- pg-connection: PASS (PostgreSQL 16.13)
- tidb-connection: PASS (TiDB v7.1.9)
- disk-space: PASS
- pg-schema-permission: PASS
- collation: PASS (UTF)
- WARN: 1 trigger (trg_update_ts)
- WARN: 1 stored function (update_timestamp)
- WARN: 1 enum type (mood)
```

### Schema 迁移结果

```
20 tables + 1 view 迁移成功
- 19 张表含主键，1 张无主键表 (no_pk_table)
- 3 个索引 (unique, normal, composite)
- 1 个外键 (fk_child → fk_parent)
- 1 个视图 (v_summary)
- 2 个不支持对象: trigger trg_update_ts, function update_timestamp
```

### Data 迁移 + 校验结果

| 表名 | PG 行数 | TiDB 行数 | 校验模式 | 状态 |
|------|---------|----------|----------|------|
| array_types | 4 | 4 | sample (L2) | ✅ PASS |
| basic_types | 3 | 3 | sample (L2) | ✅ PASS |
| composite_pk | 3 | 3 | sample (L2) | ✅ PASS |
| constraint_test | 3 | 3 | sample (L2) | ✅ PASS |
| custom_type_test | 0 | 0 | sample (L2) | ✅ PASS |
| dec_only_nopk | 3 | 3 | hash group | ✅ PASS |
| duplicate_rows_table | 20 | 20 | hash group | ✅ PASS |
| empty_table | 0 | 0 | sample (L2) | ✅ PASS |
| no_pk_table | 20 | 20 | hash group | ✅ PASS |
| composite_pk (复合主键) | 3 | 3 | sample (L2) | ✅ PASS |

L2 抽样校验: **全部 PASS**（含 DECIMAL 精度、TIMESTAMPTZ 时区、复合主键、无PK表 hash 对比）

---

## 开发

```bash
# 格式化
make fmt

# 静态检查
make vet

# 运行测试
make test

# 测试覆盖率
make test-cover

# 构建（仅 CLI）
make build

# 构建（含 Web UI，需 Node.js 20+）
make build-web

# 或使用脚本
bash build-web.sh

# 仅构建前端
make web-frontend

# 前端开发服务器
cd web/frontend && npm run dev
```

---

## 产出文件

| 文件 | 说明 |
|------|------|
| `<temp_dir>/*.csv` | 每张表的导出数据 |
| `<temp_dir>/lightning.toml` | 自动生成的 Lightning 配置 |
| `.checkpoint/` | 断点续传进度 |
| `precheck-report.json` | 预检查报告 |
| `validation-report.json` | 数据校验报告 |
| `unsupported-objects.log` | 不兼容对象清单 |
| `schema.sql`（dry-run 时） | 生成的 DDL 文件 |

---

## 常见问题

**Q: 支持哪些 PG 版本？**
A: PostgreSQL 10+ 版本。

**Q: 支持哪些数据源？**
A: 当前 PostgreSQL 为生产可用状态，MySQL 正在开发中（Dumpling 导出），Oracle/SqlServer/DB2 已注册 Stub 接口，后续逐步实现。通过 `--source` 全局 flag 选择数据源类型。

**Q: 什么是 assess（兼容性评估）？**
A: `timstool assess` 命令在迁移前扫描源端数据库，检查不兼容对象（Trigger、Stored Function、特殊索引、枚举类型等），输出风险报告。支持 `--format terminal|json` 选择输出格式。

**Q: 「数据比对」页面和迁移流水线里的数据校验有什么区别？**
A: 数据校验（validate）是迁移流水线的第 4 阶段，依赖迁移任务上下文；「数据比对」页面是**独立比对**能力——手工填写源端/目标端连接即可对任意两端数据比对，无需创建迁移任务。支持 quick / sample / checksum 三种模式、表级选择、比对历史与差异报告；连接配置可保存/加载复用（密码永不落盘）。同一时刻仅允许一个比对任务运行。API 见「独立数据比对」端点一节。

**Q: 大表迁移如何优化？**
A: timstool 支持大表分片拆分（见 `docs/large-table-chunking-design.md`），将大表按主键范围拆分为多个分片并行导出，提升迁移效率。

**Q: 支持增量同步吗？**
A: 支持，但 CDC 增量同步是**可选模块、默认关闭**（`cdc.enable: false`）；需零停机增量同步时显式开启（`cdc.enable: true` / `--enable-cdc` / `PG2TIDB_CDC_FORCE=1`，见 [CDC 增量同步（可选模块）](#cdc-增量同步可选模块)）。开启后通过 `timstool cdc` 运行（或 Web 仪表盘「启动 CDC」一键启停），覆盖 INSERT/UPDATE/DELETE、事务顺序性、NULL/特殊字符/TOAST；`sync_ddl: true`（默认）同时复制源端 DDL（CREATE/ALTER/DROP TABLE + 类型映射）；带 checkpoint-on-failure 防丢数据（at-least-once 重启重读）+ 无 PK 表结构性 halt。Web 端有 CDC 监控仪表盘（`timstool web`，一键启停 + 实时状态/LSN/吞吐，见 [docs/cdc-web-monitoring-contract.md](docs/cdc-web-monitoring-contract.md)）。

**Q: 大表迁移性能如何？**
A: PG 导出通常 50-200 MB/s，TiDB Lightning 导入 200-500 MB/s，整体瓶颈通常在 PG 导出端。

**Q: 必须安装 tidb-lightning 吗？**
A: 如果使用内置 Lightning 的构建版本（推荐），无需手动安装。否则需要安装 tidb-lightning 到系统 PATH。如果都不可用，工具会自动回退到流式 INSERT 方式。也可以设置 `use_lightning: false` 直接使用流式 INSERT。

**Q: 存储过程、触发器会迁移吗？**
A: 不会自动迁移。Pre-check 阶段会扫描并输出兼容性报告，标注不兼容对象和迁移建议。

**Q: 迁移过程中单张表失败怎么办？**
A: 配置 `on_error: skip` 跳过失败表继续迁移，最终报告汇总所有失败表。默认 `abort` 遇错即停。

**Q: 目标表已有数据怎么处理？**
A: 通过 `target_policy` 配置：`insert`（直接插入，遇主键冲突失败）、`truncate`（先清空目标表再插入）、`drop`（先 DROP 再重建）。

**Q: Web 模式的数据存在哪里？**
A: 任务数据存储在 SQLite 数据库中（默认 `.timstool/tasks.db`），可通过 `--data` 参数指定目录。

---

## 性能调优建议

1. **提高导出并行度** — `migration.parallel` 设为 CPU 核数的 1-2 倍
2. **使用 Lightning Local Backend** — `migration.use_lightning: true` 是最快导入模式
3. **确保 PD 可达** — `target.pd_addr` 正确配置 PD 地址
4. **确保磁盘 IO** — 临时目录使用高速磁盘（SSD/NVMe）
5. **TiDB 集群规模** — Lightning 导入速度与 TiKV 节点数正相关
6. **网络带宽** — PG 到中间机器、中间机器到 TiDB 的网络带宽是关键瓶颈

---

## License

MIT
