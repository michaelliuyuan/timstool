<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import apiClient from '../api'
import PageHeader from '../components/PageHeader.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import { useDataSources } from '../composables/useDataSources'

// F-02: connection comes from a saved postgres datasource (server-side
// credentials) or the legacy inline form. The localStorage memory is retired —
// passwords must not persist in the browser.

const sourceRef = ref('')
const { load: loadDataSources } = useDataSources()
// P1-1: one-shot migration — the retired localStorage key historically stored
// the inline sourceForm including a plaintext password; drop it so the value
// can never be read back.
onMounted(() => {
  localStorage.removeItem('pg2tidb-ddlexport-source')
  loadDataSources()
})

const sourceForm = ref({
  host: '',
  port: 5432,
  user: 'postgres',
  password: '',
  database: ''
})

const testingSource = ref(false)
const sourceTestResult = ref<{ success: boolean; message: string; version?: string } | null>(null)

// 测试连接（S1-UI-04）：ref 模式服务端解析（/test-connection source_ref），
// 手动模式走 {source, fields}，与向导/比对页体验一致。
async function testSourceConn() {
  testingSource.value = true
  sourceTestResult.value = null
  try {
    // S1-UI-08: unified apiClient — axios rejects non-2xx implicitly.
    const { data } = sourceRef.value
      ? await apiClient.testSourceConnectionRef(sourceRef.value)
      : await apiClient.testSourceConnection('postgres', { ...sourceForm.value })
    sourceTestResult.value = data
    if (data.success) ElMessage.success('连接成功')
    else ElMessage.error(`连接失败: ${data.message}`)
  } catch (e: any) {
    const msg = e.response?.data?.error || e.message
    sourceTestResult.value = { success: false, message: msg }
    ElMessage.error(`连接测试失败: ${msg}`)
  } finally {
    testingSource.value = false
  }
}

const schemas = ref<string[]>([])
const selectedSchemas = ref<string[]>([])
const schemasLoading = ref(false)
const exporting = ref(false)

const typeOptions = [
  { key: 'tables', label: '表（含主键/唯一约束/注释）' },
  { key: 'indexes', label: '二级索引' },
  { key: 'views', label: '视图' },
  { key: 'sequences', label: '序列' },
  { key: 'functions', label: '函数' },
  { key: 'procedures', label: '存储过程' },
  { key: 'triggers', label: '触发器' },
  { key: 'types', label: '自定义类型（枚举/复合）' }
]
const selectedTypes = ref<string[]>(typeOptions.map(t => t.key))
const includeTiDB = ref(false)

async function loadSchemas() {
  if (!sourceRef.value && (!sourceForm.value.host || !sourceForm.value.database)) {
    ElMessage.warning('请选择数据源或填写主机和数据库名')
    return
  }
  schemasLoading.value = true
  try {
    const body = sourceRef.value ? { source_ref: sourceRef.value } : { ...sourceForm.value }
    const { data } = await apiClient.ddlExportSchemas(body)
    schemas.value = data.schemas || []
    // D3: default-select public when present
    selectedSchemas.value = schemas.value.includes('public') ? ['public'] : schemas.value.slice(0, 1)
    ElMessage.success(`发现 ${schemas.value.length} 个 schema`)
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || e.message || '获取 schema 失败')
  } finally {
    schemasLoading.value = false
  }
}

async function exportDDL() {
  if (!sourceRef.value && (!sourceForm.value.host || !sourceForm.value.database)) {
    ElMessage.warning('请选择数据源或填写主机和数据库名')
    return
  }
  if (selectedSchemas.value.length === 0) {
    ElMessage.warning('请至少勾选一个 schema')
    return
  }
  if (selectedTypes.value.length === 0) {
    ElMessage.warning('请至少勾选一种对象类型')
    return
  }
  exporting.value = true
  try {
    const types: Record<string, boolean> = {}
    for (const opt of typeOptions) types[opt.key] = selectedTypes.value.includes(opt.key)
    // S1-UI-08: apiClient (blob + long timeout) — the export walks every
    // selected object server-side and must not hit the global 30s cutoff.
    const { data, headers } = await apiClient.ddlExport({
      ...(sourceRef.value ? { source_ref: sourceRef.value } : { ...sourceForm.value }),
      schemas: selectedSchemas.value,
      types,
      tidb: includeTiDB.value,
    })
    const skipped = parseInt(headers['x-tims-skipped'] || '0', 10)
    const blob = data as Blob
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `ddl-export-${new Date().toISOString().slice(0, 10).replace(/-/g, '')}.zip`
    a.click()
    URL.revokeObjectURL(url)
    if (skipped > 0) {
      ElMessage.warning(`${skipped} 项对象被跳过，详见 zip 内 manifest.json`)
    } else {
      ElMessage.success('DDL 导出完成')
    }
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || e.message || '导出失败')
  } finally {
    exporting.value = false
  }
}
</script>

<template>
  <div class="tims-page">
    <PageHeader title="DDL 导出" subtitle="一键导出源端 PostgreSQL 各类对象的 DDL（按 schema 分文件夹打包）" />

    <el-card shadow="never" style="margin-bottom: 20px;">
      <template #header>
        <span style="font-weight: 600;">数据源</span>
      </template>
      <el-form :model="sourceForm" label-width="120px" size="default">
        <el-form-item label="数据源">
          <DataSourcePicker v-model="sourceRef" :types="['postgres']" />
        </el-form-item>
        <template v-if="!sourceRef">
          <el-divider content-position="left">源数据库（PostgreSQL）</el-divider>
          <el-row :gutter="24">
            <el-col :span="12">
              <el-form-item label="主机">
                <el-input v-model="sourceForm.host" placeholder="PG 主机地址" />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="端口">
                <el-input-number v-model="sourceForm.port" :min="1" :max="65535" controls-position="right" style="width: 100%;" />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="用户">
                <el-input v-model="sourceForm.user" />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="密码">
                <el-input v-model="sourceForm.password" type="password" show-password />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="数据库">
                <el-input v-model="sourceForm.database" placeholder="数据库名" />
              </el-form-item>
            </el-col>
          </el-row>
        </template>
        <el-form-item>
          <el-button @click="testSourceConn" :loading="testingSource" :disabled="!sourceRef && !sourceForm.host">
            测试连接
          </el-button>
          <el-tag
            v-if="sourceTestResult"
            :type="sourceTestResult.success ? 'success' : 'danger'"
            style="margin-left: 12px;"
          >
            {{ sourceTestResult.success ? (sourceTestResult.version ? `连接成功（${sourceTestResult.version}）` : '连接成功') : sourceTestResult.message }}
          </el-tag>
          <el-button type="primary" style="margin-left: 12px;" @click="loadSchemas" :loading="schemasLoading">
            {{ schemasLoading ? '连接中...' : '获取 Schema' }}
          </el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card shadow="never" style="margin-bottom: 20px;" v-if="schemas.length > 0">
      <template #header><span style="font-weight: 600;">导出范围</span></template>
      <el-form label-width="120px">
        <el-row :gutter="24" style="margin-bottom: 8px;">
          <el-col :span="8">
            <div class="pick-panel">
              <div class="pick-panel-title">Schema</div>
              <el-checkbox-group v-model="selectedSchemas">
                <el-checkbox v-for="s in schemas" :key="s" :value="s">{{ s }}</el-checkbox>
              </el-checkbox-group>
            </div>
          </el-col>
          <el-col :span="16">
            <div class="pick-panel">
              <div class="pick-panel-title">对象类型</div>
              <el-checkbox-group v-model="selectedTypes">
                <el-checkbox v-for="t in typeOptions" :key="t.key" :value="t.key">{{ t.label }}</el-checkbox>
              </el-checkbox-group>
            </div>
          </el-col>
        </el-row>
        <el-form-item label="TiDB 版">
          <el-switch v-model="includeTiDB" />
          <span style="margin-left: 12px; font-size: 13px; color: var(--tims-text-2);">
            同时导出 TiDB 转换版 tidb-tables.sql（参考脚本）
          </span>
          <el-text v-if="includeTiDB && !selectedTypes.includes('tables')" type="warning" size="small" style="display: block; margin-top: 4px;">
            未勾选「表」类型，TiDB 转换版不会导出
          </el-text>
        </el-form-item>
        <el-form-item>
          <el-button type="primary" @click="exportDDL" :loading="exporting">
            {{ exporting ? '导出中...' : '导出并下载 ZIP' }}
          </el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card shadow="never" v-else>
      <template #header><span style="font-weight: 600;">说明</span></template>
      <div style="font-size: 14px; color: var(--tims-text-2); line-height: 2;">
        填写源端连接信息并点击「获取 Schema」后，勾选要导出的 schema 与对象类型。<br />
        导出结构：每个 schema 一个文件夹，每种对象一个 .sql 文件（tables.sql / indexes.sql / views.sql /
        sequences.sql / functions.sql / procedures.sql / triggers.sql / types.sql），ZIP 内含
        manifest.json（对象计数与跳过记录）与 README.txt（建议应用顺序）。<br />
        CLI 等价命令：<span class="tims-mono">timstool export-ddl -c config.yaml -o ./ddl [--schemas a,b] [--types tables,views] [--tidb]</span>
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.pick-panel {
  border: 1px solid var(--el-border-color-light, #ebeef5);
  border-radius: 8px;
  padding: 12px 16px;
  min-height: 100px;
}
.pick-panel-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--tims-text-1, #303133);
  margin-bottom: 8px;
}
.pick-panel :deep(.el-checkbox) {
  margin-right: 16px;
}
</style>
