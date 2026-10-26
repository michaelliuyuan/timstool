<script setup lang="ts">
import { ref, reactive, computed, watch, onMounted, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowLeft } from '@element-plus/icons-vue'
import apiClient from '../api'
import type { CompareTaskSpec } from '../api'
import PageHeader from '../components/PageHeader.vue'
import ConnectionForm from '../components/ConnectionForm.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import DbBrowseSelect from '../components/DbBrowseSelect.vue'
import { useSourceSchema } from '../composables/useSourceSchema'

// MongoDB compare task creation (M4): schema-driven forms for both sides
// (the /sources metadata is the single source of truth — no hard-coded
// fields), database names, and an advanced collapse for engine knobs.
// Submit: create → start (clean run) → task detail.
// M6: each side may reference a saved mongodb datasource instead of a
// manual form (server-side credential snapshot; explicit manual fields
// override the stored profile server-side).
// 第9单: optional watermark window card — global group + per-collection
// exceptions, mirroring the pg/mysql CompareView pattern (interval/single
// toggle, datetime pickers, pre-run confirm, informed-consent text).
// 水位列自动发现单（seq=39）: column candidates = _id default + suggest
// API top-5 (index prefixes ∪ bounded sampling, shared scorer) + manual.
const router = useRouter()
const { load, getSource } = useSourceSchema()

const mongoMeta = ref<ReturnType<typeof getSource>>()
const creating = ref(false)
const advancedOpen = ref(false)

const srcModel = reactive<Record<string, any>>({ host: 'localhost', port: 27017 })
const tgtModel = reactive<Record<string, any>>({ host: 'localhost', port: 27017 })
const srcRef = ref('')
const tgtRef = ref('')
const srcDb = ref('')
const tgtDb = ref('')
const tgtSameAsSrc = ref(true)

const adv = reactive({ buckets: 64, parallel_colls: 4, parallel_buckets: 4, batch_size: 1000 })
const levels = ref<string[]>(['schema', 'index', 'count', 'hash', 'diff'])
const levelOptions = [
  { label: '集合存在性 (schema)', value: 'schema' },
  { label: '索引 (index)', value: 'index' },
  { label: '文档数 (count)', value: 'count' },
  { label: '分桶哈希 (hash)', value: 'hash' },
  { label: '差异下钻 (diff)', value: 'diff' },
]

const testing = ref({ src: false, tgt: false })
const testState = ref<{ src?: boolean; tgt?: boolean }>({})

// ---- 第9单: watermark window card (global group + per-collection exceptions) ----
// Same activation/resolution chain as the pg/mysql side: an entry activates
// with column + value (single, op ≤/<) or column + full interval
// [start,end); overrides win per collection, else the global group applies,
// else full comparison. The frontend mirrors the backend resolver exactly.
// 水位列自动发现（seq=39 ④）: 候选 = _id 默认项 + suggest API 返回的整库
// 评分候选（索引前缀∪采样）；候选仅辅助——运行期探针仍是权威。
const wmEnabled = ref(false)
const wmConfig = reactive({ column: '_id', op: '<=', value: '', range_mode: false, range_start: '', range_end: '' })
// v1 kind is a UX aid (control + validation) auto-derived from the column
// pick; the backend stays the authority — it probes the real BSON type and
// converts the raw string per that type (mismatch = disclosed degrade/fail).
const wmKind = ref<'objectid' | 'datetime' | 'int'>('objectid')
// suggest-flow state: ranked candidates from /mongocompare/suggest-watermark.
interface MongoWmCandidate {
  column: string
  score: number
  coverage: number
  indexed_ratio: number
  type_histogram: Record<string, number>
  matched_tables: string[]
  unmatched_tables: string[]
  reasons: string[]
  warnings: string[]
}
const wmCandidates = ref<MongoWmCandidate[]>([])
const wmSuggestTotal = ref(0)
const wmLoadingCandidates = ref(false)
const wmCandByCol = computed(() => {
  const m: Record<string, MongoWmCandidate> = {}
  for (const c of wmCandidates.value) m[c.column] = c
  return m
})
const wmColumnOptions = computed(() => ['_id', ...wmCandidates.value.map(c => c.column).filter(c => c !== '_id')])
const activeWmCandidate = computed(() => wmCandByCol.value[wmConfig.column])
// Candidate kind derives from its type histogram (datetime → time pickers,
// bigint/integer → integer input); manual entries keep the datetime default.
function wmKindForColumn(col: string): 'objectid' | 'datetime' | 'int' {
  if (col === '_id') return 'objectid'
  const c = wmCandByCol.value[col]
  if (c) {
    const hist = c.type_histogram || {}
    if ((hist.datetime || 0) >= (hist.bigint || 0) + (hist.integer || 0)) return 'datetime'
    return 'int'
  }
  return 'datetime'
}
watch(() => wmConfig.column, (col) => {
  wmKind.value = wmKindForColumn(col)
  wmConfig.range_mode = false
})
// Candidates belong to the srcDb they were fetched from — a database switch
// invalidates coverage labels (正评席 note / 对抗席 d5).
watch(srcDb, () => {
  wmCandidates.value = []
  wmSuggestTotal.value = 0
  // coll-select ①: the loaded collection list belongs to srcDb the same
  // way — a switch must not leave a stale picker (nor a stale selection
  // silently riding into the submitted spec).
  collEntries.value = []
  collSelectedNames.value = []
})

// ---- coll-select ①: per-collection picker (default all-selected) ----
interface CollEntry { name: string; is_view: boolean }
const collLoading = ref(false)
const collEntries = ref<CollEntry[]>([])
const collSelectedNames = ref<string[]>([])
const collTable = ref<any>(null)
const pickableColls = computed(() => collEntries.value.filter(e => !e.is_view))

async function loadColls() {
  if (!srcDb.value) { ElMessage.warning('请先填写源端数据库名'); return }
  collLoading.value = true
  try {
    // 对抗席 F1: database rides inside the source snapshot (suggest twin) —
    // endpointOf's database field is still '' at this point.
    const { data } = await apiClient.listMongoCollections({ ...endpointOf('src'), database: srcDb.value })
    collEntries.value = (data.collections || []) as CollEntry[]
    collSelectedNames.value = []
    if (collEntries.value.length === 0) {
      // 正评席 N5: an empty listing also means a typo'd database name —
      // mongo returns [] for a nonexistent db, say so.
      ElMessage.warning('该数据库没有可比集合（或均为视图，或数据库名不存在）')
      return
    }
    // Default = every pickable collection checked (requirement: 默认全选).
    // Views stay unchecked AND disabled — the engine skips them by design.
    nextTick(() => {
      const t = collTable.value
      if (!t) return
      for (const row of collEntries.value) {
        if (!row.is_view) t.toggleRowSelection(row, true)
      }
    })
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '获取集合列表失败')
  } finally {
    collLoading.value = false
  }
}
function onCollSelectionChange(rows: CollEntry[]) {
  collSelectedNames.value = rows.filter(r => !r.is_view).map(r => r.name)
}
function selectAllColls() {
  const t = collTable.value
  if (!t) return
  t.clearSelection()
  for (const row of collEntries.value) {
    if (!row.is_view) t.toggleRowSelection(row, true)
  }
}
function clearCollSelection() {
  collTable.value?.clearSelection()
}
async function loadWmCandidates() {
  if (!srcDb.value) { ElMessage.warning('请先填写源端数据库名'); return }
  wmLoadingCandidates.value = true
  try {
    const { data } = await apiClient.suggestMongoWatermark({ ...endpointOf('src'), database: srcDb.value })
    wmCandidates.value = (data.candidates || []) as MongoWmCandidate[]
    wmSuggestTotal.value = data.total_tables || 0
    if (wmCandidates.value.length === 0) {
      ElMessage.warning('未发现可比类型的候选水位列（date/int/long）；可手输列名')
    }
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '获取候选水位列失败')
  } finally {
    wmLoadingCandidates.value = false
  }
}
const wmRangeEligible = computed(() => wmKind.value === 'objectid' || wmKind.value === 'datetime')

function validateWmEndpoint(raw: string, label: string): string {
  const v = raw.trim()
  if (!v) return `${label}必须填写`
  if (wmKind.value === 'objectid' && !/^[0-9a-fA-F]{24}$/.test(v)) return `${label}须为 24 位十六进制 ObjectId`
  if (wmKind.value === 'datetime' && !/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(v)) return `${label}格式应为 YYYY-MM-DD HH:mm:ss`
  if (wmKind.value === 'int' && !/^-?\d+$/.test(v)) return `${label}必须是整数`
  return ''
}
function validateWmGroup(g: { column: string; op: string; value: string; range_mode: boolean; range_start: string; range_end: string }): string {
  if (!g.column.trim()) return '请填写水位列名'
  if (g.range_mode) {
    const s = validateWmEndpoint(g.range_start, '区间起点')
    if (s) return s
    const e = validateWmEndpoint(g.range_end, '区间终点')
    if (e) return e
    // ObjectId hex orders by BYTES: case-fold first (hex is a byte
    // rendering, so lowercased ASCII order == byte order — "..0F" < "..0a"
    // as raw strings but 0x0F > 0x0A as bytes = an empty window). The
    // datetime kind is fixed-width zero-padded, plain string order holds.
    const ordLt = (a: string, b: string) => (wmKind.value === 'objectid' ? a.toLowerCase() < b.toLowerCase() : wmKind.value === 'int' ? Number(a) < Number(b) : a < b)
    if (!ordLt(g.range_start.trim(), g.range_end.trim())) return '区间起点必须小于终点（半开区间 [start, end)）'
    return ''
  }
  if (g.op !== '<=' && g.op !== '<') return '比较符必须是 ≤ 或 <'
  return validateWmEndpoint(g.value, '水位值')
}
const globalWmActive = computed(() =>
  wmEnabled.value
  && wmConfig.column.trim() !== ''
  && (wmConfig.value.trim() !== '' || (wmConfig.range_start.trim() !== '' && wmConfig.range_end.trim() !== '')))

// Per-collection overrides. The FE has no pre-run collection list (the
// engine enumerates the source db), so entries are keyed by manually typed
// collection names — resolution still mirrors the backend exactly.
interface CollWmEntry {
  column: string
  kind: 'objectid' | 'datetime' | 'int'
  op: string
  value: string
  range_mode: boolean
  range_start: string
  range_end: string
}
const collWms = ref<Record<string, CollWmEntry>>({})
function collWmActive(e: CollWmEntry | undefined): e is CollWmEntry {
  if (!e) return false
  return e.column.trim() !== ''
    && (e.value.trim() !== '' || (e.range_start.trim() !== '' && e.range_end.trim() !== ''))
}
const activeOverrideCount = computed(() => Object.values(collWms.value).filter(e => collWmActive(e)).length)

function wmCondShort(e: { column: string; op: string; value: string; range_mode: boolean; range_start: string; range_end: string }): string {
  if (e.range_mode && e.range_start && e.range_end) return `${e.column} ∈ [${e.range_start}, ${e.range_end})`
  return `${e.column} ${e.op} ${e.value}`
}

// Candidate option label: the pg-page shape (coverage/score ride along).
function wmColLabel(c: string): string {
  if (c === '_id') return '_id（默认，ObjectId 时间戳）'
  const cand = wmCandByCol.value[c]
  if (!cand) return c
  return `${c}（覆盖 ${cand.matched_tables.length}/${wmSuggestTotal.value || '?'} · ${cand.score.toFixed(0)} 分）`
}

// Override editor dialog (collection name + a full group of its own).
const cwmDialog = ref(false)
const cwmName = ref('')
const cwmForm = reactive<CollWmEntry>({ column: '_id', kind: 'objectid', op: '<=', value: '', range_mode: false, range_start: '', range_end: '' })
function openCwmDialog() {
  cwmName.value = ''
  Object.assign(cwmForm, { column: '_id', kind: 'objectid', op: '<=', value: '', range_mode: false, range_start: '', range_end: '' })
  cwmDialog.value = true
}
function saveCwmEntry() {
  const name = cwmName.value.trim()
  if (!name) { ElMessage.warning('请填写集合名'); return }
  if (collWms.value[name]) { ElMessage.warning('该集合已配置例外水位（同一集合仅一条）'); return }
  // validate with the entry's own kind (temporarily swap the global kind ref)
  const prev = wmKind.value
  wmKind.value = cwmForm.kind
  const verr = validateWmGroup(cwmForm)
  wmKind.value = prev
  if (verr) { ElMessage.warning(verr); return }
  collWms.value = { ...collWms.value, [name]: { ...cwmForm } }
  cwmDialog.value = false
}
function removeCwmEntry(name: string) {
  const next = { ...collWms.value }
  delete next[name]
  collWms.value = next
}

// Pre-run confirm: the EFFECTIVE configuration disclosure (global group +
// every override + the everything-else statement) — informed consent before
// a windowed run, mirroring the pg/mysql 例外水位确认 dialog.
async function confirmWmBeforeRun(): Promise<boolean> {
  const lines: string[] = []
  lines.push(globalWmActive.value
    ? `全局水位：${wmCondShort(wmConfig)}（未配置例外的集合按此过滤）`
    : '全局水位：未启用（未配置例外的集合将全量比对）')
  for (const [name, e] of Object.entries(collWms.value)) {
    lines.push(collWmActive(e) ? `例外 ${name}：${wmCondShort(e)}` : `例外 ${name}：（未激活，将被忽略）`)
  }
  try {
    await ElMessageBox.confirm(
      `${lines.join('\n')}\n\n水位语义：只比对窗口内文档；水位前的旧文档改动、以及两端都已删除的文档检测不到。`,
      '水位过滤确认',
      { type: 'info', confirmButtonText: '开始比对', cancelButtonText: '返回检查', customStyle: { whiteSpace: 'pre-line' } as any },
    )
    return true
  } catch {
    return false
  }
}

onMounted(async () => {
  await load()
  mongoMeta.value = getSource('mongodb')
  if (!mongoMeta.value?.implemented) {
    ElMessage.error('MongoDB 源不可用（后端未注册）')
  }
})

async function testSide(side: 'src' | 'tgt') {
  testing.value[side] = true
  testState.value[side] = undefined
  try {
    // Referenced datasource: credentials live server-side — test through
    // the datasource test endpoint, not the inline field form.
    const refId = side === 'src' ? srcRef.value : tgtRef.value
    if (refId) {
      const { data } = await apiClient.testDataSource(refId)
      testState.value[side] = !!data.success
      if (!data.success) ElMessage.warning(`连接失败: ${data.message || '未知错误'}`)
    } else {
      const model = side === 'src' ? srcModel : tgtModel
      const { data } = await apiClient.testSourceConnection('mongodb', { ...model })
      testState.value[side] = !!data.success
      if (!data.success) ElMessage.warning(`连接失败: ${data.message || '未知错误'}`)
    }
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '连接测试失败')
  } finally {
    testing.value[side] = false
  }
}

function endpointOf(side: 'src' | 'tgt'): CompareTaskSpec['source'] {
  // Datasource reference: only the database name is needed here — the
  // server folds host/port/user/password/authDb/tls from the stored
  // profile into a creation-time snapshot.
  const refId = side === 'src' ? srcRef.value : tgtRef.value
  if (refId) return { datasource_id: refId, database: '' }
  const model = side === 'src' ? srcModel : tgtModel
  const ep: CompareTaskSpec['source'] = { database: '' }
  if (model.host) ep.host = model.host
  if (model.port) ep.port = Number(model.port)
  if (model.user) ep.user = model.user
  if (model.password) ep.password = model.password
  // the /sources field key is authDb; the compare DTO key is auth_db
  if (model.authDb) ep.auth_db = model.authDb
  // the meta form renders tls as a switch (boolean)
  if (model.tls) ep.tls = true
  return ep
}

async function submit() {
  if (!srcDb.value) {
    ElMessage.warning('请填写源端数据库名')
    return
  }
  // 对抗席 F4: a submit while the picker is still loading would silently
  // fall back to the whole-database task (empty entries skip the gate).
  if (collLoading.value) {
    ElMessage.warning('集合列表加载中，请稍候再提交')
    return
  }
  // 对抗席 F3 (FE half): a watermark override pointing at a collection the
  // whitelist excludes can never fire — refuse here instead of a silently
  // lost exception (the backend 400s the same condition as the authority).
  if (wmEnabled.value && collEntries.value.length > 0 && collSelectedNames.value.length > 0 && collSelectedNames.value.length !== pickableColls.value.length) {
    const orphan = Object.keys(collWms.value).filter(t => collWmActive(collWms.value[t]) && !collSelectedNames.value.includes(t))
    if (orphan.length > 0) {
      ElMessage.warning(`例外水位指向未勾选的集合：${orphan.join('、')}（请勾选这些集合或移除对应例外）`)
      return
    }
  }
  const targetDb = tgtSameAsSrc.value ? srcDb.value : tgtDb.value
  if (!targetDb) {
    ElMessage.warning('请填写目标端数据库名')
    return
  }
  // 第9单: watermark validation + informed-consent confirm before the run.
  let watermark: CompareTaskSpec['watermark']
  let tableWatermarks: CompareTaskSpec['table_watermarks']
  if (wmEnabled.value) {
    // Even a pure-override task (global group half-filled) must confirm —
    // but the GLOBAL group only travels when it validates fully; a
    // half-filled global group is ignored by the backend resolver, and the
    // FE mirrors that (send nothing rather than an invalid group).
    if (globalWmActive.value) {
      const verr = validateWmGroup(wmConfig)
      if (verr) { ElMessage.warning(`全局水位：${verr}`); return }
      watermark = wmConfig.range_mode
        ? { column: wmConfig.column.trim(), range_start: wmConfig.range_start.trim(), range_end: wmConfig.range_end.trim() }
        : { column: wmConfig.column.trim(), op: wmConfig.op, value: wmConfig.value.trim() }
    } else if (wmConfig.value.trim() !== '' || wmConfig.range_start.trim() !== '' || wmConfig.range_end.trim() !== '') {
      ElMessage.warning('全局水位组未填完整：请补全单值或完整区间，或清空以仅用例外水位')
      return
    }
    const overrides = Object.entries(collWms.value).filter(([, e]) => collWmActive(e))
    if (!watermark && overrides.length === 0) {
      ElMessage.warning('已开启水位过滤但未配置任何有效水位（全局或至少一条例外）')
      return
    }
    tableWatermarks = overrides.map(([table, e]) => ({
      table,
      ...(e.range_mode
        ? { column: e.column.trim(), range_start: e.range_start.trim(), range_end: e.range_end.trim() }
        : { column: e.column.trim(), op: e.op, value: e.value.trim() }),
    }))
    if (!(await confirmWmBeforeRun())) return
  }
  // coll-select ①: assemble the whitelist ONLY for a partial pick. When
  // every pickable collection is selected the field is omitted — payload
  // byte-identical to the legacy full-database task; when the list was
  // loaded but nothing survives checked, refuse (an accidental empty
  // whitelist would fail loudly server-side anyway — better stopped here).
  let collections: string[] | undefined
  if (collEntries.value.length > 0 && pickableColls.value.length > 0) {
    if (collSelectedNames.value.length === 0) {
      ElMessage.warning('已加载集合列表但未勾选任何集合：请至少勾选一个，或点「全选」比对整库')
      return
    }
    if (collSelectedNames.value.length !== pickableColls.value.length) {
      collections = [...collSelectedNames.value]
    }
  }
  const spec: CompareTaskSpec = {
    source: { ...endpointOf('src'), database: srcDb.value },
    target: { ...endpointOf('tgt'), database: targetDb },
    buckets: adv.buckets,
    parallel_colls: adv.parallel_colls,
    parallel_buckets: adv.parallel_buckets,
    batch_size: adv.batch_size,
    levels: [...levels.value],
    watermark,
    table_watermarks: tableWatermarks,
    collections,
  }
  creating.value = true
  try {
    const { data } = await apiClient.createCompareTask(spec)
    await apiClient.startTask(data.id)
    ElMessage.success('对比任务已创建并启动')
    router.push(`/tasks/${data.id}`)
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '创建对比任务失败')
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <div class="tims-page">
    <PageHeader
      title="新建 MongoDB 对比任务"
      subtitle="配置两端 MongoDB 连接与对比级别，生成对比任务"
    >
      <template #actions>
        <el-button @click="router.push('/tasks')">
          <el-icon style="margin-right: 4px;"><ArrowLeft /></el-icon>返回任务列表
        </el-button>
      </template>
    </PageHeader>

    <el-alert
      type="info"
      :closable="false"
      style="margin-bottom: 20px;"
      title="对比源端与目标端的同一数据库（v1：单库对单库，两端库名可不同）"
      description="任务启动后按集合分桶哈希对比，失配桶自动下钻到字段级差异；结果包含 summary.json 与逐集合差异明细。"
    />

    <el-card v-if="mongoMeta?.implemented" style="margin-bottom: 20px;">
      <template #header>源端（被对比方）</template>
      <el-form label-width="160px">
        <el-form-item label="连接方式">
          <DataSourcePicker v-model="srcRef" :types="['mongodb']" />
        </el-form-item>
        <template v-if="!srcRef">
          <ConnectionForm :meta="mongoMeta" :model="srcModel" />
        </template>
        <el-form-item label="数据库名" required>
          <!-- MC-11: browse+single-select fill, dual-track with manual entry.
               Referenced datasource → datasource_id fold server-side; manual
               fields ride the plain {source, fields} shape. -->
          <DbBrowseSelect
            v-model="srcDb"
            :payload="() => (srcRef ? { datasource_id: srcRef } : { fields: { ...srcModel } })"
          />
        </el-form-item>
        <el-form-item>
          <el-button :loading="testing.src" @click="testSide('src')">
            测试源端连接
          </el-button>
          <el-tag v-if="testState.src === true" type="success" style="margin-left: 8px;">连接成功</el-tag>
          <el-tag v-else-if="testState.src === false" type="danger" style="margin-left: 8px;">连接失败</el-tag>
        </el-form-item>
        <!-- coll-select ①: per-collection picker — default all-selected,
             views listed but disabled (engine skips them by design). -->
        <el-form-item label="集合选择">
          <div style="width: 100%;">
            <el-space style="margin-bottom: 8px;">
              <el-button size="small" :loading="collLoading" :disabled="!srcDb" @click="loadColls">
                加载集合列表
              </el-button>
              <template v-if="collEntries.length > 0">
                <el-button size="small" @click="selectAllColls">全选</el-button>
                <el-button size="small" @click="clearCollSelection">清空</el-button>
                <el-tag type="info" size="small">已选 {{ collSelectedNames.length }} / 可选 {{ pickableColls.length }}（视图 {{ collEntries.length - pickableColls.length }} 不可比）</el-tag>
              </template>
            </el-space>
            <el-table
              v-if="collEntries.length > 0"
              ref="collTable"
              :data="collEntries"
              size="small"
              max-height="240"
              border
              @selection-change="onCollSelectionChange"
            >
              <el-table-column type="selection" width="42" :selectable="(row: CollEntry) => !row.is_view" />
              <el-table-column prop="name" label="集合名" min-width="220" show-overflow-tooltip />
              <el-table-column label="类型" width="90">
                <template #default="{ row }">
                  <el-tag v-if="row.is_view" type="warning" size="small">视图</el-tag>
                  <el-tag v-else type="success" size="small">集合</el-tag>
                </template>
              </el-table-column>
            </el-table>
            <div v-if="collEntries.length > 0" style="margin-top: 6px; font-size: var(--tims-font-xs, 12px); color: var(--tims-text-2, #999);">
              默认全选 = 对比库内全部集合；部分勾选则仅比对所选集合，目标端未勾选的集合不报差异。未加载列表时等同全选整库。
            </div>
          </div>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card v-if="mongoMeta?.implemented" style="margin-bottom: 20px;">
      <template #header>目标端（对比方）</template>
      <el-form label-width="160px">
        <el-form-item label="连接方式">
          <DataSourcePicker v-model="tgtRef" :types="['mongodb']" />
        </el-form-item>
        <template v-if="!tgtRef">
          <ConnectionForm :meta="mongoMeta" :model="tgtModel" />
        </template>
        <el-form-item>
          <el-checkbox v-model="tgtSameAsSrc">目标数据库名与源端相同</el-checkbox>
        </el-form-item>
        <el-form-item v-if="!tgtSameAsSrc" label="目标数据库名" required>
          <!-- MC-11 symmetric browse on the target side (only when the name
               differs from the source). -->
          <DbBrowseSelect
            v-model="tgtDb"
            :payload="() => (tgtRef ? { datasource_id: tgtRef } : { fields: { ...tgtModel } })"
          />
        </el-form-item>
        <el-form-item>
          <el-button :loading="testing.tgt" @click="testSide('tgt')">
            测试目标端连接
          </el-button>
          <el-tag v-if="testState.tgt === true" type="success" style="margin-left: 8px;">连接成功</el-tag>
          <el-tag v-else-if="testState.tgt === false" type="danger" style="margin-left: 8px;">连接失败</el-tag>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card v-if="mongoMeta?.implemented" style="margin-bottom: 20px;">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span>高级选项</span>
          <el-switch v-model="advancedOpen" active-text="展开" />
        </div>
      </template>
      <el-form v-if="advancedOpen" label-width="160px">
        <el-form-item label="分桶数 (buckets)">
          <el-input-number v-model="adv.buckets" :min="1" :max="4096" />
          <span class="adv-hint">_id 范围切分的桶数，默认 64</span>
        </el-form-item>
        <el-form-item label="集合并行 (parallel_colls)">
          <el-input-number v-model="adv.parallel_colls" :min="1" :max="32" />
        </el-form-item>
        <el-form-item label="桶并行 (parallel_buckets)">
          <el-input-number v-model="adv.parallel_buckets" :min="1" :max="32" />
        </el-form-item>
        <el-form-item label="批大小 (batch_size)">
          <el-input-number v-model="adv.batch_size" :min="100" :max="100000" :step="100" />
        </el-form-item>
        <el-form-item label="对比级别 (levels)">
          <el-select v-model="levels" multiple style="width: 100%;">
            <el-option v-for="o in levelOptions" :key="o.value" :label="o.label" :value="o.value" />
          </el-select>
          <span class="adv-hint">留空 = 全部级别；hash 依赖前置级别，建议保持默认</span>
        </el-form-item>
      </el-form>
      <div v-else style="color: #909399; font-size: 12px;"> <!-- #13 说明文字归 xs 档 -->
        使用引擎默认参数（64 桶 / 4+4 并行 / 批 1000 / 全部级别）
      </div>
    </el-card>

    <el-card v-if="mongoMeta?.implemented" style="margin-bottom: 20px;">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span>水位过滤（可选）</span>
          <div style="display: flex; align-items: center; gap: 8px;">
            <el-tag v-if="wmEnabled && globalWmActive" type="success" size="small">全局✓</el-tag>
            <el-tag v-else-if="wmEnabled && activeOverrideCount > 0" type="warning" size="small">仅例外</el-tag>
            <el-switch v-model="wmEnabled" active-text="开启" />
          </div>
        </div>
      </template>
      <el-form v-if="wmEnabled" label-width="160px">
        <el-form-item label="全局水位">
          <div style="width: 100%;">
            <div style="display: flex; gap: 8px; align-items: center; flex-wrap: wrap;">
              <el-select v-model="wmConfig.column" filterable allow-create default-first-option
                         style="width: 240px;">
                <el-option v-for="c in wmColumnOptions" :key="c" :value="c" :label="wmColLabel(c)" />
              </el-select>
              <el-tooltip :disabled="!!srcDb" content="请先填写源端数据库名" placement="top">
                <el-button size="small" :loading="wmLoadingCandidates" :disabled="!srcDb"
                           @click="loadWmCandidates">获取候选水位列</el-button>
              </el-tooltip>
              <el-radio-group v-model="wmKind" size="small">
                <el-radio-button value="objectid">ObjectId</el-radio-button>
                <el-radio-button value="datetime">时间</el-radio-button>
                <el-radio-button value="int">整数</el-radio-button>
              </el-radio-group>
              <el-radio-group v-if="wmRangeEligible" v-model="wmConfig.range_mode" size="small">
                <el-radio-button :value="false">单值</el-radio-button>
                <el-radio-button :value="true">区间</el-radio-button>
              </el-radio-group>
              <template v-if="wmConfig.range_mode">
                <el-date-picker v-if="wmKind === 'datetime'" v-model="wmConfig.range_start" type="datetime"
                                value-format="YYYY-MM-DD HH:mm:ss" placeholder="区间起点 YYYY-MM-DD HH:mm:ss" style="width: 230px;" />
                <el-input v-else v-model="wmConfig.range_start" placeholder="区间起点（不含）" style="width: 200px;" />
                <span style="color: var(--tims-text-2);">~</span>
                <el-date-picker v-if="wmKind === 'datetime'" v-model="wmConfig.range_end" type="datetime"
                                value-format="YYYY-MM-DD HH:mm:ss" placeholder="区间终点（不含）" style="width: 230px;" />
                <el-input v-else v-model="wmConfig.range_end" placeholder="区间终点（不含）" style="width: 200px;" />
              </template>
              <template v-else>
                <el-select v-model="wmConfig.op" style="width: 90px;">
                  <el-option value="<=" label="≤（含边界）" />
                  <el-option value="<" label="<（严格）" />
                </el-select>
                <el-date-picker v-if="wmKind === 'datetime'" v-model="wmConfig.value" type="datetime"
                                value-format="YYYY-MM-DD HH:mm:ss" placeholder="水位值 YYYY-MM-DD HH:mm:ss" style="width: 230px;" />
                <el-input v-else v-model="wmConfig.value"
                          :placeholder="wmKind === 'objectid' ? '24 位十六进制 ObjectId' : '整数水位值'"
                          style="width: 230px;" />
              </template>
            </div>
            <div style="font-size: var(--tims-font-xs); color: var(--tims-text-2); margin-top: 6px;">
              两端按同一窗口过滤后比对，消除「源端持续写入+同步滞后」的永久差异假象；缺字段文档两端一致排除。
              水位前的旧文档改动、两端都已删除的文档检测不到；时间值按服务器本地时区解析；区间为半开 [start, end)，终点不含。
              运行期水位列不可用的集合将自动退化为全量比对并在报告注明（例外集合则直接失败）。
              候选水位列基于索引与前 50 条采样文档推断、非穷尽；未列出的列可直接手输，运行期探针仍是权威。
            </div>
            <div v-if="activeWmCandidate" style="font-size: var(--tims-font-xs); color: var(--tims-text-2); margin-top: 6px;">
              覆盖 {{ activeWmCandidate.matched_tables.length }}/{{ wmSuggestTotal }} 个集合（{{ (activeWmCandidate.coverage * 100).toFixed(0) }}%）
              <span v-if="activeWmCandidate.warnings?.length" style="color: var(--tims-tag-warning-text);">；{{ activeWmCandidate.warnings[0] }}</span>
              <el-tooltip v-if="activeWmCandidate.unmatched_tables?.length" placement="top">
                <template #content>{{ activeWmCandidate.unmatched_tables.join('、') }}</template>
                <span style="color: var(--tims-tag-warning-text); cursor: help;">；缺该列 {{ activeWmCandidate.unmatched_tables.length }} 个集合（悬停查看，运行期将全量比对）</span>
              </el-tooltip>
            </div>
          </div>
        </el-form-item>
        <el-form-item label="按集合例外">
          <div style="width: 100%;">
            <div style="display: flex; align-items: center; gap: 8px; margin-bottom: 8px;">
              <el-button size="small" @click="openCwmDialog">+ 添加例外集合</el-button>
              <span style="font-size: var(--tims-font-xs); color: var(--tims-text-2);">
                例外优先于全局（同一集合仅一条）；例外预检失败 = 该集合失败，不退化
                <span v-if="activeOverrideCount > 0" style="color: var(--tims-tag-warning-text);">当前 {{ activeOverrideCount }} 条</span>
              </span>
            </div>
            <div v-for="(e, name) in collWms" :key="name"
                 style="display: flex; align-items: center; gap: 8px; padding: 2px 0;">
              <el-tag size="small" type="warning">例外</el-tag>
              <span style="min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">{{ name }}：{{ wmCondShort(e) }}</span>
              <el-button size="small" link type="danger" @click="removeCwmEntry(String(name))">删除</el-button>
            </div>
          </div>
        </el-form-item>
      </el-form>
      <div v-else style="color: #909399; font-size: 12px;">
        关闭 = 现行全量五级对比（schema/索引/计数/哈希/下钻），行为不变
      </div>
    </el-card>

    <div style="display: flex; justify-content: flex-end; gap: 12px;">
      <el-button @click="router.push('/tasks')">取消</el-button>
      <el-button type="primary" :loading="creating" @click="submit()">创建并启动对比</el-button>
    </div>

    <!-- 第9单: per-collection watermark exception editor (manual collection
         name — the engine enumerates collections server-side). -->
    <el-dialog v-model="cwmDialog" title="例外水位 — 集合" width="560px">
      <el-form label-width="100px">
        <el-form-item label="集合名" required>
          <el-input v-model="cwmName" placeholder="源库中的集合名（区分大小写）" style="width: 260px;" />
        </el-form-item>
        <el-form-item label="水位列">
          <div style="display: flex; gap: 8px; align-items: center; flex-wrap: wrap;">
            <el-select v-model="cwmForm.column" filterable allow-create default-first-option style="width: 200px;">
              <el-option v-for="c in wmColumnOptions" :key="c" :value="c"
                         :label="c === '_id' ? '_id（默认）' : c" />
            </el-select>
            <el-radio-group v-model="cwmForm.kind" size="small">
              <el-radio-button value="objectid">ObjectId</el-radio-button>
              <el-radio-button value="datetime">时间</el-radio-button>
              <el-radio-button value="int">整数</el-radio-button>
            </el-radio-group>
          </div>
        </el-form-item>
        <el-form-item label="条件">
          <el-radio-group v-if="cwmForm.kind !== 'int'" v-model="cwmForm.range_mode" size="small" style="margin-right: 12px;">
            <el-radio-button :value="false">单值</el-radio-button>
            <el-radio-button :value="true">区间</el-radio-button>
          </el-radio-group>
          <template v-if="cwmForm.range_mode">
            <el-date-picker v-if="cwmForm.kind === 'datetime'" v-model="cwmForm.range_start" type="datetime"
                            value-format="YYYY-MM-DD HH:mm:ss" placeholder="起点 YYYY-MM-DD HH:mm:ss" style="width: 210px;" />
            <el-input v-else v-model="cwmForm.range_start" placeholder="区间起点" style="width: 180px;" />
            <span style="color: var(--tims-text-2); margin: 0 4px;">~</span>
            <el-date-picker v-if="cwmForm.kind === 'datetime'" v-model="cwmForm.range_end" type="datetime"
                            value-format="YYYY-MM-DD HH:mm:ss" placeholder="终点（不含）" style="width: 210px;" />
            <el-input v-else v-model="cwmForm.range_end" placeholder="区间终点（不含）" style="width: 180px;" />
          </template>
          <template v-else>
            <el-select v-model="cwmForm.op" style="width: 90px; margin-right: 8px;">
              <el-option value="<=" label="≤（含边界）" />
              <el-option value="<" label="<（严格）" />
            </el-select>
            <el-date-picker v-if="cwmForm.kind === 'datetime'" v-model="cwmForm.value" type="datetime"
                            value-format="YYYY-MM-DD HH:mm:ss" placeholder="水位值 YYYY-MM-DD HH:mm:ss" style="width: 210px;" />
            <el-input v-else v-model="cwmForm.value" placeholder="水位值" style="width: 180px;" />
          </template>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="cwmDialog = false">取消</el-button>
        <el-button type="primary" @click="saveCwmEntry">保存例外</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.adv-hint {
  margin-left: 12px;
  color: #909399;
  font-size: 12px;
}
</style>
