# MS-10d · MySQL 迁移向导源（Wizard Source）口径册

> 基线 fd45207（V3.18.13）；笔① e991e3e（Go 主干）/笔② 90d04c1（FE）/笔③（门锚+本册）。
> 实现票 seq906 + 审定裁定 seq907 总判：向导 MySQL 管道九成已通（#t79/#t81/#t82 历史铺路），
> 本批真缺口=skip 四开关静默失效（功能缺陷级）+precheck 空+FE 文案。

## 双路架构（#t79 路由，MS-10d 补全 skip 语义）

```
Orchestrator.Run(pipelineCfg)
  ├─ srcType == "postgres"（含空 type legacy 默认）
  │    └─ pg-copy-lightning：precheck→schema(pgx+PG-catalog)→data(COPY)→validate
  │         —— Run:106-152，MS-10d 零触碰（PG 零回归结构钉）
  └─ srcType != "postgres"（现役=mysql）
       └─ source-CIR（runSourceCIR(ctx, pipelineCfg)，MS-10d 起 skip 全兑现）：
            precheck(探活+VERSION 打点) → ReadSchema→CIR → ApplyDDL
            → dumpling 快路|stream 回退 → Lightning → validate(#t82 值级)
```

- 路由前置钉：`config.SourceConfig.SourceType()` 空 type 恒归一 postgres（orchestrator_cir_test.go TestRunSourceCIRRoutingPreconditions）。
- 能力位：mysql meta `Schema:true, Data:true`（CDC:false 合法——CDC epic 边界）。

## skip 四开关兑现表（笔①；webapi:1386-1390 PipelineConfig 直读 cfg.Migration）

| 开关 | PG 路 | source-CIR 路（MS-10d 前→后） |
|---|---|---|
| skip_precheck | 兑现 | **恒 skipped 占位（谎）→ 真 precheck 探活或真 skipped** |
| skip_schema | 兑现 | **静默失效 → 跳整 schema 块（policy drop/truncate+ApplyDDL+进度注册）** |
| skip_data | 兑现 | **静默失效 → 跳导出/导入（tempDir/Lightning 全跳）** |
| skip_validate | 兑现 | **静默失效 → 跳验证（validateSuccess 恒 true，phases 留 skipped 种）** |

- InitPhases 种子=Run 入口 pipelineCfg 真值；「precheck:true」override 已废除（未设开关不再显 skipped——静默谎 bug 类根除）。
- result 语义镜像 PG：只列实际执行 phase。
- precheck 失败=中止（镜像 PG 路语义）；OnErrorContinue 不豁免 precheck（与 PG 路 runPrecheck 行为一致）。
- FE 零分域（裁定 b）：四开关全源有效；skip_precheck 对 MySQL=真跳探活。

## precheck v1 口径（裁定 a 最小形）

- 探活（Connect 已活）+`SELECT VERSION()` 版本打点（adapter 暴露 DB() 时）→ phases 真值。
- **latent（不扩刀）**：结构级告警（权限/字符集/版本地板/大库护栏）。

## 测试 seams 清单（笔①，包级 var，webapi runPipeline 先例同款）

cirOpenSource/cirOpenTargetDB/cirApplyDDL/cirLoadData/cirRunLightning/cirDropTables/cirTruncateTables/cirValidateMigration/cirFindDumpling/cirPrecheck——锚以 fake 驱动全路径（installCIRSeams save/restore）。

## latent 总账

1. **migrator.go PG 专属面（无需翻面记档）**：internal/schema（pgx 直开+pg_catalog 全套 collector+resolvePGType）仅 PG 任务经此；MySQL 不触碰。
2. **precheck 结构级告警**：见上。
3. **CDC gate**：WizardView cdc_chain postgres-only（CapCDC false 守卫，CDCView:65 邻面同批不扩刀）——CDC epic 域。
4. **scoped-CSS hash 机理**（笔②实证）：`data-v-*` 派生 SFC 源内容——**改模板文案亦变 CSS hash**（10b2「CSS 不变」先例的机理边界：彼次未动 WizardView SFC）；字节数恒 382,885、样式语义零变（唯 data-v 段 byte-diff）。
5. **dumpling.Dump 未 seam**：dumpling 快路仅 mysql+二进制在场时进入，锚以 cirFindDumpling="" 强制 stream 路；快路真库验证归黑盒。
