// B 二期「管线拓扑」PipelineView anchors（leader 19:43 派单，样张
// C:\t-work\pipeline-mockup.html）：
// ① 路由 /pipeline（meta.title 管线拓扑）挂「概览」组工作台之后；
// ② 顶部汇总条 6 chips（pl-chips）计数全从三 GET 推导，点击跳对应页；
// ③ SVG 拓扑：左源右目、贝塞尔+箭头连线、色=最近结论、运行中灰虚线 CSS
//    流动（prefers-reduced-motion 禁用）；
// ④ 节点浮层（必做）：button 语义 + 焦点环 + aria-label + Esc/点外关闭
//    + 快捷入口（任务监控 / DDL 对比 / 新建对比）；
// ⑤ 30s 自动轮询三 GET + 手动刷新（不动既有 5s 心跳）；
// ⑥ 数据诚实性红线：连线只来自 ddl-compare 的 source_label/target_label
//    真边（/api/v1/tasks 不携带 source/target 字段），migration/mongocompare
//    聚入「n 个任务未标注流向」底部注记，严禁编造连线；
// ⑦ 只读红线：零新增接口，禁一切写动词、禁裸 fetch。
// Assertion shape = spec exact match（沿用仓库口径）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('B 二期: /pipeline 路由与导航', () => {
  it('router: /pipeline → PipelineView，meta.title 管线拓扑；工作台 / 路由原样', () => {
    const r = src('../router/index.ts')
    expect(r).toContain(`path: '/pipeline',`)
    expect(r).toContain(`name: 'Pipeline',`)
    expect(r).toContain(`import('../views/PipelineView.vue')`)
    expect(r).toContain(`meta: { title: '管线拓扑' },`)
    expect(r).toContain(`path: '/',`)
    expect(r).toContain(`meta: { title: '工作台' },`)
  })

  it('App.vue navSections: 概览组工作台之后挂「管线拓扑」项', () => {
    const s = src('../App.vue')
    expect(s).toContain(`{ path: '/', label: '工作台', icon: 'Odometer' },`)
    expect(s).toContain(`{ path: '/pipeline', label: '管线拓扑', icon: 'Share' },`)
  })
})

describe('B 二期: 顶部汇总条 6 chips', () => {
  it('区块锚 pl-chips + 六档计数（数据源/迁移任务/Mongo 对比/DDL 对比/运行中/失败任务），点击跳对应页', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`data-testid="pl-chips"`)
    expect(s).toContain(`:data-testid="\`pl-chip-\${c.key}\`"`)
    expect(s).toContain(`{ key: 'ds', label: '数据源', value: datasources.value.length, to: '/datasources', tone: '' },`)
    expect(s).toContain(`{ key: 'mig', label: '迁移任务', value: migCount.value, to: '/tasks', tone: '' },`)
    expect(s).toContain(`{ key: 'mongo', label: 'Mongo 对比', value: mongoCount.value, to: '/tasks', tone: '' },`)
    expect(s).toContain(`{ key: 'ddl', label: 'DDL 对比', value: ddlTasks.value.length, to: '/ddl-compare', tone: '' },`)
    expect(s).toContain(`{ key: 'running', label: '运行中', value: runningCount.value, to: '/tasks', tone: 'teal' },`)
    expect(s).toContain(`{ key: 'failed', label: '失败任务', value: failCount.value, to: '/tasks', tone: 'red' },`)
    expect(s).toContain(`@click="router.push(c.to)"`)
  })

  it('计数推导：kind 判别迁移/Mongo；运行中双表合算；失败 chip 回归任务态口径（裁决①）', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`import { isCompareTask } from '../api'`)
    expect(s).toContain(`tasks.value.filter((t) => !isCompareTask(t)).length`)
    expect(s).toContain(`tasks.value.filter((t) => isCompareTask(t)).length`)
    expect(s).toContain(`tasks.value.filter((t) => t.status === 'running').length`)
    expect(s).toContain(`const failCount = computed(() => tasks.value.filter((t) => t.status === 'failed').length)`)
    // 裁决①负锚：失败 chip 不再双表合算 DDL 结论失败（回归 mockup 批准口径 13）。
    expect(s).not.toContain(`tasks.value.filter((t) => t.status === 'failed').length +`)
    expect(s).not.toContain(`ddlTasks.value.filter((t) => t.overall === 'fail' || t.status === 'failed').length`)
  })
})

describe('B 二期: SVG 拓扑画布与连线色档', () => {
  it('viewBox 画布 + 贝塞尔连线 + 箭头 marker + 四档色类（pass/warn/fail/running）', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(':viewBox="`0 0 ${VW} ${canvasH}`"')
    expect(s).toContain(':marker-end="`url(#pl-arr-${e.tone})`"')
    expect(s).toContain('C ${sx + 180} ${sy}, ${tx - 180} ${ty}, ${tx} ${ty}')
    for (const tone of ['pass', 'warn', 'fail', 'running']) {
      expect(s).toContain(`.pl-edge--${tone} {`)
      expect(s).toContain(`.pl-arr--${tone} {`)
    }
  })

  it('运行中连线灰虚线 CSS 流动 + reduced-motion 显式禁用', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain('stroke-dasharray: 7 6;')
    expect(s).toContain('animation: pl-flow 1.2s linear infinite;')
    expect(s).toContain('@keyframes pl-flow {')
    expect(s).toContain('@media (prefers-reduced-motion: reduce) {')
    expect(s).toContain('.pl-edge--running { animation: none; }')
  })

  it('节点：每节点 testid + button 语义 + 焦点环 + aria-label；未登记虚拟节点虚线档', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(':data-testid="`pl-node-${n.key}`"')
    expect(s).toContain('tabindex="0"')
    expect(s).toContain('role="button"')
    expect(s).toContain(':aria-label="`${n.name}（${typeLabel(n.type)}），查看节点详情`"')
    expect(s).toContain('.pl-node:focus-visible .pl-node-card { stroke: var(--tims-teal); stroke-width: 2; }')
    expect(s).toContain(`'pl-node--virtual'`)
    expect(s).toContain('stroke-dasharray: 4 3;')
  })

  it('目标卡顶条 = 入线最差结论（fail > warn > pass）', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`if (ts.includes('fail')) return 'fail'`)
    expect(s).toContain(`if (ts.includes('warn')) return 'warn'`)
    expect(s).toContain(`.pl-node-topbar--fail { fill: var(--tims-brand); }`)
  })

  it('目标卡文案让位类型点：三行 x+46 与源卡同构（B2.1 压字回归锚）', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`:x="n.x + 46" :y="n.y + 30">{{ n.name }}`)
    expect(s).toContain(`:x="n.x + 46" :y="n.y + 47">{{ typeLabel(n.type) }}`)
    expect(s).toContain(`:x="n.x + 46" :y="n.y + 65">{{ subText(n) }}`)
    // B2.1 负锚：目标卡文案起点不再落进类型圆点占位（dot cx=x+28 r=7 → x+21..35）。
    expect(s).not.toContain(`n.x + 18`)
  })
})

describe('B 二期: 节点浮层（必做交互）', () => {
  it('点节点开浮层（pl-pop）：该源任务统计 + 最近结论 + 连接测试 + 快捷入口三链', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`data-testid="pl-pop"`)
    expect(s).toContain('DDL 对比（源端）')
    expect(s).toContain('最近 DDL 对比')
    expect(s).toContain('连接测试')
    expect(s).toContain(`router.push('/tasks')">任务监控 →`)
    expect(s).toContain(`router.push('/ddl-compare')">DDL 对比 →`)
    expect(s).toContain(`router.push('/mongocompare/new')">新建对比 →`)
  })

  it('Esc + 点外关闭：document keydown 闸 + 透明覆盖层', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`if (e.key === 'Escape') openNode.value = null`)
    expect(s).toContain(`document.addEventListener('keydown', onDocKey)`)
    expect(s).toContain(`document.removeEventListener('keydown', onDocKey)`)
    expect(s).toContain(`class="pl-overlay" @click="openNode = null"`)
  })
})

describe('B 二期: 30s 轮询 + 手动刷新', () => {
  it('onMounted 起 30s 轮询，onUnmounted 清；手动刷新按钮 pl-refresh', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain('pollTimer = window.setInterval(refresh, 30000)')
    expect(s).toContain('window.clearInterval(pollTimer)')
    expect(s).toContain(`data-testid="pl-refresh"`)
    expect(s).toContain(`:loading="loading" @click="refresh"`)
  })
})

describe('B 二期: 数据诚实性红线（真边 / 未标注降级）', () => {
  it('连线只来自 DDL 对比任务 source_label/target_label（host/schema 拆解）', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`const sh = hostOf(t.source_label)`)
    expect(s).toContain(`const th = hostOf(t.target_label)`)
    expect(s).toContain(`.split('/')[0]`)
    // 负锚：不得引用 /tasks 线上不存在的 source/target 字段臆测画边。
    expect(s).not.toMatch(/\bt\.source\b/)
    expect(s).not.toMatch(/\bt\.target\b/)
  })

  it('推不出流向的任务聚入底部注记 chip（仅 >0 渲染），文案锚「n 个任务未标注流向」', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`const unlabeledCount = computed(() => migCount.value + mongoCount.value)`)
    expect(s).toContain(`v-if="unlabeledCount > 0"`)
    expect(s).toContain(`个任务未标注流向（迁移 {{ migCount }} · Mongo 对比 {{ mongoCount }}）`)
    expect(s).toContain(`data-testid="pl-unlabeled"`)
  })
})

describe('B 二期: 只读红线与护栏', () => {
  it('只读复用三个既有 GET；禁一切写动词、禁裸 fetch', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain('apiClient.listDataSources()')
    expect(s).toContain('apiClient.listTasks()')
    expect(s).toContain('apiClient.ddlCompareTasks()')
    expect(s).not.toMatch(/apiClient\.(create|update|delete|start|pause|resume|cancel|run|save|import|ddlCompareRun)\w*\(/)
    expect(s).not.toMatch(/fetch\('/)
  })

  it('响应式护栏：SVG width:100% 整体缩放 + chips 换行 + ≤480px 画布横向滚动兜底', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain('display: block;')
    expect(s).toContain('width: 100%;')
    expect(s).toContain('height: auto;')
    expect(s).toContain('flex-wrap: wrap; /* ≤900px chips 换行 */')
    expect(s).toContain('overflow-x: auto;')
    expect(s).toContain('@media (max-width: 480px) {')
    expect(s).toContain('.pl-svg { min-width: 720px; }')
  })
})

// B 三期拓扑微调（刘源 21:08 点单 ④⑤⑥）：浮层披露归并名 / 焦点闭环 /
// 失败 chip 悬浮拆分（DDL 结论失败仅 tooltip，不进计数）。
describe('B 三期: 浮层披露④ + 焦点闭环⑤ + 失败 chip 悬浮拆分⑥', () => {
  it('④ 同主机归并披露：merged 名单入 PlNode，浮层加行「同主机数据源」（空则不渲染）', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`merged: string[]`)
    expect(s).toContain(`n.merged.push(name)`)
    expect(s).toContain(`v-if="openNode.merged.length"`)
    expect(s).toContain(`<span>同主机数据源</span>`)
    expect(s).toContain(`{{ openNode.merged.join('，') }}`)
  })

  it('⑤ 焦点闭环：开浮层焦点入层（tabindex=-1 + focus），关闭归位来源节点 g', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`ref="popEl"`)
    expect(s).toContain(`tabindex="-1"`)
    expect(s).toContain(`const popTrigger = ref<SVGGElement | null>(null)`)
    expect(s).toContain(`popTrigger.value = e.currentTarget as SVGGElement`)
    expect(s).toContain(`if (n) nextTick(() => popEl.value?.focus())`)
    expect(s).toContain(`popTrigger.value.focus()`)
    // 关闭通道原样：Esc 闸 + 点外 overlay（watch 统一归位焦点）。
    expect(s).toContain(`if (e.key === 'Escape') openNode.value = null`)
    expect(s).toContain(`class="pl-overlay" @click="openNode = null"`)
  })

  it('⑥ 失败 chip 悬浮拆分：值维持任务态 13 口径，DDL 结论失败仅 tooltip 披露', () => {
    const s = src('PipelineView.vue')
    expect(s).toContain(`:disabled="c.key !== 'failed'"`)
    expect(s).toContain(`:content="failedChipTip"`)
    expect(s).toContain(`const ddlFailCount = computed(() => ddlTasks.value.filter(isDdlFailed).length)`)
    expect(s).toContain(`任务执行失败 · `)
    expect(s).toContain(`DDL 对比结论失败`)
    // 负锚（裁决①不回归）：chip 计数仍单行任务态，DDL 合算形态全源退场。
    expect(s).toContain(`const failCount = computed(() => tasks.value.filter((t) => t.status === 'failed').length)`)
    expect(s).not.toContain(`tasks.value.filter((t) => t.status === 'failed').length +`)
    expect(s).not.toContain(`ddlTasks.value.filter((t) => t.overall === 'fail' || t.status === 'failed').length`)
  })
})
