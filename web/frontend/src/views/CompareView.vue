<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient from '../api'
import type { CompareTask, CompareReport, CompareOptions, CreateCompareRequest } from '../api'
import ConnectionForm from '../components/ConnectionForm.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import PageHeader from '../components/PageHeader.vue'
import { useSourceSchema } from '../composables/useSourceSchema'
import { useDataSources } from '../composables/useDataSources'
import { reconcileModel } from '../composables/reconcileModel'

// Standalone data comparison (独立数据比对): configure source + target, pick
// tables and a compare mode, run the validator directly — no migration.

const { sources, load: loadSources, getSource, capable, implementedKinds } = useSourceSchema()
const { load: loadDataSources, get: getDataSource } = useDataSources()
const sourceType = ref('postgres')
const currentMeta = computed(() => getSource(sourceType.value))

// MS-01 示范落位: the compare capability bit drives UI greying (single truth
// with the backend 400 guard — source.Capable). PG is unaffected (bit=true);
// a mysql profile is selectable but visibly gated.
const sourceCompareCapable = computed(() => capable(effectiveSourceType.value, 'compare'))
const comparePickerTypes = computed(() => {
  const kinds = implementedKinds().filter(k => k !== 'tidb')
  return kinds.length > 0 ? kinds : ['postgres']
})

// F-02 datasource refs ('' = manual entry).
const sourceRef = ref('')
const targetRef = ref('')
const effectiveSourceType = computed(() =>
  sourceRef.value ? (getDataSource(sourceRef.value)?.type || 'postgres') : sourceType.value)

const form = reactive({
  name: '',
  source: {} as Record<string, any>,
  target: {
    host: 'localhost',
    port: 4000,
    user: 'root',
    password: '',
    database: '',
  },
  mode: 'sample',
  sample_ratio: 0.01,
  checksum_chunk_size: 50000,
  // #t4: unified total concurrency budget (was parallel + checksum_parallel)
  concurrency: 4,
})

const compareModes = [
  { value: 'quick', label: '快速', color: '#0fa3a3', desc: '仅行数估算，最快' },
  { value: 'sample', label: '采样', color: '#2c4a8f', desc: '行数+随机采样（推荐）' },
  { value: 'checksum', label: '校验', color: '#d97e00', desc: '行数+分块Hash' },
  { value: 'watermark', label: '水位', color: '#7a4fd3', desc: '按水位列过滤后比对' },
]

// ---- #t3 watermark filter group ----
// "水位" is a filter DIMENSION, not a fourth algorithm: picking the card
// reveals a config area with the underlying algorithm (base_mode), the
// unified watermark column, the comparison operator and a type-aware value.
const wmConfig = reactive({
  base_mode: 'checksum',
  column: '',
  op: '<=',
  value: '',
})
const wmBaseModes = [
  { value: 'quick', label: '快速（行数）' },
  { value: 'sample', label: '采样' },
  { value: 'checksum', label: '校验（推荐）' },
]
const wmCommonColumns = ref<{ name: string; data_type: string; indexed: boolean }[]>([])
const wmLoadingCols = ref(false)

async function loadWmColumns() {
  if (!sourceRef.value) {
    ElMessage.warning('手填连接模式下请直接输入水位列名（运行期会逐表校验）')
    return
  }
  if (selectedTables.value.length === 0) {
    ElMessage.warning('请先选择表，再获取共用水位列')
    return
  }
  wmLoadingCols.value = true
  try {
    const { data } = await apiClient.columnsBatch(sourceRef.value, selectedTables.value)
    let inter: Map<string, { data_type: string; indexed: boolean }> | null = null
    for (const t of (data.tables || [])) {
      if (t.error) continue
      const m = new Map<string, { data_type: string; indexed: boolean }>()
      for (const c of (t.columns || [])) if (c.comparable) m.set(c.name, { data_type: c.data_type, indexed: c.indexed })
      if (inter === null) inter = m
      else for (const k of [...inter.keys()]) if (!m.has(k)) inter.delete(k)
    }
    wmCommonColumns.value = inter ? [...inter.entries()].map(([name, v]) => ({ name, ...v })) : []
    if (wmCommonColumns.value.length === 0) {
      ElMessage.warning('所选表没有可比类型的共用水位列（交集为空），请手输列名或调整表选择')
    }
  } catch (e: any) {
    ElMessage.error(`获取共用水位列失败: ${e.response?.data?.error || e.message}`)
  } finally {
    wmLoadingCols.value = false
  }
}

const wmSelectedCol = computed(() => wmCommonColumns.value.find(c => c.name === wmConfig.column))
const wmKind = computed<'datetime' | 'date' | 'int' | 'text'>(() => {
  const dt = wmSelectedCol.value?.data_type || ''
  if (dt.includes('timestamp')) return 'datetime'
  if (dt === 'date') return 'date'
  if (dt === 'integer' || dt === 'bigint') return 'int'
  return 'text'
})
const wmIsTimestamptz = computed(() => (wmSelectedCol.value?.data_type || '').includes('time zone'))

// 手填模式（无 columns-batch 元数据）时按值形态推断控件。
const wmKindInferred = computed(() => {
  if (wmSelectedCol.value) return wmKind.value
  if (/^\d+$/.test(wmConfig.value)) return 'int'
  if (/^\d{4}-\d{2}-\d{2}$/.test(wmConfig.value)) return 'date'
  if (/^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}/.test(wmConfig.value)) return 'datetime'
  return 'text'
})

function validateWmValue(): string {
  const v = wmConfig.value.trim()
  if (!v) return '请填写水位值'
  if (wmKindInferred.value === 'int' && !/^-?\d+$/.test(v)) return '整数水位列的值必须是整数'
  if (wmKindInferred.value === 'datetime' && !/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(v)) return '时间水位格式应为 YYYY-MM-DD HH:mm:ss'
  if (wmKindInferred.value === 'date' && !/^\d{4}-\d{2}-\d{2}$/.test(v)) return '日期水位格式应为 YYYY-MM-DD'
  const t = Date.parse(v.replace(' ', 'T') + (wmIsTimestamptz.value ? 'Z' : ''))
  if (!Number.isNaN(t) && t > Date.now()) return '水位值晚于当前时间，两侧可能都比不出任何行'
  return ''
}

// ---- table selection ----
const availableTables = ref<{ name: string; row_estimate: number }[]>([])
const loadingTables = ref(false)
const tableSearch = ref('')
const selectedTables = ref<string[]>([])

async function loadTables() {
  loadingTables.value = true
  availableTables.value = []
  selectedTables.value = []
  try {
    let data: { tables: { name: string; row_estimate: number }[] }
    if (sourceRef.value) {
      ;({ data } = await apiClient.getRefTables(sourceRef.value))
    } else if (sourceType.value === 'postgres') {
      ;({ data } = await apiClient.listTables({
        type: 'source',
        host: form.source.host,
        port: form.source.port,
        user: form.source.user,
        password: form.source.password,
        database: form.source.database,
        schema: form.source.schema,
        sslmode: form.source.sslmode,
      }))
    } else {
      ;({ data } = await apiClient.getSourceTables(sourceType.value, { ...form.source }))
    }
    availableTables.value = data.tables || []
  } catch (e: any) {
    ElMessage.error(`加载表列表失败: ${e.response?.data?.error || e.message}`)
  } finally {
    loadingTables.value = false
  }
}

const filteredTables = computed(() => {
  if (!tableSearch.value) return availableTables.value
  const kw = tableSearch.value.toLowerCase()
  return availableTables.value.filter(t => t.name.toLowerCase().includes(kw))
})

function handleTableSelection(rows: { name: string }[]) {
  selectedTables.value = rows.map(r => r.name)
}

// ---- connection tests ----
const testingSource = ref(false)
const sourceTestResult = ref<any>(null)
const testingTarget = ref(false)
const targetTestResult = ref<any>(null)

async function testSource() {
  if (sourceRef.value) {
    testingSource.value = true
    sourceTestResult.value = null
    try {
      const { data } = await apiClient.testDataSource(sourceRef.value)
      sourceTestResult.value = { success: data.success, message: data.message }
      if (data.success) ElMessage.success('数据源连接成功')
      else ElMessage.error(`连接失败: ${data.message}`)
    } catch (e: any) {
      ElMessage.error(`连接测试失败: ${e.response?.data?.error || e.message}`)
    } finally {
      testingSource.value = false
    }
    return
  }
  if (!currentMeta.value?.implemented) return
  testingSource.value = true
  sourceTestResult.value = null
  try {
    const { data } = await apiClient.testSourceConnection(sourceType.value, { ...form.source })
    sourceTestResult.value = data
    if (data.success) ElMessage.success(`${currentMeta.value.displayName} 连接成功`)
    else ElMessage.error(`连接失败: ${data.message}`)
  } catch (e: any) {
    ElMessage.error(`连接测试失败: ${e.message}`)
  } finally {
    testingSource.value = false
  }
}

async function testTarget() {
  testingTarget.value = true
  targetTestResult.value = null
  try {
    if (targetRef.value) {
      const { data } = await apiClient.testDataSource(targetRef.value)
      targetTestResult.value = { ok: data.success, error: data.message }
      if (data.success) ElMessage.success('TiDB 数据源连接成功')
      else ElMessage.error(`连接失败: ${data.message}`)
      return
    }
    const { data } = await apiClient.testConnection({
      type: 'target',
      host: form.target.host,
      port: form.target.port,
      user: form.target.user,
      password: form.target.password,
      database: form.target.database,
    })
    targetTestResult.value = data
    if (data.ok) ElMessage.success('TiDB 连接成功')
    else ElMessage.error(`连接失败: ${data.error}`)
  } catch (e: any) {
    ElMessage.error(`连接测试失败: ${e.message}`)
  } finally {
    testingTarget.value = false
  }
}

function onSourceTypeChange(name: string) {
  const oldMeta = getSource(sourceType.value)
  const newMeta = getSource(name)
  if (!newMeta) return
  const reconciled = reconcileModel(form.source, oldMeta ?? newMeta, newMeta)
  Object.keys(form.source).forEach(k => { delete form.source[k] })
  Object.assign(form.source, reconciled)
  sourceType.value = name
  sourceTestResult.value = null
}

// ---- compare options memory (F-02b: silent, params only) ----
// Connections are configured via the datasource registry + Picker; what stays
// remembered here is just the comparison parameters (mode/sample/parallel).
// optionsLoaded guards against the auto-fill racing user input: the form is
// read-only until the saved options (if any) are applied on mount.
const optionsLoaded = ref(false)

function applyCompareOptions(opts: CompareOptions) {
  if (!opts) return
  // Connection fields in the payload (legacy saves) are intentionally ignored.
  // #t3: a saved watermark group restores the "水位" card + its config.
  if (opts.watermark) {
    const w = opts.watermark
    wmConfig.base_mode = ['quick', 'sample', 'checksum'].includes(w.base_mode) ? w.base_mode : 'checksum'
    wmConfig.column = w.column || ''
    wmConfig.op = w.op === '<' ? '<' : '<='
    wmConfig.value = w.value || ''
    form.mode = 'watermark'
  } else if (opts.mode && ['quick', 'sample', 'checksum'].includes(opts.mode)) {
    form.mode = opts.mode
  }
  const num = (v: number | undefined) => (v !== undefined && v !== null ? v : null)
  const sr = num(opts.sample_ratio); if (sr !== null) form.sample_ratio = sr
  const cs = num(opts.checksum_chunk_size); if (cs !== null) form.checksum_chunk_size = cs
  // #t4: unified budget; legacy saves map through the same max() the backend uses.
  const c = num(opts.concurrency)
  if (c !== null && c > 0) {
    form.concurrency = Math.min(Math.max(c, 1), 8)
  } else {
    const lp = num(opts.parallel) ?? 0
    const lcp = num(opts.checksum_parallel) ?? 0
    if (lp > 0 || lcp > 0) form.concurrency = Math.min(Math.max(lp, lcp), 8)
  }
}

async function loadSavedConnection() {
  try {
    const { data } = await apiClient.getCompareOptions()
    applyCompareOptions(data)
  } catch {
    // Silent memory: a failed load just means defaults.
  } finally {
    optionsLoaded.value = true
  }
}

// Silent save of the comparison parameters after a successful run start
// (no button, no connection fields in the payload).
async function saveCompareParams() {
  try {
    await apiClient.saveCompareOptions({
      mode: form.mode === 'watermark' ? wmConfig.base_mode : form.mode,
      sample_ratio: form.sample_ratio,
      checksum_chunk_size: form.checksum_chunk_size,
      concurrency: form.concurrency,
      // present-and-null clears (partial-merge by presence)
      watermark: form.mode === 'watermark'
        ? { column: wmConfig.column, value: wmConfig.value, op: wmConfig.op, base_mode: wmConfig.base_mode }
        : null,
    } as any)
  } catch {
    // Silent memory: a failed save must never disturb the running compare.
  }
}

// ---- run + progress ----
const starting = ref(false)
const tasks = ref<CompareTask[]>([])
const activeTask = ref<CompareTask | null>(null)
const activeReport = ref<CompareReport | null>(null)
const loadingReport = ref(false)
let ws: WebSocket | null = null
let pollTimer: number | undefined

async function refreshTasks() {
  try {
    const { data } = await apiClient.listCompares()
    tasks.value = data || []
    if (activeTask.value) {
      const prevStatus = activeTask.value.status
      const fresh = tasks.value.find(t => t.id === activeTask.value!.id)
      if (fresh) {
        activeTask.value = fresh
        // Fallback when the terminal WS message was dropped (L3): the poll
        // alone must still load the finished report.
        if (prevStatus === 'running' && fresh.status === 'completed') {
          loadReport(fresh.id)
        }
      }
    }
  } catch {}
}

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  ws = new WebSocket(`${proto}//${location.host}/api/v1/ws`)
  ws.onmessage = async (event) => {
    try {
      const data = JSON.parse(event.data)
      if (data.type !== 'compare' || !data.task_id) return
      if (activeTask.value && data.task_id === activeTask.value.id) {
        activeTask.value.tables_done = data.tables_done ?? activeTask.value.tables_done
        activeTask.value.tables_total = data.tables_total ?? activeTask.value.tables_total
        activeTask.value.current_table = data.current_table ?? ''
        activeTask.value.status = data.status ?? activeTask.value.status
        if (data.status && data.status !== 'running') {
          await refreshTasks()
          await loadReport(data.task_id)
        }
      }
      await refreshTasks()
    } catch {}
  }
}

async function loadReport(id: string) {
  loadingReport.value = true
  try {
    const { data } = await apiClient.getCompareReport(id)
    activeReport.value = data
  } catch {
    activeReport.value = null
  } finally {
    loadingReport.value = false
  }
}

async function startCompare() {
  if (!sourceRef.value && !form.source.host) {
    ElMessage.warning('请先选择源库数据源或填写连接信息')
    return
  }
  if (!targetRef.value && !form.target.host) {
    ElMessage.warning('请先选择目标库数据源或填写连接信息')
    return
  }
  starting.value = true
  try {
    // #t3: the "水位" card maps to mode=base_mode + watermark group.
    let watermark: CreateCompareRequest['watermark']
    let mode = form.mode
    if (form.mode === 'watermark') {
      wmConfig.column = wmConfig.column.trim()
      if (!wmConfig.column) {
        ElMessage.warning('请填写水位列名')
        starting.value = false
        return
      }
      const verr = validateWmValue()
      if (verr) {
        ElMessage.warning(`水位值：${verr}`)
        starting.value = false
        return
      }
      watermark = { column: wmConfig.column, value: wmConfig.value.trim(), op: wmConfig.op, base_mode: wmConfig.base_mode }
      mode = wmConfig.base_mode
    }
    const { data } = await apiClient.createCompare({
      name: form.name || `Compare ${new Date().toLocaleString()}`,
      source_ref: sourceRef.value || undefined,
      target_ref: targetRef.value || undefined,
      source: { ...form.source, type: effectiveSourceType.value },
      target: { ...form.target },
      mode,
      sample_ratio: form.sample_ratio,
      checksum_chunk_size: form.checksum_chunk_size,
      concurrency: form.concurrency,
      tables: selectedTables.value,
      watermark,
    })
    activeTask.value = data
    activeReport.value = null
    ElMessage.success('比对任务已发起')
    saveCompareParams() // silent params memory (fire-and-forget)
    await refreshTasks()
  } catch (e: any) {
    ElMessage.error(`发起失败: ${e.response?.data?.error || e.message}`)
  } finally {
    starting.value = false
  }
}

async function cancelActive() {
  if (!activeTask.value) return
  try {
    await ElMessageBox.confirm('确认取消当前比对任务？进行中的比对将中断。', '确认', { type: 'warning' })
  } catch { return }
  try {
    await apiClient.cancelCompare(activeTask.value.id)
    ElMessage.success('已请求取消')
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || e.message)
  }
}

async function viewTask(t: CompareTask) {
  activeTask.value = t
  activeReport.value = null
  if (t.status === 'completed') await loadReport(t.id)
}

async function removeTask(t: CompareTask) {
  try {
    await ElMessageBox.confirm(`确认删除比对任务「${t.name}」？删除后不可恢复。`, '确认', { type: 'warning' })
  } catch { return }
  try {
    await apiClient.deleteCompare(t.id)
    if (activeTask.value?.id === t.id) {
      activeTask.value = null
      activeReport.value = null
    }
    await refreshTasks()
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || e.message)
  }
}

const progressPct = computed(() => {
  const t = activeTask.value
  if (!t || !t.tables_total) return 0
  return Math.round((t.tables_done / t.tables_total) * 100)
})

function statusTag(s: string) {
  return s === 'completed' ? 'success' : s === 'running' ? 'primary' : s === 'failed' ? 'danger' : 'info'
}
function statusText(s: string) {
  return { completed: '已完成', running: '比对中', failed: '失败', cancelled: '已取消' }[s] || s
}
function tableStatusTag(s: string) {
  return { pass: 'success', fail: 'danger', warn: 'warning', skip: 'info' }[s] || 'info'
}

// Diff-count heat scale: 0 → teal, small → amber, large → brand red.
function diffHeat(n: number | undefined): string {
  const d = n ?? 0
  if (d === 0) return 'diff-zero'
  if (d < 100) return 'diff-low'
  return 'diff-high'
}

// ---- report export (FEAT-0933, client-side CSV + HTML) ----

function reportFileName(ext: string) {
  const d = new Date()
  const p = (n: number) => String(n).padStart(2, '0')
  const ts = `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`
  return `compare-report-${activeTask.value?.id ?? 'x'}-${ts}.${ext}`
}

function downloadBlob(content: BlobPart, mime: string, fileName: string) {
  const url = URL.createObjectURL(new Blob([content], { type: mime }))
  const a = document.createElement('a')
  a.href = url
  a.download = fileName
  a.click()
  URL.revokeObjectURL(url)
}

function csvCell(v: unknown): string {
  let s = v === undefined || v === null ? '' : String(v)
  // Excel formula-injection guard: a cell starting with =/+/@ (or a '-'
  // not part of a plain negative number) would be interpreted as a
  // formula in Excel — prefix a single quote to defuse it.
  if (/^[=+@]/.test(s) || (/^-/.test(s) && !/^-?\d+(\.\d+)?$/.test(s))) s = `'` + s
  return /[",\n\r]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s
}

function exportCsv() {
  const r = activeReport.value
  if (!r) return
  const lines: string[] = []
  lines.push(['总体状态', csvCell(r.overall_status)].join(','))
  lines.push(['开始时间', csvCell(r.start_time)].join(','))
  lines.push(['结束时间', csvCell(r.end_time)].join(','))
  lines.push(['耗时', csvCell(r.duration)].join(','))
  if (r.summary) lines.push(['摘要', csvCell(r.summary)].join(','))
  lines.push('')
  lines.push(
    ['总表数', '通过', '失败', '警告', '跳过', '源总行数', '目标总行数', '差异总行数'].map(csvCell).join(','),
  )
  lines.push(
    [
      r.stats.total_tables, r.stats.pass_tables, r.stats.fail_tables,
      r.stats.warn_tables, r.stats.skip_tables,
      r.stats.total_source_rows, r.stats.total_target_rows, r.stats.total_diff_rows,
    ].map(csvCell).join(','),
  )
  lines.push('')
  lines.push(
    ['表名', '状态', '源行数', '目标行数', '差异行数', '耗时', '错误', '建议'].map(csvCell).join(','),
  )
  for (const t of r.tables) {
    lines.push(
      [t.table_name, t.status, t.source_rows, t.target_rows, t.diff_rows, t.duration, t.error, t.suggestion]
        .map(csvCell).join(','),
    )
  }
  // UTF-8 BOM so Excel opens Chinese cells correctly.
  try {
    downloadBlob('\uFEFF' + lines.join('\r\n'), 'text/csv;charset=utf-8', reportFileName('csv'))
  } catch (e: any) {
    ElMessage.error(`导出 CSV 失败：${e?.message || e}`)
  }
}

function esc(s: unknown): string {
  return String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')
}

function exportHtml() {
  const r = activeReport.value
  if (!r) return
  // FEAT-0933b: style aligned with the server-rendered report family
  // (reporter.ReportCSS tokens: ink #0C1222 / workspace #F6F7FB / brand
  // #E13C3C / teal #0FA3A3 / amber #D97E00; banner + badges + stat cards
  // + zebra/hover table + print-safe colors).
  const badge = (s: string) =>
    `<span class="badge badge-${esc(s)}">${esc(s)}</span>`
  const overallClass =
    r.overall_status === 'pass' ? 'overall-pass' : r.overall_status === 'fail' ? 'overall-fail' : 'overall-warn'
  const rows = r.tables
    .map(
      (t) => `<tr>
<td>${esc(t.table_name)}</td><td>${badge(t.status)}</td>
<td class="num">${esc(t.source_rows)}</td><td class="num">${esc(t.target_rows)}</td>
<td class="num">${esc(t.diff_rows)}</td><td>${esc(t.duration)}</td>
<td>${esc(t.error || '')}</td><td>${esc(t.suggestion || '')}</td></tr>`,
    )
    .join('\n')
  const stat = (label: string, value: number | string, cls = '') =>
    `<div class="stat"><div class="value ${cls}">${esc(value)}</div><div class="label">${esc(label)}</div></div>`
  const html = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>数据比对报告 ${esc(activeTask.value?.name || '')}</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:'Inter','HarmonyOS Sans SC','MiSans','Segoe UI','PingFang SC','Microsoft YaHei',system-ui,sans-serif;background:#F6F7FB;color:#2A3040;line-height:1.6;-webkit-font-smoothing:antialiased}
.container{max-width:1200px;margin:0 auto;padding:24px}
.num{font-family:'JetBrains Mono','Cascadia Mono',Consolas,ui-monospace,monospace;font-weight:500}
.banner{background:linear-gradient(135deg,#0C1222,#223052);border-radius:12px;padding:28px 32px;color:#EEF1F8;margin-bottom:20px;box-shadow:0 2px 4px rgba(12,18,34,.06),0 10px 28px rgba(12,18,34,.10)}
.banner h1{font-size:24px;font-weight:600}
.banner .sub{font-size:13px;opacity:.75;margin-top:6px}
.banner .overall{float:right;font-size:15px;font-weight:600;padding:4px 16px;border-radius:999px;background:rgba(255,255,255,.16)}
.card{background:#FFF;border:1px solid #E6E8F0;border-radius:12px;padding:24px;margin-bottom:20px;box-shadow:0 1px 2px rgba(12,18,34,.04),0 4px 14px rgba(12,18,34,.06)}
.card h2{font-size:18px;margin-bottom:16px;color:#0C1222;border-bottom:1px solid #E6E8F0;padding-bottom:8px}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:16px;margin-bottom:12px}
.stat{text-align:center;padding:14px 18px;background:#FFF;border:1px solid #E6E8F0;border-radius:12px;box-shadow:0 1px 2px rgba(12,18,34,.04),0 4px 14px rgba(12,18,34,.06)}
.stat .value{font-family:'JetBrains Mono','Cascadia Mono',Consolas,ui-monospace,monospace;font-weight:500;font-size:26px;line-height:1.15;color:#0C1222}
.stat .label{font-size:12px;color:#6B7280;margin-top:4px;letter-spacing:.4px}
.info-row{display:flex;justify-content:space-between;padding:8px 0;border-bottom:1px solid #E6E8F0}
.info-row:last-child{border-bottom:none}
.info-label{color:#6B7280;min-width:120px}
.info-value{font-weight:500;text-align:right}
table{width:100%;border-collapse:collapse;font-size:14px}
th{background:#F3F4FA;color:#2A3040;padding:10px 12px;text-align:left;font-weight:600;border-bottom:1px solid #E6E8F0}
td{padding:10px 12px;border-bottom:1px solid #E6E8F0}
tr:nth-child(even) td{background:#FAFBFD}
tr:hover td{background:#F6F7FB}
.badge{display:inline-block;padding:2px 10px;border-radius:999px;font-size:12px;font-weight:600}
.badge-pass{background:rgba(15,163,163,.10);color:#0FA3A3}
.badge-fail{background:rgba(225,60,60,.09);color:#E13C3C}
.badge-warn{background:rgba(217,126,0,.10);color:#D97E00}
.badge-skip{background:#EEF0F5;color:#6B7280}
.overall-pass{color:#0FA3A3}.overall-fail{color:#E13C3C}.overall-warn{color:#D97E00}
.c-teal{color:#0FA3A3}.c-brand{color:#E13C3C}
.footer{text-align:center;color:#97A0B5;font-size:12px;margin-top:24px}
@media print{body{background:#FFF}.container{padding:0}.card,.stat{box-shadow:none}.banner{box-shadow:none;-webkit-print-color-adjust:exact;print-color-adjust:exact}.badge,tr:nth-child(even) td{-webkit-print-color-adjust:exact;print-color-adjust:exact}}
</style></head><body>
<div class="container">
<div class="banner">
<span class="overall ${overallClass}">${esc(r.overall_status)}</span>
<h1>数据比对报告 — ${esc(activeTask.value?.name || '')}</h1>
<div class="sub">任务 ${esc(activeTask.value?.id || '')} · 模式 ${esc(activeTask.value?.mode || '')} · 耗时 ${esc(r.duration)}</div>
</div>
<div class="card">
<h2>汇总</h2>
<div class="stats">
${stat('总表数', r.stats.total_tables)}
${stat('通过', r.stats.pass_tables, 'c-teal')}
${stat('失败', r.stats.fail_tables, 'c-brand')}
${stat('警告', r.stats.warn_tables)}
${stat('跳过', r.stats.skip_tables)}
${stat('源总行数', r.stats.total_source_rows)}
${stat('目标总行数', r.stats.total_target_rows)}
${stat('差异总行数', r.stats.total_diff_rows, 'c-brand')}
</div>
<div class="info-row"><span class="info-label">开始时间</span><span class="info-value">${esc(r.start_time)}</span></div>
<div class="info-row"><span class="info-label">结束时间</span><span class="info-value">${esc(r.end_time)}</span></div>
${r.summary ? `<div class="info-row"><span class="info-label">摘要</span><span class="info-value">${esc(r.summary)}</span></div>` : ''}
</div>
<div class="card">
<h2>逐表明细</h2>
<table>
<thead><tr><th>表名</th><th>状态</th><th>源行数</th><th>目标行数</th><th>差异行数</th><th>耗时</th><th>错误</th><th>建议</th></tr></thead>
<tbody>
${rows}
</tbody></table>
</div>
<div class="footer">由 TimsTool 生成 · ${new Date().toLocaleString()}（浏览器打印可另存为 PDF）</div>
</div>
</body></html>`
  try {
    downloadBlob(html, 'text/html;charset=utf-8', reportFileName('html'))
  } catch (e: any) {
    ElMessage.error(`导出 HTML 失败：${e?.message || e}`)
  }
}

onMounted(async () => {
  await loadSources()
  loadDataSources()
  const meta = getSource(sourceType.value)
  if (meta) Object.assign(form.source, reconcileModel({}, meta, meta))
  // Silent params memory: auto-fill the last compare parameters; the form is
  // disabled until this completes so the fill can't race user input.
  await loadSavedConnection()
  await refreshTasks()
  connectWS()
  pollTimer = window.setInterval(() => {
    if (activeTask.value?.status === 'running') refreshTasks()
  }, 5000)
})

onUnmounted(() => {
  ws?.close()
  if (pollTimer) window.clearInterval(pollTimer)
})
</script>

<template>
  <div class="tims-page">
    <PageHeader title="数据比对" subtitle="配置源/目标连接后手工比对，不执行迁移" />
    <el-card shadow="never" style="margin-bottom: 20px;">
      <template #header>
        <div style="display: flex; align-items: center; gap: 8px;">
          <el-icon size="24"><Grid /></el-icon>
          <span style="font-size: var(--tims-font-md); font-weight: 600;">连接配置</span>
        </div>
      </template>

      <el-form label-width="120px" :disabled="!optionsLoaded" v-loading="!optionsLoaded">
        <el-row :gutter="24">
          <el-col :span="12">
            <el-divider content-position="left">源数据库（{{ effectiveSourceType === 'postgres' ? 'PostgreSQL' : effectiveSourceType }}）</el-divider>
            <el-form-item label="数据源">
              <DataSourcePicker v-model="sourceRef" :types="comparePickerTypes" />
            </el-form-item>
            <template v-if="!sourceRef">
            <el-form-item label="数据源类型" v-if="sources.length > 0">
              <el-select :model-value="sourceType" style="width: 100%" @change="onSourceTypeChange">
                <el-option v-for="s in sources" :key="s.name" :label="s.displayName" :value="s.name" :disabled="!s.implemented" />
              </el-select>
            </el-form-item>
            <ConnectionForm v-if="currentMeta" :meta="currentMeta" :model="form.source" />
            </template>
            <el-form-item>
              <el-button :loading="testingSource" :disabled="!sourceRef && !currentMeta?.implemented" @click="testSource">
                {{ sourceRef ? '测试数据源连接' : `测试 ${currentMeta?.displayName || 'PostgreSQL'} 连接` }}
              </el-button>
              <el-tag v-if="sourceTestResult" :type="sourceTestResult.success ? 'success' : 'danger'" style="margin-left: 12px;">
                {{ sourceTestResult.success ? '连接成功' : sourceTestResult.message }}
              </el-tag>
            </el-form-item>
          </el-col>

          <el-col :span="12">
            <el-divider content-position="left">目标数据库（TiDB）</el-divider>
            <el-form-item label="数据源">
              <DataSourcePicker v-model="targetRef" :types="['tidb']" />
            </el-form-item>
            <template v-if="!targetRef">
            <el-form-item label="主机地址"><el-input v-model="form.target.host" /></el-form-item>
            <el-form-item label="端口"><el-input-number v-model="form.target.port" :min="1" :max="65535" /></el-form-item>
            <el-form-item label="用户名"><el-input v-model="form.target.user" /></el-form-item>
            <el-form-item label="密码"><el-input v-model="form.target.password" type="password" show-password /></el-form-item>
            <el-form-item label="数据库名"><el-input v-model="form.target.database" /></el-form-item>
            </template>
            <el-form-item>
              <el-button :loading="testingTarget" @click="testTarget">{{ targetRef ? '测试数据源连接' : '测试 TiDB 连接' }}</el-button>
              <el-tag v-if="targetTestResult" :type="targetTestResult.ok ? 'success' : 'danger'" style="margin-left: 12px;">
                {{ targetTestResult.ok ? '连接成功' : targetTestResult.error }}
              </el-tag>
            </el-form-item>
          </el-col>
        </el-row>

        <el-divider content-position="left">比对配置</el-divider>
        <el-form-item label="任务名称">
          <el-input v-model="form.name" placeholder="可选，自动生成" style="width: 350px;" />
        </el-form-item>
        <el-form-item label="对比模式">
          <div style="display: flex; gap: 12px; flex-wrap: wrap;">
            <div
              v-for="m in compareModes" :key="m.value"
              @click="sourceCompareCapable && (form.mode = m.value)"
              :style="{
                padding: '10px 16px', borderRadius: '8px',
                cursor: sourceCompareCapable ? 'pointer' : 'not-allowed',
                opacity: sourceCompareCapable ? 1 : 0.45,
                textAlign: 'center',
                border: form.mode === m.value ? '2px solid ' + m.color : '2px solid #dcdfe6',
                background: form.mode === m.value ? m.color + '10' : '#fff',
              }"
            >
              <div style="font-weight: bold;">{{ m.label }}</div>
              <div style="font-size: var(--tims-font-xs); color: var(--tims-text-2);">{{ m.desc }}</div>
            </div>
          </div>
          <el-alert
            v-if="!sourceCompareCapable"
            type="warning" :closable="false" style="margin-top: 8px; width: 100%;"
            :title="`当前源类型（${effectiveSourceType}）暂不支持数据比对，等待多源化任务（MS-08）接入后开放`"
          />
        </el-form-item>
        <el-form-item v-if="form.mode === 'sample' || (form.mode === 'watermark' && wmConfig.base_mode === 'sample')" label="采样率">
          <el-input-number v-model="form.sample_ratio" :min="0.001" :max="1" :step="0.01" />
        </el-form-item>
        <el-form-item v-if="form.mode === 'checksum' || (form.mode === 'watermark' && wmConfig.base_mode === 'checksum')" label="分块大小">
          <el-input-number v-model="form.checksum_chunk_size" :min="1000" :step="10000" />
        </el-form-item>
        <template v-if="form.mode === 'watermark'">
          <el-form-item label="底层算法">
            <el-radio-group v-model="wmConfig.base_mode">
              <el-radio v-for="m in wmBaseModes" :key="m.value" :value="m.value">{{ m.label }}</el-radio>
            </el-radio-group>
          </el-form-item>
          <el-form-item label="水位列">
            <div style="width: 100%;">
              <div style="display: flex; gap: 8px; align-items: center; flex-wrap: wrap;">
                <el-select v-if="wmCommonColumns.length" v-model="wmConfig.column" filterable placeholder="所选表共有可比列" style="width: 300px;">
                  <el-option v-for="c in wmCommonColumns" :key="c.name" :value="c.name"
                             :label="`${c.name}（${c.data_type}${c.indexed ? '' : ' · 无索引'}）`" />
                </el-select>
                <el-input v-else v-model="wmConfig.column" placeholder="水位列名（如 updated_at）" style="width: 300px;" />
                <el-button size="small" :loading="wmLoadingCols" :disabled="!sourceRef" @click="loadWmColumns">获取共用水位列</el-button>
                <el-select v-model="wmConfig.op" style="width: 90px;">
                  <el-option value="<=" label="≤（含边界）" />
                  <el-option value="<" label="<（严格）" />
                </el-select>
                <el-date-picker v-if="wmKindInferred === 'datetime'" v-model="wmConfig.value" type="datetime"
                                value-format="YYYY-MM-DD HH:mm:ss" placeholder="水位值 YYYY-MM-DD HH:mm:ss" style="width: 230px;" />
                <el-date-picker v-else-if="wmKindInferred === 'date'" v-model="wmConfig.value" type="date"
                                value-format="YYYY-MM-DD" placeholder="水位值 YYYY-MM-DD" style="width: 230px;" />
                <el-input v-else v-model="wmConfig.value" placeholder="水位值" style="width: 230px;" />
              </div>
              <div style="font-size: var(--tims-font-xs); color: var(--tims-text-2); margin-top: 6px;">
                两端按同一条件过滤后比对，消除「源端持续写入+同步滞后」的永久差异假象；NULL 水位行两端一致排除。
                <span v-if="wmIsTimestamptz" style="color: var(--tims-tag-warning-text);">timestamptz 列请输入 UTC 时间。</span>
                <span v-if="wmSelectedCol && !wmSelectedCol.indexed" style="color: var(--tims-tag-warning-text);">该列无索引，过滤可能全表扫描。</span>
              </div>
            </div>
          </el-form-item>
        </template>
        <el-form-item label="总并发数">
          <el-input-number v-model="form.concurrency" :min="1" :max="8" />
          <span style="color: var(--tims-text-2); font-size: var(--tims-font-xs); margin-left: 8px;">任务同时执行的数据库查询数（所有模式共用，1–8）</span>
        </el-form-item>

        <el-form-item label="选择表">
          <div style="width: 100%;">
            <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px;">
              <el-button size="small" @click="loadTables">加载表列表</el-button>
              <span style="color: var(--tims-text-2); font-size: var(--tims-font-sm);">已选 {{ selectedTables.length }} / {{ availableTables.length }} 张表</span>
            </div>
            <el-input v-model="tableSearch" placeholder="搜索表名" style="width: 300px; margin-bottom: 8px;" clearable />
            <el-table
              v-if="availableTables.length > 0"
              ref="tableRef"
              :data="filteredTables"
              @selection-change="handleTableSelection"
              max-height="260"
              :row-key="(row: any) => row.name"
            >
              <el-table-column type="selection" width="55" :reserve-selection="true" />
              <el-table-column prop="name" label="表名" />
              <el-table-column label="预估行数" width="160" align="right">
                <template #default="{ row }">
                  <span style="color: var(--tims-text-2);">{{ row.row_estimate >= 0 ? row.row_estimate.toLocaleString() : '-' }}</span>
                </template>
              </el-table-column>
            </el-table>
            <el-alert v-if="selectedTables.length === 0" type="info" :closable="false" title="未选择任何表，将比对全部表" style="margin-top: 8px;" />
          </div>
        </el-form-item>

        <el-form-item>
          <el-button type="primary" :loading="starting" :disabled="!sourceCompareCapable" @click="startCompare">
            开始比对
          </el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card v-if="activeTask" shadow="never" style="margin-bottom: 20px;">
      <template #header>
        <div style="display: flex; justify-content: space-between; align-items: center;">
          <span style="font-weight: bold;">{{ activeTask.name }}（{{ activeTask.id }}）</span>
          <el-space>
            <el-tag :type="statusTag(activeTask.status)">{{ statusText(activeTask.status) }}</el-tag>
            <el-button v-if="activeTask.status === 'running'" size="small" type="danger" @click="cancelActive">取消</el-button>
          </el-space>
        </div>
      </template>

      <el-progress
        :percentage="progressPct"
        :status="activeTask.status === 'failed' ? 'exception' : activeTask.status === 'completed' ? 'success' : undefined"
        style="margin-bottom: 12px;"
      />
      <div v-if="activeTask.status === 'running'" style="color: var(--tims-text-2); margin-bottom: 12px;">
        比对进度：{{ activeTask.tables_done }} / {{ activeTask.tables_total || '?' }} 张表
        <span v-if="activeTask.current_table">（当前：{{ activeTask.current_table }}）</span>
      </div>
      <el-alert v-if="activeTask.error" type="error" :closable="false" :title="activeTask.error" style="margin-bottom: 12px;" />

      <template v-if="activeReport">
        <div style="margin-bottom: 12px; text-align: right;">
          <el-button size="small" @click="exportCsv">导出 CSV</el-button>
          <el-button size="small" @click="exportHtml">导出 HTML</el-button>
        </div>
        <div class="tims-gauge-row">
          <div class="tims-gauge">
            <span class="tims-gauge-label">总表数</span>
            <span class="tims-gauge-value tims-num">{{ activeReport.stats.total_tables }}</span>
          </div>
          <div class="tims-gauge">
            <span class="tims-gauge-label">通过</span>
            <span class="tims-gauge-value tims-num is-teal">{{ activeReport.stats.pass_tables }}</span>
          </div>
          <div class="tims-gauge">
            <span class="tims-gauge-label">失败</span>
            <span class="tims-gauge-value tims-num is-brand">{{ activeReport.stats.fail_tables }}</span>
          </div>
          <div class="tims-gauge">
            <span class="tims-gauge-label">耗时</span>
            <span class="tims-gauge-value tims-num">{{ activeReport.duration }}</span>
          </div>
        </div>
        <el-table :data="activeReport.tables" max-height="400" size="small" style="margin-bottom: 12px;">
          <el-table-column prop="table_name" label="表名" min-width="140" />
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag :type="tableStatusTag(row.status)" size="small">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="source_rows" label="源行数" width="110" align="right" class-name="tims-num-col" />
          <el-table-column prop="target_rows" label="目标行数" width="110" align="right" class-name="tims-num-col" />
          <el-table-column label="差异行数" width="100" align="right">
            <template #default="{ row }">
              <span class="tims-num" :class="diffHeat(row.diff_rows)">{{ row.diff_rows }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="duration" label="耗时" width="90" />
          <el-table-column prop="error" label="错误/建议" min-width="200" show-overflow-tooltip />
        </el-table>
      </template>
      <div v-else-if="activeTask.status === 'completed'" v-loading="loadingReport" style="min-height: 80px;"></div>
    </el-card>

    <el-card shadow="never">
      <template #header><span style="font-weight: bold;">比对历史</span></template>
      <el-table :data="tasks" size="small">
        <el-table-column prop="id" label="ID" width="90" />
        <el-table-column prop="name" label="名称" min-width="160" />
        <el-table-column label="模式" width="90">
          <template #default="{ row }">
            <span>{{ row.mode }}</span>
            <el-tag v-if="row.watermark" size="small" type="warning" style="margin-left: 4px;">水位</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="statusTag(row.status)" size="small">{{ statusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="表进度" width="120">
          <template #default="{ row }">{{ row.tables_done }} / {{ row.tables_total || '?' }}</template>
        </el-table-column>
        <el-table-column label="创建时间" width="170">
          <template #default="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
        </el-table-column>
        <el-table-column label="操作" width="140">
          <template #default="{ row }">
            <el-button size="small" link type="primary" @click="viewTask(row)">查看</el-button>
            <el-button size="small" link type="danger" :disabled="row.status === 'running'" @click="removeTask(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>
  </div>
</template>

<style scoped>
.tims-gauge-row {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 12px;
  margin-bottom: 12px;
}
.diff-zero { color: var(--tims-tag-success-text); } /* #t2 对比度二轮 */
.diff-low { color: var(--tims-amber); }
.diff-high { color: var(--tims-brand); font-weight: 500; }
:deep(.tims-num-col .cell) { font-family: var(--tims-font-mono); }

@media (max-width: 900px) {
  .tims-gauge-row { grid-template-columns: repeat(2, 1fr); }
}
</style>
