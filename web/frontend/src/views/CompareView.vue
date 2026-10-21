<script setup lang="ts">
import { ref, reactive, computed, watch, onMounted, onUnmounted } from 'vue'
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
  // #range: single-value vs half-open interval [range_start, range_end).
  // range_mode is only offered for datetime/date columns (int/text stay
  // single-value); when active, op/value are ignored and both endpoints
  // are bound together.
  range_mode: false,
  range_start: '',
  range_end: '',
})
const wmBaseModes = [
  { value: 'quick', label: '快速（行数）' },
  { value: 'sample', label: '采样' },
  { value: 'checksum', label: '校验（推荐）' },
]
// ---- #ignore: value-comparison exclusion columns ----
// Columns that are structurally divergent on the two ends (auto-increment
// ids, side-specific created_at/updated_at clocks) can be excluded from
// value comparison. The list is a global multi-tag input (exact column
// names, matched case-insensitively); absent = compare every column.
const ignoreCols = ref<string[]>([])
// ---- #wm-candidates: suggest 式 Top-N 候选水位列 ----
// The compare page now rides the SAME suggest scorer as the watermark
// module (coverage/name/type/index scoring over the SELECTED tables), with
// allow-create as the manual-entry fallback and missing/unmatched tables
// disclosed explicitly — the strict columns-batch intersection is gone.
interface WmSuggestCandidate {
  column: string
  score: number
  coverage: number
  indexed_ratio: number
  default_now_ratio: number
  type_histogram: Record<string, number>
  matched_tables: string[]
  unmatched_tables: string[]
  reasons: string[]
  warnings: string[]
}
const wmCandidates = ref<WmSuggestCandidate[]>([])
const wmMissingTables = ref<string[]>([])
const wmSuggestTotal = ref(0)
const wmLoadingCols = ref(false)

// The candidate matching the current column pick (undefined for a
// hand-typed name — those keep the value-shape inference path).
const activeWmCandidate = computed(() => wmCandidates.value.find(c => c.column === wmConfig.column))

// Dominant comparable type (type_histogram max key) drives the value
// control kind; null when no candidate is active.
const wmDominantType = computed<string>(() => {
  const h = activeWmCandidate.value?.type_histogram || {}
  let best = ''
  let n = -1
  for (const [k, v] of Object.entries(h)) {
    if (v > n) { n = v; best = k }
  }
  return best
})

// Disclosure states (error 显式披露): all-missing is a hard error; a
// partial missing set is a warning; no candidates over existing tables
// points at manual entry — never the old misleading empty-intersection
// wording.
const wmAllMissing = computed(() =>
  wmMissingTables.value.length > 0 && wmMissingTables.value.length >= selectedTables.value.length)
const wmNoCandidates = computed(() =>
  wmCandidates.value.length === 0 && !wmAllMissing.value)

async function loadWmColumns() {
  if (!sourceRef.value) {
    ElMessage.warning('手填连接模式下请直接输入水位列名（运行期缺列表将全量比对）')
    return
  }
  if (selectedTables.value.length === 0) {
    ElMessage.warning('请先选择表，再获取候选水位列')
    return
  }
  wmLoadingCols.value = true
  try {
    const { data } = await apiClient.suggestWatermark(sourceRef.value, selectedTables.value)
    wmCandidates.value = (data.candidates || []) as WmSuggestCandidate[]
    wmMissingTables.value = data.missing_tables || []
    wmSuggestTotal.value = data.total_tables || 0
    if (wmAllMissing.value) {
      ElMessage.error('所选表在源库均不存在，无法获取候选水位列')
    } else {
      if (wmMissingTables.value.length > 0) {
        ElMessage.warning(`以下 ${wmMissingTables.value.length} 张表在源库不存在，已排除：${wmMissingTables.value.join('、')}`)
      }
      if (wmNoCandidates.value) {
        ElMessage.warning('所选表均无可比类型列，请手输列名或调整表选择')
      }
    }
    // Preselect the Top-1 candidate only when nothing is picked yet — an
    // existing (possibly hand-typed) column is never overwritten.
    if (!wmConfig.column && wmCandidates.value.length > 0) {
      wmConfig.column = wmCandidates.value[0].column
    }
  } catch (e: any) {
    ElMessage.error(`获取候选水位列失败: ${e.response?.data?.error || e.message}`)
  } finally {
    wmLoadingCols.value = false
  }
}

const wmKind = computed<'datetime' | 'date' | 'int' | 'text'>(() => {
  const t = wmDominantType.value
  if (t === 'timestamptz' || t === 'timestamp' || t === 'datetime') return 'datetime'
  if (t === 'date') return 'date'
  if (t === 'bigint' || t === 'integer') return 'int'
  return 'text'
})
const wmIsTimestamptz = computed(() => wmDominantType.value === 'timestamptz')

// 手填模式（未命中候选、无元数据）时按值形态推断控件。
const wmKindInferred = computed(() => {
  if (activeWmCandidate.value) return wmKind.value
  if (/^\d+$/.test(wmConfig.value)) return 'int'
  if (/^\d{4}-\d{2}-\d{2}$/.test(wmConfig.value)) return 'date'
  if (/^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}/.test(wmConfig.value)) return 'datetime'
  return 'text'
})

// #range: interval conditions are only offered for datetime/date columns
// (PG timestamptz/timestamp/date; MySQL datetime/timestamp/date). int/text
// columns stay single-value — the range toggle is hidden for them.
const wmRangeEligible = computed(() => wmKindInferred.value === 'datetime' || wmKindInferred.value === 'date')

// #range B: once the column stops being range-eligible (switched to an
// int/text candidate, or the manual value no longer looks datetime/date) the
// radio group is hidden — reset range_mode so the form cannot deadlock with
// no way back to the single-value branch.
watch(wmRangeEligible, (eligible) => {
  if (!eligible) wmConfig.range_mode = false
})

function validateWmValue(): string {
  if (wmConfig.range_mode) return validateWmRange()
  const v = wmConfig.value.trim()
  if (!v) return '请填写水位值'
  if (wmKindInferred.value === 'int' && !/^-?\d+$/.test(v)) return '整数水位列的值必须是整数'
  if (wmKindInferred.value === 'datetime' && !/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(v)) return '时间水位格式应为 YYYY-MM-DD HH:mm:ss'
  if (wmKindInferred.value === 'date' && !/^\d{4}-\d{2}-\d{2}$/.test(v)) return '日期水位格式应为 YYYY-MM-DD'
  const t = Date.parse(v.replace(' ', 'T') + (wmIsTimestamptz.value ? 'Z' : ''))
  if (!Number.isNaN(t) && t > Date.now()) return '水位值晚于当前时间，两侧可能都比不出任何行'
  return ''
}

// #range: interval validation — both endpoints required, per-kind format
// (datetime → YYYY-MM-DD HH:mm:ss, date → YYYY-MM-DD), and start >= end is
// rejected (the half-open interval [start, end) is empty at start == end).
function validateWmRange(): string {
  const s = wmConfig.range_start.trim()
  const e = wmConfig.range_end.trim()
  if (!s || !e) return '区间起点与终点都必须填写'
  if (wmKindInferred.value === 'datetime') {
    if (!/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(s)) return '区间起点格式应为 YYYY-MM-DD HH:mm:ss'
    if (!/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(e)) return '区间终点格式应为 YYYY-MM-DD HH:mm:ss'
  }
  if (wmKindInferred.value === 'date') {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return '区间起点格式应为 YYYY-MM-DD'
    if (!/^\d{4}-\d{2}-\d{2}$/.test(e)) return '区间终点格式应为 YYYY-MM-DD'
  }
  const ts = Date.parse(s.replace(' ', 'T') + (wmIsTimestamptz.value ? 'Z' : ''))
  const te = Date.parse(e.replace(' ', 'T') + (wmIsTimestamptz.value ? 'Z' : ''))
  if (!Number.isNaN(ts) && !Number.isNaN(te) && ts >= te) return '区间起点不能晚于或等于终点（半开区间 [start, end)）'
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
      // P2-4: mysql refs route through the multi-source adapter endpoint —
      // /config/list-tables is PG-wire only and would 400; effectiveSourceType
      // falls back to 'postgres' on a dead ref, keeping the legacy PG path.
      if (effectiveSourceType.value === 'mysql') {
        ;({ data } = await apiClient.getRefTablesMulti(sourceRef.value))
      } else {
        ;({ data } = await apiClient.getRefTables(sourceRef.value))
      }
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
  // #ignore: a saved exclusion list restores the multi-tag input (absent/
  // empty = no exclusions).
  if (Array.isArray(opts.ignore_columns)) {
    ignoreCols.value = [...opts.ignore_columns]
  }
  // Connection fields in the payload (legacy saves) are intentionally ignored.
  // #t3: a saved watermark group restores the "水位" card + its config.
  if (opts.watermark) {
    const w = opts.watermark
    wmConfig.base_mode = ['quick', 'sample', 'checksum'].includes(w.base_mode) ? w.base_mode : 'checksum'
    wmConfig.column = w.column || ''
    wmConfig.op = w.op === '<' ? '<' : '<='
    wmConfig.value = w.value || ''
    // #range: a saved interval group restores range mode; a single-value
    // group clears the range fields.
    if (w.range_start && w.range_end) {
      wmConfig.range_mode = true
      wmConfig.range_start = w.range_start
      wmConfig.range_end = w.range_end
    } else {
      wmConfig.range_mode = false
      wmConfig.range_start = ''
      wmConfig.range_end = ''
    }
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
        ? wmConfig.range_mode
          ? { column: wmConfig.column, range_start: wmConfig.range_start, range_end: wmConfig.range_end, base_mode: wmConfig.base_mode }
          : { column: wmConfig.column, value: wmConfig.value, op: wmConfig.op, base_mode: wmConfig.base_mode }
        : null,
      // #ignore: always send the current list (empty = clear).
      ignore_columns: ignoreCols.value,
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
    // MC-16: explicit format='json' — the endpoint's no-param default is
    // now the server-rendered html family report.
    const { data } = await apiClient.getCompareReport(id, 'json')
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
      watermark = wmConfig.range_mode
        ? { column: wmConfig.column, range_start: wmConfig.range_start.trim(), range_end: wmConfig.range_end.trim(), base_mode: wmConfig.base_mode }
        : { column: wmConfig.column, value: wmConfig.value.trim(), op: wmConfig.op, base_mode: wmConfig.base_mode }
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
      // #ignore: value-comparison exclusion list (trimmed; server dedups).
      ignore_columns: ignoreCols.value.map((c) => c.trim()).filter((c) => c !== ''),
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
// skip 显示为独立的灰色「跳过」标签（含原因披露），绝不与 fail 混同。
function tableStatusText(s: string) {
  return { pass: '通过', fail: '失败', warn: '警告', skip: '跳过' }[s] || s
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

// MC-16: the HTML report moved server-side (family single source of truth —
// TiMS banner, meta grid, Chinese badges, # column; the client-side assembly
// is deleted). The button downloads the server-rendered artifact as a blob,
// same channel as the CSV export.
async function downloadReport() {
  const t = activeTask.value
  if (!t) return
  try {
    const { data } = await apiClient.getCompareReport(t.id, 'html')
    downloadBlob(data, 'text/html;charset=utf-8', `compare-report-${t.id}.html`)
  } catch (e: any) {
    // MC-17 ③: the html face is responseType 'text', so an error body that
    // the server wrote as JSON arrives as a raw string — parse it for the
    // cause (404 dual-cause wording etc.); non-JSON text falls back to
    // e.message instead of leaking "undefined".
    const raw = e?.response?.data
    let cause = e?.message
    if (typeof raw === 'string' && raw) {
      try { cause = JSON.parse(raw)?.error || cause } catch { /* non-JSON body */ }
    } else if (raw?.error) {
      cause = String(raw.error)
    }
    ElMessage.error(`下载报告失败：${cause}`)
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
        <el-row :gutter="24" class="compare-conn-grid">
          <!-- #16 375px 溢出：:xs=24 让源/目标双列 <768px 堆叠（「测试 TiDB 连接」按钮
               右缘溢出 7px 横滚的根因是 span=12 固定 50% 列宽），≥768px 桌面双列不变 -->
          <el-col :span="12" :xs="24">
            <el-divider content-position="left">源数据库</el-divider>
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

          <el-col :span="12" :xs="24">
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
        <el-form-item label="选择表">
          <div style="width: 100%;">
            <div style="font-size: var(--tims-font-xs); color: var(--tims-text-2); margin-bottom: 8px;">水位模式下请先选表，再配置候选水位列</div>
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
            :title="`当前源类型（${effectiveSourceType}）暂不支持数据比对（支持 postgres 与 mysql）`"
          />
        </el-form-item>
        <el-form-item v-if="form.mode === 'sample' || (form.mode === 'watermark' && wmConfig.base_mode === 'sample')" label="采样率">
          <el-input-number v-model="form.sample_ratio" :min="0.001" :max="1" :step="0.01" />
        </el-form-item>
        <el-form-item v-if="form.mode === 'checksum' || (form.mode === 'watermark' && wmConfig.base_mode === 'checksum')" label="分块大小">
          <el-input-number v-model="form.checksum_chunk_size" :min="1000" :step="10000" />
        </el-form-item>
        <el-form-item label="排除字段">
          <div style="width: 100%;">
            <el-select v-model="ignoreCols" multiple allow-create filterable default-first-option
                       placeholder="如 created_at, updated_at（回车添加，逗号分隔）" style="width: 100%;">
            </el-select>
            <div style="font-size: var(--tims-font-xs); color: var(--tims-text-2); margin-top: 6px;">
              自增字段、自动时间戳等两端注定不一致的列可在此排除，不参与值比对（行数统计与对齐键不受影响；排除列命中某表主键时该表自动保留该列）。
            </div>
          </div>
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
                <el-select v-model="wmConfig.column" filterable allow-create default-first-option clearable
                           placeholder="候选列（先获取）或手输列名" style="width: 300px;">
                  <el-option v-for="c in wmCandidates" :key="c.column" :value="c.column"
                             :label="`${c.column}（覆盖 ${c.matched_tables.length}/${wmSuggestTotal || selectedTables.length} · ${c.score.toFixed(0)} 分）`" />
                </el-select>
                <el-tooltip :disabled="sourceRef && selectedTables.length > 0" content="请先选择表" placement="top">
                  <el-button size="small" :loading="wmLoadingCols" :disabled="!sourceRef || selectedTables.length === 0" @click="loadWmColumns">获取候选水位列</el-button>
                </el-tooltip>
                <el-radio-group v-if="wmRangeEligible" v-model="wmConfig.range_mode" size="small">
                  <el-radio-button :value="false">单值</el-radio-button>
                  <el-radio-button :value="true">区间</el-radio-button>
                </el-radio-group>
                <template v-if="wmConfig.range_mode">
                  <el-date-picker v-if="wmKindInferred === 'datetime'" v-model="wmConfig.range_start" type="datetime"
                                  value-format="YYYY-MM-DD HH:mm:ss" placeholder="区间起点 YYYY-MM-DD HH:mm:ss" style="width: 230px;" />
                  <el-date-picker v-else-if="wmKindInferred === 'date'" v-model="wmConfig.range_start" type="date"
                                  value-format="YYYY-MM-DD" placeholder="区间起点 YYYY-MM-DD" style="width: 200px;" />
                  <span style="color: var(--tims-text-2);">~</span>
                  <el-date-picker v-if="wmKindInferred === 'datetime'" v-model="wmConfig.range_end" type="datetime"
                                  value-format="YYYY-MM-DD HH:mm:ss" placeholder="区间终点（不含）" style="width: 230px;" />
                  <el-date-picker v-else-if="wmKindInferred === 'date'" v-model="wmConfig.range_end" type="date"
                                  value-format="YYYY-MM-DD" placeholder="区间终点（不含）" style="width: 200px;" />
                </template>
                <template v-else>
                  <el-select v-model="wmConfig.op" style="width: 90px;">
                    <el-option value="<=" label="≤（含边界）" />
                    <el-option value="<" label="<（严格）" />
                  </el-select>
                  <el-date-picker v-if="wmKindInferred === 'datetime'" v-model="wmConfig.value" type="datetime"
                                  value-format="YYYY-MM-DD HH:mm:ss" placeholder="水位值 YYYY-MM-DD HH:mm:ss" style="width: 230px;" />
                  <el-date-picker v-else-if="wmKindInferred === 'date'" v-model="wmConfig.value" type="date"
                                  value-format="YYYY-MM-DD" placeholder="水位值 YYYY-MM-DD" style="width: 230px;" />
                  <el-input v-else v-model="wmConfig.value" placeholder="水位值" style="width: 230px;" />
                </template>
              </div>
              <el-alert v-if="wmAllMissing" type="error" :closable="false" style="margin-top: 6px;"
                        :title="`所选 ${wmMissingTables.length} 张表在源库均不存在，无法获取候选水位列，请核对表选择或数据源`" />
              <template v-else>
                <el-alert v-if="wmMissingTables.length" type="warning" :closable="false" style="margin-top: 6px;"
                          :title="`以下 ${wmMissingTables.length} 张表在源库不存在，已排除：${wmMissingTables.join('、')}`" />
                <el-alert v-if="wmNoCandidates" type="warning" :closable="false" style="margin-top: 6px;"
                          title="所选表均无可比类型列，请手输列名或调整表选择" />
              </template>
              <div v-if="activeWmCandidate" style="font-size: var(--tims-font-xs); color: var(--tims-text-2); margin-top: 6px;">
                覆盖 {{ activeWmCandidate.matched_tables.length }}/{{ wmSuggestTotal || selectedTables.length }} 张表（{{ (activeWmCandidate.coverage * 100).toFixed(0) }}%）
                <span v-if="activeWmCandidate.warnings?.length" style="color: var(--tims-tag-warning-text);">；{{ activeWmCandidate.warnings[0] }}</span>
                <el-tooltip v-if="activeWmCandidate.unmatched_tables?.length" placement="top">
                  <template #content>{{ activeWmCandidate.unmatched_tables.join('、') }}</template>
                  <span style="color: var(--tims-tag-warning-text); cursor: help;">；缺该列 {{ activeWmCandidate.unmatched_tables.length }} 张表（悬停查看，运行期将全量比对）</span>
                </el-tooltip>
              </div>
              <div style="font-size: var(--tims-font-xs); color: var(--tims-text-2); margin-top: 6px;">
                两端按同一条件过滤后比对，消除「源端持续写入+同步滞后」的永久差异假象；NULL 水位行两端一致排除。
                运行期缺列或类型不符的表将自动退化为全量比对（报告逐表注明）。
                <span v-if="wmConfig.range_mode">区间为半开 [start, end)，终点行不包含。</span>
                <span v-if="wmIsTimestamptz" style="color: var(--tims-tag-warning-text);">timestamptz 列请输入 UTC 时间。</span>
              </div>
            </div>
          </el-form-item>
        </template>
        <el-form-item label="总并发数">
          <el-input-number v-model="form.concurrency" :min="1" :max="8" />
          <span style="color: var(--tims-text-2); font-size: var(--tims-font-xs); margin-left: 8px;">任务同时执行的数据库查询数（所有模式共用，1–8）</span>
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
          <el-button size="small" @click="downloadReport">下载报告</el-button>
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
        <div class="tims-table-scroll">
          <el-table :data="activeReport.tables" max-height="400" size="small" style="margin-bottom: 12px;">
          <el-table-column prop="table_name" label="表名" min-width="140" />
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag :type="tableStatusTag(row.status)" size="small">{{ tableStatusText(row.status) }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="source_rows" label="源行数" width="110" align="right" class-name="tims-num-col" />
          <el-table-column prop="target_rows" label="目标行数" width="110" align="right" class-name="tims-num-col" />
          <el-table-column label="差异行数" width="100" align="right">
            <template #default="{ row }">
              <span class="tims-num" :class="diffHeat(row.diff_rows)">{{ row.diff_rows }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="duration" label="耗时" width="90" class-name="hide-sm" />
          <el-table-column label="错误/建议" min-width="200" show-overflow-tooltip>
            <template #default="{ row }">{{ row.error ? row.error : row.suggestion }}</template>
          </el-table-column>
          </el-table>
        </div>
      </template>
      <div v-else-if="activeTask.status === 'completed'" v-loading="loadingReport" style="min-height: 80px;"></div>
    </el-card>

    <el-card shadow="never">
      <template #header><span style="font-weight: bold;">比对历史</span></template>
      <div class="tims-table-scroll">
        <el-table :data="tasks" size="small">
          <el-table-column prop="id" label="ID" width="90" class-name="hide-sm" />
          <el-table-column prop="name" label="名称" width="220" show-overflow-tooltip />
          <el-table-column label="模式" width="130">
            <template #default="{ row }">
            <span style="white-space: nowrap;">{{ row.mode }}</span>
            <el-tag v-if="row.watermark" size="small" type="warning" style="margin-left: 4px;">水位</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="statusTag(row.status)" size="small">{{ statusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="表进度" width="120" class-name="hide-sm">
          <template #default="{ row }">{{ row.tables_done }} / {{ row.tables_total || '?' }}</template>
        </el-table-column>
          <el-table-column label="创建时间" min-width="170" class-name="hide-sm">
          <template #default="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
        </el-table-column>
        <el-table-column label="操作" width="140">
          <template #default="{ row }">
            <el-button size="small" link type="primary" @click="viewTask(row)">查看</el-button>
            <el-button size="small" link type="danger" :disabled="row.status === 'running'" @click="removeTask(row)">删除</el-button>
          </template>
        </el-table-column>
        </el-table>
      </div>
    </el-card>
  </div>
</template>

<style scoped>
/* #16 窄视口（≤768px）源/目标两列堆叠为单列：375px 下 el-col-12 各仅 ~135px，
   表单控件与「测试连接」按钮被裁切；桌面双列布局不受扰 */
@media (max-width: 768px) {
  .compare-conn-grid :deep(.el-col) {
    max-width: 100%;
    flex: 0 0 100%;
  }
}
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
