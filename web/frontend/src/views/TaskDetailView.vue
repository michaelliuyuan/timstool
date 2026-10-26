<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed, nextTick, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient from '../api'
import type { Task, TaskLogEntry, PhaseInfo } from '../api'
import { isCompareTask, parseCompareResult } from '../api'

const route = useRoute()
const router = useRouter()
const taskId = route.params.id as string
const task = ref<Task | null>(null)
const loading = ref(true)
const ws = ref<WebSocket | null>(null)
const wsProgress = ref<any>(null)
// Server-computed elapsed seconds (last WS progress message) + local
// receive-time, so the display ticks every second without re-differencing
// the (possibly skewed) browser clock against server-written started_at.
const serverElapsed = ref<number | null>(null)
const serverElapsedAt = ref(0)
const nowTick = ref(Date.now())

// Compare tasks (M4): type-aware degradation — migration stats/phases/report
// are meaningless for compare; a compare-specific result card renders instead.
const isCompare = computed(() => isCompareTask(task.value))
const compareResult = computed(() => parseCompareResult(task.value))
const bucketsDone = ref<number | null>(null)

const overallMeta: Record<string, { type: string; label: string }> = {
  equal: { type: 'success', label: '一致 (equal)' },
  diff: { type: 'warning', label: '存在差异 (diff)' },
  error: { type: 'danger', label: '对比出错 (error)' },
  ttl_skip: { type: 'info', label: 'TTL 跳过 (ttl_skip)' },
}

// ---- coll-select ②: in-page per-collection result table + CSV export ----
// Data source is the verbatim summary.json artifact (the same bytes the
// 「下载 JSON」button saves) — no new read endpoint, one parse client-side.
interface McBucket { bucket: number; match: boolean; src_count: number; dst_count: number; src_hash?: string; dst_hash?: string }
interface McCollectionRow {
  db?: string
  coll: string
  overall: string
  wm_state?: string
  note?: string
  checks?: {
    schema?: Record<string, any>
    index?: Record<string, any>
    count?: Record<string, any>
    hash?: McBucket[]
    diff?: Record<string, any>
    skipped?: string
    error?: string
  }
}
interface McSummary {
  collections?: McCollectionRow[]
  counts?: Record<string, number>
  stats?: Record<string, any>
}
const mcSummary = ref<McSummary | null>(null)
const mcSummaryLoading = ref(false)
const mcOverallFilter = ref('all')
const mcCollSearch = ref('')

async function loadMcSummary() {
  if (!isCompare.value) return
  if (task.value?.status !== 'completed' && task.value?.status !== 'failed') return
  // In-flight guard (正评席 N2): the 5s task poll re-fires this watcher —
  // without it a slow request would overlap itself.
  if (mcSummary.value || mcSummaryLoading.value) return
  mcSummaryLoading.value = true
  try {
    const { data } = await apiClient.getTaskReport(taskId, 'json')
    mcSummary.value = JSON.parse(data as string) as McSummary
  } catch {
    // Artifact not finalized yet (race with restart) — stays null and the
    // next poll watch retries; never surfaces a half-state to the user.
  } finally {
    mcSummaryLoading.value = false
  }
}
watch(task, (t) => {
  if (!t) return
  if (isCompare.value && (t.status === 'completed' || t.status === 'failed')) {
    loadMcSummary()
  } else if (mcSummary.value) {
    // A restart/resume invalidates the artifact view until the new round
    // finalizes — showing the previous run's rows would be misleading.
    mcSummary.value = null
  }
})

const mcRows = computed<McCollectionRow[]>(() => mcSummary.value?.collections || [])
const mcFilteredRows = computed(() => {
  let rows = mcRows.value
  if (mcOverallFilter.value !== 'all') {
    rows = rows.filter(r => r.overall === mcOverallFilter.value)
  }
  const q = mcCollSearch.value.trim().toLowerCase()
  if (q) rows = rows.filter(r => (r.coll || '').toLowerCase().includes(q))
  return rows
})
function mcHashMismatch(row: McCollectionRow): number {
  return (row.checks?.hash || []).filter(b => !b.match).length
}
// FE-side CSV (FEAT-0933 client-side pattern, BOM so Excel opens UTF-8
// Chinese correctly); exports exactly the filtered view the user sees.
// 对抗席 F2: CSV formula injection — a cell starting with = + - @ (or a
// tab/CR) is executed as a formula by Excel/WPS; collection names and notes
// come from database content (dst-only rows even from the FAR side), so
// every cell is prefixed with a single quote when it matches (OWASP CSV
// injection mitigation).
function mcCsvCell(raw: string): string {
  let c = raw.replace(/"/g, '""')
  // Prefix first, THEN the quote-wrap decision (对抗席 N1: returning early
  // here would skip the wrap and misalign columns for `=a,b`-style values).
  if (/^[=+\-\t\r@]/.test(c)) c = `'${c}`
  return /[",\n\r]/.test(c) ? `"${c}"` : c
}
function mcExportCsv() {
  const head = ['集合名', '结果', '水位状态', '源行数', '目标行数', 'hash分桶数', 'hash不一致分桶', '差异样本数', '备注']
  const lines = [head.join(',')]
  for (const r of mcFilteredRows.value) {
    const cells = [
      r.coll || '',
      overallMeta[r.overall]?.label || r.overall || '',
      r.wm_state || '',
      String(r.checks?.count?.src ?? ''),
      String(r.checks?.count?.dst ?? ''),
      String(r.checks?.hash?.length ?? ''),
      String(mcHashMismatch(r)),
      String(r.checks?.diff?.count ?? ''),
      r.note || '',
    ]
    lines.push(cells.map(mcCsvCell).join(','))
  }
  const blob = new Blob(['\uFEFF' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `mongo-collections-${taskId}.csv`
  a.click()
  URL.revokeObjectURL(url)
}

const logDrawerVisible = ref(false)
const logs = ref<TaskLogEntry[]>([])
const logsLoading = ref(false)
const logAutoScroll = ref(true)
const logWs = ref<WebSocket | null>(null)
const logContainer = ref<HTMLElement | null>(null)
const logPhaseFilter = ref('all')

const activePhaseTab = ref('overview')
const phaseData = ref<PhaseInfo[]>([])

async function fetchPhases() {
  if (isCompare.value) return // compare has no migration phase timeline
  try {
    const { data } = await apiClient.getTaskPhases(taskId)
    phaseData.value = data.phases || []
  } catch {}
}

const statusMap: Record<string, { type: string; label: string }> = {
  created: { type: 'info', label: '已创建' },
  running: { type: 'warning', label: '运行中' },
  paused: { type: '', label: '已暂停' },
  completed: { type: 'success', label: '已完成' },
  failed: { type: 'danger', label: '失败' },
  cancelled: { type: 'info', label: '已取消' },
}

const phaseMap: Record<string, string> = {
  precheck: '预检查',
  schema: 'Schema 迁移',
  data: '数据迁移',
  validate: '数据验证',
  completed: '已完成',
}

const phases = ['precheck', 'schema', 'data', 'validate']

// Phase status → tag type/label, covering the P1 state machine values
// (pending/running/completed/failed/skipped) plus the warn overlay.
function phaseStatusMeta(status: string, warn?: boolean): { type: string; label: string } {
  if (status === 'completed') {
    return warn ? { type: 'warning', label: '带警告完成' } : { type: 'success', label: '已完成' }
  }
  switch (status) {
    case 'running': return { type: '', label: '进行中' }
    case 'failed': return { type: 'danger', label: '失败' }
    case 'skipped': return { type: 'info', label: '已跳过' }
    default: return { type: 'info', label: '等待中' }
  }
}

// Phase duration formatter: 12s / 1m03s / 2h05m.
function fmtPhaseDuration(sec?: number): string {
  if (!sec || sec <= 0) return ''
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = Math.floor(sec % 60)
  if (h > 0) return `${h}h${String(m).padStart(2, '0')}m`
  if (m > 0) return `${m}m${String(s).padStart(2, '0')}s`
  return `${s}s`
}

const statusInfo = computed(() => {
  if (!task.value) return { type: 'info', label: '-' }
  return statusMap[task.value.status] || { type: 'info', label: task.value.status }
})

const progressPercent = computed(() => {
  if (!task.value) return 0
  return Math.round(task.value.progress * 100)
})

const importSubText = computed(() => {
  const p = wsProgress.value
  if (!p || !task.value || task.value.phase !== 'data') return ''
  const imported = p.imported_tables
  if (typeof imported !== 'number' || imported <= 0) return ''
  const total = p.tables_total ?? task.value.tables_total
  return `数据导入中（${imported}/${total} 表）`
})

const elapsedSeconds = computed(() => {
  if (!task.value?.started_at) return null
  // Finished: exact server-written interval (clock-skew free).
  if (task.value.finished_at) {
    const d = Math.floor((new Date(task.value.finished_at).getTime() - new Date(task.value.started_at).getTime()) / 1000)
    return Math.max(0, d)
  }
  // Running: server-computed base + locally advanced seconds.
  if (serverElapsed.value !== null) {
    return Math.max(0, serverElapsed.value + Math.floor((nowTick.value - serverElapsedAt.value) / 1000))
  }
  // Fallback (no WS yet): browser clock, clamped non-negative.
  const d = Math.floor((nowTick.value - new Date(task.value.started_at).getTime()) / 1000)
  return Math.max(0, d)
})

const elapsed = computed(() => {
  const s = elapsedSeconds.value
  if (s === null) return '-'
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  return `${h}h ${m}m ${sec}s`
})

const rowsPerSec = computed(() => {
  if (!task.value?.started_at || !task.value?.rows_done) return 0
  let secs = elapsedSeconds.value
  if (secs === null) {
    secs = Math.max(0, Math.floor((nowTick.value - new Date(task.value.started_at).getTime()) / 1000))
  }
  if (secs <= 0) return 0
  return Math.round(task.value.rows_done / secs)
})

const filteredLogs = computed(() => {
  if (logPhaseFilter.value === 'all') return logs.value
  const phaseLabel = `Phase: ${phaseMap[logPhaseFilter.value] || logPhaseFilter.value}`
  const phaseLabelLower = phaseLabel.toLowerCase()
  let inPhase = false
  const result: TaskLogEntry[] = []
  for (const entry of logs.value) {
    if (entry.message.toLowerCase().includes(phaseLabelLower)) {
      inPhase = true
      result.push(entry)
      continue
    }
    if (inPhase && entry.message.includes('Phase:')) {
      inPhase = false
      continue
    }
    if (inPhase) result.push(entry)
  }
  return result
})

const phaseLogs = computed(() => {
  const result: Record<string, TaskLogEntry[]> = {}
  for (const phase of phases) {
    const phaseLabel = `Phase: ${phaseMap[phase]}`
    let inPhase = false
    const entries: TaskLogEntry[] = []
    for (const entry of logs.value) {
      if (entry.message.includes(phaseLabel)) {
        inPhase = true
        entries.push(entry)
        continue
      }
      if (inPhase && entry.message.includes('Phase:')) {
        inPhase = false
        continue
      }
      if (inPhase) entries.push(entry)
    }
    result[phase] = entries
  }
  return result
})

async function fetchTask() {
  try {
    const { data } = await apiClient.getTask(taskId)
    task.value = data
  } catch (e: any) {
    ElMessage.error('获取任务信息失败')
  } finally {
    loading.value = false
  }
}

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  ws.value = new WebSocket(`${proto}//${location.host}/api/v1/ws`)
  ws.value.onmessage = (event) => {
    try {
      const data = JSON.parse(event.data)
      if (data.task_id === taskId && task.value) {
        wsProgress.value = data
        if (typeof data.elapsed_seconds === 'number') {
          serverElapsed.value = Math.max(0, data.elapsed_seconds)
          serverElapsedAt.value = Date.now()
        }
        if (data.phase) task.value.phase = data.phase
        if (data.progress !== undefined) task.value.progress = data.progress
        if (data.tables_done !== undefined) task.value.tables_done = data.tables_done
        if (data.tables_total !== undefined) task.value.tables_total = data.tables_total
        if (data.rows_done !== undefined) task.value.rows_done = data.rows_done
        if (data.rows_total !== undefined) task.value.rows_total = data.rows_total
        if (data.imported_tables !== undefined) task.value.imported_tables = data.imported_tables
        if (data.status) task.value.status = data.status
        // compare heartbeat: completed collection checkpoints so far
        if (data.buckets_done !== undefined) bucketsDone.value = data.buckets_done
      }
    } catch {}
  }
}

let pollTimer: any = null
let tickTimer: any = null

onMounted(() => {
  fetchTask()
  connectWS()
  pollTimer = setInterval(() => {
    fetchTask()
    fetchPhases()
  }, 5000)
  // 1s heartbeat so elapsed (server base + local advance) renders smoothly.
  tickTimer = setInterval(() => { nowTick.value = Date.now() }, 1000)
  loadLogs()
  fetchPhases()
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
  if (tickTimer) clearInterval(tickTimer)
  if (ws.value) ws.value.close()
  if (logWs.value) logWs.value.close()
})

async function action(actionName: string) {
  try {
    await (apiClient as any)[`${actionName}Task`](taskId)
    ElMessage.success('操作成功')
    await fetchTask()
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '操作失败')
  }
}

// Compare restart paths (M4): fresh start wipes the compare OutDir
// server-side; resume=true continues from engine checkpoints. Shown for
// compare tasks outside created/running (the create flow already starts).
async function restartCompare(resume: boolean) {
  try {
    await apiClient.startTask(taskId, { resume })
    ElMessage.success(resume ? '已从断点续跑对比' : '已重新开始对比')
    await fetchTask()
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '操作失败')
  }
}

async function cancelTask() {
  try {
    await ElMessageBox.confirm('确认取消该迁移任务？进行中的同步将中断。', '确认', { type: 'warning' })
  } catch { return }
  try {
    await apiClient.cancelTask(taskId)
    ElMessage.success('任务已取消')
    await fetchTask()
  } catch (e: any) {
    ElMessage.error('取消失败')
  }
}

async function deleteTask() {
  try {
    await ElMessageBox.confirm('确认删除该任务？删除后不可恢复。', '确认', { type: 'warning' })
  } catch { return }
  try {
    await apiClient.deleteTask(taskId)
    ElMessage.success('任务已删除')
    router.push('/tasks')
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '删除失败')
  }
}

// MC-14: report downloads for both families — compare tasks render the
// detailed HTML report server-side; the error body may arrive as raw text
// (responseType text), so extract .error defensively.
function reportDownloadError(e: any) {
  let msg = e?.response?.data?.error
  if (!msg && typeof e?.response?.data === 'string') {
    try { msg = JSON.parse(e.response.data).error } catch { msg = e.response.data }
  }
  ElMessage.error(`报告下载失败: ${msg || e.message}`)
}

async function downloadReport() {
  try {
    const { data } = await apiClient.getTaskReport(taskId, 'html')
    const blob = new Blob([data], { type: 'text/html' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = isCompare.value ? `compare-report-${taskId}.html` : `report-${taskId}.html`
    a.click()
    URL.revokeObjectURL(url)
  } catch (e: any) {
    reportDownloadError(e)
  }
}

// MC-14: verbatim summary.json download for finished compare tasks (the
// byte-identical engine artifact carries the full five-level detail).
async function downloadCompareSummary() {
  try {
    const { data } = await apiClient.getTaskReport(taskId, 'json')
    const blob = new Blob([data], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `compare-summary-${taskId}.json`
    a.click()
    URL.revokeObjectURL(url)
  } catch (e: any) {
    reportDownloadError(e)
  }
}

async function loadLogs() {
  logsLoading.value = true
  try {
    const { data } = await apiClient.getTaskLogs(taskId)
    logs.value = data.logs || []
  } catch {
  } finally {
    logsLoading.value = false
  }
  connectLogWS()
}

function openLogs() {
  logDrawerVisible.value = true
  if (logs.value.length === 0) {
    loadLogs()
  }
  nextTick(() => scrollToBottom())
}

function closeLogs() {
  logDrawerVisible.value = false
}

function connectLogWS() {
  if (logWs.value) {
    logWs.value.close()
  }
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  logWs.value = new WebSocket(`${proto}//${location.host}/api/v1/tasks/${taskId}/logs?ws=true`)
  logWs.value.onmessage = (event) => {
    try {
      const entry: TaskLogEntry = JSON.parse(event.data)
      logs.value.push(entry)
      if (logAutoScroll.value && logDrawerVisible.value) {
        nextTick(() => scrollToBottom())
      }
    } catch {}
  }
}

function scrollToBottom() {
  if (logContainer.value) {
    logContainer.value.scrollTop = logContainer.value.scrollHeight
  }
}

function logLevelClass(level: string): string {
  switch (level) {
    case 'ERROR': case 'FATAL': return 'log-error'
    case 'WARN': return 'log-warn'
    case 'DEBUG': return 'log-debug'
    default: return 'log-info'
  }
}
</script>

<template>
  <div v-loading="loading" class="tims-page">
    <el-page-header @back="router.push('/tasks')" style="margin-bottom: 20px;">
      <template #content>
        <span>{{ task?.name || '任务详情' }}</span>
        <el-tag :type="statusInfo.type as any" style="margin-left: 12px;">{{ statusInfo.label }}</el-tag>
      </template>
    </el-page-header>

    <template v-if="task">
      <!-- Status Overview -->
      <el-card style="margin-bottom: 20px;">
        <template v-if="!isCompare">
          <el-row :gutter="20">
            <el-col :span="4">
              <el-statistic title="阶段" :value="phaseMap[task.phase] || task.phase || '-'" />
            </el-col>
            <el-col :span="4">
              <el-statistic title="表进度" :value="`${task.tables_done}/${task.tables_total}`" />
            </el-col>
            <el-col :span="4">
              <el-statistic title="行数" :value="task.rows_done.toLocaleString()" />
              <div style="font-size: var(--tims-font-xs); color: var(--tims-text-2);">/ {{ task.rows_total.toLocaleString() }}</div>
            </el-col>
            <el-col :span="4">
              <el-statistic title="吞吐量" :value="rowsPerSec.toLocaleString()" suffix="rows/s" />
            </el-col>
            <el-col :span="4">
              <el-statistic title="耗时" :value="elapsed" />
            </el-col>
            <el-col :span="4">
              <el-statistic title="总进度" :value="progressPercent" suffix="%" />
            </el-col>
          </el-row>
          <el-progress :percentage="progressPercent" :stroke-width="20" style="margin-top: 16px;"
            :status="task.status === 'completed' ? 'success' : task.status === 'failed' ? 'exception' : undefined" />
          <div v-if="importSubText" style="margin-top: 6px; font-size: var(--tims-font-sm); color: var(--tims-text-2);">{{ importSubText }}</div>
        </template>
        <template v-else>
          <el-row :gutter="20">
            <el-col :span="6">
              <el-statistic title="阶段" :value="task.phase === 'compare' ? '对比中' : (task.phase || '-')" />
            </el-col>
            <el-col :span="6">
              <el-statistic title="已完成集合" :value="bucketsDone ?? task.tables_done" />
              <div style="font-size: 12px; color: #999;">总数在结束时确定</div>
            </el-col>
            <el-col :span="6">
              <el-statistic title="耗时" :value="elapsed" />
            </el-col>
            <el-col :span="6">
              <el-statistic title="总体结果" :value="compareResult ? (overallMeta[compareResult.overall]?.label || compareResult.overall) : (task.status === 'running' ? '对比中' : '-')" />
            </el-col>
          </el-row>
          <el-progress :percentage="compareResult ? 100 : 0" :stroke-width="20" style="margin-top: 16px;"
            :indeterminate="task.status === 'running'"
            :status="task.status === 'completed' ? (compareResult?.overall === 'equal' ? 'success' : 'warning') : task.status === 'failed' ? 'exception' : undefined" />
        </template>
      </el-card>

      <!-- Compare result card (M4): counts + artifact paths, option A -->
      <el-card v-if="isCompare && compareResult" style="margin-bottom: 20px;">
        <template #header>
          <div style="display: flex; align-items: center; gap: 8px;">
            <span>对比结果</span>
            <el-tag :type="(overallMeta[compareResult.overall]?.type || 'info') as any">
              {{ overallMeta[compareResult.overall]?.label || compareResult.overall }}
            </el-tag>
          </div>
        </template>
        <el-row :gutter="20" style="margin-bottom: 8px;">
          <el-col :span="6"><el-statistic title="一致集合" :value="compareResult.counts?.equal ?? 0" /></el-col>
          <el-col :span="6"><el-statistic title="差异集合" :value="compareResult.counts?.diff ?? 0" /></el-col>
          <el-col :span="6"><el-statistic title="出错集合" :value="compareResult.counts?.error ?? 0" /></el-col>
          <el-col :span="6"><el-statistic title="TTL 跳过" :value="compareResult.counts?.ttl_skip ?? 0" /></el-col>
        </el-row>
          <el-descriptions :column="1" border size="small" style="margin-top: 12px;">
            <el-descriptions-item label="对比耗时">{{ compareResult.duration_sec?.toFixed(2) ?? '-' }} 秒</el-descriptions-item>
            <el-descriptions-item label="结果摘要 (summary.json)">
              <el-text type="info" class="tims-mono">{{ compareResult.summary_path }}</el-text>
            </el-descriptions-item>
            <el-descriptions-item label="差异明细目录 (diff)">
              <el-text type="info" class="tims-mono">{{ compareResult.diff_dir }}</el-text>
            </el-descriptions-item>
          </el-descriptions>
        </el-card>

        <!-- coll-select ②: per-collection result table (summary.json parsed
             in-page; the HTML/JSON downloads below stay as the archival
             faces). Deliberately a different gate expression than the
             MC-14 download buttons' literal (spec anchors index by it). -->
        <el-card
          v-if="isCompare && ['completed', 'failed'].includes(task.status)"
          v-loading="mcSummaryLoading"
          style="margin-bottom: 20px;"
        >
          <template #header>
            <div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap;">
              <span>集合结果</span>
              <el-tag v-if="mcSummary" type="info" size="small">{{ mcRows.length }} 个集合</el-tag>
              <el-tag
                v-if="mcSummary?.stats && mcSummary.stats.selected_collections !== undefined"
                type="warning" size="small"
              >
                部分选择：已选 {{ mcSummary.stats.selected_collections }} · 实际比对 {{ mcSummary.stats.matched_collections ?? '-' }}
              </el-tag>
              <span style="flex: 1;"></span>
              <el-select v-model="mcOverallFilter" size="small" style="width: 150px;">
                <el-option label="全部结果" value="all" />
                <el-option label="一致" value="equal" />
                <el-option label="差异" value="diff" />
                <el-option label="出错" value="error" />
                <el-option label="TTL 跳过" value="ttl_skip" />
              </el-select>
              <el-input
                v-model="mcCollSearch" size="small" placeholder="搜索集合名" clearable
                style="width: 180px;" />
              <el-button size="small" type="primary" plain :disabled="!mcSummary" @click="mcExportCsv">
                <el-icon><Download /></el-icon> 导出 CSV
              </el-button>
            </div>
          </template>
          <el-table v-if="mcSummary" :data="mcFilteredRows" size="small" max-height="420" border>
            <el-table-column type="expand">
              <template #default="{ row }">
                <el-descriptions :column="2" border size="small" style="padding: 8px;">
                  <el-descriptions-item label="Schema">
                    <span v-if="row.checks?.schema?.ok" style="color: var(--el-color-success);">一致</span>
                    <span v-else>{{ Array.isArray(row.checks?.schema?.diff) ? row.checks.schema.diff.join('、') : (row.checks?.skipped || '-') }}</span>
                  </el-descriptions-item>
                  <el-descriptions-item label="索引">
                    <span v-if="row.checks?.index?.ok" style="color: var(--el-color-success);">一致</span>
                    <span v-else-if="Array.isArray(row.checks?.index?.diff)">{{ row.checks.index.diff.join('；') }}</span>
                    <span v-else>-</span>
                  </el-descriptions-item>
                  <el-descriptions-item label="行数 (源/目标)">
                    {{ row.checks?.count?.src ?? '-' }} / {{ row.checks?.count?.dst ?? '-' }}
                  </el-descriptions-item>
                  <el-descriptions-item label="hash 分桶">
                    共 {{ row.checks?.hash?.length ?? 0 }} 桶，不一致 {{ mcHashMismatch(row) }} 桶
                  </el-descriptions-item>
                  <el-descriptions-item label="差异样本">
                    {{ row.checks?.diff?.count ?? 0 }} 条（明细见 diff 目录 ndjson）
                  </el-descriptions-item>
                  <el-descriptions-item label="错误/跳过">
                    {{ row.checks?.error || row.checks?.skipped || '-' }}
                  </el-descriptions-item>
                  <el-descriptions-item v-if="row.note" label="备注" :span="2">{{ row.note }}</el-descriptions-item>
                </el-descriptions>
              </template>
            </el-table-column>
            <el-table-column prop="coll" label="集合名" min-width="220" show-overflow-tooltip />
            <el-table-column label="结果" width="130">
              <template #default="{ row }">
                <el-tag :type="(overallMeta[row.overall]?.type || 'info') as any" size="small">
                  {{ overallMeta[row.overall]?.label || row.overall }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="水位" width="90">
              <template #default="{ row }">
                <el-tag v-if="row.wm_state === 'windowed'" type="success" size="small">窗口内</el-tag>
                <el-tag v-else-if="row.wm_state === 'degraded'" type="warning" size="small">已退化</el-tag>
                <el-tag v-else-if="row.wm_state === 'failed'" type="danger" size="small">预检失败</el-tag>
                <span v-else>-</span>
              </template>
            </el-table-column>
            <el-table-column label="行数 (源/目标)" width="130">
              <template #default="{ row }">
                {{ row.checks?.count?.src ?? '-' }} / {{ row.checks?.count?.dst ?? '-' }}
              </template>
            </el-table-column>
            <el-table-column label="hash 不一致" width="100">
              <template #default="{ row }">
                <span :style="mcHashMismatch(row) > 0 ? 'color: var(--el-color-warning);' : ''">{{ mcHashMismatch(row) }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="note" label="备注" min-width="180" show-overflow-tooltip />
          </el-table>
          <el-empty v-else-if="!mcSummaryLoading" description="结果摘要不可用（任务未产生 summary.json，可刷新重试）" :image-size="60" />
        </el-card>

      <!-- Actions -->
      <el-card style="margin-bottom: 20px;">
        <template #header>操作</template>
        <el-space>
          <el-button v-if="isCompare && task.status !== 'created' && task.status !== 'running'" type="primary" @click="restartCompare(false)">
            <el-icon><VideoPlay /></el-icon> 重新对比
          </el-button>
          <el-button v-if="isCompare && task.status !== 'created' && task.status !== 'running'" plain @click="restartCompare(true)">
            <el-icon><RefreshRight /></el-icon> 断点续跑
          </el-button>
          <el-button v-if="task.status === 'running'" type="warning" @click="action('pause')">
            <el-icon><VideoPause /></el-icon> 暂停
          </el-button>
          <el-button v-if="task.status === 'paused'" type="success" @click="action('resume')">
            <el-icon><VideoPlay /></el-icon> 恢复
          </el-button>
          <el-button v-if="task.status === 'running' || task.status === 'paused'" type="danger" @click="cancelTask()">
            <el-icon><CircleClose /></el-icon> 取消
          </el-button>
          <el-button v-if="!isCompare && (task.status === 'completed' || task.status === 'failed' || task.status === 'cancelled')" @click="downloadReport">
            <el-icon><Download /></el-icon> 下载报告
          </el-button>
          <!-- MC-14: compare tasks download their own report family (summary
               exists only for completed/failed — cancelled runs have none and
               would 404). -->
          <el-button v-if="isCompare && (task.status === 'completed' || task.status === 'failed')" type="primary" plain @click="downloadReport">
            <el-icon><Download /></el-icon> 下载对比报告
          </el-button>
          <el-button v-if="isCompare && (task.status === 'completed' || task.status === 'failed')" @click="downloadCompareSummary">
            <el-icon><Download /></el-icon> 下载 JSON
          </el-button>
          <el-button v-if="task.status !== 'running'" type="danger" plain @click="deleteTask()">
            <el-icon><Delete /></el-icon> 删除任务
          </el-button>
          <el-button v-if="task.status !== 'created'" type="primary" plain @click="openLogs">
            <el-icon><Document /></el-icon> 查看日志
          </el-button>
        </el-space>
      </el-card>

      <!-- Phase Timeline (migration only; compare tasks have no phases) -->
      <el-card v-if="!isCompare" style="margin-bottom: 20px;">
        <template #header>迁移阶段</template>
        <el-tabs v-model="activePhaseTab">
          <el-tab-pane label="阶段概览" name="overview">
            <el-timeline>
              <el-timeline-item
                v-for="phase in phaseData"
                :key="phase.name"
                :type="phase.status === 'completed' ? 'success' : phase.status === 'running' ? 'primary' : phase.status === 'failed' ? 'danger' : 'info'"
                :hollow="phase.status === 'pending' || phase.status === 'skipped'"
                :timestamp="phase.sub_label ? `${phase.label}（${phase.sub_label}）` : phase.label"
                placement="top"
              >
                <div style="display: flex; align-items: center; gap: 8px;">
                  <span :class="['phase-dot', `phase-dot--${phase.status}`]" aria-hidden="true"></span>
                  <el-tag :type="phaseStatusMeta(phase.status, phase.warn).type" size="small">
                    {{ phaseStatusMeta(phase.status, phase.warn).label }}
                  </el-tag>
                  <span v-if="fmtPhaseDuration(phase.duration)" style="color: var(--tims-text-2); font-size: var(--tims-font-sm);">{{ fmtPhaseDuration(phase.duration) }}</span>
                  <el-tooltip v-if="phase.error" :content="phase.error" placement="top">
                    <el-icon style="color: var(--el-color-danger);"><Warning /></el-icon>
                  </el-tooltip>
                </div>
                <div v-if="phase.table_count > 0 && phase.name === 'data'" style="margin-top: 8px; color: #606266; font-size: var(--tims-font-sm);">
                  <template v-if="phase.sub_label === '数据导入'">数据导入 · 已导入表: {{ phase.imported_tables }}/{{ phase.table_count }}</template>
                  <template v-else>表: {{ phase.tables_done }}/{{ phase.table_count }} · 行: {{ phase.rows_done.toLocaleString() }}/{{ phase.rows_total.toLocaleString() }}</template>
                </div>
                <div v-else-if="phase.table_count > 0 && phase.name === 'schema'" style="margin-top: 8px; color: #606266; font-size: var(--tims-font-sm);">
                  表: {{ phase.tables_done }}/{{ phase.table_count }}
                </div>
                <div v-if="phaseLogs[phase.name]?.length" style="margin-top: 8px;">
                  <el-tag size="small" type="info" style="cursor: pointer;" @click="logPhaseFilter = phase.name; logDrawerVisible = true">
                    {{ phaseLogs[phase.name].length }} 条日志 - 点击查看
                  </el-tag>
                </div>
              </el-timeline-item>
            </el-timeline>
          </el-tab-pane>

          <el-tab-pane
            v-for="phase in phaseData"
            :key="phase.name"
            :label="phase.label"
            :name="phase.name"
          >
            <div v-if="phase.status === 'pending'" style="color: var(--tims-text-2); text-align: center; padding: 30px;">
              该阶段尚未开始
            </div>
            <div v-else-if="phase.status === 'skipped'" style="color: var(--tims-text-2); text-align: center; padding: 30px;">
              该阶段已跳过
            </div>
            <template v-else>
              <div v-if="phase.table_count > 0 && phase.name === 'data'" style="margin-bottom: 16px;">
                <el-descriptions :column="3" border size="small">
                  <el-descriptions-item label="表总数">{{ phase.table_count }}</el-descriptions-item>
                  <el-descriptions-item label="已完成">{{ phase.tables_done }}</el-descriptions-item>
                  <el-descriptions-item label="行进度">{{ phase.rows_done.toLocaleString() }} / {{ phase.rows_total.toLocaleString() }}</el-descriptions-item>
                </el-descriptions>
                <el-table v-if="phase.tables?.length" :data="phase.tables" size="small" style="margin-top: 12px;" max-height="200">
                  <el-table-column prop="name" label="表名" />
                  <el-table-column prop="state" label="状态" width="100">
                    <template #default="{ row }">
                      <el-tag :type="row.state === 'completed' ? 'success' : row.state === 'running' ? '' : row.state === 'failed' ? 'danger' : 'info'" size="small">
                        {{ row.state === 'completed' ? '完成' : row.state === 'running' ? '运行中' : row.state === 'failed' ? '失败' : row.state === 'pending' ? '等待' : row.state }}
                      </el-tag>
                    </template>
                  </el-table-column>
                  <el-table-column label="行进度" width="180">
                    <template #default="{ row }">
                      {{ row.rows_done.toLocaleString() }} / {{ row.rows_total.toLocaleString() }}
                    </template>
                  </el-table-column>
                </el-table>
              </div>
              <div v-else-if="phase.table_count > 0 && phase.name === 'schema'" style="margin-bottom: 16px;">
                <el-descriptions :column="2" border size="small">
                  <el-descriptions-item label="表总数">{{ phase.table_count }}</el-descriptions-item>
                  <el-descriptions-item label="已完成">{{ phase.tables_done }}</el-descriptions-item>
                </el-descriptions>
                <el-table v-if="phase.tables?.length" :data="phase.tables" size="small" style="margin-top: 12px;" max-height="200">
                  <el-table-column prop="name" label="表名" />
                  <el-table-column prop="state" label="状态" width="100">
                    <template #default="{ row }">
                      <el-tag :type="row.state === 'completed' ? 'success' : row.state === 'running' ? '' : row.state === 'failed' ? 'danger' : 'info'" size="small">
                        {{ row.state === 'completed' ? '完成' : row.state === 'running' ? '运行中' : row.state === 'failed' ? '失败' : row.state === 'pending' ? '等待' : row.state }}
                      </el-tag>
                    </template>
                  </el-table-column>
                </el-table>
              </div>
              <div v-if="phase.logs?.length === 0 && phaseLogs[phase.name]?.length === 0" style="color: var(--tims-text-2); text-align: center; padding: 20px;">
                暂无日志
              </div>
              <div v-else-if="phase.logs?.length" class="phase-log-container">
                <div v-for="(entry, idx) in phase.logs" :key="idx" class="log-line" :class="'log-' + entry.level.toLowerCase()">
                  <span class="log-time">{{ entry.timestamp ? entry.timestamp.replace('T', ' ').substring(11, 19) : '' }}</span>
                  <span class="log-level">[{{ entry.level }}]</span>
                  <span class="log-msg">{{ entry.message }}</span>
                </div>
              </div>
              <div v-else class="phase-log-container">
                <div v-for="(entry, idx) in phaseLogs[phase.name]" :key="idx" class="log-line" :class="logLevelClass(entry.level)">
                  <span class="log-time">{{ entry.timestamp ? entry.timestamp.replace('T', ' ').substring(11, 19) : '' }}</span>
                  <span class="log-level">[{{ entry.level }}]</span>
                  <span class="log-msg">{{ entry.message }}</span>
                </div>
              </div>
            </template>
          </el-tab-pane>
        </el-tabs>
      </el-card>

      <!-- Error -->
      <el-card v-if="task.error" style="margin-bottom: 20px;">
        <el-alert :title="task.error" type="error" :closable="false" show-icon />
      </el-card>

      <!-- Info -->
      <el-card>
        <template #header>任务信息</template>
        <el-descriptions :column="2" border>
          <el-descriptions-item label="任务 ID">{{ task.id }}</el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ new Date(task.created_at).toLocaleString() }}</el-descriptions-item>
          <el-descriptions-item label="开始时间">{{ task.started_at ? new Date(task.started_at).toLocaleString() : '-' }}</el-descriptions-item>
          <el-descriptions-item label="结束时间">{{ task.finished_at ? new Date(task.finished_at).toLocaleString() : '-' }}</el-descriptions-item>
        </el-descriptions>
      </el-card>
    </template>

    <!-- Log Drawer -->
    <el-drawer v-model="logDrawerVisible" title="任务日志" size="60%" :before-close="closeLogs" direction="rtl">
      <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px;">
        <el-checkbox v-model="logAutoScroll">自动滚动</el-checkbox>
        <el-select v-model="logPhaseFilter" size="small" style="width: 160px;">
          <el-option label="全部日志" value="all" />
          <el-option label="预检查" value="precheck" />
          <el-option label="Schema 迁移" value="schema" />
          <el-option label="数据迁移" value="data" />
          <el-option label="数据验证" value="validate" />
        </el-select>
      </div>
      <div ref="logContainer" v-loading="logsLoading" class="log-container">
        <div v-if="filteredLogs.length === 0 && !logsLoading" style="color: var(--tims-text-2); text-align: center; padding: 40px;">
          暂无日志
        </div>
        <div v-for="(log, idx) in filteredLogs" :key="idx" class="log-line" :class="logLevelClass(log.level)">
          <span class="log-time">{{ log.timestamp ? log.timestamp.replace('T', ' ').substring(0, 19) : '' }}</span>
          <span class="log-level">[{{ log.level }}]</span>
          <span class="log-msg">{{ log.message }}</span>
          <span v-if="log.caller" class="log-caller">({{ log.caller }})</span>
        </div>
      </div>
    </el-drawer>
  </div>
</template>

<style scoped>
.log-container {
  background: var(--tims-ink);
  border: 1px solid var(--tims-border-ink);
  color: var(--tims-text-inv);
  font-family: var(--tims-font-mono);
  font-size: var(--tims-font-xs);
  line-height: 1.7;
  padding: 14px 16px;
  border-radius: var(--tims-radius);
  height: calc(100vh - 240px);
  overflow-y: auto;
}

.phase-log-container {
  background: var(--tims-ink);
  border: 1px solid var(--tims-border-ink);
  color: var(--tims-text-inv);
  font-family: var(--tims-font-mono);
  font-size: var(--tims-font-xs);
  line-height: 1.7;
  padding: 14px 16px;
  border-radius: var(--tims-radius);
  max-height: 400px;
  overflow-y: auto;
}

.log-line {
  padding: 2px 0;
  border-bottom: 1px solid rgba(255, 255, 255, 0.05);
  white-space: pre-wrap;
  word-break: break-all;
}

.log-time {
  color: var(--tims-text-inv-2);
  margin-right: 8px;
}

.log-level {
  font-weight: 500;
  margin-right: 8px;
}

.log-msg {
  color: var(--tims-text-inv);
}

.log-caller {
  color: var(--tims-text-inv-2);
  margin-left: 8px;
  font-size: var(--tims-font-xs);
}

.log-info .log-level { color: #4fb5b5; } /* #t2 深底日志面板用浅青（深青在暗底不达标） */
.log-warn .log-level { color: var(--tims-amber); }
.log-error .log-level { color: #ee6a6a; } /* #t2 深底用浅红（品牌加深后 #cc3030 在暗底不足） */
.log-debug .log-level { color: #7fb069; }
.log-error { background: rgba(225, 60, 60, 0.10); }
.log-warn { background: rgba(217, 126, 0, 0.08); }
</style>
