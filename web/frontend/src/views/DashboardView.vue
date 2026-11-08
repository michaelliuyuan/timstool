<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import apiClient from '../api'
import type { DataSource, Task, CompareTask } from '../api'
import { isCompareTask, parseCompareResult } from '../api'
import PageHeader from '../components/PageHeader.vue'

// v3.20 方案A：概览工作台（/ 路由）。只读汇聚三个既有 GET 接口——
// /api/v1/datasources · /api/v1/tasks · /api/v1/ddl-compare/tasks——
// 零后端逻辑/接口改动。四指标卡 + 迁移管线 + 最近 DDL 对比历史 +
// MongoDB 对比空状态示例卡。
// FEAT-DASH-CMP-CARD：第五聚合源 /api/v1/compare/tasks（数据对比独立
// 任务体系，纯 FE 只读接入）——第五指标卡 + 最近数据对比历史卡。
// FEAT-DASH-UNIFY：两历史卡七列同构（任务ID/任务名/类型/状态/进度·耗时/
// 时间/操作）+ 非表格卡节奏对齐 + 成果速览条 + 八项细节统一。纯 FE。

const router = useRouter()

const datasources = ref<DataSource[]>([])
const tasks = ref<Task[]>([])
const ddlTasks = ref<any[]>([])
const compareTasks = ref<CompareTask[]>([])
const loading = ref(true)

onMounted(async () => {
  const [ds, tk, dc, cp] = await Promise.allSettled([
    apiClient.listDataSources(),
    apiClient.listTasks(),
    apiClient.ddlCompareTasks(),
    apiClient.listCompares(),
  ])
  if (ds.status === 'fulfilled') datasources.value = ds.value.data.datasources || []
  if (tk.status === 'fulfilled') tasks.value = tk.value.data || []
  if (dc.status === 'fulfilled') ddlTasks.value = dc.value.data.tasks || []
  if (cp.status === 'fulfilled') compareTasks.value = cp.value.data || []
  loading.value = false
})

// ---- 类型档案：连接器徽章色 + 展示名（PG/MySQL/TiDB/Mongo 四档类型色） ----
const typeMeta: Record<string, { label: string; dot: string; short: string }> = {
  postgres: { label: 'PostgreSQL', dot: 'is-pg', short: 'PG' },
  mysql: { label: 'MySQL', dot: 'is-mysql', short: 'MySQL' },
  tidb: { label: 'TiDB', dot: 'is-tidb', short: 'TiDB' },
  mongodb: { label: 'MongoDB', dot: 'is-mongodb', short: 'Mongo' },
}
const metaOf = (t?: string) => typeMeta[t || ''] || { label: t || '—', dot: 'is-mysql', short: t || '—' }

// ---- 指标卡 ----
const dsSub = computed(() => {
  const counts: Record<string, number> = {}
  for (const d of datasources.value) counts[d.type] = (counts[d.type] || 0) + 1
  const parts = Object.entries(counts).map(([t, n]) => `${n > 1 ? `${metaOf(t).short}×${n}` : metaOf(t).short}`)
  return parts.length ? parts.join(' · ') : '尚未登记'
})
const runningCount = computed(() => tasks.value.filter((t) => t.status === 'running').length)
const completedCount = computed(() => tasks.value.filter((t) => t.status === 'completed').length)

// ---- DDL 对比：最新一条（服务端已按 created_at 倒序，客户端再稳一手） ----
const ddlSorted = computed(() =>
  ddlTasks.value.slice().sort((a: any, b: any) => (b.created_at || '').localeCompare(a.created_at || '')),
)
const latestDdl = computed(() => ddlSorted.value[0] || null)
const recentDdl = computed(() => ddlSorted.value.slice(0, 5))

const verdictPill = (v?: string): string =>
  v === 'pass' ? 'tims-pill--pass' : v === 'hint' ? 'tims-pill--hint' : v === 'warn' ? 'tims-pill--warn' : 'tims-pill--fail'
const verdictText = (v?: string): string => {
  if (v === 'pass') return '通过'
  if (v === 'hint') return '提示'
  if (v === 'warn') return '警告'
  if (v === 'fail') return '失败'
  return '—'
}
const ddlLatestText = computed(() => (latestDdl.value?.overall ? verdictText(latestDdl.value.overall) : '—'))
const ddlLatestTone = computed(() =>
  latestDdl.value?.overall === 'pass' ? 'is-teal' : latestDdl.value?.overall ? 'is-brand' : '',
)

function fmtTime(s?: string): string {
  if (!s) return '-'
  return s.replace('T', ' ').slice(0, 16)
}

function openDdlReport(id: string, format: 'html' | 'json') {
  window.open(apiClient.ddlCompareReportURL(id, format), '_blank')
}

// ---- MongoDB 对比卡：有记录给最近一条结论，无记录渲染空状态（示例卡） ----
const mongoTasks = computed(() => tasks.value.filter((t) => isCompareTask(t)))
const latestMongo = computed(() => {
  const sorted = mongoTasks.value
    .slice()
    .sort((a, b) => (b.finished_at || b.created_at || '').localeCompare(a.finished_at || a.created_at || ''))
  return sorted[0] || null
})
// mongocompare 的结论在 result_json 里（CompareResult.overall），仅 completed
// 且有 result 才有结论；其余按任务 status 分流（leader 裁定①：运行态/未决态
// 不得误落 fail 红档，失败任务标「失败」而非「运行中」）。
const mongoOverallPill = (o?: string): string =>
  o === 'equal' ? 'tims-pill--pass' : o === 'ttl_skip' ? 'tims-pill--hint' : 'tims-pill--fail'
const mongoOverallText = (o?: string): string =>
  o === 'equal' ? '一致' : o === 'ttl_skip' ? 'TTL 跳过' : o === 'diff' ? '存在差异' : o === 'error' ? '错误' : '—'
const MONGO_STATUS_PILL: Record<string, { cls: string; text: string }> = {
  running: { cls: 'tims-pill--info', text: '运行中' },
  failed: { cls: 'tims-pill--fail', text: '失败' },
  created: { cls: 'tims-pill--muted', text: '已创建' },
  paused: { cls: 'tims-pill--muted', text: '已暂停' },
  cancelled: { cls: 'tims-pill--muted', text: '已取消' },
}
const mongoLatestPill = computed<{ cls: string; text: string }>(() => {
  const t = latestMongo.value
  if (!t) return { cls: '', text: '' }
  if (t.status === 'completed') {
    const o = parseCompareResult(t)?.overall
    // 仅完成且有结果才给四档结论胶囊；完成但无 result 走中性兜底。
    return o ? { cls: mongoOverallPill(o), text: mongoOverallText(o) } : { cls: 'tims-pill--muted', text: '已完成' }
  }
  return MONGO_STATUS_PILL[t.status] || { cls: 'tims-pill--muted', text: t.status }
})

// ---- 数据对比（FEAT-DASH-CMP-CARD）：快照式加载，不加 WS/轮询（D1=v2） ----
// 服务端已按 created_at 倒序，客户端再稳一手（与 DDL 历史同口径）。
const cmpSorted = computed(() =>
  compareTasks.value.slice().sort((a, b) => (b.created_at || '').localeCompare(a.created_at || '')),
)
const latestCmp = computed(() => cmpSorted.value[0] || null)
const recentCmp = computed(() => cmpSorted.value.slice(0, 5))
const cmpRunningCount = computed(() => compareTasks.value.filter((t) => t.status === 'running').length)
const cmpSub = computed(() => {
  if (cmpRunningCount.value > 0) return `${cmpRunningCount.value} 个对比执行中`
  return latestCmp.value ? `最近比对 ${fmtTime(latestCmp.value.created_at)}` : '暂无对比'
})
const CMP_MODE_TEXT: Record<string, string> = { quick: '快速', sample: '采样', checksum: '校验和' }
const cmpModeText = (m: string) => CMP_MODE_TEXT[m] || m
// 状态胶囊按任务 status 诚实呈现（结论级胶囊 D2 已裁 v2：completed 不误红）。
// FEAT-DASH-UNIFY D②：运行态统一 info 蓝档（与 muted 灰/结论四档分离，不回退红档）。
const CMP_STATUS_PILL: Record<string, { cls: string; text: string }> = {
  running: { cls: 'tims-pill--info', text: '运行中' },
  completed: { cls: 'tims-pill--muted', text: '已完成' },
  failed: { cls: 'tims-pill--fail', text: '失败' },
  cancelled: { cls: 'tims-pill--muted', text: '已取消' },
}
const cmpStatusPill = (t: CompareTask) => CMP_STATUS_PILL[t.status] || { cls: 'tims-pill--muted', text: t.status }
const cmpReportURL = (id: string) => `/api/v1/compare/tasks/${id}/report?format=html`
</script>

<template>
  <div class="tims-page">
    <PageHeader title="工作台" subtitle="源端到 TiDB 的迁移、校验与同步总览，数字实时取自本机服务">
      <template #actions>
        <el-button type="primary" @click="router.push('/wizard')">
          <el-icon><Plus /></el-icon> 新建迁移
        </el-button>
      </template>
    </PageHeader>

    <!-- ① 四指标卡（数据源 / 运行中任务 / 迁移任务累计 / 最近 DDL 对比结论） -->
    <div class="dash-gauges" v-loading="loading" data-testid="dash-gauges">
      <div class="tims-gauge">
        <div class="tims-gauge-label">数据源</div>
        <div class="tims-gauge-value">{{ datasources.length }}</div>
        <div class="tims-gauge-sub">{{ dsSub }}</div>
      </div>
      <div class="tims-gauge">
        <div class="tims-gauge-label">运行中任务</div>
        <div class="tims-gauge-value" :class="{ 'is-teal': runningCount > 0 }">{{ runningCount }}</div>
        <div class="tims-gauge-sub">{{ runningCount > 0 ? `${runningCount} 个任务执行中` : '全部空闲' }}</div>
      </div>
      <div class="tims-gauge">
        <div class="tims-gauge-label">迁移任务累计</div>
        <div class="tims-gauge-value">{{ tasks.length }}</div>
        <div class="tims-gauge-sub">{{ tasks.length ? `已完成 ${completedCount}` : '暂无任务' }}</div>
      </div>
      <div class="tims-gauge">
        <div class="tims-gauge-label">最近 DDL 对比</div>
        <div class="tims-gauge-value" :class="ddlLatestTone">{{ ddlLatestText }}</div>
        <div class="tims-gauge-sub">
          {{ latestDdl ? `${latestDdl.id}${latestDdl.duration ? ' · ' + latestDdl.duration : ''}` : '暂无对比记录' }}
        </div>
      </div>
      <!-- FEAT-DASH-CMP-CARD ① 第五指标卡：数据对比（值=累计任务数；运行态优先 sub） -->
      <div class="tims-gauge" data-testid="dash-cmp-gauge">
        <div class="tims-gauge-label">数据对比</div>
        <div class="tims-gauge-value" :class="{ 'is-teal': cmpRunningCount > 0 }">{{ compareTasks.length }}</div>
        <div class="tims-gauge-sub">{{ cmpSub }}</div>
      </div>
    </div>

    <!-- FEAT-DASH-UNIFY C：成果速览高亮条（数字全部来自既有只读聚合源，零后端零新接口） -->
    <div class="dash-hl" data-testid="dash-highlights">
      <span class="dash-hl-label">成果速览</span>
      <span class="dash-hl-cap">表级耗时</span>
      <span class="dash-hl-cap">水位增量</span>
      <span class="dash-hl-item">数据对比 <b>{{ compareTasks.length }}</b> 次</span>
      <span class="dash-hl-item">DDL 校验 <b>{{ ddlTasks.length }}</b> 次</span>
      <span class="dash-hl-item">迁移 <b>{{ tasks.length }}</b> 批</span>
      <span class="dash-hl-item">Mongo 比对 <b>{{ mongoTasks.length }}</b> 次</span>
    </div>

    <!-- ② 迁移管线条：源 → 目标（最近一次 DDL 对比口径；PG 蓝 / TiDB 红连接器徽章） -->
    <el-card v-if="latestDdl" shadow="never" class="dash-card" data-testid="dash-pipeline">
      <template #header>
        <span style="font-weight: 600;">迁移管线<span class="dash-card-sub">源 → 目标（最近一次 DDL 对比口径）</span></span>
      </template>
      <div class="dash-pipe">
        <div class="dash-conn">
          <span class="tims-conn-dot" :class="metaOf(latestDdl.source_type).dot">
            <el-icon :size="18"><Coin /></el-icon>
          </span>
          <span class="dash-conn-text">
            <span class="dash-conn-name">{{ latestDdl.source_label }}</span>
            <span class="dash-conn-meta">{{ metaOf(latestDdl.source_type).label }} · {{ latestDdl.source_schema }}</span>
          </span>
        </div>
        <div class="dash-pipe-mid">
          <el-icon class="dash-pipe-arrow"><Right /></el-icon>
          <span>{{ latestDdl.source_schema }} → {{ latestDdl.target_schema }}</span>
        </div>
        <div class="dash-conn">
          <span class="tims-conn-dot" :class="metaOf(latestDdl.target_type).dot">
            <el-icon :size="18"><Lightning /></el-icon>
          </span>
          <span class="dash-conn-text">
            <span class="dash-conn-name">{{ latestDdl.target_label }}</span>
            <span class="dash-conn-meta">{{ metaOf(latestDdl.target_type).label }} · {{ latestDdl.target_schema }}</span>
          </span>
        </div>
        <span class="tims-pill" :class="verdictPill(latestDdl.overall)">{{ verdictText(latestDdl.overall) }}</span>
        <el-button link type="primary" @click="openDdlReport(latestDdl.id, 'html')">详情 →</el-button>
      </div>
    </el-card>

    <!-- ③ 最近 5 条 DDL 对比历史（FEAT-DASH-UNIFY A：与数据对比卡同构七列模板） -->
    <el-card shadow="never" class="dash-card" data-testid="dash-ddl-history">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span style="font-weight: 600;">最近 DDL 对比任务<span class="dash-card-sub">结构差异校验</span></span>
          <el-button size="small" @click="router.push('/ddl-compare')">全部历史</el-button>
        </div>
      </template>
      <div v-if="recentDdl.length > 0" class="tims-table-scroll">
        <el-table stripe :data="recentDdl" size="small" class="dash-tbl">
          <el-table-column label="任务 ID" width="110">
            <template #default="{ row }"><span class="tims-mono">{{ row.id }}</span></template>
          </el-table-column>
          <el-table-column label="任务名" min-width="220" show-overflow-tooltip>
            <template #default="{ row }">{{ row.name || '（未命名）' }}</template>
          </el-table-column>
          <el-table-column label="类型" width="150">
            <template #default="{ row }">
              {{ metaOf(row.source_type).short }} → {{ metaOf(row.target_type).short }}
            </template>
          </el-table-column>
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <span v-if="row.overall" class="tims-pill" :class="verdictPill(row.overall)">{{ verdictText(row.overall) }}</span>
              <!-- 裁定③：仅 error/failed 落红「失败」；运行中/未决走中性胶囊，不再误红兜底。 -->
              <el-tooltip v-else-if="row.error || row.status === 'failed'" :content="row.error || '失败'" placement="top">
                <span class="tims-pill tims-pill--fail">失败</span>
              </el-tooltip>
              <span v-else-if="row.status === 'running'" class="tims-pill tims-pill--info">运行中</span>
              <span v-else class="tims-pill tims-pill--muted">—</span>
            </template>
          </el-table-column>
          <el-table-column label="进度·耗时" width="110" class-name="hide-sm">
            <template #default="{ row }"><span class="dash-muted tims-mono">{{ row.duration || '—' }}</span></template>
          </el-table-column>
          <el-table-column label="时间" width="150" align="right" class-name="hide-sm">
            <template #default="{ row }"><span class="dash-muted tims-mono">{{ fmtTime(row.created_at) }}</span></template>
          </el-table-column>
          <el-table-column label="操作" width="150">
            <template #default="{ row }">
              <!-- 报告 HTML/JSON 直链仅 completed；JSON 收组进操作列（功能保留） -->
              <template v-if="row.status === 'completed'">
                <el-link type="primary" :href="apiClient.ddlCompareReportURL(row.id, 'html')" target="_blank">报告</el-link>
                <el-link type="primary" class="dash-act" :href="apiClient.ddlCompareReportURL(row.id, 'json')" target="_blank">JSON</el-link>
              </template>
              <span v-else class="dash-muted">—</span>
              <el-button size="small" link type="primary" class="dash-act" @click="router.push('/ddl-compare')">查看</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <div v-else class="tims-empty">
        <el-icon class="tims-empty-icon" :size="42"><Document /></el-icon>
        <div class="tims-empty-title">尚无 DDL 对比记录</div>
        <div class="tims-empty-sub">发起一次结构对比即可在这里看到结果</div>
        <el-button type="primary" size="small" style="margin-top: 4px;" @click="router.push('/ddl-compare')">去发起对比</el-button>
      </div>
    </el-card>

    <!-- ④ 最近 5 条数据对比历史（FEAT-DASH-CMP-CARD ②；FEAT-DASH-UNIFY A：与 DDL 卡同构七列模板） -->
    <el-card shadow="never" class="dash-card" data-testid="dash-cmp-history">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span style="font-weight: 600;">最近数据对比任务<span class="dash-card-sub">数据一致性校验</span></span>
          <el-button size="small" @click="router.push('/compare')">全部历史</el-button>
        </div>
      </template>
      <div v-if="recentCmp.length > 0" class="tims-table-scroll">
        <el-table stripe :data="recentCmp" size="small" class="dash-tbl">
          <el-table-column label="任务 ID" width="110">
            <template #default="{ row }"><span class="tims-mono">{{ row.id }}</span></template>
          </el-table-column>
          <el-table-column label="任务名" min-width="220" show-overflow-tooltip>
            <template #default="{ row }">{{ row.name || '（未命名）' }}</template>
          </el-table-column>
          <el-table-column label="类型" width="150">
            <template #default="{ row }">
              {{ cmpModeText(row.mode) }}
              <!-- #t3 水位过滤组存在才挂「水位」tag（absent = 未过滤比对） -->
              <span v-if="row.watermark" class="tims-pill tims-pill--hint">水位</span>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <span class="tims-pill" :class="cmpStatusPill(row).cls">{{ cmpStatusPill(row).text }}</span>
            </template>
          </el-table-column>
          <el-table-column label="进度·耗时" width="110" class-name="hide-sm">
            <template #default="{ row }"><span class="dash-muted tims-mono">{{ row.tables_done }}/{{ row.tables_total }}</span></template>
          </el-table-column>
          <el-table-column label="时间" width="150" align="right" class-name="hide-sm">
            <template #default="{ row }"><span class="dash-muted tims-mono">{{ fmtTime(row.created_at) }}</span></template>
          </el-table-column>
          <el-table-column label="操作" width="150">
            <template #default="{ row }">
              <!-- 报告 HTML 直链仅 completed（未完成任务无 report.json） -->
              <el-link v-if="row.status === 'completed'" type="primary" :href="cmpReportURL(row.id)" target="_blank">报告</el-link>
              <span v-else class="dash-muted">—</span>
              <el-button size="small" link type="primary" class="dash-act" @click="router.push(`/compare?task=${row.id}`)">查看</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <div v-else class="tims-empty">
        <el-icon class="tims-empty-icon" :size="42"><Histogram /></el-icon>
        <div class="tims-empty-title">尚无数据对比记录</div>
        <div class="tims-empty-sub">发起一次数据比对即可在这里看到结果</div>
        <el-button type="primary" size="small" style="margin-top: 4px;" @click="router.push('/compare')">去发起比对</el-button>
      </div>
    </el-card>

    <!-- ⑤ MongoDB 对比卡：空状态示例（插画 + 引导按钮）；有记录时显示最近一条 -->
    <el-card shadow="never" class="dash-card" data-testid="dash-mongo-card">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span style="font-weight: 600;">MongoDB 对比<span class="dash-card-sub">集合级数据一致性比对</span></span>
          <el-button size="small" @click="router.push('/tasks')">全部历史</el-button>
        </div>
      </template>
      <div v-if="latestMongo" class="dash-mongo-latest">
        <span class="tims-pill" :class="mongoLatestPill.cls">{{ mongoLatestPill.text }}</span>
        <span class="dash-mongo-name">{{ latestMongo.name || latestMongo.id }}</span>
        <span class="dash-muted tims-mono">{{ fmtTime(latestMongo.finished_at || latestMongo.created_at) }}</span>
        <el-button link type="primary" @click="router.push(`/tasks/${latestMongo.id}`)">详情 →</el-button>
      </div>
      <div v-else class="tims-empty">
        <el-icon class="tims-empty-icon" :size="42"><Switch /></el-icon>
        <div class="tims-empty-title">尚无 MongoDB 对比记录</div>
        <div class="tims-empty-sub">创建一次对比即可在这里看到结果</div>
        <el-button type="primary" size="small" style="margin-top: 4px;" @click="router.push('/mongocompare/new')">去新建对比</el-button>
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.dash-gauges {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
  gap: 14px;
  margin-bottom: 18px;
}
.dash-card { margin-bottom: 18px; }
.dash-card-sub {
  font-weight: 400;
  font-size: var(--tims-font-xs);
  color: var(--tims-text-2);
  margin-left: 10px;
}
.dash-pipe {
  display: flex;
  align-items: center;
  gap: 14px;
  flex-wrap: wrap;
}
.dash-conn {
  display: flex;
  align-items: center;
  gap: 10px;
  border: 1px solid var(--tims-border);
  border-radius: 10px;
  padding: 10px 14px;
  background: #fbfcfe;
}
.dash-conn-text { display: flex; flex-direction: column; min-width: 0; }
.dash-conn-name {
  font-weight: 600;
  font-size: var(--tims-font-sm);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 260px;
}
.dash-conn-meta {
  font-size: var(--tims-font-xs);
  color: var(--tims-text-2);
  font-family: var(--tims-font-mono);
  white-space: nowrap;
}
.dash-pipe-mid {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 2px;
  color: var(--tims-text-2);
  font-size: 11px;
  font-family: var(--tims-font-mono);
}
.dash-pipe-arrow {
  color: var(--tims-teal);
  font-size: 20px;
}
.dash-muted { color: var(--tims-text-2); }
/* FEAT-DASH-UNIFY A/D⑤：两历史表行高统一 34px（line-height 20 + 上下 padding 7） */
.dash-tbl :deep(td.el-table__cell) { padding-top: 7px; padding-bottom: 7px; }
.dash-tbl :deep(.el-table__cell .cell) { line-height: 20px; }
/* FEAT-DASH-UNIFY D④/D⑧：操作列同款间距（行级=报告/JSON/查看） */
.dash-act { margin-left: 8px; }
/* FEAT-DASH-UNIFY C：成果速览高亮条 */
.dash-hl {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  margin-bottom: 18px;
  padding: 10px 14px;
  border: 1px solid var(--tims-border);
  border-radius: 10px;
  background: #fbfcfe;
}
.dash-hl-label {
  font-size: var(--tims-font-xs);
  font-weight: 600;
  color: var(--tims-text-2);
  letter-spacing: 0.5px;
}
.dash-hl-cap {
  padding: 2px 10px;
  border-radius: 999px;
  font-size: var(--tims-font-xs);
  font-weight: 600;
  background: var(--tims-teal-soft);
  color: var(--tims-tag-success-text);
}
.dash-hl-item { font-size: var(--tims-font-xs); color: var(--tims-text-2); }
.dash-hl-item b { font-family: var(--tims-font-mono); font-weight: 600; }
/* FEAT-DASH-UNIFY B：Mongo 最近一条行与表格行同节奏（min-height 34） */
.dash-mongo-latest {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  min-height: 34px;
  padding: 7px 2px;
}
.dash-mongo-name { font-weight: 600; }
</style>
