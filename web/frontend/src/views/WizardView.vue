<script setup lang="ts">
import { ref, reactive, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, type FormRules } from 'element-plus'
import apiClient from '../api'
import ConnectionForm from '../components/ConnectionForm.vue'
import { useSourceSchema } from '../composables/useSourceSchema'
import { reconcileModel } from '../composables/reconcileModel'


const router = useRouter()
const loading = ref(false)
const activeStep = ref(0)

// Multi-source config: schema-driven selector + dynamic connection form (#t77).
const { sources, load: loadSources, getSource } = useSourceSchema()
const sourceType = ref('postgres')
const currentMeta = computed(() => getSource(sourceType.value))

const availableTables = ref<{name: string; row_estimate: number}[]>([])
const loadingTables = ref(false)
const tableSearch = ref('')
const selectedTables = ref<string[]>([])
const tableRef = ref<any>(null)

const form = reactive({
  name: '',
  source: {} as Record<string, any>,
  target: {
    host: 'localhost',
    port: 4000,
    user: 'root',
    password: '',
    database: '',
    pd_addr: '',
    status_port: 0, // 0 = not provided: PD/Status probes are skipped unless the user sets them (Lightning only)
  },
  opts: {
    parallel: 4,
    batch_size: 100000,
    temp_dir: '/tmp/timstool',
    tables: [] as string[],
    exclude_tables: [] as string[],
    use_lightning: false,
    lightning_path: '',
    skip_precheck: false,
    skip_schema: false,
    skip_data: false,
    skip_validate: false,
    cdc_chain: false,
    target_policy: 'insert',
		compare_mode: 'sample',
		sample_ratio: 0.01,
		checksum_chunk_size: 50000,
		checksum_parallel: 4,
  },
})

const sourceTestResult = ref<any>(null)
const targetTestResult = ref<any>(null)
const testingSource = ref(false)
const testingTarget = ref(false)

// Lightning path gate (迁移选项页门禁): must validate successfully (explicit
// path OR auto-discovery probe) before the wizard may advance past step 3.
const lightningValidated = ref(false)
const lightningResolvedPath = ref('')
const validatingLightning = ref(false)

async function validateLightning() {
  validatingLightning.value = true
  try {
    const { data } = await apiClient.validateLightning(form.opts.lightning_path.trim())
    lightningValidated.value = data.success
    lightningResolvedPath.value = data.success ? data.resolved_path : ''
    if (data.success) ElMessage.success(data.message)
    else ElMessage.error(data.message)
  } catch (e: any) {
    lightningValidated.value = false
    lightningResolvedPath.value = ''
    ElMessage.error(`Lightning 路径验证失败: ${e.response?.data?.error || e.message}`)
  } finally {
    validatingLightning.value = false
  }
}

// Editing the path or toggling the switch invalidates a previous validation.
function onLightningPathChanged() {
  lightningValidated.value = false
  lightningResolvedPath.value = ''
}
function onLightningSwitchChanged(val: boolean) {
  lightningValidated.value = false
  lightningResolvedPath.value = ''
  if (!val) form.opts.lightning_path = ''
}

const savedConnections = ref<Array<{ name: string; sourceType?: string; source: any; target: any }>>([])
const saveConnName = ref('')
const saveConnDialogVisible = ref(false)
const loadConnDialogVisible = ref(false)

const STORAGE_KEY = 'timstool_saved_connections'

function loadSavedConnections() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw) {
      savedConnections.value = JSON.parse(raw)
    }
  } catch {}
}

function saveConnectionsToStorage() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(savedConnections.value))
}

function saveCurrentConnection() {
  if (!saveConnName.value.trim()) {
    ElMessage.warning('请输入连接配置名称')
    return
  }
  const idx = savedConnections.value.findIndex(c => c.name === saveConnName.value.trim())
  const entry = {
    name: saveConnName.value.trim(),
    sourceType: sourceType.value,
    source: { ...form.source },
    target: { ...form.target },
  }
  if (idx >= 0) {
    savedConnections.value[idx] = entry
  } else {
    savedConnections.value.push(entry)
  }
  saveConnectionsToStorage()
  saveConnDialogVisible.value = false
  saveConnName.value = ''
  ElMessage.success('连接配置已保存')
}

function loadConnection(conn: { sourceType?: string; source: any; target: any }) {
  // Restore the source type first (switch selector + reconcile the dynamic form
  // fields), then overlay the saved values — same mechanism as the manual
  // selector switch (onSourceTypeChange).
  if (conn.sourceType && conn.sourceType !== sourceType.value) {
    onSourceTypeChange(conn.sourceType)
  }
  Object.assign(form.source, conn.source)
  Object.assign(form.target, conn.target)
  sourceTestResult.value = null
  loadConnDialogVisible.value = false
  ElMessage.success('连接配置已加载')
}

function deleteConnection(idx: number) {
  savedConnections.value.splice(idx, 1)
  saveConnectionsToStorage()
}

onMounted(async () => {
  // Multi-source: load source metas, then seed the dynamic source form from the
  // default source (postgres) defaults.
  await loadSources()
  const meta = getSource(sourceType.value)
  if (meta) Object.assign(form.source, reconcileModel({}, meta, meta))

  loadSavedConnections()
  const last = localStorage.getItem('pg2tidb_last_connection')
  if (last) {
    try {
      const c = JSON.parse(last)
      if (c.sourceType && c.sourceType !== sourceType.value) {
        onSourceTypeChange(c.sourceType)
      }
      if (c.source) Object.assign(form.source, c.source)
      if (c.target) Object.assign(form.target, c.target)
    } catch {}
  }
})


const compareModes = [
  { value: 'quick', label: '\u26A1 快速', color: '#67c23a', desc: '仅行数估算，最快' },
  { value: 'sample', label: '\uD83D\uDDD1 采样', color: '#409eff', desc: '行数+随机采样（推荐）' },
  { value: 'checksum', label: '\uD83D\uDFE1 校验', color: '#e6a23c', desc: '行数+分块Hash' },
]

const rules: FormRules = {
  'source.host': [{ required: true, message: '请输入源数据库地址', trigger: 'blur' }],
  'source.database': [{ required: true, message: '请输入源数据库名', trigger: 'blur' }],
  'target.host': [{ required: true, message: '请输入目标数据库地址', trigger: 'blur' }],
  'target.database': [{ required: true, message: '请输入目标数据库名', trigger: 'blur' }],
}

// Switch source: reconcile the model to the new source's fields (drops stale
// source-specific keys — FL-REQ) and reset the connection-test state.
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

async function testConnection(type: 'source' | 'target') {
  if (type === 'source') {
    if (!currentMeta.value?.implemented) return
    testingSource.value = true
    sourceTestResult.value = null
    try {
      // FL-REQ: send only the reconciled field keys (no stale source-specific keys).
      const { data } = await apiClient.testSourceConnection(sourceType.value, { ...form.source })
      sourceTestResult.value = data
      if (data.success) ElMessage.success(`${currentMeta.value.displayName} 连接成功`)
      else ElMessage.error(`连接失败: ${data.message}`)
    } catch (e: any) {
      ElMessage.error(`连接测试失败: ${e.message}`)
    } finally {
      testingSource.value = false
    }
    return
  }
  // target (TiDB): legacy /config/test-connection
  testingTarget.value = true
  targetTestResult.value = null
  try {
    const { data } = await apiClient.testConnection({
      type,
      host: form.target.host,
      port: form.target.port,
      user: form.target.user,
      password: form.target.password,
      database: form.target.database,
      pd_addr: form.target.pd_addr || undefined,
      status_port: form.target.status_port || undefined,
    })
    targetTestResult.value = data
    if (data.ok) {
      ElMessage.success('TiDB 连接成功')
      // A successful target test is a good moment to persist the
      // Lightning-only extras (pd_addr / status_port) for the next task.
      saveMigrationOptions()
    }
    else ElMessage.error(`连接失败: ${data.error}`)
  } catch (e: any) {
    ElMessage.error(`连接测试失败: ${e.message}`)
  } finally {
    testingTarget.value = false
  }
}

async function loadTables() {
  loadingTables.value = true
  availableTables.value = []
  selectedTables.value = []
  try {
    // PG keeps its dedicated endpoint (reltuples row estimates, zero-regression);
    // non-PG lists tables via the adapter's SchemaReader (#t79 Phase 1).
    const { data } = sourceType.value === 'postgres'
      ? await apiClient.listTables({
          type: 'source',
          host: form.source.host,
          port: form.source.port,
          user: form.source.user,
          password: form.source.password,
          database: form.source.database,
          schema: form.source.schema,
          sslmode: form.source.sslmode,
        })
      : await apiClient.getSourceTables(sourceType.value, { ...form.source })
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

function handleTableSelection(rows: { name: string; row_estimate: number }[]) {
  selectedTables.value = rows.map(r => r.name)
}

function toggleSelectAll() {
  if (!tableRef.value) return
  if (selectedTables.value.length === filteredTables.value.length && filteredTables.value.length > 0) {
    tableRef.value.clearSelection()
  } else {
    filteredTables.value.forEach(row => {
      tableRef.value!.toggleRowSelection(row, true)
    })
  }
}

async function submit() {
  loading.value = true
  try {
    localStorage.setItem('timstool_last_connection', JSON.stringify({ source: form.source, target: form.target, sourceType: sourceType.value }))

    const { data } = await apiClient.createTask({
      name: form.name || `Migration ${new Date().toLocaleString()}`,
      source: { ...form.source, type: sourceType.value },
      target: { ...form.target },
      opts: {
        parallel: form.opts.parallel,
        batch_size: form.opts.batch_size,
        temp_dir: form.opts.temp_dir,
        tables: selectedTables.value,
        exclude_tables: [],
        use_lightning: form.opts.use_lightning,
        lightning_path: form.opts.use_lightning ? (lightningResolvedPath.value || form.opts.lightning_path.trim()) : '',
        skip_precheck: form.opts.skip_precheck,
        skip_schema: form.opts.skip_schema,
        skip_data: form.opts.skip_data,
        skip_validate: form.opts.skip_validate,
        cdc_chain: form.opts.cdc_chain && sourceType.value === 'postgres',
        target_policy: form.opts.target_policy,
        compare_mode: form.opts.compare_mode,
        sample_ratio: form.opts.sample_ratio,
        checksum_chunk_size: form.opts.checksum_chunk_size,
        checksum_parallel: form.opts.checksum_parallel,
      },
    })
    ElMessage.success('迁移任务创建成功')
    await apiClient.startTask(data.id)
    router.push(`/tasks/${data.id}`)
  } catch (e: any) {
    ElMessage.error(`创建失败: ${e.response?.data?.error || e.message}`)
  } finally {
    loading.value = false
  }
}

// Migration options memory (迁移选项记忆): prefill from server when the user
// first enters step 3, persist on advancing past it. Server is the single
// source of truth (survives refresh / restart / other browsers). The prefill
// is AWAITED before step 3 becomes visible, so a late GET response can never
// overwrite values the user has already edited (H1 race).
const optionsLoaded = ref(false)

async function loadMigrationOptions() {
  if (optionsLoaded.value) return
  optionsLoaded.value = true
  try {
    const { data } = await apiClient.getMigrationOptions()
    if (data.temp_dir) form.opts.temp_dir = data.temp_dir
    if (data.use_lightning) {
      form.opts.use_lightning = true
      form.opts.lightning_path = data.lightning_path || ''
    }
    // Lightning-only target extras: prefill here as well so the PUT issued
    // when leaving step 2 (目标库) carries the REMEMBERED option values
    // instead of form defaults — otherwise that save would clobber the
    // remembered temp_dir / lightning settings with defaults. Empty
    // pd_addr / status_port 0 mean "not remembered" and never overwrite.
    if (data.pd_addr) form.target.pd_addr = data.pd_addr
    if (data.status_port && data.status_port > 0) form.target.status_port = data.status_port
  } catch {}
}

async function saveMigrationOptions() {
  try {
    await apiClient.saveMigrationOptions({
      temp_dir: form.opts.temp_dir.trim(),
      use_lightning: form.opts.use_lightning,
      lightning_path: form.opts.use_lightning ? form.opts.lightning_path.trim() : '',
      pd_addr: form.target.pd_addr.trim(),
      status_port: form.target.status_port,
    })
  } catch (e: any) {
    ElMessage.warning(`迁移选项保存失败：${e.response?.data?.error || e.message}（不影响本次迁移）`)
  }
}

async function nextStep() {
  if (activeStep.value === 0 && !sourceTestResult.value?.success) {
    ElMessage.warning('请先测试源数据库连接')
    return
  }
  if (activeStep.value === 1 && !targetTestResult.value?.ok) {
    ElMessage.warning('请先测试目标数据库连接')
    return
  }
  // Entering step 2 (目标库): await the remembered options prefill (which
  // includes pd_addr / status_port) BEFORE the step becomes editable —
  // same H1-race guard as step 3, and it guarantees the save issued when
  // leaving step 2 preserves previously remembered option values.
  if (activeStep.value === 0) {
    await loadMigrationOptions()
  }
  if (activeStep.value === 1) {
    loadTables()
    // Leaving step 2: persist the Lightning-only target extras along with
    // the other remembered options so the next task prefills them.
    saveMigrationOptions()
  }
  // Entering step 3 (迁移选项): await last-saved options prefill BEFORE the
  // step becomes editable, so the user never races the GET response.
  if (activeStep.value === 2) {
    await loadMigrationOptions()
  }
  // Lightning gate (step 3 迁移选项): enabled Lightning requires a successful
  // path validation (explicit path or auto-discovery) before advancing.
  if (activeStep.value === 3 && form.opts.use_lightning && !lightningValidated.value) {
    ElMessage.error('已开启 Lightning：请先配置并验证 tidb-lightning 可执行文件路径（或留空点验证走自动发现），验证通过才能进入下一步')
    return
  }
  if (activeStep.value === 3) {
    // Persist options so the next migration prefills them.
    saveMigrationOptions()
  }
  activeStep.value++
}
function prevStep() {
  activeStep.value--
}
</script>

<template>
  <div style="max-width: 900px; margin: 0 auto;">
    <el-card>
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <div style="display: flex; align-items: center;">
            <el-icon size="24" style="margin-right: 8px;"><Connection /></el-icon>
            <span style="font-size: 18px; font-weight: bold;">新建迁移任务</span>
          </div>
          <el-space>
            <el-button size="small" @click="loadConnDialogVisible = true">
              <el-icon><FolderOpened /></el-icon> 加载连接
            </el-button>
            <el-button size="small" @click="saveConnDialogVisible = true">
              <el-icon><FolderAdd /></el-icon> 保存连接
            </el-button>
          </el-space>
        </div>
      </template>

      <el-steps :active="activeStep" finish-status="success" align-center style="margin-bottom: 30px;">
        <el-step title="源数据库" />
        <el-step title="目标数据库" />
        <el-step title="选择表" />
        <el-step title="迁移选项" />
        <el-step title="确认执行" />
      </el-steps>

      <el-form ref="formRef" :model="form" :rules="rules" label-width="120px">
        <!-- Step 0: Source -->
        <div v-show="activeStep === 0">
          <el-form-item label="任务名称">
            <el-input v-model="form.name" placeholder="可选，自动生成" />
          </el-form-item>
          <el-form-item label="数据源类型" v-if="sources.length > 0">
            <el-select :model-value="sourceType" placeholder="选择数据源" style="width: 100%" @change="onSourceTypeChange">
              <el-option v-for="s in sources" :key="s.name" :label="s.displayName" :value="s.name" :disabled="!s.implemented">
                <span>{{ s.displayName }}</span>
                <span v-if="!s.implemented" style="color: #c0c4cc; font-size: 12px; margin-left: 8px;">即将支持</span>
              </el-option>
            </el-select>
          </el-form-item>
          <ConnectionForm v-if="currentMeta" :meta="currentMeta" :model="form.source" />
          <el-form-item>
            <el-button type="primary" :loading="testingSource" :disabled="!currentMeta?.implemented" @click="testConnection('source')">
              测试 {{ currentMeta?.displayName || 'PostgreSQL' }} 连接
            </el-button>
            <el-tag v-if="sourceTestResult" :type="sourceTestResult.success ? 'success' : 'danger'" style="margin-left: 12px;">
              {{ sourceTestResult.success ? '连接成功' : sourceTestResult.message }}
            </el-tag>
          </el-form-item>
        </div>

        <!-- Step 1: Target -->
        <div v-show="activeStep === 1">
          <el-form-item label="主机地址" prop="target.host">
            <el-input v-model="form.target.host" />
          </el-form-item>
          <el-form-item label="端口">
            <el-input-number v-model="form.target.port" :min="1" :max="65535" />
          </el-form-item>
          <el-form-item label="用户名">
            <el-input v-model="form.target.user" />
          </el-form-item>
          <el-form-item label="密码">
            <el-input v-model="form.target.password" type="password" show-password />
          </el-form-item>
          <el-form-item label="数据库名" prop="target.database">
            <el-input v-model="form.target.database" />
          </el-form-item>
          <el-form-item label="PD 地址">
            <el-input v-model="form.target.pd_addr" placeholder="host:2379（PD 对外端口，经代理时填真实 PD 端口），留空则自动推断；仅 Lightning 需要" />
            <div class="form-hint">PD 地址与 Status 端口仅在使用 Lightning 导入时才需要填写：不使用 Lightning 请保持留空/0（不会检测、不影响连接测试）；需要时填真实 PD/Status 端口（SQL 端口经代理（如 haproxy 5000）时不能填 SQL 端口）</div>
          </el-form-item>
            <el-form-item label="TiDB 状态端口">
            <el-input-number v-model="form.target.status_port" :min="0" :max="65535" placeholder="10080" />
          </el-form-item>
          <el-form-item>
            <el-button type="primary" :loading="testingTarget" @click="testConnection('target')">
              测试 TiDB 连接
            </el-button>
            <template v-if="targetTestResult">
              <el-tag :type="targetTestResult.mysql_ok === false || (!targetTestResult.mysql_ok && !targetTestResult.ok) ? 'danger' : 'success'" style="margin-left: 12px;">
                MySQL {{ (targetTestResult.mysql_ok ?? targetTestResult.ok) ? `连接成功 (${targetTestResult.version?.substring(0, 50)})` : targetTestResult.error }}
              </el-tag>
              <el-tag v-if="targetTestResult.pd_ok !== undefined" :type="targetTestResult.pd_ok ? 'success' : 'danger'" style="margin-left: 4px;">
                PD {{ targetTestResult.pd_ok ? `通过${targetTestResult.pd_cluster_id ? ' (cluster ' + targetTestResult.pd_cluster_id + ')' : ''}` : targetTestResult.pd_error }}
              </el-tag>
              <el-tag v-if="targetTestResult.status_ok !== undefined" :type="targetTestResult.status_ok ? 'success' : 'danger'" style="margin-left: 4px;">
                Status {{ targetTestResult.status_ok ? '通过' : targetTestResult.status_error }}
              </el-tag>
            </template>
          </el-form-item>
        </div>

        <!-- Step 2: Select Tables -->
        <div v-show="activeStep === 2">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px;">
            <el-input v-model="tableSearch" placeholder="搜索表名" style="width: 300px;" clearable>
              <template #prefix><el-icon><Search /></el-icon></template>
            </el-input>
            <el-space>
              <el-button size="small" @click="toggleSelectAll">
                {{ selectedTables.length === filteredTables.length && filteredTables.length > 0 ? '取消全选' : '全选' }}
              </el-button>
              <span style="color: #909399; font-size: 13px;">已选 {{ selectedTables.length }} / {{ availableTables.length }} 张表</span>
            </el-space>
          </div>
          <div v-if="loadingTables" v-loading="true" style="min-height: 200px;"></div>
          <div v-else-if="availableTables.length === 0" style="color: #909399; text-align: center; padding: 40px;">
            暂无表数据，请确认源数据库连接配置
          </div>
          <template v-else>
            <el-table
              ref="tableRef"
              :data="filteredTables"
              @selection-change="handleTableSelection"
              style="width: 100%;"
              max-height="460"
              :row-key="(row: any) => row.name"
            >
              <el-table-column type="selection" width="55" :reserve-selection="true" />
              <el-table-column prop="name" label="表名" />
              <el-table-column label="预估行数" width="180" align="right">
                <template #default="{ row }: { row: { row_estimate: number } }">
                  <span style="color: #909399;">{{ row.row_estimate >= 0 ? row.row_estimate.toLocaleString() : '-' }}</span>
                </template>
              </el-table-column>
            </el-table>
            <div v-if="selectedTables.length === 0 && availableTables.length > 0" style="margin-top: 8px;">
              <el-alert type="info" :closable="false" title="未选择任何表，将迁移所有表" />
            </div>
          </template>
        </div>

        <!-- Step 3: Options -->
        <div v-show="activeStep === 3">
          <el-form-item label="并发数">
            <el-input-number v-model="form.opts.parallel" :min="1" :max="32" />
            <span style="color: #909399; font-size: 12px; margin-left: 8px;">同时迁移的表个数</span>
          </el-form-item>
          <el-form-item label="批次大小">
            <el-input-number v-model="form.opts.batch_size" :min="1000" :step="10000" />
          </el-form-item>
          <el-form-item label="使用 Lightning">
            <el-switch v-model="form.opts.use_lightning" @change="onLightningSwitchChanged" />
          </el-form-item>
          <el-form-item v-if="form.opts.use_lightning" label="Lightning 路径">
            <div style="display: flex; gap: 8px; width: 100%;">
              <el-input
                v-model="form.opts.lightning_path"
                placeholder="tidb-lightning 可执行文件路径，留空则自动发现（PATH/内嵌）"
                style="width: 350px;"
                @input="onLightningPathChanged"
              />
              <el-button :loading="validatingLightning" @click="validateLightning">验证</el-button>
            </div>
            <div :style="{ color: lightningValidated ? '#67c23a' : '#e6a23c', fontSize: '12px', marginTop: '4px' }">
              <template v-if="lightningValidated">✅ 验证通过：{{ lightningResolvedPath }}</template>
              <template v-else>开启 Lightning 后必须点击「验证」且通过（远端 Linux 将校验执行权限），才能进入下一步</template>
            </div>
          </el-form-item>
          <el-form-item label="数据临时目录">
            <el-input v-model="form.opts.temp_dir" placeholder="/tmp/timstool" style="width: 350px;" />
            <div style="color: #909399; font-size: 12px; margin-top: 4px;">
              源端数据导出为 CSV 的临时存储目录，需确保磁盘空间充足（至少能容纳全部待迁移数据）。
            </div>
          </el-form-item>
          <el-divider>增量同步衔接</el-divider>
          <el-form-item>
            <template #label>
              <span style="color: var(--tims-brand); font-weight: 600;">全量+增量衔接</span>
            </template>
            <div class="chain-emphasis" style="width: 100%;">
              <el-switch v-model="form.opts.cdc_chain" :disabled="sourceType !== 'postgres'" />
              <div style="color: var(--tims-text-2); font-size: 12px; margin-top: 4px;">
                仅 PostgreSQL 源端可用。开启后任务启动前会自动预建 CDC 的 publication + replication
                slot，全量期间源端 WAL 被保留；全量成功后自动启动 CDC 增量同步，从预建点位重放，实现零丢失衔接
                （重放与全量重叠的数据按 conflict_strategy=replace 幂等去重）。注意：全量期间源端
                WAL 会持续累积，max_slot_wal_keep_size 不要设置过小。
              </div>
            </div>
          </el-form-item>
          <el-divider>目标数据处理策略</el-divider>
          <el-form-item label="数据冲突策略">
            <el-radio-group v-model="form.opts.target_policy">
              <el-radio value="insert">直接插入（INSERT）</el-radio>
              <el-radio value="truncate">先清空表（TRUNCATE）</el-radio>
              <el-radio value="drop">先删除表（DROP）</el-radio>
            </el-radio-group>
            <div style="color: #909399; font-size: 12px; margin-top: 4px;">
              重复迁移时如何处理目标库已有数据。选择"先清空表"会删除表内数据但保留结构，"先删除表"会完全重建表。
            </div>
          </el-form-item>
          <el-divider>数据对比模式</el-divider>
          <el-form-item label="对比模式">
            <div style="display: flex; gap: 12px; flex-wrap: wrap;">
              <div
                v-for="m in compareModes" :key="m.value"
                @click="form.opts.compare_mode = m.value"
                :style="{
                  border: form.opts.compare_mode === m.value ? '2px solid ' + m.color : '2px solid #dcdfe6',
                  borderRadius: '8px',
                  padding: '12px 16px',
                  cursor: 'pointer',
                  minWidth: '140px',
                  transition: 'all 0.2s',
                  background: form.opts.compare_mode === m.value ? m.color + '10' : '#fff',
                }"
              >
                <div style="font-weight: bold; font-size: 14px;">{{ m.label }}</div>
                <div style="color: #909399; font-size: 12px; margin-top: 4px;">{{ m.desc }}</div>
              </div>
            </div>
          </el-form-item>
          <el-form-item v-if="form.opts.compare_mode === 'sample'" label="采样率">
            <el-slider v-model="form.opts.sample_ratio" :min="0.001" :max="1" :step="0.001" :format-tooltip="(v: number) => (v * 100).toFixed(1) + '%'" style="width: 300px;" />
          </el-form-item>
          <el-form-item v-if="form.opts.compare_mode === 'checksum'" label="分块大小">
            <el-input-number v-model="form.opts.checksum_chunk_size" :min="1000" :step="10000" />
            <span style="color: #909399; font-size: 12px; margin-left: 8px;">每块的行数</span>
          </el-form-item>
          <el-form-item v-if="form.opts.compare_mode === 'checksum'" label="并行数">
            <el-input-number v-model="form.opts.checksum_parallel" :min="1" :max="16" />
          </el-form-item>
          <el-divider>跳过阶段（高级）</el-divider>
          <el-form-item label="跳过预检">
            <el-switch v-model="form.opts.skip_precheck" />
          </el-form-item>
          <el-form-item label="跳过 Schema">
            <el-switch v-model="form.opts.skip_schema" />
          </el-form-item>
          <el-form-item label="跳过数据">
            <el-switch v-model="form.opts.skip_data" />
          </el-form-item>
          <el-form-item label="跳过验证">
            <el-switch v-model="form.opts.skip_validate" />
          </el-form-item>
        </div>

        <!-- Step 4: Confirm -->
        <div v-show="activeStep === 4">
          <el-descriptions title="迁移配置确认" :column="2" border>
            <el-descriptions-item label="任务名称">{{ form.name || '自动生成' }}</el-descriptions-item>
            <el-descriptions-item label="并发数">{{ form.opts.parallel }}</el-descriptions-item>
            <el-descriptions-item label="源数据库">{{ form.source.host }}:{{ form.source.port }}/{{ form.source.database }}</el-descriptions-item>
            <el-descriptions-item label="目标数据库">{{ form.target.host }}:{{ form.target.port }}/{{ form.target.database }}</el-descriptions-item>
            <el-descriptions-item label="迁移表数">{{ selectedTables.length > 0 ? selectedTables.length : '全部 (' + availableTables.length + ')' }}</el-descriptions-item>
            <el-descriptions-item label="对比模式">{{ compareModes.find(m => m.value === form.opts.compare_mode)?.label }}</el-descriptions-item>
            <el-descriptions-item label="使用 Lightning">{{ form.opts.use_lightning ? '是' : '否' }}</el-descriptions-item>
            <el-descriptions-item v-if="form.opts.use_lightning" label="Lightning 路径" :span="2">
              {{ lightningResolvedPath || form.opts.lightning_path || '自动发现' }}
            </el-descriptions-item>
            <el-descriptions-item label="数据临时目录">{{ form.opts.temp_dir }}</el-descriptions-item>
            <el-descriptions-item label="全量+增量衔接">
              <el-tag v-if="form.opts.cdc_chain && sourceType === 'postgres'" type="success">已开启（预建 slot，零丢失）</el-tag>
              <template v-else>否</template>
            </el-descriptions-item>
            <el-descriptions-item label="数据冲突策略">
              {{ form.opts.target_policy === 'truncate' ? '先清空表' : form.opts.target_policy === 'drop' ? '先删除表' : '直接插入' }}
            </el-descriptions-item>
            <el-descriptions-item label="跳过阶段" :span="2">
              <template v-if="form.opts.skip_precheck || form.opts.skip_schema || form.opts.skip_data || form.opts.skip_validate">
                <el-tag v-if="form.opts.skip_precheck" type="warning" style="margin-right: 4px;">跳过预检</el-tag>
                <el-tag v-if="form.opts.skip_schema" type="warning" style="margin-right: 4px;">跳过 Schema</el-tag>
                <el-tag v-if="form.opts.skip_data" type="danger" style="margin-right: 4px;">跳过数据迁移</el-tag>
                <el-tag v-if="form.opts.skip_validate" type="warning" style="margin-right: 4px;">跳过验证</el-tag>
              </template>
              <template v-else>无（完整流程）</template>
            </el-descriptions-item>
          </el-descriptions>
          <el-alert
            title="点击「开始迁移」将创建任务并立即开始执行迁移"
            type="warning"
            :closable="false"
            style="margin-top: 16px;"
          />
        </div>

        <el-form-item style="margin-top: 24px;">
          <el-button v-if="activeStep > 0" @click="prevStep">上一步</el-button>
          <el-button v-if="activeStep < 4" type="primary" @click="nextStep">下一步</el-button>
          <el-button v-if="activeStep === 4" type="success" :loading="loading" @click="submit">
            开始迁移
          </el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <!-- Save Connection Dialog -->
    <el-dialog v-model="saveConnDialogVisible" title="保存连接配置" width="400px">
      <el-input v-model="saveConnName" placeholder="输入配置名称（如：生产环境）" />
      <template #footer>
        <el-button @click="saveConnDialogVisible = false">取消</el-button>
        <el-button type="primary" @click="saveCurrentConnection">保存</el-button>
      </template>
    </el-dialog>

    <!-- Load Connection Dialog -->
    <el-dialog v-model="loadConnDialogVisible" title="加载连接配置" width="500px">
      <div v-if="savedConnections.length === 0" style="color: #999; text-align: center; padding: 20px;">
        暂无保存的连接配置
      </div>
      <div v-for="(conn, idx) in savedConnections" :key="idx" style="border: 1px solid #ebeef5; border-radius: 8px; padding: 12px; margin-bottom: 10px;">
        <div style="display: flex; justify-content: space-between; align-items: center;">
          <div>
            <strong>{{ conn.name }}</strong>
            <div style="color: #909399; font-size: 12px; margin-top: 4px;">
              {{ getSource(conn.sourceType || 'postgres')?.displayName || 'Source' }}: {{ conn.source.host }}:{{ conn.source.port }}/{{ conn.source.database }}
              → TiDB: {{ conn.target.host }}:{{ conn.target.port }}/{{ conn.target.database }}
            </div>
          </div>
          <el-space>
            <el-button size="small" type="primary" @click="loadConnection(conn)">加载</el-button>
            <el-button size="small" type="danger" plain @click="deleteConnection(idx)">删除</el-button>
          </el-space>
        </div>
      </div>
    </el-dialog>
  </div>
</template>

<style scoped>
.form-hint {
  color: var(--tims-text-2);
  font-size: 12px;
  line-height: 1.4;
  margin-top: 4px;
}

/* Step bar: brand-red repaint + check pop on finish */
:deep(.el-step__head.is-process) { color: var(--tims-brand); border-color: var(--tims-brand); }
:deep(.el-step__head.is-process .el-step__icon) {
  background: var(--tims-brand);
  color: #fff;
  border-color: var(--tims-brand);
  box-shadow: 0 0 0 4px var(--tims-brand-soft);
}
:deep(.el-step__title.is-process) { color: var(--tims-brand); font-weight: 600; }
:deep(.el-step__head.is-finish) { color: var(--tims-teal); border-color: var(--tims-teal); }
:deep(.el-step__title.is-finish) { color: var(--tims-teal); }
:deep(.el-step__head.is-finish .el-step__icon) {
  animation: step-check 0.35s cubic-bezier(0.34, 1.56, 0.64, 1);
}
@keyframes step-check {
  0% { transform: scale(0.6); }
  60% { transform: scale(1.15); }
  100% { transform: scale(1); }
}

/* Chain option: brand emphasis while the wizard is on the options step */
:deep(.chain-emphasis) {
  border: 1px solid rgba(225, 60, 60, 0.35);
  border-radius: var(--tims-radius-s);
  padding: 10px 12px;
  background: var(--tims-brand-soft);
}
</style>
