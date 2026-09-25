<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient from '../api'
import type { CompareTask, CompareReport, CompareOptions } from '../api'
import ConnectionForm from '../components/ConnectionForm.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import PageHeader from '../components/PageHeader.vue'
import { useSourceSchema } from '../composables/useSourceSchema'
import { useDataSources } from '../composables/useDataSources'
import { reconcileModel } from '../composables/reconcileModel'

// Standalone data comparison (独立数据比对): configure source + target, pick
// tables and a compare mode, run the validator directly — no migration.

const { sources, load: loadSources, getSource } = useSourceSchema()
const { load: loadDataSources, get: getDataSource } = useDataSources()
const sourceType = ref('postgres')
const currentMeta = computed(() => getSource(sourceType.value))

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
  checksum_parallel: 4,
  parallel: 4,
})

const compareModes = [
  { value: 'quick', label: '快速', color: '#0fa3a3', desc: '仅行数估算，最快' },
  { value: 'sample', label: '采样', color: '#2c4a8f', desc: '行数+随机采样（推荐）' },
  { value: 'checksum', label: '校验', color: '#d97e00', desc: '行数+分块Hash' },
]

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
  if (opts.mode && ['quick', 'sample', 'checksum'].includes(opts.mode)) form.mode = opts.mode
  const num = (v: number | undefined) => (v !== undefined && v !== null ? v : null)
  const sr = num(opts.sample_ratio); if (sr !== null) form.sample_ratio = sr
  const cs = num(opts.checksum_chunk_size); if (cs !== null) form.checksum_chunk_size = cs
  const cp = num(opts.checksum_parallel); if (cp !== null) form.checksum_parallel = cp
  const p = num(opts.parallel); if (p !== null) form.parallel = p
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
      mode: form.mode,
      sample_ratio: form.sample_ratio,
      checksum_chunk_size: form.checksum_chunk_size,
      checksum_parallel: form.checksum_parallel,
      parallel: form.parallel,
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
    const { data } = await apiClient.createCompare({
      name: form.name || `Compare ${new Date().toLocaleString()}`,
      source_ref: sourceRef.value || undefined,
      target_ref: targetRef.value || undefined,
      source: { ...form.source, type: effectiveSourceType.value },
      target: { ...form.target },
      mode: form.mode,
      sample_ratio: form.sample_ratio,
      checksum_chunk_size: form.checksum_chunk_size,
      checksum_parallel: form.checksum_parallel,
      parallel: form.parallel,
      tables: selectedTables.value,
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
          <span style="font-size: 15px; font-weight: 600;">连接配置</span>
        </div>
      </template>

      <el-form label-width="120px" :disabled="!optionsLoaded" v-loading="!optionsLoaded">
        <el-row :gutter="24">
          <el-col :span="12">
            <el-divider content-position="left">源数据库（PostgreSQL）</el-divider>
            <el-form-item label="数据源">
              <DataSourcePicker v-model="sourceRef" :types="['postgres']" />
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
              @click="form.mode = m.value"
              :style="{
                padding: '10px 16px', borderRadius: '6px', cursor: 'pointer', textAlign: 'center',
                border: form.mode === m.value ? '2px solid ' + m.color : '2px solid #dcdfe6',
                background: form.mode === m.value ? m.color + '10' : '#fff',
              }"
            >
              <div style="font-weight: bold;">{{ m.label }}</div>
              <div style="font-size: 12px; color: #909399;">{{ m.desc }}</div>
            </div>
          </div>
        </el-form-item>
        <el-form-item v-if="form.mode === 'sample'" label="采样率">
          <el-input-number v-model="form.sample_ratio" :min="0.001" :max="1" :step="0.01" />
        </el-form-item>
        <el-form-item v-if="form.mode === 'checksum'" label="分块大小">
          <el-input-number v-model="form.checksum_chunk_size" :min="1000" :step="10000" />
        </el-form-item>
        <el-form-item v-if="form.mode === 'checksum'" label="并行数">
          <el-input-number v-model="form.checksum_parallel" :min="1" :max="32" />
        </el-form-item>
        <el-form-item label="并发数">
          <el-input-number v-model="form.parallel" :min="1" :max="32" />
          <span style="color: #909399; font-size: 12px; margin-left: 8px;">同时比对的表个数</span>
        </el-form-item>

        <el-form-item label="选择表">
          <div style="width: 100%;">
            <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px;">
              <el-button size="small" @click="loadTables">加载表列表</el-button>
              <span style="color: #909399; font-size: 13px;">已选 {{ selectedTables.length }} / {{ availableTables.length }} 张表</span>
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
                  <span style="color: #909399;">{{ row.row_estimate >= 0 ? row.row_estimate.toLocaleString() : '-' }}</span>
                </template>
              </el-table-column>
            </el-table>
            <el-alert v-if="selectedTables.length === 0" type="info" :closable="false" title="未选择任何表，将比对全部表" style="margin-top: 8px;" />
          </div>
        </el-form-item>

        <el-form-item>
          <el-button type="primary" size="large" :loading="starting" @click="startCompare">
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
      <div v-if="activeTask.status === 'running'" style="color: #909399; margin-bottom: 12px;">
        比对进度：{{ activeTask.tables_done }} / {{ activeTask.tables_total || '?' }} 张表
        <span v-if="activeTask.current_table">（当前：{{ activeTask.current_table }}）</span>
      </div>
      <el-alert v-if="activeTask.error" type="error" :closable="false" :title="activeTask.error" style="margin-bottom: 12px;" />

      <template v-if="activeReport">
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
          <template #default="{ row }">{{ row.mode }}</template>
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
.diff-zero { color: var(--tims-teal); }
.diff-low { color: var(--tims-amber); }
.diff-high { color: var(--tims-brand); font-weight: 500; }
:deep(.tims-num-col .cell) { font-family: var(--tims-font-mono); }

@media (max-width: 900px) {
  .tims-gauge-row { grid-template-columns: repeat(2, 1fr); }
}
</style>
