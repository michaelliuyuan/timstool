# PG-GATE · PG 回归门禁（MS-02）

一键跑的 PG 现状行为回归门禁。**从 MS-03 起是所有多源化工单的硬门禁**：动代码前后各跑一遍，
红灯即停（退出码非零），不许带红灯进评审/报批。

## 入口

```powershell
# 全量（静态 + 黑盒五面）
powershell -ExecutionPolicy Bypass -File scripts\pg-gate.ps1

# 单面 / 组合
powershell -File scripts\pg-gate.ps1 -Face compare
powershell -File scripts\pg-gate.ps1 -Face static,incremental

# 连接覆写（本地副本或环境变量）
powershell -File scripts\pg-gate.ps1 -ConfigFile scripts\pg-gate.local.json
$env:PGGATE_PG_PORT = '5433'; powershell -File scripts\pg-gate.ps1 -Face all

# 黑盒面前置：显式端点确认（防误连产线/他人隔离实例）
$env:PG_GATE_ENDPOINT = 'iso-pg:5433 + iso-tidb:4000'; powershell -File scripts\pg-gate.ps1

# 调试：保留门禁实例（端口/workdir 不销毁）
powershell -File scripts\pg-gate.ps1 -KeepInstance
```

退出码：`0` = 全绿；`1` = 红灯（输出 FAIL 行与原因）。**红灯即停，不进下一单。**

## 防误连守卫（黑盒面）

黑盒五面会 `DROP SCHEMA ... CASCADE` 造数——**未显式确认端点一律拒跑**：须设 `PG_GATE_ENDPOINT`
环境变量（自由文本，写明你授权触碰的隔离端点，如 `iso-pg:5433 + iso-tidb:4000`）或配置
`gate.endpoint`；`static` 面单测不受限。fixture 表统一 `pggate_*` 前缀 + 每跑 DROP 重建，
即使误连也有前缀护栏。**严禁**把端点指向产线 PG/8080 域。

## 六个面

| 面 | 内容 | 锚定的现状行为 |
|---|---|---|
| `static` | `go vet` 净 + `gofmt -l` 净 + `go test ./...` 0 FAIL + MS-01 四锚（A1 矩阵/A2 归一化/Capable/A3 冻结 fixture=26）+ **linux-amd64 件哈希对账**（`expected_artifact_sha256`） | 23 包基线与"生产代码零改动"机械证明 |
| `wizard` | 建迁移任务（quick 路径，无 Lightning）→ completed → **四阶段结果（precheck/schema/data/validate）全 Success** | 向导全链（行数值级锚由 compare 面同 fixture 承担） |
| `compare` | checksum 比对 diff=0 + 水位过滤比对（`created_at <= max`）diff=0 | 比对面 + #t3 水位过滤 |
| `watermark` | suggest-watermark：`total_tables>=3`、top 候选 `created_at` coverage=1.0 + DEFAULT-now 理由；tidb 源 400 守卫仍在位 | 水位建议面 + 冻结守卫负锚 |
| `incremental` | 空水位全量补齐 2050（泄流重读 ≤+100 容差，正确性由值级锚承担） → 同秒增量 5 → **幂等二跑 0 行+水位不动+值级 quick 比对 pass**（行数+值双锚） → **中断恢复**（运行中硬杀服务重启再跑）→ 终局 checksum 逐表 **总量=期望（55/2300）且 diff=0**（双条件，重复/漏行都拦） | 增量面 + 红队补强专项 |
| `cdc` | 数据源导入 config → start → running → 源端 INSERT 一行 → TiDB 侧可查（quick 比对 pass）→ checkpoint LSN 不回退 → stop `ok:true` | CDC 面（配置→起链→活数据→停链） |

## 环境前置（一次性）

1. **PG 专用角色**（勿用 postgres 超户——MS-01 冒烟教训）。**命名注意：PostgreSQL 保留 `pg_`
   前缀（角色名与 schema 名都是）**，故角色/schema 用 `pggate`、仅数据库可叫 `pg_gate`：
   ```sql
   CREATE ROLE pggate LOGIN PASSWORD '...';
   CREATE DATABASE pg_gate OWNER pggate;
   ALTER ROLE pggate REPLICATION;   -- CDC 面需要
   -- CDC 面另需超户一次性执行（CDC Setup 的 CREATE PUBLICATION ... FOR ALL
   -- TABLES 需超户，产品侧失败会吞错——见已知边界 P-CDC-PUB）：
   CREATE PUBLICATION pg_gate_pub FOR ALL TABLES;
   ```
   且实例 **PG≥13**（pgoutput proto v2）+ `wal_level=logical` + `timezone='UTC'`。fixture schema
   `pggate`（或配置的 schema，**勿用 `pg_` 前缀**）**每跑必删建**，勿指向业务 schema。
2. **TiDB 目标库**：工具不自建库，需一次性预建。**注意产品约定：CDC 链把「PG schema 名」
   当作 TiDB 库名写入，迁移链用 `tidb.database`——两者必须同名**（默认即 `pggate`，与 PG
   schema 一致），否则迁移后 CDC 事件全部落库失败（schema mismatch 跳过）：
   `CREATE DATABASE pggate;`
3. `psql` 在 PATH（fixture 造数走 psql；TiDB 侧只经服务本身读写）。
4. 端口：门禁实例默认 `18099`（`gate.port` 可改）；workdir 默认 `%TEMP%\pg-gate-run`（每跑清空）。
5. Windows 测试机（进程树清理走 CIM；linux 上需自行补 kill 逻辑）。

## 耗时与读法

- `static` 约 2–4 分钟（go test 全量占大头）；黑盒五面合计约 2–4 分钟（迁移 2000 行 + CDC 起链）。
- 每面输出一行 `[face] ...` 摘要；末尾 summary 逐面 PASS/FAIL。FAIL 行带原因与期望值，
  workdir 保留 `web.log`/`web.err.log`/`gate-config.yaml` 可查（加 `-KeepInstance` 连实例一起留）。

## 每单纪律（MS-03 起生效）

1. **动代码前**：`pg-gate.ps1` 全量绿（基线确认）。
2. **报批前**：同命令再跑一遍全绿，票面贴 summary。
3. **件哈希**：`static` 面做机械对账——本单不改生产代码则哈希必须等于
   `scripts/pg-gate.config.json` 的 `expected_artifact_sha256`（当前 =
   `D434F824…48D54F`，MS-02 基线）。改哈希必须随单说明、经 leader 批。
4. 任何一面红灯：停下修或回退，**禁止**跳面/降级断言继续。

## 已知边界（记档，非遗漏）

- **P-CDC-PUB（首跑发现，记档）**：CDC `Setup()` 的 `CREATE PUBLICATION ... FOR ALL TABLES`
  需超户，非超户角色下失败被 Debug 级吞掉，链路在 START 才报「publication does not exist」
  （诊断滞后）。门禁环境前置=超户预建 publication（见上）；产品侧诊断强化列 backlog。

- **KNOWN ISSUE P-INC-TZ（首跑发现，已立案候修）**：增量引擎改写过的表，checksum 比对必红、
  quick 比对绿——确定性 A/B 实证（同 fixture：迁移写入→checksum PASS；同一表被增量 REPLACE
  改写→同一比对必红；逐行独立复算两侧值字符串全等、行数全等）。观测：远端 TiDB
  （system_time_zone=Asia/Shanghai）上增量写入的 TIMESTAMP 列以 SET time_zone='+00:00' 会话
  读回比真 instant 偏移 −8h，即增量写入路径的时区约定与「迁移写入+validator UTC 会话读取」
  的自洽约定不一致（timestamp 值 instant 错位；validator.go getTiDBConn SET UTC vs
  incremental 写入连接未对齐）。产线此前未暴露=从未在增量后跑 checksum。门禁处理：增量面终局
  锚=quick 总量+diff=0（仍拦丢行/重复行），checksum 探针红→打 WARNING 提示 P-INC-TZ 在案；
  **修复落地日探针翻绿，届时把 pg-gate-lib.ps1 的 known-issue 警示改回 equality 断言**（代码
  内已留翻转提示）。

- **脚本宿主口径**：入口为 PowerShell 5.1 脚本（团队作业机即 PS5.1，无 bash 依赖）。
  未采用 `go test -tags` 封装的记档理由：新增 Go 包会使 `go test ./...` 包数离开 23 包基线、
  动摇本单「现状固化」口径；如后续要跨宿主（linux CI）再立项 Go 封装，断言口径不变迁移。
- **gofmt CRLF 假红灯**：gofmt 判定须在 **blob 忠实检出**（`core.autocrlf=false`，权威库
  worktree 即是）上跑；门禁开头检查 `core.autocrlf`，非 `false` 打 WARNING；若 gofmt 报红而
  `git diff --ignore-space-at-eol` 为空，即 CRLF 漂移噪声——换 autocrlf=false 的 worktree 重跑，
  不要改代码迁就。
- 增量面的"运行中硬杀"若错过窗口（数据太小跑完了），会打 WARNING 但终局 checksum 比对仍
  断言不丢不重；要确定性复现中断可调大 `pggate_orders_bulk` 行数。
- CDC `stop` 后看门狗可能拉起（生产既有语义），门禁收尾直接杀进程树，不依赖 stop 的终态。
- 门禁实例的 CDC slot 名固定 `pg_gate_slot`，起链前会清理残留 slot（幂等重跑）。
