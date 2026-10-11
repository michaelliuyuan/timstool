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

function onRowCommand(command: string, row: DataSource) {
  if (command === 'edit') openEdit(row)
  else if (command === 'remove') removeDS(row)
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
          <el-button type="primary" @click="openCreate">新建数据源</el-button>
        </div>
      </template>

      <el-empty v-if="sorted.length === 0" description="还没有数据源；新建后，迁移向导 / 数据比对 / 兼容评估 / DDL 导出 / CDC 可直接引用" />

      <!-- MS-11i pen3: card grid replaces the flat table (te survey #0) —
           same fields, same handlers (test/edit/remove), responsive columns. -->
      <div v-else class="ds-grid">
        <div v-for="d in sorted" :key="d.id" class="ds-card">
          <div class="ds-card-head">
            <el-tag :type="d.type === 'tidb' ? 'warning' : d.type === 'mysql' ? 'info' : 'success'" effect="plain" size="small">
              {{ typeLabels[d.type] || d.type }}
            </el-tag>
            <span class="ds-card-name" :title="d.name">{{ d.name }}</span>
          </div>
          <div class="ds-card-conn ds-mono">
            {{ d.fields?.host || '-' }}<template v-if="d.fields?.port">:{{ d.fields.port }}</template><template v-if="d.fields?.database">/{{ d.fields.database }}</template>
          </div>
          <div class="ds-card-meta">
            <el-tag v-if="d.has_password" size="small" type="success" effect="plain">已存密码</el-tag>
            <el-tag v-else size="small" type="info" effect="plain">无密码</el-tag>
            <template v-if="d.last_tested">
              <el-tag :type="d.last_test_ok ? 'success' : 'danger'" size="small" effect="plain">
                {{ d.last_test_ok ? '通过' : '失败' }}
              </el-tag>
              <span class="ds-dim ds-mono" :title="fmtTime(d.last_tested)">{{ fmtTime(d.last_tested) }}</span>
            </template>
            <span v-else class="ds-dim">未测试</span>
          </div>
          <div class="ds-card-actions">
            <el-button size="small" :loading="testingId === d.id" @click="testDS(d)">测试连接</el-button>
            <el-dropdown trigger="click" @command="(command: string) => onRowCommand(command, d)">
              <el-button size="small" link>更多<el-icon class="el-icon--right"><ArrowDown /></el-icon></el-button>
              <template #dropdown>
                <el-dropdown-menu>
                  <el-dropdown-item command="edit">编辑</el-dropdown-item>
                  <el-dropdown-item command="remove" divided class="row-del">删除</el-dropdown-item>
                </el-dropdown-menu>
              </template>
            </el-dropdown>
          </div>
        </div>
      </div>

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
  font-size: var(--tims-font-xs);
}
.ds-dim { color: var(--tims-text-2, #6b7280); font-size: var(--tims-font-xs); margin-left: 6px; }
.ds-hint { color: var(--tims-text-2, #6b7280); font-size: var(--tims-font-xs); line-height: 1.4; margin-top: 2px; }
.ds-test-result { margin-top: 8px; font-size: var(--tims-font-xs); }
.ds-test-result.ok { color: var(--tims-tag-success-text, #0b7a7a); }
.ds-test-result.bad { color: var(--tims-tag-danger-text, #c02f2f); }

.row-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  flex-wrap: nowrap;
  gap: 8px;
}

.row-actions .el-button + .el-button {
  margin-left: 0;
}

.row-del {
  color: var(--el-color-danger);
}

.el-dropdown-menu__item.row-del:hover,
.el-dropdown-menu__item.row-del:focus {
  color: var(--el-color-danger);
}

/* MS-11i pen3: responsive card grid */
.ds-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 14px;
}
.ds-card {
  border: 1px solid var(--tims-border, #e6e8f0);
  border-radius: var(--tims-radius, 12px);
  background: var(--tims-card, #fff);
  box-shadow: var(--tims-shadow, none);
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.ds-card-head {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.ds-card-name {
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ds-card-conn {
  color: var(--tims-text, #2a3040);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ds-card-meta {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  min-height: 24px;
}
.ds-card-meta .ds-dim { margin-left: 0; }
.ds-card-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: auto;
  padding-top: 6px;
  border-top: 1px dashed var(--tims-border, #e6e8f0);
}
</style>
