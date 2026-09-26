<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient from '../api'
import type { IncrementalJob } from '../api'
import PageHeader from '../components/PageHeader.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import SyncCompareCard from '../components/SyncCompareCard.vue'
import { useDataSources } from '../composables/useDataSources'

// S1-UI-06: renamed 时间戳水位补齐 (was 增量同步) to avoid the CDC naming
// clash; v1 boundaries stay stated up-front inside the shared compare card.

// F-04 增量同步: pull-based timestamp-watermark jobs. v1 boundaries are
// stated up-front: deletes are never captured, UPDATEs that don't touch the
// watermark column are missed, and boundary rows are re-read every run.

// #t1 (时间戳水位优化): table picker fed by the source's table list
// (allow-create keeps manual entry as fallback), batch binding for tables
// sharing one watermark column, and type-aware initial-watermark inputs
// with persistent hints + validation.

interface ColInfo { name: string; data_type: string; comparable: boolean; indexed: boolean }
interface TableRow {
  table: string
  watermark_column: string
  initial_watermark: string
  columns: ColInfo[]
  missing?: boolean // not present in the source's current table list (edit backfill)
}

type WmKind = 'datetime' | 'date' | 'int' | 'text'

function wmKindOf(dataType: string): WmKind {
  if (dataType === 'timestamp with time zone' || dataType === 'timestamp without time zone') return 'datetime'
  if (dataType === 'date') return 'date'
  if (dataType === 'integer' || dataType === 'bigint') return 'int'
  return 'text'
}

function wmHintOf(dataType: string): string {
  switch (wmKindOf(dataType)) {
    case 'datetime': return '格式 YYYY-MM-DD HH:mm:ss；首次同步只取晚于该时刻的行，此后自动续传'
    case 'date': return '格式 YYYY-MM-DD；首次同步只取晚于该日期的行，此后自动续传'
    case 'int': return '整数（如自增/版本号）；首次同步只取大于该值的行，此后自动续传'
    default: return '与列类型一致的字符串'
  }
}

// Initial watermark format validation; returns error text or '' (a future
// value is allowed but warned by the caller — e.g. backfilling from a known
// archive clock).
function validateInitialWM(value: string, dataType: string): string {
  const v = value.trim()
  if (!v) return ''
  switch (wmKindOf(dataType)) {
    case 'datetime':
      if (!/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(v)) return '时间戳格式应为 YYYY-MM-DD HH:mm:ss'
      if (Number.isNaN(Date.parse(v.replace(' ', 'T')))) return '无法解析的时间戳'
      break
    case 'date':
      if (!/^\d{4}-\d{2}-\d{2}$/.test(v)) return '日期格式应为 YYYY-MM-DD'
      if (Number.isNaN(Date.parse(v))) return '无法解析的日期'
      break
    case 'int':
      if (!/^-?\d+$/.test(v)) return '应为整数'
      break
  }
  return ''
}

function isFutureWM(value: string, dataType: string): boolean {
  const v = value.trim()
  if (!v) return false
  const kind = wmKindOf(dataType)
  if (kind === 'datetime' || kind === 'date') {
    const t = Date.parse(kind === 'date' ? v : v.replace(' ', 'T'))
    return !Number.isNaN(t) && t > Date.now()
  }
  return false
}

const { load: loadSources } = useDataSources()
onMounted(async () => {
  await Promise.all([loadSources(true), loadJobs()])
})

const jobs = ref<IncrementalJob[]>([])
const loading = ref(false)

async function loadJobs() {
  loading.value = true
  try {
    const { data } = await apiClient.listIncrementalJobs()
    jobs.value = data || []
  } catch (e: any) {
    ElMessage.error(`加载失败: ${e.response?.data?.error || e.message}`)
  } finally {
    loading.value = false
  }
}

// ---- create / edit dialog ----
const dialogVisible = ref(false)
const editing = ref<IncrementalJob | null>(null)
const saving = ref(false)
const form = reactive({
  name: '',
  source_ref: '',
  target_ref: '',
  batch_size: 1000,
  strict_mode: false,
  conflict_strategy: 'replace' as 'replace' | 'ignore' | 'error',
  tables: [] as TableRow[],
})

function resetTablePicker() {
  tableList.value = []
  tablesLoaded.value = false
  batchTables.value = []
  batchColumn.value = ''
  batchInitialWM.value = ''
  batchPreview.value = {}
}

function openCreate() {
  editing.value = null
  form.name = ''
  form.source_ref = ''
  form.target_ref = ''
  form.batch_size = 1000
  form.strict_mode = false
  form.conflict_strategy = 'replace'
  form.tables = []
  resetTablePicker()
  batchMode.value = false
  dialogVisible.value = true
}

function openEdit(j: IncrementalJob) {
  editing.value = j
  form.name = j.name
  form.source_ref = j.source_ref
  form.target_ref = j.target_ref
  form.batch_size = j.batch_size
  form.strict_mode = j.strict_mode
  form.conflict_strategy = j.conflict_strategy
  form.tables = j.tables.map(t => ({ table: t.table, watermark_column: t.watermark_column, initial_watermark: t.initial_watermark || '', columns: [] }))
  resetTablePicker()
  // #t1 fix: the source_ref watch only fires on value CHANGE — reopening
  // the edit dialog for the same source leaves the table list empty (no
  // missing-table flags, no type-aware watermark controls). Load it
  // explicitly; openCreate clears source_ref so it needs nothing.
  if (form.source_ref) loadTableList()
  // #t1: backfilled rows carry no column info — the type-aware watermark
  // control would degrade to a plain text box until the user manually
  // clicked "fetch columns". Load columns for configured rows only (empty
  // rows fire no request); a failed load silently falls back to the text
  // box, same as before.
  form.tables.forEach((t, i) => {
    if (t.watermark_column) loadColumns(i, true)
  })
  batchMode.value = false
  dialogVisible.value = true
}

// ---- #t1-A: table list picker (loaded from the selected source) ----
const tableList = ref<{ name: string; row_estimate: number }[]>([])
const loadingTables = ref(false)
const tablesLoaded = ref(false)

watch(() => form.source_ref, v => {
  resetTablePicker()
  if (v) loadTableList()
})

async function loadTableList() {
  loadingTables.value = true
  try {
    const { data } = await apiClient.getRefTablesMulti(form.source_ref)
    tableList.value = data.tables || []
    tablesLoaded.value = true
    // Edit backfill: flag configured tables that no longer exist upstream.
    const names = new Set(tableList.value.map(t => t.name))
    for (const row of form.tables) row.missing = row.table ? !names.has(row.table) : false
    if (form.tables.some(r => r.missing)) {
      ElMessage.warning('部分已配置的表不在当前源库表清单中（已标红），保存后仍会保留')
    }
  } catch (e: any) {
    ElMessage.warning(`表清单加载失败（可手输表名兜底）: ${e.response?.data?.error || e.message}`)
  } finally {
    loadingTables.value = false
  }
}

// ---- per-table rows (single config + fallback channel for batch rejects) ----
function addRow() {
  // S1-UI-08: guard against adding a duplicate table entry.
  if (form.tables.some(t => !t.table.trim())) {
    ElMessage.warning('请先填写上一行的表名再添加新表')
    return
  }
  form.tables.push({ table: '', watermark_column: '', initial_watermark: '', columns: [] })
}
function removeRow(i: number) {
  form.tables.splice(i, 1)
}

function onTableChanged(i: number) {
  const row = form.tables[i]
  row.columns = []
  row.watermark_column = ''
  const name = row.table.trim().toLowerCase()
  if (name && form.tables.some((t, idx) => idx !== i && t.table.trim().toLowerCase() === name)) {
    ElMessage.warning(`表「${row.table.trim()}」已添加，请勿重复`)
    row.table = ''
    return
  }
  row.missing = tablesLoaded.value ? !tableList.value.some(t => t.name === row.table.trim()) : false
  // #t1-A: columns load automatically once the table is picked.
  if (row.table.trim()) loadColumns(i)
}

async function loadColumns(i: number, silent = false) {
  const row = form.tables[i]
  if (!form.source_ref || !row.table) {
    if (!silent) ElMessage.warning('请先选择源数据源并填写表名')
    return
  }
  try {
    const { data } = await apiClient.getSourceTableColumns(form.source_ref, row.table)
    row.columns = data.columns || []
    if (row.columns.length > 0 && !row.columns.some(c => c.comparable) && !silent) {
      ElMessage.warning('该表没有可比类型的水位列（timestamp/date/int/bigint）')
    }
  } catch (e: any) {
    // silent=true is the edit-backfill auto-load: a ghost table (deleted
    // from source) keeps the red missing-tag path; a raw error toast per
    // row would be noise.
    if (!silent) ElMessage.error(`获取列失败: ${e.response?.data?.error || e.message}`)
  }
}

function onColumnPicked(i: number) {
  const row = form.tables[i]
  const col = row.columns.find(c => c.name === row.watermark_column)
  if (col && !col.indexed) {
    ElMessage.warning(`水位列 ${col.name} 上没有索引，同步时可能全表扫描`)
  }
}

function rowWmCol(i: number): ColInfo | undefined {
  return form.tables[i].columns.find(c => c.name === form.tables[i].watermark_column)
}
function rowWmKind(i: number): WmKind {
  const c = rowWmCol(i)
  return c ? wmKindOf(c.data_type) : 'text'
}

// ---- #t1-B: batch binding ----
const batchMode = ref(false)
const batchTables = ref<string[]>([])
const batchColumn = ref('')
const batchInitialWM = ref('')
const batchPreview = ref<Record<string, { columns: ColInfo[]; error?: string }>>({})
const batchChecking = ref(false)

// Drop preview rows for deselected tables; keep the rest (removing one
// table must not invalidate the others' checks).
watch(batchTables, nv => {
  const keep = new Set(nv)
  const p = { ...batchPreview.value }
  for (const k of Object.keys(p)) if (!keep.has(k)) delete p[k]
  batchPreview.value = p
  if (nv.length === 0) batchColumn.value = ''
})

async function checkBatch() {
  if (!form.source_ref || batchTables.value.length === 0) {
    ElMessage.warning('请先选择源数据源并勾选表')
    return
  }
  batchChecking.value = true
  try {
    const { data } = await apiClient.columnsBatch(form.source_ref, batchTables.value)
    const map: Record<string, { columns: ColInfo[]; error?: string }> = {}
    for (const t of data.tables || []) map[t.table] = { columns: t.columns || [], error: t.error }
    batchPreview.value = map
    const bad = Object.entries(map).filter(([, v]) => v.error).length
    if (bad > 0) ElMessage.warning(`${bad} 张表校验失败，已在预览区标红`)
  } catch (e: any) {
    ElMessage.error(`批量校验失败: ${e.response?.data?.error || e.message}`)
  } finally {
    batchChecking.value = false
  }
}

const batchCommonColumns = computed<ColInfo[]>(() => {
  const entries = Object.values(batchPreview.value).filter(v => !v.error && v.columns.length)
  if (entries.length === 0) return []
  const counts = new Map<string, number>()
  const meta = new Map<string, ColInfo>()
  for (const e of entries) {
    for (const c of e.columns) {
      counts.set(c.name, (counts.get(c.name) || 0) + 1)
      meta.set(c.name, c)
    }
  }
  const n = entries.length
  return [...counts.entries()].filter(([name, k]) => k === n && meta.get(name)!.comparable)
    .map(([name]) => meta.get(name)!)
})

function batchTableState(name: string): 'ok' | 'missing-col' | 'error' | 'unchecked' {
  const p = batchPreview.value[name]
  if (!p) return 'unchecked'
  if (p.error) return 'error'
  if (!p.columns.some(c => c.name === batchColumn.value && c.comparable)) return 'missing-col'
  return 'ok'
}

function batchRemoveTable(name: string) {
  batchTables.value = batchTables.value.filter(t => t !== name)
}

function batchMoveToSingle(name: string) {
  if (form.tables.some(t => t.table.toLowerCase() === name.toLowerCase())) {
    ElMessage.warning(`表「${name}」已在单独配置中`)
    return
  }
  form.tables.push({ table: name, watermark_column: '', initial_watermark: '', columns: batchPreview.value[name]?.columns || [] })
  batchRemoveTable(name)
}

const batchColumnType = computed(() => {
  for (const p of Object.values(batchPreview.value)) {
    const c = p.columns.find(c => c.name === batchColumn.value)
    if (c) return c.data_type
  }
  return ''
})
const batchWmKind = computed(() => (batchColumnType.value ? wmKindOf(batchColumnType.value) : 'text') as WmKind)

function onBatchColumnPicked() {
  for (const p of Object.values(batchPreview.value)) {
    const c = p.columns.find(c => c.name === batchColumn.value)
    if (c && !c.indexed) {
      ElMessage.warning(`部分表的 ${c.name} 列上没有索引，同步时可能全表扫描`)
      break
    }
  }
}

// ---- save ----
async function save() {
  if (!form.name.trim() || !form.source_ref || !form.target_ref) {
    ElMessage.warning('请填写名称并选择源/目标数据源')
    return
  }
  const rows: { table: string; watermark_column: string; initial_watermark: string; dataType?: string }[] = []
  if (batchMode.value) {
    if (batchTables.value.length === 0) {
      ElMessage.warning('批量模式下请先勾选表')
      return
    }
    if (!batchColumn.value) {
      ElMessage.warning('批量模式下请选择统一水位列')
      return
    }
    if (Object.keys(batchPreview.value).length === 0) {
      ElMessage.warning('请先点击「校验所选表」')
      return
    }
    const invalid = batchTables.value.filter(t => batchTableState(t) !== 'ok')
    if (invalid.length > 0) {
      ElMessage.warning(`表 ${invalid.join('、')} 不含可比类型的 ${batchColumn.value} 列，请移除或转入单独配置`)
      return
    }
    for (const t of batchTables.value) rows.push({ table: t, watermark_column: batchColumn.value, initial_watermark: batchInitialWM.value || '', dataType: batchColumnType.value })
  }
  for (const t of form.tables) rows.push({ table: t.table, watermark_column: t.watermark_column, initial_watermark: t.initial_watermark || '', dataType: rowWmCol(form.tables.indexOf(t))?.data_type })

  if (rows.length === 0) {
    ElMessage.warning('请至少添加一张表')
    return
  }
  const seen = new Set<string>()
  for (const t of rows) {
    if (!t.table || !t.watermark_column) {
      ElMessage.warning('每张表都需要表名和水位列')
      return
    }
    const key = t.table.trim().toLowerCase()
    if (seen.has(key)) {
      ElMessage.warning(`表「${t.table.trim()}」重复添加`)
      return
    }
    seen.add(key)
    if (t.dataType) {
      const err = validateInitialWM(t.initial_watermark, t.dataType)
      if (err) {
        ElMessage.warning(`表「${t.table.trim()}」初始水位：${err}`)
        return
      }
      if (isFutureWM(t.initial_watermark, t.dataType)) {
        ElMessage.warning(`表「${t.table.trim()}」初始水位晚于当前时间，首次同步可能拉不到任何行`)
      }
    }
  }
  saving.value = true
  try {
    const body = {
      name: form.name.trim(),
      source_ref: form.source_ref,
      target_ref: form.target_ref,
      batch_size: form.batch_size,
      strict_mode: form.strict_mode,
      conflict_strategy: form.conflict_strategy,
      tables: rows.map(t => ({ table: t.table, watermark_column: t.watermark_column, initial_watermark: t.initial_watermark || '' })),
    }
    if (editing.value) {
      await apiClient.updateIncrementalJob(editing.value.id, body)
      ElMessage.success('任务已更新')
    } else {
      await apiClient.createIncrementalJob(body)
      ElMessage.success('任务已创建')
    }
    dialogVisible.value = false
    await loadJobs()
  } catch (e: any) {
    ElMessage.error(`保存失败: ${e.response?.data?.error || e.message}`)
  } finally {
    saving.value = false
  }
}

async function removeJob(j: IncrementalJob) {
  try {
    await ElMessageBox.confirm(`确定删除任务「${j.name}」？表级状态与历史将一并删除。`, '删除确认', { type: 'warning' })
  } catch { return }
  try {
    await apiClient.deleteIncrementalJob(j.id)
    ElMessage.success('已删除')
    await loadJobs()
  } catch (e: any) {
    ElMessage.error(`删除失败: ${e.response?.data?.error || e.message}`)
  }
}

// ---- run + history ----
const running = ref<string | null>(null)

async function runJob(j: IncrementalJob) {
  running.value = j.id
  try {
    const { data } = await apiClient.runIncrementalJob(j.id)
    const errs = data.tables.filter(t => t.error)
    const rows = data.tables.reduce((n, t) => n + (t.rows || 0), 0)
    if (errs.length > 0) {
      ElMessage.warning(`同步完成：${rows} 行，但 ${errs.length} 张表失败（见运行历史）`)
    } else {
      ElMessage.success(`同步完成：${rows} 行，耗时 ${data.duration_ms} ms`)
    }
    await loadJobs()
  } catch (e: any) {
    ElMessage.error(`同步失败: ${e.response?.data?.error || e.message}`)
  } finally {
    running.value = null
  }
}

const historyVisible = ref(false)
const historyJob = ref<IncrementalJob | null>(null)
function openHistory(j: IncrementalJob) {
  historyJob.value = j
  historyVisible.value = true
}

const strategyLabels: Record<string, string> = { replace: 'REPLACE INTO', ignore: 'INSERT IGNORE', error: '报错停止' }

// Initial-watermark form item: shared tooltip + hint text (#t1-C).
const wmTooltip = '首次同步的起点：只同步水位列晚于（大于）该值的行；仅第一轮生效，此后自动按上次水位续传。留空 = 从列的最小值开始全量补齐。'
</script>

<template>
  <div class="tims-page">
    <PageHeader title="时间戳水位补齐" subtitle="基于更新时间列的拉式批同步，手动触发可重跑，适合无 CDC 权限或定时补齐场景" />

    <SyncCompareCard current="watermark">
      <el-alert type="warning" :closable="false" style="margin-top: 4px;">
        <p>本模块边界：① 源端 DELETE 不会被捕获（可用「数据比对」兜底核对）；② 不更新水位列的 UPDATE 会漏同步；③ 默认 ≥ 模式每轮会重读边界时刻的行（保证同秒迟到行不丢，代价极小）。</p>
      </el-alert>
    </SyncCompareCard>

    <el-card shadow="never">
      <template #header>
        <div style="display: flex; justify-content: space-between; align-items: center;">
          <span>同步任务</span>
          <el-button type="primary" @click="openCreate">新建任务</el-button>
        </div>
      </template>
      <el-table :data="jobs" v-loading="loading">
        <template #empty>
          <div class="inc-empty">
            <el-icon :size="42" class="inc-empty-icon"><Calendar /></el-icon>
            <p class="inc-empty-text">暂无补齐任务</p>
            <el-button type="primary" @click="openCreate">
              <el-icon><Plus /></el-icon> 新建任务
            </el-button>
          </div>
        </template>
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column label="表" min-width="200">
          <template #default="{ row }">
            <span v-for="(t, i) in row.tables" :key="t.table">
              {{ t.table }}<el-tag v-if="row.states?.[t.table]?.failed" size="small" type="danger" style="margin-left: 4px;">异常</el-tag>{{ i < row.tables.length - 1 ? '、' : '' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="冲突策略" width="120">
          <template #default="{ row }">{{ strategyLabels[row.conflict_strategy] || row.conflict_strategy }}</template>
        </el-table-column>
        <el-table-column label="累计同步行数" width="120">
          <template #default="{ row }">{{ Object.values(row.states || {}).reduce((n: number, s: any) => n + (s.total_rows || 0), 0).toLocaleString() }}</template>
        </el-table-column>
        <el-table-column label="最近同步" width="160">
          <template #default="{ row }">
            <span v-if="row.tables.length">
              {{ Object.values(row.states || {}).map((s: any) => s.last_sync_at || '').filter(Boolean).sort().pop() || '—' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="280" fixed="right">
          <template #default="{ row }">
            <el-button type="primary" size="small" :loading="running === row.id" @click="runJob(row)">立即同步</el-button>
            <el-button size="small" @click="openHistory(row)">运行历史</el-button>
            <el-button size="small" @click="openEdit(row)">编辑</el-button>
            <el-button size="small" type="danger" @click="removeJob(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="dialogVisible" :title="editing ? '编辑补齐任务' : '新建补齐任务'" width="880px" :close-on-click-modal="false" :close-on-press-escape="false">
      <el-form label-width="120px">
        <el-form-item label="任务名称">
          <el-input v-model="form.name" placeholder="例如：订单表水位补齐" style="width: 360px;" />
        </el-form-item>
        <el-divider content-position="left">源数据库（仅 PostgreSQL）</el-divider>
        <el-form-item label="源数据源">
          <DataSourcePicker v-model="form.source_ref" :types="['postgres']" />
        </el-form-item>
        <el-divider content-position="left">目标数据库（TiDB）</el-divider>
        <el-form-item label="目标数据源">
          <DataSourcePicker v-model="form.target_ref" :types="['tidb']" />
        </el-form-item>
        <el-divider content-position="left">同步配置</el-divider>
        <el-form-item label="冲突策略">
          <div>
            <el-radio-group v-model="form.conflict_strategy">
              <el-radio value="replace">REPLACE INTO（默认，幂等）</el-radio>
              <el-radio value="ignore">INSERT IGNORE（跳过冲突）</el-radio>
              <el-radio value="error">报错停止</el-radio>
            </el-radio-group>
            <div v-if="form.conflict_strategy === 'error'" style="margin-top: 4px; font-size: var(--tims-font-sm); color: var(--tims-tag-warning-text); line-height: 1.6;">
              ⚠️ 批次之间无事务：同步失败或中途崩溃后重跑，该策略会撞到已写入行的重复键错误且无自愈——重跑前建议改用 REPLACE/IGNORE。
            </div>
          </div>
        </el-form-item>
        <el-form-item label="批大小">
          <el-input-number v-model="form.batch_size" :min="1" :max="100000" :step="500" controls-position="right" />
        </el-form-item>
        <el-form-item label="严格模式">
          <el-switch v-model="form.strict_mode" />
          <span style="margin-left: 12px; font-size: var(--tims-font-sm); color: var(--tims-text-2);">使用 &gt; 代替 ≥（跳过边界重读，但同秒迟到行可能丢失）</span>
        </el-form-item>

        <el-form-item label="表与水位列">
          <div style="width: 100%;">
            <div style="margin-bottom: 8px;">
              <el-switch v-model="batchMode" active-text="批量配置（多表共用同一水位列）" inactive-text="逐表配置" />
            </div>

            <!-- #t1-B: batch binding -->
            <div v-if="batchMode" class="batch-panel">
              <el-form-item label="选择表">
                <el-select v-model="batchTables" multiple filterable :loading="loadingTables" placeholder="可搜索多选（显示行数估计）" style="width: 100%;">
                  <el-option v-for="t in tableList" :key="t.name" :value="t.name" :label="t.row_estimate >= 0 ? `${t.name}（约 ${t.row_estimate.toLocaleString()} 行）` : t.name" />
                </el-select>
              </el-form-item>
              <el-form-item>
                <el-button :loading="batchChecking" :disabled="batchTables.length === 0" @click="checkBatch">校验所选表</el-button>
                <span style="margin-left: 8px; font-size: var(--tims-font-sm); color: var(--tims-text-2);">单连接批量拉取列信息，校验各表是否都含所选水位列</span>
              </el-form-item>
              <template v-if="batchTables.length > 0">
                <el-form-item label="统一水位列">
                  <el-select v-if="batchCommonColumns.length" v-model="batchColumn" placeholder="所选表共有的可比类型列" style="width: 300px;" @change="onBatchColumnPicked">
                    <el-option v-for="c in batchCommonColumns" :key="c.name" :value="c.name" :label="c.name" />
                  </el-select>
                  <el-input v-else v-model="batchColumn" placeholder="先校验所选表后从共有列中选择" style="width: 300px;" disabled />
                </el-form-item>
                <el-form-item label="初始水位">
                  <div style="width: 100%;">
                    <el-date-picker v-if="batchWmKind === 'datetime'" v-model="batchInitialWM" type="datetime" value-format="YYYY-MM-DD HH:mm:ss" placeholder="留空=从最小值全量补齐" style="width: 300px;" />
                    <el-date-picker v-else-if="batchWmKind === 'date'" v-model="batchInitialWM" type="date" value-format="YYYY-MM-DD" placeholder="留空=从最小值全量补齐" style="width: 300px;" />
                    <el-input v-else-if="batchWmKind === 'int'" v-model="batchInitialWM" placeholder="整数，留空=从最小值全量补齐" style="width: 300px;" />
                    <el-input v-else v-model="batchInitialWM" placeholder="留空=从最小值全量补齐" style="width: 300px;" />
                    <div class="wm-hint">{{ wmHintOf(batchColumnType) }}</div>
                  </div>
                </el-form-item>
                <el-form-item label="校验预览">
                  <el-table :data="batchTables.map(t => ({ name: t, state: batchTableState(t) }))" size="small" max-height="240">
                    <el-table-column prop="name" label="表" min-width="160" />
                    <el-table-column label="状态" min-width="200">
                      <template #default="{ row }">
                        <span v-if="row.state === 'ok'" style="color: var(--el-color-success);">✓ 含 {{ batchColumn || '（待选列）' }} 可比列</span>
                        <span v-else-if="row.state === 'unchecked'" style="color: var(--tims-text-2);">未校验</span>
                        <span v-else style="color: var(--el-color-danger);">
                          {{ row.state === 'error' ? (batchPreview[row.name]?.error || '校验失败') : `不含可比类型的 ${batchColumn} 列` }}
                        </span>
                      </template>
                    </el-table-column>
                    <el-table-column label="操作" width="180">
                      <template #default="{ row }">
                        <el-button size="small" text type="danger" @click="batchRemoveTable(row.name)">移除</el-button>
                        <el-button size="small" text @click="batchMoveToSingle(row.name)">转单独配置</el-button>
                      </template>
                    </el-table-column>
                  </el-table>
                </el-form-item>
              </template>
              <el-divider content-position="left">例外表（单独配置）</el-divider>
            </div>

            <div v-for="(t, i) in form.tables" :key="i" class="inc-table-row">
              <el-select v-model="t.table" filterable allow-create default-first-option clearable
                         :loading="loadingTables" placeholder="搜索或手输表名" style="width: 220px;" @change="onTableChanged(i)">
                <el-option v-for="tb in tableList" :key="tb.name" :value="tb.name" :label="tb.row_estimate >= 0 ? `${tb.name}（约 ${tb.row_estimate.toLocaleString()} 行）` : tb.name" />
              </el-select>
              <el-tag v-if="t.missing" type="danger" size="small" style="margin-left: 4px;">不在源库表清单</el-tag>
              <el-button size="small" style="margin: 0 4px 0 8px;" @click="loadColumns(i)">获取列</el-button>
              <el-select v-if="t.columns.length" v-model="t.watermark_column" placeholder="水位列" style="width: 260px;" @change="onColumnPicked(i)">
                <el-option v-for="c in t.columns" :key="c.name" :value="c.name" :label="`${c.name}（${c.data_type}${c.indexed ? '' : ' · 无索引'}）`" :disabled="!c.comparable" />
              </el-select>
              <el-input v-else v-model="t.watermark_column" placeholder="水位列（如 update_time）" style="width: 260px;" />
              <el-tooltip :content="wmTooltip" placement="top">
                <div class="wm-input-wrap">
                  <el-date-picker v-if="rowWmKind(i) === 'datetime'" v-model="t.initial_watermark" type="datetime" value-format="YYYY-MM-DD HH:mm:ss" placeholder="留空=从最小值全量补齐" style="width: 200px;" />
                  <el-date-picker v-else-if="rowWmKind(i) === 'date'" v-model="t.initial_watermark" type="date" value-format="YYYY-MM-DD" placeholder="留空=从最小值全量补齐" style="width: 200px;" />
                  <el-input v-else v-model="t.initial_watermark" placeholder="初始水位，留空=全量补齐" style="width: 200px;" />
                </div>
              </el-tooltip>
              <el-button type="danger" text style="margin-left: 8px;" @click="removeRow(i)">删除</el-button>
              <div v-if="rowWmCol(i)" class="wm-hint row-hint">{{ wmHintOf(rowWmCol(i)!.data_type) }}</div>
            </div>
            <el-button size="small" style="margin-top: 8px;" @click="addRow">+ 添加表</el-button>
            <el-alert v-if="!form.source_ref" type="info" :closable="false" style="margin-top: 8px;">
              选择源数据源后自动加载表清单与列信息；清单加载失败时仍可手输表名兜底。
            </el-alert>
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>

    <el-drawer v-model="historyVisible" :title="`运行历史 — ${historyJob?.name || ''}`" size="720px">
      <el-empty v-if="!historyJob?.history?.length" description="暂无运行记录" />
      <el-collapse v-else>
        <el-collapse-item v-for="h in historyJob.history" :key="h.run_id" :title="`${h.started_at} · ${h.duration_ms} ms`">
          <el-table :data="h.tables" size="small">
            <el-table-column prop="table" label="表" width="140" />
            <el-table-column prop="from_watermark" label="起始水位" min-width="160" />
            <el-table-column prop="to_watermark" label="推进至" min-width="160" />
            <el-table-column prop="rows" label="行数" width="90" />
            <el-table-column label="结果" min-width="160">
              <template #default="{ row }">
                <span v-if="row.error" style="color: var(--el-color-danger);">{{ row.error }}</span>
                <span v-else style="color: var(--el-color-success);">成功</span>
              </template>
            </el-table-column>
          </el-table>
        </el-collapse-item>
      </el-collapse>
    </el-drawer>
  </div>
</template>

<style scoped>
.inc-table-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px 0;
  margin-bottom: 8px;
  width: 100%;
}

.row-hint {
  width: 100%;
  padding-left: 2px;
}

.wm-hint {
  font-size: 12px;
  color: var(--tims-text-2);
  line-height: 1.5;
  margin-top: 2px;
}

.batch-panel {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--tims-radius-s);
  padding: 12px 12px 0 0;
  margin-bottom: 12px;
  width: 100%;
}

/* Empty state: icon + guidance CTA (reuses the page-level 新建任务 entry) */
.inc-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 28px 0 24px;
}
.inc-empty-icon { color: var(--tims-text-2); }
.inc-empty-text {
  margin: 0;
  font-size: var(--tims-font-sm);
  color: var(--tims-text-2);
}
.inc-empty .el-button { margin-bottom: 4px; }

.wm-input-wrap {
  display: inline-block;
  margin-left: 8px;
}
</style>
