<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import apiClient from '../api'
import type { DataSource, Task } from '../api'
import { isCompareTask } from '../api'
import PageHeader from '../components/PageHeader.vue'

// B 二期「管线拓扑」（/pipeline，样张 C:\t-work\pipeline-mockup.html）：只读汇聚
// 三个既有 GET——/api/v1/datasources · /api/v1/tasks · /api/v1/ddl-compare/tasks，
// 零后端逻辑/接口改动（只读红线同 Dashboard）。
//
// 数据诚实性红线（leader 19:43 派单）：/api/v1/tasks 线上形状不携带
// source/target 字段（store.Task ConfigJSON json:"-" 防泄密），migration /
// mongocompare 的流向 FE 侧不可推导——严禁编造连线；真实边只来自 DDL 对比
// 任务的 source_label/target_label（格式 host/schema），推不出的任务聚入
// 底部注记「n 个任务未标注流向」。

const router = useRouter()

const datasources = ref<DataSource[]>([])
const tasks = ref<Task[]>([])
const ddlTasks = ref<any[]>([])
const loading = ref(false)
let pollTimer: number | undefined

// 30s 自动轮询（不动 App.vue 既有 5s 心跳）+ 手动刷新。
async function refresh() {
  loading.value = true
  const [ds, tk, dc] = await Promise.allSettled([
    apiClient.listDataSources(),
    apiClient.listTasks(),
    apiClient.ddlCompareTasks(),
  ])
  if (ds.status === 'fulfilled') datasources.value = ds.value.data.datasources || []
  if (tk.status === 'fulfilled') tasks.value = tk.value.data || []
  if (dc.status === 'fulfilled') ddlTasks.value = dc.value.data.tasks || []
  loading.value = false
}

// 浮层 Esc 关闭（document 级：焦点在节点或浮层内均生效）。
const openNode = ref<PlNode | null>(null)
function onDocKey(e: KeyboardEvent) {
  if (e.key === 'Escape') openNode.value = null
}

onMounted(() => {
  refresh()
  pollTimer = window.setInterval(refresh, 30000)
  document.addEventListener('keydown', onDocKey)
})
onUnmounted(() => {
  window.clearInterval(pollTimer)
  document.removeEventListener('keydown', onDocKey)
})

// ---- 类型档案（连接器类型色与展示名，同 Dashboard 四档） ----
const typeMeta: Record<string, { label: string; short: string }> = {
  postgres: { label: 'PostgreSQL', short: 'PG' },
  mysql: { label: 'MySQL', short: 'MySQL' },
  tidb: { label: 'TiDB', short: 'TiDB' },
  mongodb: { label: 'MongoDB', short: 'Mongo' },
}
const typeLabel = (t: string) => typeMeta[t]?.label || t || '—'

// ddl label 格式 host/schema（dcmpEndpointLabel）。
const hostOf = (label: string) => String(label || '').split('/')[0] || ''
const schemaOf = (label: string) => {
  const i = String(label || '').indexOf('/')
  return i >= 0 ? String(label).slice(i + 1) : ''
}
const isDdlFailed = (t: any) => t.overall === 'fail' || t.status === 'failed'

// ---- 画布常量（样张坐标系：左源 x42/w220，右目 x862/w170） ----
const VW = 1056

interface PlNode {
  key: string
  host: string
  name: string
  type: string
  virtual: boolean
  column: 'source' | 'target'
  asSource: boolean
  asTarget: boolean
  x: number
  y: number
  w: number
  h: number
  srcCount: number
  tgtCount: number
  inEdges: number
  failed: number
  schemas: Set<string>
  latest: any | null
  lastTestOk?: boolean
  lastTested?: string
}

interface PlEdge {
  fromKey: string
  toKey: string
  label: string
  tone: 'pass' | 'warn' | 'fail' | 'running'
  d: string
  lx: number
  ly: number
}

const graph = computed<{ nodeList: PlNode[]; edges: PlEdge[]; canvasH: number }>(() => {
  // 真边分组：仅 DDL 对比任务（source_label/target_label）；自环（同 host
  // 对比）不画线，计数仍进 chips 与节点统计。
  const groups = new Map<string, { sh: string; th: string; tasks: any[] }>()
  for (const t of ddlTasks.value) {
    const sh = hostOf(t.source_label)
    const th = hostOf(t.target_label)
    if (!sh || !th || sh === th) continue
    const k = `${sh}→${th}`
    let g = groups.get(k)
    if (!g) {
      g = { sh, th, tasks: [] }
      groups.set(k, g)
    }
    g.tasks.push(t)
  }

  // 节点登记：注册数据源优先（host 取 fields.host，缺省回落 name/id）；
  // DDL 出现而未登记的 host → 虚线「临时连接」虚拟节点。同 host 多数据源
  // 合并到首个（host 即节点身份）。
  const nodes = new Map<string, PlNode>()
  const register = (host: string, key: string, name: string, type: string, virtual: boolean): PlNode => {
    let n = nodes.get(host)
    if (!n) {
      n = {
        key, host, name, type, virtual,
        column: 'source', asSource: false, asTarget: false,
        x: 0, y: 0, w: 220, h: 80,
        srcCount: 0, tgtCount: 0, inEdges: 0, failed: 0,
        schemas: new Set<string>(), latest: null,
      }
      nodes.set(host, n)
    }
    return n
  }
  for (const d of datasources.value) {
    const host = String(d.fields?.host || d.name || d.id)
    const n = register(host, d.id, d.name, d.type, false)
    n.lastTestOk = d.last_test_ok
    n.lastTested = d.last_tested
  }
  for (const g of groups.values()) {
    if (!nodes.has(g.sh)) register(g.sh, `adhoc-${g.sh}`, `${g.sh}（临时连接）`, g.tasks[0].source_type || 'mysql', true)
    if (!nodes.has(g.th)) register(g.th, `adhoc-${g.th}`, `${g.th}（临时连接）`, g.tasks[0].target_type || 'tidb', true)
  }

  // 列位：有入线 → 右（目标）；有出线 → 左（源）；无线时 TiDB 归右（迁移
  // 工具目标位惯例），其余归左——纯布局约定，不伪造流向。
  for (const g of groups.values()) {
    nodes.get(g.sh)!.asSource = true
    nodes.get(g.th)!.asTarget = true
  }
  for (const n of nodes.values()) {
    n.column = n.asTarget ? 'target' : n.asSource ? 'source' : n.type === 'tidb' ? 'target' : 'source'
  }

  // 节点统计（源端/目标端各自计，最近一条 DDL 全量取最新）。
  const touchLatest = (n: PlNode, t: any) => {
    if (!n.latest || (t.created_at || '') > (n.latest.created_at || '')) n.latest = t
  }
  for (const t of ddlTasks.value) {
    const s = nodes.get(hostOf(t.source_label))
    if (s) {
      s.srcCount++
      const sc = schemaOf(t.source_label)
      if (sc) s.schemas.add(sc)
      if (isDdlFailed(t)) s.failed++
      touchLatest(s, t)
    }
    const tg = nodes.get(hostOf(t.target_label))
    if (tg) {
      tg.tgtCount++
      touchLatest(tg, t)
    }
  }

  // 布局：左源右目两层堆叠（虚拟节点缀尾）。
  const left = [...nodes.values()].filter((n) => n.column === 'source')
  const right = [...nodes.values()].filter((n) => n.column === 'target')
  let y = 40
  for (const n of left) {
    n.w = 220
    n.h = n.virtual ? 62 : 80
    n.x = 42
    n.y = y
    y += n.h + 32
  }
  const leftBottom = y
  y = 40
  for (const n of right) {
    n.w = 170
    n.h = 86
    n.x = 862
    n.y = y
    y += n.h + 32
  }
  const canvasH = Math.max(leftBottom, y, 200) + 16

  // 连线：贝塞尔 + 箭头；色 = 最近结论（pass/hint→teal 健康、warn→amber 差异、
  // fail→red 失败；hint 非差异非失败归健康档，suffix 仍披露「最近提示」）。
  const edges: PlEdge[] = []
  for (const g of groups.values()) {
    const from = nodes.get(g.sh)!
    const to = nodes.get(g.th)!
    to.inEdges++
    const sorted = g.tasks.slice().sort((a, b) => (b.created_at || '').localeCompare(a.created_at || ''))
    const latest = sorted[0] || null
    const failed = g.tasks.filter(isDdlFailed).length
    const running = g.tasks.some((t) => t.status === 'running')
    const tone: PlEdge['tone'] = running
      ? 'running'
      : latest && (latest.overall === 'fail' || latest.status === 'failed')
        ? 'fail'
        : latest && latest.overall === 'warn'
          ? 'warn'
          : 'pass'
    const suffix = running
      ? ' · 运行中'
      : latest && (latest.overall === 'fail' || latest.status === 'failed')
        ? ' · 最近失败'
        : failed > 0
          ? ` · ${failed} 失败`
          : latest?.overall === 'warn'
            ? ' · 最近警告'
            : latest?.overall === 'hint'
              ? ' · 最近提示'
              : latest?.overall === 'pass'
                ? ' · 最近通过'
                : ''
    const sx = from.x + from.w
    const sy = from.y + from.h / 2
    const tx = to.x
    const ty = to.y + to.h / 2
    edges.push({
      fromKey: from.key,
      toKey: to.key,
      label: `DDL 对比 ${g.tasks.length}${suffix}`,
      tone,
      d: `M ${sx} ${sy} C ${sx + 180} ${sy}, ${tx - 180} ${ty}, ${tx} ${ty}`,
      lx: (sx + tx) / 2,
      ly: (sy + ty) / 2 - 8,
    })
  }

  return { nodeList: [...nodes.values()], edges, canvasH }
})

const nodeList = computed(() => graph.value.nodeList)
const edges = computed(() => graph.value.edges)
const canvasH = computed(() => graph.value.canvasH)

// 目标卡顶条：入线最差结论（fail > warn > pass）。
const topbarTone = (n: PlNode) => {
  const ts = edges.value.filter((e) => e.toKey === n.key).map((e) => e.tone)
  if (ts.includes('fail')) return 'fail'
  if (ts.includes('warn')) return 'warn'
  return 'pass'
}

const subText = (n: PlNode) =>
  n.column === 'target'
    ? `入线 ${n.inEdges} · DDL 对比 ${n.tgtCount}`
    : n.srcCount > 0
      ? `DDL 对比 ${n.srcCount} · ${n.schemas.size} schema`
      : n.virtual
        ? '未登记数据源'
        : '暂无标注任务'

// ---- 汇总条 6 chips（数字全从三 GET 推导；点击跳对应页） ----
const migCount = computed(() => tasks.value.filter((t) => !isCompareTask(t)).length)
const mongoCount = computed(() => tasks.value.filter((t) => isCompareTask(t)).length)
const runningCount = computed(
  () =>
    tasks.value.filter((t) => t.status === 'running').length +
    ddlTasks.value.filter((t) => t.status === 'running').length,
)
// leader 终门裁决①（20:25）：失败 chip 回归任务态口径（mockup 批准口径 13，
// 与点击落 /tasks 实显一致）；DDL 结论失败由连线红档/目标顶条/浮层「最近失败」三处披露。
const failCount = computed(() => tasks.value.filter((t) => t.status === 'failed').length)
// 推不出流向的任务（迁移 + Mongo 对比）→ 底部注记 chip，不画线。
const unlabeledCount = computed(() => migCount.value + mongoCount.value)

const chips = computed(() => [
  { key: 'ds', label: '数据源', value: datasources.value.length, to: '/datasources', tone: '' },
  { key: 'mig', label: '迁移任务', value: migCount.value, to: '/tasks', tone: '' },
  { key: 'mongo', label: 'Mongo 对比', value: mongoCount.value, to: '/tasks', tone: '' },
  { key: 'ddl', label: 'DDL 对比', value: ddlTasks.value.length, to: '/ddl-compare', tone: '' },
  { key: 'running', label: '运行中', value: runningCount.value, to: '/tasks', tone: 'teal' },
  { key: 'failed', label: '失败任务', value: failCount.value, to: '/tasks', tone: 'red' },
])

// ---- 浮层 ----
const ddlVerdictText = (v?: string) =>
  v === 'pass' ? '通过' : v === 'hint' ? '提示' : v === 'warn' ? '警告' : v === 'fail' ? '失败' : '—'
const latestText = (n: PlNode) =>
  n.latest ? `${ddlVerdictText(n.latest.overall)} · ${fmtTime(n.latest.created_at)}` : '—'
const latestClass = (n: PlNode) =>
  n.latest
    ? n.latest.overall === 'fail' || n.latest.status === 'failed'
      ? 'is-fail'
      : n.latest.overall === 'warn'
        ? 'is-warn'
        : 'is-pass'
    : ''
const connText = (n: PlNode) => (n.lastTestOk ? '正常' : n.lastTested ? '异常' : '—')

function fmtTime(s?: string): string {
  if (!s) return '-'
  return s.replace('T', ' ').slice(0, 16)
}

// 浮层定位：SVG viewBox 等比缩放 → 百分比锚定；目标列节点浮层翻到左侧。
const popStyle = computed(() => {
  const n = openNode.value
  if (!n) return {}
  const topPx = Math.min(n.y + 4, Math.max(8, canvasH.value - 190))
  const side =
    n.column === 'target'
      ? { right: `${(((VW - n.x) / VW) * 100).toFixed(2)}%` }
      : { left: `${(((n.x + n.w) / VW) * 100).toFixed(2)}%` }
  return { ...side, top: `${((topPx / canvasH.value) * 100).toFixed(2)}%` }
})
</script>

<template>
  <div class="tims-page">
    <PageHeader title="管线拓扑" subtitle="数据源与任务流向一张图：节点/连线实时取自本机服务（只读，30s 自动刷新）">
      <template #actions>
        <el-button data-testid="pl-refresh" :loading="loading" @click="refresh">
          <el-icon><Refresh /></el-icon>&nbsp;手动刷新
        </el-button>
      </template>
    </PageHeader>

    <!-- ① 顶部汇总条：6 chips，点击跳对应页 -->
    <div class="pl-chips" data-testid="pl-chips">
      <button
        v-for="c in chips"
        :key="c.key"
        class="pl-chip"
        :data-testid="`pl-chip-${c.key}`"
        @click="router.push(c.to)"
      >
        {{ c.label }}<b class="tims-num" :class="c.tone">{{ c.value }}</b>
      </button>
    </div>

    <!-- ② 拓扑画布：SVG 左源右目 + 贝塞尔连线 + 节点浮层 -->
    <el-card shadow="never" class="pl-card">
      <template #header>
        <div class="pl-card-head">
          <span>数据流向拓扑</span>
          <span class="pl-legend">
            <span><i class="pl-lg pl-lg--pass"></i>健康/完成</span>
            <span><i class="pl-lg pl-lg--warn"></i>有差异</span>
            <span><i class="pl-lg pl-lg--fail"></i>最近失败</span>
            <span><i class="pl-lg pl-lg--running"></i>运行中(流动)</span>
          </span>
        </div>
      </template>

      <div class="pl-canvas-wrap" data-testid="pl-canvas">
        <template v-if="nodeList.length">
          <svg :viewBox="`0 0 ${VW} ${canvasH}`" class="pl-svg" role="img" aria-label="数据流向拓扑图">
            <defs>
              <marker id="pl-arr-pass" markerWidth="8" markerHeight="8" refX="7" refY="3" orient="auto">
                <path d="M0,0 L7,3 L0,6 Z" class="pl-arr pl-arr--pass" />
              </marker>
              <marker id="pl-arr-warn" markerWidth="8" markerHeight="8" refX="7" refY="3" orient="auto">
                <path d="M0,0 L7,3 L0,6 Z" class="pl-arr pl-arr--warn" />
              </marker>
              <marker id="pl-arr-fail" markerWidth="8" markerHeight="8" refX="7" refY="3" orient="auto">
                <path d="M0,0 L7,3 L0,6 Z" class="pl-arr pl-arr--fail" />
              </marker>
              <marker id="pl-arr-running" markerWidth="8" markerHeight="8" refX="7" refY="3" orient="auto">
                <path d="M0,0 L7,3 L0,6 Z" class="pl-arr pl-arr--running" />
              </marker>
            </defs>

            <!-- 连线（贝塞尔 + 箭头；色 = 最近结论） -->
            <template v-for="e in edges" :key="`${e.fromKey}-${e.toKey}`">
              <path
                :d="e.d"
                class="pl-edge"
                :class="`pl-edge--${e.tone}`"
                :marker-end="`url(#pl-arr-${e.tone})`"
                :data-testid="`pl-edge-${e.fromKey}-${e.toKey}`"
              />
              <text :x="e.lx" :y="e.ly" class="pl-edge-label" :class="`pl-edge-label--${e.tone}`" text-anchor="middle">
                {{ e.label }}
              </text>
            </template>

            <!-- 节点（button 语义 + 焦点环 + aria-label；点击开浮层） -->
            <g
              v-for="n in nodeList"
              :key="n.key"
              class="pl-node"
              :class="{ 'pl-node--virtual': n.virtual }"
              tabindex="0"
              role="button"
              :data-testid="`pl-node-${n.key}`"
              :aria-label="`${n.name}（${typeLabel(n.type)}），查看节点详情`"
              @click="openNode = n"
              @keydown.enter.prevent="openNode = n"
              @keydown.space.prevent="openNode = n"
            >
              <rect class="pl-node-card" :x="n.x" :y="n.y" :width="n.w" :height="n.h" rx="10" />
              <rect
                v-if="n.column === 'target'"
                class="pl-node-topbar"
                :class="`pl-node-topbar--${topbarTone(n)}`"
                :x="n.x"
                :y="n.y"
                :width="n.w"
                height="4"
                rx="2"
              />
              <circle class="pl-dot" :class="`pl-dot--${n.type}`" :cx="n.x + 28" :cy="n.y + n.h / 2" r="7" />
              <template v-if="n.column === 'target'">
                <text class="pl-t-name" :x="n.x + 18" :y="n.y + 30">{{ n.name }}</text>
                <text class="pl-t-type" :x="n.x + 18" :y="n.y + 47">{{ typeLabel(n.type) }}</text>
                <text class="pl-t-sub" :x="n.x + 18" :y="n.y + 65">{{ subText(n) }}</text>
              </template>
              <template v-else>
                <text class="pl-t-name" :x="n.x + 46" :y="n.y + (n.virtual ? 27 : 35)">{{ n.name }}</text>
                <text v-if="!n.virtual" class="pl-t-type" :x="n.x + 46" :y="n.y + 53">{{ typeLabel(n.type) }}</text>
                <text class="pl-t-sub" :x="n.x + 46" :y="n.y + (n.virtual ? 44 : 69)">{{ subText(n) }}</text>
              </template>
            </g>
          </svg>

          <!-- 点外关闭（透明覆盖层）+ 节点详情浮层 -->
          <div v-if="openNode" class="pl-overlay" @click="openNode = null"></div>
          <div v-if="openNode" class="pl-pop" :class="{ 'pl-pop--flip': openNode.column === 'target' }" data-testid="pl-pop" :style="popStyle">
            <h4>{{ openNode.name }} · {{ typeLabel(openNode.type) }}</h4>
            <div class="pl-pop-row">
              <span>DDL 对比（源端）</span>
              <b>{{ openNode.srcCount > 0 ? `${openNode.srcCount} 次 · ${openNode.schemas.size} schema` : '—' }}</b>
            </div>
            <div class="pl-pop-row">
              <span>最近 DDL 对比</span>
              <b :class="latestClass(openNode)">{{ latestText(openNode) }}</b>
            </div>
            <div v-if="!openNode.virtual" class="pl-pop-row">
              <span>连接测试</span>
              <b>{{ connText(openNode) }}</b>
            </div>
            <div class="pl-pop-ft">
              <el-button link type="primary" @click="router.push('/tasks')">任务监控 →</el-button>
              <el-button link type="primary" @click="router.push('/ddl-compare')">DDL 对比 →</el-button>
              <el-button link type="primary" @click="router.push('/mongocompare/new')">新建对比 →</el-button>
            </div>
          </div>
        </template>
        <el-empty v-else description="尚无数据源与 DDL 对比记录；登记数据源或发起一次对比即可在这里看到拓扑" />
      </div>

      <!-- ③ 未标注流向降级注记（迁移/Mongo 对比：任务接口不携带源/目标端点） -->
      <button v-if="unlabeledCount > 0" class="pl-note" data-testid="pl-unlabeled" @click="router.push('/tasks')">
        {{ unlabeledCount }} 个任务未标注流向（迁移 {{ migCount }} · Mongo 对比 {{ mongoCount }}）——任务接口不携带源/目标端点，不画臆测连线
      </button>
    </el-card>
  </div>
</template>

<style scoped>
.pl-chips {
  display: flex;
  gap: 12px;
  margin-bottom: 16px;
  flex-wrap: wrap; /* ≤900px chips 换行 */
}
.pl-chip {
  background: var(--tims-card);
  border: 1px solid var(--tims-border);
  border-radius: var(--tims-radius-s);
  padding: 10px 16px;
  font-size: var(--tims-font-xs);
  color: var(--tims-text-2);
  cursor: pointer;
  transition: border-color 0.15s ease;
}
.pl-chip:hover { border-color: var(--tims-teal); }
.pl-chip b {
  display: block;
  font-size: 18px;
  color: var(--tims-text);
  margin-top: 2px;
}
.pl-chip b.teal { color: var(--tims-teal); }
.pl-chip b.red { color: var(--tims-brand); }

.pl-card-head {
  display: flex;
  align-items: center;
  gap: 10px;
  font-weight: 600;
}
.pl-legend {
  margin-left: auto;
  display: flex;
  gap: 14px;
  font-size: 11px;
  font-weight: 400;
  color: var(--tims-text-2);
  align-items: center;
  flex-wrap: wrap;
}
.pl-lg {
  width: 14px;
  height: 3px;
  border-radius: 2px;
  display: inline-block;
  margin-right: 5px;
  vertical-align: 2px;
}
.pl-lg--pass { background: var(--tims-teal); }
.pl-lg--warn { background: var(--tims-amber); }
.pl-lg--fail { background: var(--tims-brand); }
.pl-lg--running {
  width: 16px;
  background: none;
  border-top: 2px dashed var(--tims-text-2);
  border-radius: 0;
}

/* 画布：SVG viewBox 等比整体缩放（width:100% + height:auto）；
   ≤480px 收敛为横向滚动兜底（min-width 保可读），不破版。 */
.pl-canvas-wrap {
  position: relative;
  overflow-x: auto;
}
.pl-svg {
  display: block;
  width: 100%;
  height: auto;
}
@media (max-width: 480px) {
  .pl-svg { min-width: 720px; }
}

/* 连线：色 = 最近结论（teal 健康 / amber 差异 / red 失败 / 灰虚线运行中流动） */
.pl-edge { fill: none; stroke-width: 2.5; }
.pl-edge--pass { stroke: var(--tims-teal); }
.pl-edge--warn { stroke: var(--tims-amber); }
.pl-edge--fail { stroke: var(--tims-brand); }
.pl-edge--running {
  stroke: var(--tims-text-2);
  stroke-width: 2;
  stroke-dasharray: 7 6;
  opacity: 0.8;
  animation: pl-flow 1.2s linear infinite;
}
@keyframes pl-flow {
  from { stroke-dashoffset: 26; }
  to { stroke-dashoffset: 0; }
}
/* 运行中流动动效的 reduced-motion 闸（全局令牌闸之外显式收口） */
@media (prefers-reduced-motion: reduce) {
  .pl-edge--running { animation: none; }
}
.pl-arr--pass { fill: var(--tims-teal); }
.pl-arr--warn { fill: var(--tims-amber); }
.pl-arr--fail { fill: var(--tims-brand); }
.pl-arr--running { fill: var(--tims-text-2); }

.pl-edge-label { font-size: 11px; font-family: var(--tims-font-ui); }
.pl-edge-label--pass { fill: var(--tims-tag-success-text); }
.pl-edge-label--warn { fill: var(--tims-tag-warning-text); }
.pl-edge-label--fail { fill: var(--tims-tag-danger-text); }
.pl-edge-label--running { fill: var(--tims-text-2); }

/* 节点卡：注册数据源实线 / 未登记虚拟虚线；焦点环随主操作色 */
.pl-node { cursor: pointer; outline: none; }
.pl-node-card { fill: var(--tims-card); stroke: var(--tims-border); }
.pl-node--virtual .pl-node-card { fill: var(--tims-table-zebra); stroke-dasharray: 4 3; }
.pl-node:focus-visible .pl-node-card { stroke: var(--tims-teal); stroke-width: 2; }
.pl-node-topbar--pass { fill: var(--tims-teal); }
.pl-node-topbar--warn { fill: var(--tims-amber); }
.pl-node-topbar--fail { fill: var(--tims-brand); }
.pl-dot { fill: #97a0b5; }
.pl-dot--postgres { fill: var(--tims-pg); }
.pl-dot--mysql { fill: var(--tims-mysql); }
.pl-dot--tidb { fill: var(--tims-brand); }
.pl-dot--mongodb { fill: var(--tims-mongo); }
.pl-t-name { font-size: 14px; font-weight: 600; fill: var(--tims-text); font-family: var(--tims-font-ui); }
.pl-t-type { font-size: 11px; fill: var(--tims-text-2); font-family: var(--tims-font-ui); }
.pl-t-sub { font-size: 10px; fill: #97a0b5; font-family: var(--tims-font-ui); }

/* 浮层：点外关闭覆盖层 + 详情卡（样张 .pop 口径） */
.pl-overlay { position: absolute; inset: 0; z-index: 4; }
.pl-pop {
  position: absolute;
  z-index: 5;
  width: 260px;
  margin-left: 12px;
  background: var(--tims-card);
  border: 1px solid var(--tims-border);
  border-radius: var(--tims-radius);
  box-shadow: var(--tims-shadow-lift);
}
.pl-pop--flip { margin-left: 0; margin-right: 12px; }
.pl-pop h4 {
  margin: 0;
  font-size: var(--tims-font-xs);
  color: var(--tims-text);
  padding: 10px 14px 6px;
  border-bottom: 1px solid var(--tims-border);
}
.pl-pop-row {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  font-size: 11px;
  color: var(--tims-text-2);
  padding: 6px 14px;
}
.pl-pop-row b { font-weight: 600; color: var(--tims-text); text-align: right; }
.pl-pop-row b.is-pass { color: var(--tims-tag-success-text); }
.pl-pop-row b.is-warn { color: var(--tims-tag-warning-text); }
.pl-pop-row b.is-fail { color: var(--tims-tag-danger-text); }
.pl-pop-ft {
  border-top: 1px solid var(--tims-border);
  padding: 6px 14px 8px;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

/* 未标注流向降级注记 chip（数据诚实红线：不画臆测连线） */
.pl-note {
  display: block;
  width: fit-content;
  margin: 10px 16px 14px;
  padding: 6px 12px;
  background: var(--tims-table-zebra);
  border: 1px dashed var(--tims-border);
  border-radius: var(--tims-radius-s);
  font-size: var(--tims-font-xs);
  color: var(--tims-text-2);
  cursor: pointer;
}
.pl-note:hover { border-color: var(--tims-teal); color: var(--tims-text); }
</style>
