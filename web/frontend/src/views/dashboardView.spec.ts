// v3.20 方案A：概览工作台 DashboardView anchors（leader 派单 14:09）:
// ① / 路由由 redirect /wizard 改为 DashboardView（wizard 路由不动）；
// ② 四区块：指标卡（dash-gauges）/ 迁移管线（dash-pipeline）/ DDL 历史
//    （dash-ddl-history）/ MongoDB 对比空状态示例卡（dash-mongo-card）；
// ③ 数据面零新增：只读复用三个既有 GET（datasources / tasks / ddl-compare/tasks），
//    禁一切写动词（零后端逻辑/接口改动红线）。
// Assertion shape = spec exact match（沿用仓库口径）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('v3.20 方案A: / 路由指向概览工作台', () => {
  it('router: / → DashboardView（redirect /wizard 退场；wizard 路由原样）', () => {
    const r = src('../router/index.ts')
    expect(r).toContain(`path: '/',`)
    expect(r).toContain(`name: 'Dashboard',`)
    expect(r).toContain(`import('../views/DashboardView.vue')`)
    expect(r).toContain(`meta: { title: '工作台' },`)
    expect(r).not.toContain(`redirect: '/wizard',`)
    expect(r).toContain(`path: '/wizard',`)
    expect(r).toContain(`import('../views/WizardView.vue')`)
  })
})

describe('v3.20 方案A: DashboardView 四区块', () => {
  it('区块锚：dash-gauges / dash-pipeline / dash-ddl-history / dash-mongo-card', () => {
    const s = src('DashboardView.vue')
    for (const tid of ['dash-gauges', 'dash-pipeline', 'dash-ddl-history', 'dash-mongo-card']) {
      expect(s).toContain(`data-testid="${tid}"`)
    }
  })

  it('指标卡四档：数据源 / 运行中任务 / 迁移任务累计 / 最近 DDL 对比', () => {
    const s = src('DashboardView.vue')
    for (const label of ['数据源', '运行中任务', '迁移任务累计', '最近 DDL 对比']) {
      expect(s).toContain(`tims-gauge-label">${label}</div>`)
    }
  })

  it('迁移管线条：连接器类型色徽章 + 箭头 + 结论胶囊 + 报告链接', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('tims-conn-dot')
    expect(s).toContain(`metaOf(latestDdl.source_type).dot`)
    expect(s).toContain(`metaOf(latestDdl.target_type).dot`)
    expect(s).toContain('dash-pipe-arrow')
    expect(s).toContain(`verdictPill(latestDdl.overall)`)
    expect(s).toContain(`openDdlReport(latestDdl.id, 'html')`)
  })

  it('DDL 历史表：最近 5 条（slice(0, 5)）+ 全部历史入口', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('ddlSorted.value.slice(0, 5)')
    expect(s).toContain(`router.push('/ddl-compare')`)
    expect(s).toContain(`apiClient.ddlCompareReportURL(row.id, 'html')`)
  })

  it('MongoDB 对比空状态示例卡：插画 + 引导按钮；有记录时降级为最近一条', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('class="tims-empty"')
    expect(s).toContain('尚无 MongoDB 对比记录')
    expect(s).toContain(`router.push('/mongocompare/new')`)
    expect(s).toContain('parseCompareResult')
  })
})

describe('v3.20 方案A: Dashboard 数据面红线（零后端改动）', () => {
  it('只读复用三个既有 GET；禁一切写动词', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('apiClient.listDataSources()')
    expect(s).toContain('apiClient.listTasks()')
    expect(s).toContain('apiClient.ddlCompareTasks()')
    expect(s).not.toMatch(/apiClient\.(create|update|delete|start|pause|resume|cancel|run|save|import|ddlCompareRun)\w*\(/)
    // 无新增裸 fetch 接口面（App 心跳轮询不在此文件）。
    expect(s).not.toMatch(/fetch\('/)
  })
})
