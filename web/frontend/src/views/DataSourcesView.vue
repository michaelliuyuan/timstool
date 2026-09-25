<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient from '../api'
import type { DataSource } from '../api'
import PageHeader from '../components/PageHeader.vue'
import { useDataSources } from '../composables/useDataSources'
import { useSourceSchema } from '../composables/useSourceSchema'
import type { FieldSpec } from '../composables/sourceTypes'

// F-02 数据源管理: list + create/edit (schema-driven form) + test + delete.
// Passwords are write-only: create sends it, update keeps it when left blank,
// and no response ever echoes it back (has_password only).

const { datasources, load } = useDataSources()
const { load: loadSources, getSource } = useSourceSchema()

onMounted(async () => {
  await Promise.all([load(true), loadSources()])
})

const typeOptions = [
  { value: 'postgres', label: 'PostgreSQL' },
  { value: 'mysql', label: 'MySQL' },
  { value: 'tidb', label: 'TiDB（目标端）' },
]
const typeLabels: Record<string, string> = { postgres: 'PostgreSQL', mysql: 'MySQL', tidb: 'TiDB' }

// tidb is not a migration *source* adapter, so it has no FieldSpec meta —
// its form is defined here (target-side fields only).
const tidbFields: FieldSpec[] = [
  { key: 'host', label: '主机地址', type: 'text', required: true, default: 'localhost', placeholder: 'localhost', group: 'common' },
  { key: 'port', label: '端口', type: 'number', required: true, default: 4000, group: 'common' },
  { key: 'user', label: '用户名', type: 'text', required: true, default: 'root', group: 'common' },
  { key: 'password', label: '密码', type: 'password', group: 'common' },
  { key: 'database', label: '数据库名', type: 'text', required: true, group: 'common' },
  { key: 'pd_addr', label: 'PD 地址', type: 'text', placeholder: 'host:2379', help: '可选。PD 地址，主要用于 Lightning 物理导入；SQL 端口经代理时务必填真实 PD 端口，留空则导入时按 主机:2379 推断', group: 'common' },
  { key: 'status_port', label: '状态端口', type: 'number', placeholder: '10080', help: '可选。TiDB 状态端口（默认 10080），主要用于 Lightning 导入与连通探测；留空表示未设置', group: 'common' },
]

function fieldsFor(type: string): FieldSpec[] {
  if (type === 'tidb') return tidbFields
  return getSource(type)?.fields || []
}

// ---- dialog state ----
const dialogVisible = ref(false)
const editing = ref<DataSource | null>(null)
const saving = ref(false)
const form = reactive({ name: '', type: 'postgres', fields: {} as Record<string, any> })

function seedFields(type: string) {
  const fresh: Record<string, any> = {}
  for (const f of fieldsFor(type)) {
    fresh[f.key] = f.default !== undefined ? f.default : (f.type === 'number' ? undefined : '')
  }
  Object.keys(form.fields).forEach(k => { delete form.fields[k] })
  Object.assign(form.fields, fresh)
}

function openCreate() {
  editing.value = null
  form.name = ''
  form.type = 'postgres'
  seedFields(form.type)
  dialogVisible.value = true
}

function openEdit(d: DataSource) {
  editing.value = d
  form.name = d.name
  form.type = d.type
  const restored: Record<string, any> = {}
  for (const f of fieldsFor(d.type)) {
    restored[f.key] = d.fields?.[f.key] !== undefined ? d.fields[f.key] : (f.default !== undefined ? f.default : '')
  }
  restored.password = '' // write-only: blank keeps the stored one
  Object.keys(form.fields).forEach(k => { delete form.fields[k] })
  Object.assign(form.fields, restored)
  dialogVisible.value = true
}

function onFormTypeChange(t: string) {
  form.type = t
  seedFields(t)
}

async function save() {
  if (!form.name.trim()) {
    ElMessage.warning('请输入数据源名称')
    return
  }
  if (!form.fields.host) {
    ElMessage.warning('请填写主机地址')
    return
  }
  saving.value = true
  try {
    if (editing.value) {
      await apiClient.updateDataSource(editing.value.id, {
        name: form.name.trim(),
        fields: { ...form.fields },
      })
      ElMessage.success('数据源已更新')
    } else {
      await apiClient.createDataSource({
        name: form.name.trim(),
        type: form.type,
        fields: { ...form.fields },
      })
      ElMessage.success('数据源已创建')
    }
    dialogVisible.value = false
    await load(true)
  } catch (e: any) {
    ElMessage.error(`保存失败: ${e.response?.data?.error || e.message}`)
  } finally {
    saving.value = false
  }
}

// ---- test ----
const testingId = ref('')
const testResults = reactive<Record<string, { success: boolean; message: string; version?: string }>>({})

async function testDS(d: DataSource) {
  testingId.value = d.id
  try {
    const { data } = await apiClient.testDataSource(d.id)
    testResults[d.id] = { success: data.success, message: data.message, version: data.version }
    if (data.success) ElMessage.success(`「${d.name}」连接成功${data.version ? '：' + data.version : ''}`)
    else ElMessage.error(`「${d.name}」连接失败：${data.message}`)
    await load(true)
  } catch (e: any) {
    testResults[d.id] = { success: false, message: e.response?.data?.error || e.message }
    ElMessage.error(`测试失败: ${e.response?.data?.error || e.message}`)
  } finally {
    testingId.value = ''
  }
}

// ---- delete ----
async function removeDS(d: DataSource) {
  try {
    await ElMessageBox.confirm(
      `确认删除数据源「${d.name}」？已创建的迁移/比对任务不受影响（连接在创建时已快照保存）。`,
      '删除数据源',
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await apiClient.deleteDataSource(d.id)
    ElMessage.success('已删除')
    await load(true)
  } catch (e: any) {
    ElMessage.error(`删除失败: ${e.response?.data?.error || e.message}`)
  }
}

function fmtTime(s?: string): string {
  if (!s) return '-'
  const d = new Date(s)
  return isNaN(d.getTime()) ? '-' : d.toLocaleString()
}

const sorted = computed(() => datasources.value.slice().sort((a, b) => a.name.localeCompare(b.name)))
</script>

<template>
  <div class="tims-page">
    <PageHeader title="数据源管理" subtitle="统一保存源端与目标端连接：密码只存服务端，各功能页按引用免密使用" />

    <el-card shadow="never">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span style="font-weight: 600;">已保存数据源（{{ sorted.length }}）</span>
          <el-button type="primary" size="small" @click="openCreate">新建数据源</el-button>
        </div>
      </template>

      <el-empty v-if="sorted.length === 0" description="还没有数据源；新建后，迁移向导 / 数据比对 / 兼容评估 / DDL 导出 / CDC 可直接引用" />

      <el-table v-else :data="sorted" style="width: 100%">
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column label="类型" width="110">
          <template #default="{ row }">
            <el-tag :type="row.type === 'tidb' ? 'warning' : row.type === 'mysql' ? 'info' : 'success'" effect="plain" size="small">
              {{ typeLabels[row.type] || row.type }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="连接" min-width="220">
          <template #default="{ row }">
            <span class="ds-mono">
              {{ row.fields?.host || '-' }}<template v-if="row.fields?.port">:{{ row.fields.port }}</template><template v-if="row.fields?.database">/{{ row.fields.database }}</template>
            </span>
            <el-tag v-if="row.has_password" size="small" type="success" effect="plain" style="margin-left: 8px;">已存密码</el-tag>
            <el-tag v-else size="small" type="info" effect="plain" style="margin-left: 8px;">无密码</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="最近测试" width="200">
          <template #default="{ row }">
            <template v-if="row.last_tested">
              <el-tag :type="row.last_test_ok ? 'success' : 'danger'" size="small" effect="plain">
                {{ row.last_test_ok ? '通过' : '失败' }}
              </el-tag>
              <span class="ds-mono ds-dim">{{ fmtTime(row.last_tested) }}</span>
            </template>
            <span v-else class="ds-dim">未测试</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="200" align="right">
          <template #default="{ row }">
            <el-button size="small" :loading="testingId === row.id" @click="testDS(row)">测试连接</el-button>
            <el-button size="small" @click="openEdit(row)">编辑</el-button>
            <el-button size="small" type="danger" plain @click="removeDS(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div v-for="(r, id) in testResults" :key="id" class="ds-test-result" :class="r.success ? 'ok' : 'bad'">
        {{ r.success ? (r.version || '连接成功') : r.message }}
      </div>
    </el-card>

    <!-- Create / edit dialog -->
    <el-dialog v-model="dialogVisible" :title="editing ? '编辑数据源' : '新建数据源'" width="560px" :close-on-click-modal="false" :close-on-press-escape="false">
      <el-form label-width="100px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="如：生产 PG / 测试 TiDB" />
        </el-form-item>
        <el-form-item label="类型" required>
          <el-select :model-value="form.type" :disabled="!!editing" style="width: 100%" @change="onFormTypeChange">
            <el-option v-for="t in typeOptions" :key="t.value" :value="t.value" :label="t.label" />
          </el-select>
          <div v-if="editing" class="ds-hint">类型创建后不可修改（如需更换请新建）</div>
        </el-form-item>
        <el-form-item v-for="f in fieldsFor(form.type)" :key="f.key" :label="f.label" :required="f.required">
          <el-input-number
            v-if="f.type === 'number'"
            v-model="form.fields[f.key]"
            :min="1"
            :max="65535"
            style="width: 180px"
          />
          <el-input
            v-else
            v-model="form.fields[f.key]"
            :type="f.type === 'password' ? 'password' : 'text'"
            :show-password="f.type === 'password'"
            :placeholder="editing && f.type === 'password' ? '留空保持不变' : f.placeholder"
          />
          <div v-if="f.help" class="ds-hint">{{ f.help }}</div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">{{ editing ? '保存' : '创建' }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.ds-mono {
  font-family: var(--tims-mono, 'JetBrains Mono', Consolas, monospace);
  font-size: 12.5px;
}
.ds-dim { color: var(--tims-text-2, #909399); font-size: 12px; margin-left: 6px; }
.ds-hint { color: var(--tims-text-2, #909399); font-size: 12px; line-height: 1.4; margin-top: 2px; }
.ds-test-result { margin-top: 8px; font-size: 12.5px; }
.ds-test-result.ok { color: var(--tims-teal, #0fa3a3); }
.ds-test-result.bad { color: var(--tims-brand, #e13c3c); }
</style>
