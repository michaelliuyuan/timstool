<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import apiClient from '../api'
import type { DataSource, Task } from '../api'
import { isCompareTask, parseCompareResult } from '../api'
import PageHeader from '../components/PageHeader.vue'

// v3.20 方案A：概览工作台（/ 路由）。只读汇聚三个既有 GET 接口——
// /api/v1/datasources · /api/v1/tasks · /api/v1/ddl-compare/tasks——
// 零后端逻辑/接口改动。四指标卡 + 迁移管线 + 最近 DDL 对比历史 +
// MongoDB 对比空状态示例卡。

const router = useRouter()

const datasources = ref<DataSource[]>([])
const tasks = ref<Task[]>([])
const ddlTasks = ref<any[]>([])
const loading = ref(true)

onMounted(async () => {
  const [ds, tk, dc] = await Promise.allSettled([
    apiClient.listDataSources(),
    apiClient.listTasks(),
    apiClient.ddlCompareTasks(),
  ])
  if (ds.status === 'fulfilled') datasources.value = ds.value.data.datasources || []
  if (tk.status === 'fulfilled') tasks.value = tk.value.data || []
  if (dc.status === 'fulfilled') ddlTasks.value = dc.value.data.tasks || []
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
const latestMongo = computed(() => {
  const sorted = tasks.value
    .filter((t) => isCompareTask(t))
    .sort((a, b) => (b.finished_at || b.created_at || '').localeCompare(a.finished_at || a.created_at || ''))
  return sorted[0] || null
})
// mongocompare 的结论在 result_json 里（CompareResult.overall），运行中/失败任务无结论。
const latestMongoOverall = computed(() => (latestMongo.value ? parseCompareResult(latestMongo.value)?.overall : undefined))
const mongoOverallPill = (o?: string): string =>
  o === 'equal' ? 'tims-pill--pass' : o === 'ttl_skip' ? 'tims-pill--hint' : 'tims-pill--fail'
const mongoOverallText = (o?: string): string =>
  o === 'equal' ? '一致' : o === 'ttl_skip' ? 'TTL 跳过' : o === 'diff' ? '存在差异' : o === 'error' ? '错误' : (o || '运行中')
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
        <el-button link type="primary" @click="openDdlReport(latestDdl.id, 'html')">查看报告 →</el-button>
      </div>
    </el-card>

    <!-- ③ 最近 5 条 DDL 对比历史 -->
    <el-card shadow="never" class="dash-card" data-testid="dash-ddl-history">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span style="font-weight: 600;">最近 DDL 对比任务</span>
          <el-button size="small" @click="router.push('/ddl-compare')">全部历史</el-button>
        </div>
      </template>
      <div v-if="recentDdl.length > 0" class="tims-table-scroll">
        <el-table stripe :data="recentDdl" size="small">
          <el-table-column label="任务 ID" width="110">
            <template #default="{ row }"><span class="tims-mono">{{ row.id }}</span></template>
          </el-table-column>
          <el-table-column label="任务名" min-width="180" show-overflow-tooltip>
            <template #default="{ row }">{{ row.name || '（未命名）' }}</template>
          </el-table-column>
          <el-table-column label="链路" min-width="180">
            <template #default="{ row }">
              {{ metaOf(row.source_type).short }} → {{ metaOf(row.target_type).short }}
            </template>
          </el-table-column>
          <el-table-column label="结论" width="90">
            <template #default="{ row }">
              <span v-if="row.overall" class="tims-pill" :class="verdictPill(row.overall)">{{ verdictText(row.overall) }}</span>
              <el-tooltip v-else :content="row.error || '失败'" placement="top">
                <span class="tims-pill tims-pill--fail">失败</span>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="耗时" width="90" class-name="hide-sm">
            <template #default="{ row }"><span class="dash-muted tims-mono">{{ row.duration || '—' }}</span></template>
          </el-table-column>
          <el-table-column label="时间" width="140" class-name="hide-sm">
            <template #default="{ row }"><span class="dash-muted">{{ fmtTime(row.created_at) }}</span></template>
          </el-table-column>
          <el-table-column label="报告" width="110">
            <template #default="{ row }">
              <template v-if="row.status === 'completed'">
                <el-link type="primary" :href="apiClient.ddlCompareReportURL(row.id, 'html')" target="_blank">HTML</el-link>
                <el-link type="primary" style="margin-left: 8px;" :href="apiClient.ddlCompareReportURL(row.id, 'json')" target="_blank">JSON</el-link>
              </template>
              <span v-else class="dash-muted">—</span>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <el-empty v-else description="还没有 DDL 对比记录；在「DDL 对比」页发起一次结构对比即可在这里看到结果" />
    </el-card>

    <!-- ④ MongoDB 对比卡：空状态示例（插画 + 引导按钮）；有记录时显示最近一条 -->
    <el-card shadow="never" class="dash-card" data-testid="dash-mongo-card">
      <template #header>
        <span style="font-weight: 600;">MongoDB 对比<span class="dash-card-sub">集合级数据一致性比对</span></span>
      </template>
      <div v-if="latestMongo" class="dash-mongo-latest">
        <span class="tims-pill" :class="mongoOverallPill(latestMongoOverall)">{{ mongoOverallText(latestMongoOverall) }}</span>
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
.dash-mongo-latest {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.dash-mongo-name { font-weight: 600; }
</style>
