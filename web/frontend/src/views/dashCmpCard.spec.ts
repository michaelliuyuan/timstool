// FEAT-DASH-CMP-CARD anchors（实现令 seq=6）：数据对比接入工作台——
// ① 第五指标卡「数据对比」（值=compare 任务累计；sub 运行中优先）；
// ② 「最近数据对比任务」历史卡（镜像 DDL 历史卡，前 5 条）；
// ③ CompareView ?task=<id> query 钩子自动选中；
// ④ 递版 3.23.4（构建链三件套由 cmd/static 对照与版本扫描另证）。
// 红线：零后端/接口/schema 改动——第五聚合源只读复用既有 GET
// /api/v1/compare/tasks，禁一切写动词；既有四指标卡零回归。
// Assertion shape = spec exact match（沿用仓库口径）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('FEAT-DASH-CMP-CARD ①: 第五指标卡「数据对比」', () => {
  it('gauge 落位：label=数据对比，值=compareTasks 累计数', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('data-testid="dash-cmp-gauge"')
    expect(s).toContain('tims-gauge-label">数据对比</div>')
    expect(s).toContain('{{ compareTasks.length }}')
  })

  it('sub 分流：运行中优先（N 个对比执行中）/ 否则最近一条（最近比对 {时间}）/ 空态', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('if (cmpRunningCount.value > 0) return `${cmpRunningCount.value} 个对比执行中`')
    expect(s).toContain('`最近比对 ${fmtTime(latestCmp.value.created_at)}`')
    expect(s).toContain("return latestCmp.value ? `最近比对 ${fmtTime(latestCmp.value.created_at)}` : '暂无对比'")
    // 既有「运行中任务」卡迁移口径不动（防双计）：compare 卡计数独立源 listCompares。
    expect(s).toContain('apiClient.listCompares()')
  })
})

describe('FEAT-DASH-CMP-CARD ②: 「最近数据对比任务」历史卡', () => {
  it('区块锚 + 前 5 条（created_at 倒序客户端再稳一手）+ 全部历史入口', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('data-testid="dash-cmp-history"')
    expect(s).toContain('recentCmp')
    expect(s).toContain('cmpSorted.value.slice(0, 5)')
    expect(s).toContain(`router.push('/compare')`)
  })

  it('列结构：ID/任务名/模式/状态/表进度/时间/操作（报告+查看）', () => {
    const s = src('DashboardView.vue')
    for (const label of ['任务 ID', '任务名', '模式', '状态', '表进度', '时间', '操作']) {
      expect(s).toContain(`label="${label}"`)
    }
    expect(s).toContain('{{ row.tables_done }}/{{ row.tables_total }}')
    expect(s).toContain('`/compare?task=${row.id}`')
  })

  it('模式文案 quick=快速·sample=采样·checksum=校验和；watermark 存在挂「水位」tag', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain("{ quick: '快速', sample: '采样', checksum: '校验和' }")
    expect(s).toContain('v-if="row.watermark"')
    expect(s).toContain('tims-pill--hint">水位</span>')
  })

  it('状态胶囊四档（running/completed/failed/cancelled）；completed 不误红（D2=v2）', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain("running: { cls: 'tims-pill--muted', text: '运行中' },")
    expect(s).toContain("completed: { cls: 'tims-pill--muted', text: '已完成' },")
    expect(s).toContain("failed: { cls: 'tims-pill--fail', text: '失败' },")
    expect(s).toContain("cancelled: { cls: 'tims-pill--muted', text: '已取消' },")
  })

  it('报告 HTML 直链仅 completed（未完成任务无 report.json）', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain("const cmpReportURL = (id: string) => `/api/v1/compare/tasks/${id}/report?format=html`")
    expect(s).toContain('v-if="row.status === \'completed\'" type="primary" :href="cmpReportURL(row.id)"')
  })

  it('空状态引导同 Mongo 卡风格：插画 + 标题 + 副文 + 去发起按钮', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('尚无数据对比记录')
    expect(s).toContain('发起一次数据比对即可在这里看到结果')
    expect(s).toContain('去发起比对')
    expect(s).toContain('<Histogram />')
  })
})

describe('FEAT-DASH-CMP-CARD ③: CompareView query 钩子', () => {
  it('onMounted 读 route.query.task 命中即 viewTask 选中（数组参容错+router-less 挂载安全）', () => {
    const s = src('CompareView.vue')
    expect(s).toContain("import { useRoute } from 'vue-router'")
    expect(s).toContain('const qid = route?.query?.task')
    expect(s).toContain('String(Array.isArray(qid) ? qid[0] : qid)')
    expect(s).toContain('if (hit) await viewTask(hit)')
  })
})

describe('FEAT-DASH-CMP-CARD ④: 递版与既有面零回归', () => {
  it('package.json 递版 3.23.4', () => {
    const p = readFileSync(resolve(__dirname, '../../package.json'), 'utf8')
    expect(p).toContain('"version": "3.23.4"')
    expect(p).not.toContain('"version": "3.23.3"')
  })

  it('既有四指标卡四档 label 原样在位（第五卡纯增）', () => {
    const s = src('DashboardView.vue')
    for (const label of ['数据源', '运行中任务', '迁移任务累计', '最近 DDL 对比']) {
      expect(s).toContain(`tims-gauge-label">${label}</div>`)
    }
  })

  it('数据面红线：第五源只读 GET；仍禁一切写动词与裸 fetch', () => {
    const s = src('DashboardView.vue')
    expect(s).toContain('apiClient.listCompares()')
    expect(s).not.toMatch(/apiClient\.(create|update|delete|start|pause|resume|cancel|run|save|import|ddlCompareRun)\w*\(/)
    expect(s).not.toMatch(/fetch\('/)
  })
})
