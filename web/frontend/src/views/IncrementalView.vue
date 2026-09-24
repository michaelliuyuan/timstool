<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient from '../api'
import type { IncrementalJob } from '../api'
import PageHeader from '../components/PageHeader.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import { useDataSources } from '../composables/useDataSources'

// F-04 增量同步: pull-based timestamp-watermark jobs. v1 boundaries are
// stated up-front: deletes are never captured, UPDATEs that don't touch the
// watermark column are missed, and boundary rows are re-read every run.

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
  tables: [] as { table: string; watermark_column: string; initial_watermark: string; columns: { name: string; data_type: string; comparable: boolean; indexed: boolean }[] }[],
})

function openCreate() {
  editing.value = null
  form.name = ''
  form.source_ref = ''
  form.target_ref = ''
  form.batch_size = 1000
  form.strict_mode = false
  form.conflict_strategy = 'replace'
  form.tables = []
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
  dialogVisible.value = true
}

function addRow() {
  form.tables.push({ table: '', watermark_column: '', initial_watermark: '', columns: [] })
}
function removeRow(i: number) {
  form.tables.splice(i, 1)
}

// Fetch the column list for a table row (marks comparable types + unindexed).
async function loadColumns(i: number) {
  const row = form.tables[i]
  if (!form.source_ref || !row.table) {
    ElMessage.warning('请先选择源数据源并填写表名')
    return
  }
  try {
    const { data } = await apiClient.getSourceTableColumns(form.source_ref, row.table)
    row.columns = data.columns || []
    if (row.columns.length > 0 && !row.columns.some(c => c.comparable)) {
      ElMessage.warning('该表没有可比类型的水位列（timestamp/date/int/bigint）')
    }
  } catch (e: any) {
    ElMessage.error(`获取列失败: ${e.response?.data?.error || e.message}`)
  }
}

function onColumnPicked(i: number) {
  const row = form.tables[i]
  const col = row.columns.find(c => c.name === row.watermark_column)
  if (col && !col.indexed) {
    ElMessage.warning(`水位列 ${col.name} 上没有索引，同步时可能全表扫描`)
  }
}

async function save() {
  if (!form.name.trim() || !form.source_ref || !form.target_ref) {
    ElMessage.warning('请填写名称并选择源/目标数据源')
    return
  }
  if (form.tables.length === 0) {
    ElMessage.warning('请至少添加一张表')
    return
  }
  for (const t of form.tables) {
    if (!t.table || !t.watermark_column) {
      ElMessage.warning('每张表都需要表名和水位列')
      return
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
      tables: form.tables.map(t => ({ table: t.table, watermark_column: t.watermark_column, initial_watermark: t.initial_watermark || '' })),
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
</script>

<template>
  <div class="tims-page">
    <PageHeader title="增量同步" subtitle="基于时间戳/整数水位列的拉式增量补齐（手动触发，仅 PostgreSQL 源）" />

    <el-alert type="info" :closable="false" style="margin-bottom: 16px;">
      <p>功能边界：① 源端 DELETE 不会被捕获（可用「数据比对」兜底核对）；② 不更新水位列的 UPDATE 会漏同步；③ 默认 ≥ 模式每轮会重读边界时刻的行（保证同秒迟到行不丢，代价极小）。</p>
    </el-alert>

    <el-card shadow="never">
      <template #header>
        <div style="display: flex; justify-content: space-between; align-items: center;">
          <span>同步任务</span>
          <el-button type="primary" @click="openCreate">新建任务</el-button>
        </div>
      </template>
      <el-table :data="jobs" v-loading="loading" empty-text="暂无增量同步任务">
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

    <el-dialog v-model="dialogVisible" :title="editing ? '编辑增量同步任务' : '新建增量同步任务'" width="860px">
      <el-form label-width="120px">
        <el-form-item label="任务名称">
          <el-input v-model="form.name" placeholder="例如：订单表增量补齐" style="width: 360px;" />
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
            <div v-if="form.conflict_strategy === 'error'" style="margin-top: 4px; font-size: 13px; color: var(--el-color-warning); line-height: 1.6;">
              ⚠️ 批次之间无事务：同步失败或中途崩溃后重跑，该策略会撞到已写入行的重复键错误且无自愈——重跑前建议改用 REPLACE/IGNORE。
            </div>
          </div>
        </el-form-item>
        <el-form-item label="批大小">
          <el-input-number v-model="form.batch_size" :min="1" :max="100000" :step="500" controls-position="right" />
        </el-form-item>
        <el-form-item label="严格模式">
          <el-switch v-model="form.strict_mode" />
          <span style="margin-left: 12px; font-size: 13px; color: var(--tims-text-2);">使用 &gt; 代替 ≥（跳过边界重读，但同秒迟到行可能丢失）</span>
        </el-form-item>
        <el-form-item label="表与水位列">
          <div style="width: 100%;">
            <div v-for="(t, i) in form.tables" :key="i" class="inc-table-row">
              <el-input v-model="t.table" placeholder="表名" style="width: 180px;" @change="t.columns = []; t.watermark_column = ''" />
              <el-button size="small" style="margin: 0 4px 0 8px;" @click="loadColumns(i)">获取列</el-button>
              <el-select v-if="t.columns.length" v-model="t.watermark_column" placeholder="水位列" style="width: 260px;" @change="onColumnPicked(i)">
                <el-option v-for="c in t.columns" :key="c.name" :value="c.name" :label="`${c.name}（${c.data_type}${c.indexed ? '' : ' · 无索引'}）`" :disabled="!c.comparable" />
              </el-select>
              <el-input v-else v-model="t.watermark_column" placeholder="水位列（如 update_time）" style="width: 260px;" />
              <el-input v-model="t.initial_watermark" placeholder="初始水位，留空=从最小值全量补齐" style="flex: 1; margin-left: 8px;" />
              <el-button type="danger" text style="margin-left: 8px;" @click="removeRow(i)">删除</el-button>
            </div>
            <el-button size="small" style="margin-top: 8px;" @click="addRow">+ 添加表</el-button>
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
  margin-bottom: 8px;
  width: 100%;
}
</style>
