// FEAT-DASH-UNIFY anchors（实现令：te 方案 seq=25 A-D 逐项）：
// A 两历史卡七列同构（任务ID/任务名/类型/状态/进度·耗时/时间/操作，
//   列宽模板统一、行高 34、DDL JSON 直链收组保留）；
// B 非表格卡（Mongo/管线）节奏对齐 + 按钮文案统一（卡片级=详情 →，行级=报告/JSON/查看）；
// C 成果速览高亮条（数字全部来自既有只读聚合源，零后端零新接口）；
// D 八项细节：①mono 补齐时间列 ②pill info 蓝档 ③时间 mono 右对齐 ④操作列同款
//   ⑤行高 34 ⑥空态三卡同款 ⑦卡头同款（标题+副题+右侧全部历史） ⑧按钮文案。
// 红线：零后端零新接口；JSON 直链/全部历史/深链/报告链零丢失；水位 tag/胶囊
// 裁定③口径不回退（completed 不误红、运行态不落红档）；CSV/reporter/CompareView 零动。
// Assertion shape = spec exact match（沿用仓库口径）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('FEAT-DASH-UNIFY A: 两历史卡七列同构', () => {
  it('统一列头同集：任务 ID/任务名/类型/状态/进度·耗时/时间/操作（两表逐列同集）', () => {
    const s = src('DashboardView.vue')
    for (const label of ['任务 ID', '任务名', '类型', '状态', '进度·耗时', '时间', '操作']) {
      expect(s).toContain(`label="${label}"`)
    }
    // 负锚：旧列头（链路/结论/耗时/报告/模式/表进度）全部退场
    for (const old of ['label="链路"', 'label="结论"', 'label="耗时"', 'label="报告"', 'label="模式"', 'label="表进度"']) {
      expect(s).not.toContain(old)
    }
  })

  it('列宽模板统一 + 行高 34：两表 dash-tbl 同类、CSS line-height 20 + padding 7', () => {
    const s = src('DashboardView.vue')
    expect((s.match(/class="dash-tbl"/g) || []).length).toBe(2)
    expect(s).toContain('.dash-tbl :deep(td.el-table__cell) { padding-top: 7px; padding-bottom: 7px; }')
    expect(s).toContain('.dash-tbl :deep(.el-table__cell .cell) { line-height: 20px; }')
    // 时间列定宽 150 右对齐（两表同款）
    expect(s).toContain('label="时间" width="150" align="right"')
  })

  it('DDL JSON 直链收组保留（功能零丢失）+ 报告/查看 同款操作组', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain(`apiClient.ddlCompareReportURL(row.id, 'html')`)
    expect(s).toContain(`apiClient.ddlCompareReportURL(row.id, 'json')`)
    expect(s).toContain('class="dash-act" @click="router.push(\'/ddl-compare\')">查看')
    expect(s).toContain('`/compare?task=${row.id}`')
  })
})

describe('FEAT-DASH-UNIFY D②③: pill info 蓝档 + 时间列 mono 右对齐', () => {
  it('design-tokens 新增 .tims-pill--info（#2c6bb3 on #ecf5ff 4.94:1 AA，红队勘正后色值，不占 hint 跨方言语义）', () => {
    const c = src('../styles/design-tokens.css')
    expect(c).toContain('.tims-pill--info { background: #ecf5ff; color: #2c6bb3; }')
    expect(c).not.toContain('#337ecc')
    // 既有四档+中性档原样在位
    for (const cls of ['.tims-pill--pass', '.tims-pill--fail', '.tims-pill--warn', '.tims-pill--hint', '.tims-pill--muted']) {
      expect(c).toContain(cls)
    }
  })

  it('运行中三处统一 info 档（CMP/MONGO 字典 + DDL 兜底）', () => {
    const s = src('DashboardView.vue')
    expect((s.match(/tims-pill--info/g) || []).length).toBe(3) // CMP/MONGO 字典 + DDL 兜底
    expect(s).toContain(`running: { cls: 'tims-pill--info', text: '运行中' },`)
    expect(s).toContain('<span v-else-if="row.status === \'running\'" class="tims-pill tims-pill--info">运行中</span>')
  })

  it('时间列 mono 补齐（tims-mono 全局类在位，时间格挂 mono）', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('dash-muted tims-mono">{{ fmtTime(')
  })
})

describe('FEAT-DASH-UNIFY C: 成果速览高亮条', () => {
  it('区块锚 + 能力 chips（表级耗时/水位增量）+ 四类计数（既有只读源）', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('data-testid="dash-highlights"')
    expect(s).toContain('dash-hl-cap">表级耗时')
    expect(s).toContain('dash-hl-cap">水位增量')
    expect(s).toContain('{{ compareTasks.length }}')
    expect(s).toContain('{{ ddlTasks.length }}')
    expect(s).toContain('{{ tasks.length }}')
    expect(s).toContain('{{ mongoTasks.length }}')
  })
})

describe('FEAT-DASH-UNIFY B/D⑥⑦⑧: 非表格卡对齐 + 空态/卡头/文案统一', () => {
  it('管线按钮文案统一卡片级「详情 →」（查看报告 → 退场）', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain(`openDdlReport(latestDdl.id, 'html')">详情 →`)
    expect(s).not.toContain('查看报告 →')
  })

  it('空态三卡同款：tims-empty（icon+标题+副文+CTA）；el-empty 退场', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('尚无 DDL 对比记录')
    expect(s).toContain('去发起对比')
    expect(s).toContain('尚无数据对比记录')
    expect(s).toContain('尚无 MongoDB 对比记录')
    expect(s).not.toContain('<el-empty')
  })

  it('卡头同款：三历史卡标题+副题（dash-card-sub）+ 右侧「全部历史」', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('最近 DDL 对比任务<span class="dash-card-sub">结构差异校验</span>')
    expect(s).toContain('最近数据对比任务<span class="dash-card-sub">数据一致性校验</span>')
    expect(s).toContain('MongoDB 对比<span class="dash-card-sub">集合级数据一致性比对</span>')
    expect(s).toContain(`router.push('/tasks')">全部历史`)
  })

  it('Mongo 最近一条行与表格行同节奏（min-height 34 + padding 7）', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('min-height: 34px;')
    expect(s).toContain('padding: 7px 2px;')
  })
})

describe('FEAT-DASH-UNIFY 红线: 零后端零新接口 + 递版', () => {
  it('只读聚合面不动：禁一切写动词与裸 fetch', () => {
    const s = src('DashboardView.vue')
    expect(s).not.toMatch(/apiClient\.(create|update|delete|start|pause|resume|cancel|run|save|import|ddlCompareRun)\w*\(/)
    expect(s).not.toMatch(/fetch\('/)
  })

  it('package.json 递版 3.23.6（旧版零残留）', () => {
    const p = readFileSync(resolve(__dirname, '../../package.json'), 'utf8')
    expect(p).toContain('"version": "3.23.6"')
    expect(p).not.toContain('"version": "3.23.5"')
  })
})
