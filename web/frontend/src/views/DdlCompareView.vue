<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import apiClient from '../api'
import PageHeader from '../components/PageHeader.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import { useDataSources } from '../composables/useDataSources'

// DDL 对比 (ddlcompare): source (postgres/mysql datasource) vs target
// (tidb/mysql datasource) structural compare over eight object classes
// (表/视图/存储过程/自定义函数/触发器/序列/用户/权限, v1.3 口径: 表比主键+
// 字符集+排序规则, 字段级与索引级不比较). The run is synchronous and returns
// the full result; history + report downloads come from the task namespace
// persisted server-side.

interface CategoryResult {
  key: string
  label: string
  source_count: number
  target_count: number
  matched: number
  verdict: 'pass' | 'hint' | 'warn' | 'fail'
  tables?: any[]
  names?: { source_only?: string[]; target_only?: string[]; common?: string[] } | null
  privileges?: {
    comparable: boolean
    cross_dialect?: boolean
    users: { user: string; source?: string[]; target?: string[]; equal: boolean; source_only?: boolean; target_only?: boolean }[]
  } | null
  note?: string
}

interface CompareResult {
  generated_at: string
  duration: string
  categories: CategoryResult[]
  overall: 'pass' | 'hint' | 'warn' | 'fail'
  warnings?: string[]
}

const { load: loadDataSources } = useDataSources()
onMounted(() => {
  loadDataSources()
  loadHistory()
})

// ---- sides ----
const sourceRef = ref('')
const targetRef = ref('')
const sourceSchemas = ref<string[]>([])
const targetSchemas = ref<string[]>([])
const sourceSchema = ref('')
const targetSchema = ref('')
const schemasLoading = ref(false)

async function loadSchemas(side: 'source' | 'target') {
  const ref = side === 'source' ? sourceRef.value : targetRef.value
  if (!ref) {
    ElMessage.warning(side === 'source' ? '请先选择源端数据源' : '请先选择目标端数据源')
    return
  }
  schemasLoading.value = true
  try {
    const { data } = await apiClient.ddlCompareSchemas(side, ref)
    const list = data.schemas || []
    if (side === 'source') {
      sourceSchemas.value = list
      sourceSchema.value = list.includes('public') ? 'public' : list[0] || ''
    } else {
      targetSchemas.value = list
      targetSchema.value = list[0] || ''
    }
    ElMessage.success(`发现 ${list.length} 个 ${side === 'source' ? 'Schema' : '数据库'}`)
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || e.message || '获取失败')
  } finally {
    schemasLoading.value = false
  }
}

// ---- options + run ----
const taskName = ref('')
const ignoreCase = ref(true)
const running = ref(false)
const result = ref<CompareResult | null>(null)
const resultTaskID = ref('')

const canRun = computed(
  () => !!sourceRef.value && !!targetRef.value && !!sourceSchema.value && !!targetSchema.value && !running.value,
)

async function runCompare() {
  if (!canRun.value) {
    ElMessage.warning('请完成两端数据源与 Schema 选择')
    return
  }
  running.value = true
  result.value = null
  try {
    const { data } = await apiClient.ddlCompareRun({
      name: taskName.value || undefined,
      source_ref: sourceRef.value,
      target_ref: targetRef.value,
      source_schema: sourceSchema.value,
      target_schema: targetSchema.value,
      options: { ignore_case: ignoreCase.value },
    })
    resultTaskID.value = data.task_id
    result.value = data.result
    // v1.4 提示级: hint categories (cross-dialect charset/collation/privilege
    // vocab deltas) never fail the run — surface them as an informational
    // suffix on the success toast instead.
    const hintCats = (data.result.categories || []).filter((c: any) => c.verdict === 'hint').length
    if (data.result.overall === 'pass') ElMessage.success(hintCats ? `DDL 对比完成：结构一致（${hintCats} 类跨方言提示）` : 'DDL 对比完成：全部一致')
    else if (data.result.overall === 'warn') ElMessage.warning('DDL 对比完成：存在警告')
    else ElMessage.error('DDL 对比完成：存在差异')
    loadHistory()
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || e.message || '对比失败')
  } finally {
    running.value = false
  }
}

// ---- result rendering helpers ----
const verdictTag = (v: string) => (v === 'pass' ? 'success' : v === 'hint' ? 'info' : v === 'warn' ? 'warning' : 'danger')
const verdictText = (v: string) => (v === 'pass' ? '通过' : v === 'hint' ? '提示' : v === 'warn' ? '警告' : '失败')
const joinNames = (list?: string[]) => (list && list.length ? list.join('、') : '—')
const pkText = (cols?: string[]) => (cols && cols.length ? cols.join(', ') : '无主键')
const attrText = (d?: any) => {
  if (!d) return '—'
  const s = d.source || '(无)'
  const t = d.target || '(无)'
  return `${s} → ${t}`
}

const tableCategories = computed(() =>
  result.value ? result.value.categories.filter((c) => c.key === 'tables') : [],
)
const privilegeCategory = computed(() =>
  result.value ? result.value.categories.find((c) => c.key === 'privileges') : undefined,
)
const nameCategories = computed(() =>
  result.value
    ? result.value.categories.filter((c) => ['views', 'procedures', 'functions', 'triggers', 'sequences', 'users'].includes(c.key))
    : [],
)

function privilegeRows(cat?: CategoryResult) {
  const rows: { user: string; side: string; source: string; target: string; verdict: string; equal: boolean }[] = []
  for (const u of cat?.privileges?.users || []) {
    let verdict = '一致'
    if (u.source_only) verdict = '仅源端'
    else if (u.target_only) verdict = '仅目标端'
    else if (!u.equal) verdict = '不一致'
    let side = ''
    if (u.source_only) side = '仅源端'
    else if (u.target_only) side = '仅目标端'
    rows.push({ user: u.user, side, source: joinNames(u.source), target: joinNames(u.target), verdict, equal: u.equal })
  }
  return rows
}

function downloadReport(format: 'html' | 'json') {
  if (!resultTaskID.value) return
  window.open(apiClient.ddlCompareReportURL(resultTaskID.value, format), '_blank')
}

// ---- history ----
const history = ref<any[]>([])
const historyLoading = ref(false)

async function loadHistory() {
  historyLoading.value = true
  try {
    const { data } = await apiClient.ddlCompareTasks()
    history.value = data.tasks || []
  } catch {
    // history is a convenience face — a failure here must not block the page.
    history.value = []
  } finally {
    historyLoading.value = false
  }
}

function historyReportURL(id: string, format: 'html' | 'json') {
  return apiClient.ddlCompareReportURL(id, format)
}
</script>

<template>
  <div class="tims-page">
    <PageHeader title="DDL 对比" subtitle="源端（MySQL / PostgreSQL）与目标端（TiDB）结构对比：表 / 视图 / 存储过程 / 自定义函数 / 触发器 / 序列 / 用户及权限" />

    <el-card shadow="never" style="margin-bottom: 20px;">
      <template #header><span style="font-weight: 600;">两端选择</span></template>
      <el-row :gutter="24">
        <el-col :span="12">
          <el-form label-width="110px">
            <el-form-item label="源端数据源">
              <DataSourcePicker v-model="sourceRef" :types="['postgres', 'mysql']" />
            </el-form-item>
            <el-form-item label="源端 Schema">
              <el-select v-model="sourceSchema" placeholder="先获取 Schema" style="width: 100%;" data-testid="dc-source-schema">
                <el-option v-for="s in sourceSchemas" :key="s" :label="s" :value="s" />
              </el-select>
            </el-form-item>
            <el-form-item>
              <el-button @click="loadSchemas('source')" :loading="schemasLoading" :disabled="!sourceRef">
                获取 Schema
              </el-button>
            </el-form-item>
          </el-form>
        </el-col>
        <el-col :span="12">
          <el-form label-width="110px">
            <el-form-item label="目标端数据源">
              <DataSourcePicker v-model="targetRef" :types="['tidb', 'mysql']" />
            </el-form-item>
            <el-form-item label="目标端数据库">
              <el-select v-model="targetSchema" placeholder="先获取数据库" style="width: 100%;" data-testid="dc-target-schema">
                <el-option v-for="s in targetSchemas" :key="s" :label="s" :value="s" />
              </el-select>
            </el-form-item>
            <el-form-item>
              <el-button @click="loadSchemas('target')" :loading="schemasLoading" :disabled="!targetRef">
                获取数据库
              </el-button>
            </el-form-item>
          </el-form>
        </el-col>
      </el-row>
    </el-card>

    <el-card shadow="never" style="margin-bottom: 20px;">
      <template #header><span style="font-weight: 600;">对比口径与运行</span></template>
      <el-form label-width="130px">
        <el-form-item label="任务名">
          <el-input v-model="taskName" placeholder="可选，便于历史回看" style="max-width: 360px;" />
        </el-form-item>
        <el-form-item label="忽略大小写">
          <el-switch v-model="ignoreCase" />
          <span class="dc-hint">标识符大小写不敏感（默认开）；v1.3 表对比 = 主键 / 字符集 / 排序规则（字段级与索引级不再比较）</span>
        </el-form-item>
        <el-form-item>
          <el-button type="primary" @click="runCompare" :loading="running" :disabled="!canRun" data-testid="dc-run">
            {{ running ? '对比中...' : '开始对比' }}
          </el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card v-if="result" shadow="never" style="margin-bottom: 20px;">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span style="font-weight: 600;">对比结果（{{ result.duration }}）</span>
          <div>
            <el-button size="small" @click="downloadReport('html')">下载 HTML 报告</el-button>
            <el-button size="small" @click="downloadReport('json')">下载 JSON</el-button>
          </div>
        </div>
      </template>

      <el-alert
        v-for="(wn, i) in result.warnings || []"
        :key="i"
        :title="wn"
        type="warning"
        :closable="false"
        style="margin-bottom: 8px;"
      />

      <div style="margin-bottom: 16px;">
        <el-tag :type="verdictTag(result.overall)" size="large" data-testid="dc-overall">
          总体结论：{{ verdictText(result.overall) }}
        </el-tag>
      </div>

      <el-table :data="result.categories" size="small" style="margin-bottom: 20px;" data-testid="dc-summary">
        <el-table-column prop="label" label="类别" width="120" />
        <el-table-column prop="source_count" label="源端数" width="90" align="right" />
        <el-table-column prop="target_count" label="目标端数" width="100" align="right" />
        <el-table-column prop="matched" label="一致" width="80" align="right" />
        <el-table-column label="结论" width="90">
          <template #default="{ row }">
            <el-tag :type="verdictTag(row.verdict)" size="small">{{ verdictText(row.verdict) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="差异摘要">
          <template #default="{ row }">
            <span v-if="row.key === 'tables'" class="dc-diff">
              仅源 {{ (row.tables || []).filter((t: any) => t.source_only).length }} · 仅目标 {{ (row.tables || []).filter((t: any) => t.target_only).length }} · 结构不一致 {{ (row.tables || []).filter((t: any) => !t.source_only && !t.target_only && t.verdict !== 'pass' && t.verdict !== 'hint').length }} · 提示 {{ (row.tables || []).filter((t: any) => t.verdict === 'hint').length }}
            </span>
            <span v-else-if="row.names" class="dc-diff">
              仅源 {{ (row.names.source_only || []).length }} · 仅目标 {{ (row.names.target_only || []).length }} · 一致 {{ (row.names.common || []).length }}
            </span>
            <span v-else-if="row.privileges" class="dc-diff">
              用户 {{ (row.privileges.users || []).length }} · 不一致 {{ (row.privileges.users || []).filter((u: any) => !u.equal).length }}
            </span>
            <span v-else class="dc-diff">—</span>
          </template>
        </el-table-column>
        <el-table-column label="注解" width="90" v-if="result.categories.some((c) => c.note)">
          <template #default="{ row }">
            <el-tooltip v-if="row.note" :content="row.note" placement="top">
              <el-tag type="warning" size="small" effect="plain">无此对象类型</el-tag>
            </el-tooltip>
            <span v-else>—</span>
          </template>
        </el-table-column>
      </el-table>

      <template v-for="cat in tableCategories" :key="cat.key">
        <h3 class="dc-section">表（主键 / 字符集 / 排序规则）</h3>
        <el-table :data="cat.tables || []" size="small" style="margin-bottom: 20px;">
          <el-table-column prop="table" label="表名" min-width="140" />
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag :type="row.source_only ? 'danger' : row.target_only ? 'warning' : verdictTag(row.verdict)" size="small">
                {{ row.source_only ? '仅源端' : row.target_only ? '仅目标端' : verdictText(row.verdict) }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="主键（源 → 目标）" min-width="160">
            <template #default="{ row }">
              <span v-if="row.pk_diff">{{ pkText(row.pk_diff.source) }} → {{ pkText(row.pk_diff.target) }}</span>
              <span v-else>—</span>
            </template>
          </el-table-column>
          <el-table-column label="字符集（源 → 目标）" min-width="150">
            <template #default="{ row }">
              <span :class="row.charset_diff && !row.charset_diff.equal ? 'dc-amber' : ''">{{ attrText(row.charset_diff) }}</span>
              <div v-if="row.charset_diff && !row.charset_diff.equal && row.charset_diff.note" class="dc-note">{{ row.charset_diff.note }}</div>
            </template>
          </el-table-column>
          <el-table-column label="排序规则（源 → 目标）" min-width="180">
            <template #default="{ row }">
              <span :class="row.collation_diff && !row.collation_diff.equal ? 'dc-amber' : ''">{{ attrText(row.collation_diff) }}</span>
              <div v-if="row.collation_diff && !row.collation_diff.equal && row.collation_diff.note" class="dc-note">{{ row.collation_diff.note }}</div>
            </template>
          </el-table-column>
        </el-table>
      </template>

      <template v-if="privilegeCategory">
        <h3 class="dc-section">权限（每用户全局权限）</h3>
        <el-alert
          v-if="!privilegeCategory.privileges?.comparable && privilegeCategory.note"
          type="warning"
          :title="privilegeCategory.note"
          :description="'MySQL 全局权限（mysql.user 权限列）与 PG 布尔/角色成员词汇表无交集，以下仅按用户全量披露两侧原始权限清单。'"
          :closable="false"
          style="margin-bottom: 10px;"
        />
        <el-table :data="privilegeRows(privilegeCategory)" size="small" style="margin-bottom: 20px;">
          <el-table-column prop="user" label="用户" min-width="140" />
          <el-table-column label="一侧" width="90">
            <template #default="{ row }">
              <el-tag v-if="row.side" :type="row.side === '仅源端' ? 'danger' : 'warning'" size="small">{{ row.side }}</el-tag>
              <span v-else>—</span>
            </template>
          </el-table-column>
          <el-table-column prop="source" label="源端权限" min-width="220">
            <template #default="{ row }"><code class="dc-priv">{{ row.source }}</code></template>
          </el-table-column>
          <el-table-column prop="target" label="目标端权限" min-width="220">
            <template #default="{ row }"><code class="dc-priv">{{ row.target }}</code></template>
          </el-table-column>
          <el-table-column label="结论" width="90">
            <template #default="{ row }">
              <el-tag
                :type="!privilegeCategory.privileges?.comparable ? 'info' : row.equal ? 'success' : 'danger'"
                size="small"
              >
                {{ !privilegeCategory.privileges?.comparable ? '提示（不判）' : row.verdict }}
              </el-tag>
            </template>
          </el-table-column>
        </el-table>
      </template>

      <template v-for="cat in nameCategories" :key="cat.key">
        <h3 class="dc-section">
          {{ cat.label }}（源端 {{ cat.source_count }} · 目标端 {{ cat.target_count }} · 一致 {{ cat.matched }}）
          <el-tag v-if="cat.note" type="warning" size="small" effect="plain" style="margin-left: 8px;">目标端无此对象类型</el-tag>
        </h3>
        <el-alert v-if="cat.note" :title="cat.note" type="warning" :closable="false" style="margin-bottom: 10px;" />
        <el-row :gutter="16" style="margin-bottom: 20px;">
          <el-col :span="8">
            <div class="dc-names dc-names-src">
              <div class="dc-names-title">仅源端（{{ (cat.names?.source_only || []).length }}）</div>
              <div>{{ joinNames(cat.names?.source_only) }}</div>
            </div>
          </el-col>
          <el-col :span="8">
            <div class="dc-names dc-names-dst">
              <div class="dc-names-title">仅目标端（{{ (cat.names?.target_only || []).length }}）</div>
              <div>{{ joinNames(cat.names?.target_only) }}</div>
            </div>
          </el-col>
          <el-col :span="8">
            <div class="dc-names dc-names-both">
              <div class="dc-names-title">两边一致（{{ (cat.names?.common || []).length }}）</div>
              <div>{{ joinNames(cat.names?.common) }}</div>
            </div>
          </el-col>
        </el-row>
      </template>
    </el-card>

    <el-card shadow="never">
      <template #header><span style="font-weight: 600;">历史记录</span></template>
      <el-table :data="history" size="small" v-loading="historyLoading">
        <el-table-column prop="created_at" label="时间" width="180">
          <template #default="{ row }">{{ (row.created_at || '').replace('T', ' ').slice(0, 19) }}</template>
        </el-table-column>
        <el-table-column prop="name" label="任务名" min-width="120">
          <template #default="{ row }">{{ row.name || '—' }}</template>
        </el-table-column>
        <el-table-column label="源端 → 目标" min-width="220">
          <template #default="{ row }">{{ row.source_label }}（{{ row.source_type }}） → {{ row.target_label }}（{{ row.target_type }}）</template>
        </el-table-column>
        <el-table-column prop="status" label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="row.status === 'completed' ? 'success' : 'danger'" size="small">
              {{ row.status === 'completed' ? '完成' : '失败' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="结论" width="90">
          <template #default="{ row }">
            <el-tag v-if="row.overall" :type="verdictTag(row.overall)" size="small">{{ verdictText(row.overall) }}</el-tag>
            <span v-else>—</span>
          </template>
        </el-table-column>
        <el-table-column label="报告" width="150">
          <template #default="{ row }">
            <el-link v-if="row.status === 'completed'" type="primary" :href="historyReportURL(row.id, 'html')" target="_blank">HTML</el-link>
            <el-link v-if="row.status === 'completed'" type="primary" style="margin-left: 8px;" :href="historyReportURL(row.id, 'json')" target="_blank">JSON</el-link>
            <el-tooltip v-else :content="row.error || '失败'" placement="top">
              <span class="dc-red">失败原因</span>
            </el-tooltip>
          </template>
        </el-table-column>
      </el-table>
    </el-card>
  </div>
</template>

<style scoped>
.dc-hint {
  margin-left: 12px;
  font-size: var(--tims-font-sm, 12px);
  color: var(--tims-text-2, #97a0b5);
}
.dc-section {
  font-size: 14px;
  margin: 4px 0 10px 0;
}
.dc-diff {
  font-size: 12px;
  color: var(--tims-text-2, #97a0b5);
}
.dc-red {
  color: #e13c3c;
}
.dc-amber {
  color: #d97e00;
}
.dc-expand {
  font-size: 12px;
  line-height: 1.8;
  padding: 0 12px;
}
.dc-note {
  font-size: 12px;
  line-height: 1.5;
  color: var(--tims-text-2, #97a0b5);
  margin-top: 2px;
  overflow-wrap: anywhere;
}
.dc-names {
  border: 1px solid var(--el-border-color-light, #ebeef5);
  border-radius: 8px;
  padding: 10px 12px;
  font-size: 13px;
  line-height: 1.8;
  min-height: 60px;
  overflow-wrap: anywhere;
}
.dc-names-title {
  font-size: 12px;
  color: var(--tims-text-2, #97a0b5);
  margin-bottom: 4px;
}
.dc-names-src { border-color: #f3c1c1; }
.dc-names-dst { border-color: #f0dfc2; }
.dc-names-both { border-color: #bfe3dd; }
.dc-priv {
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
