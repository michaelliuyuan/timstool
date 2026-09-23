<script setup lang="ts">
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import PageHeader from '../components/PageHeader.vue'

const STORAGE_KEY = 'pg2tidb-ddlexport-source'

const savedSource = localStorage.getItem(STORAGE_KEY)
const sourceForm = ref(savedSource ? JSON.parse(savedSource) : {
  host: '',
  port: 5432,
  user: 'postgres',
  password: '',
  database: ''
})

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

function saveSourceConfig() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(sourceForm.value))
  ElMessage.success('数据源配置已保存')
}

async function loadSchemas() {
  if (!sourceForm.value.host || !sourceForm.value.database) {
    ElMessage.warning('请填写主机和数据库名')
    return
  }
  schemasLoading.value = true
  try {
    const resp = await fetch('/api/v1/ddl-export/schemas', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(sourceForm.value)
    })
    if (!resp.ok) {
      const err = await resp.json()
      throw new Error(err.error || `HTTP ${resp.status}`)
    }
    const data = await resp.json()
    schemas.value = data.schemas || []
    // D3: default-select public when present
    selectedSchemas.value = schemas.value.includes('public') ? ['public'] : schemas.value.slice(0, 1)
    ElMessage.success(`发现 ${schemas.value.length} 个 schema`)
  } catch (e: any) {
    ElMessage.error(e.message || '获取 schema 失败')
  } finally {
    schemasLoading.value = false
  }
}

async function exportDDL() {
  if (!sourceForm.value.host || !sourceForm.value.database) {
    ElMessage.warning('请填写主机和数据库名')
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
    const resp = await fetch('/api/v1/ddl-export', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        ...sourceForm.value,
        schemas: selectedSchemas.value,
        types,
        tidb: includeTiDB.value
      })
    })
    if (!resp.ok) {
      const err = await resp.json().catch(() => ({ error: `HTTP ${resp.status}` }))
      throw new Error(err.error || `HTTP ${resp.status}`)
    }
    const blob = await resp.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `ddl-export-${new Date().toISOString().slice(0, 10).replace(/-/g, '')}.zip`
    a.click()
    URL.revokeObjectURL(url)
    ElMessage.success('DDL 导出完成')
  } catch (e: any) {
    ElMessage.error(e.message || '导出失败')
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
        <div style="display: flex; justify-content: space-between; align-items: center;">
          <span style="font-weight: 600;">数据源</span>
          <el-button size="small" @click="saveSourceConfig">保存配置</el-button>
        </div>
      </template>
      <el-form :model="sourceForm" label-width="80px" size="default" inline>
        <el-form-item label="主机">
          <el-input v-model="sourceForm.host" placeholder="PG 主机地址" style="width: 160px;" />
        </el-form-item>
        <el-form-item label="端口">
          <el-input-number v-model="sourceForm.port" :min="1" :max="65535" style="width: 120px;" />
        </el-form-item>
        <el-form-item label="用户">
          <el-input v-model="sourceForm.user" style="width: 120px;" />
        </el-form-item>
        <el-form-item label="密码">
          <el-input v-model="sourceForm.password" type="password" show-password style="width: 140px;" />
        </el-form-item>
        <el-form-item label="数据库">
          <el-input v-model="sourceForm.database" placeholder="数据库名" style="width: 140px;" />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" @click="loadSchemas" :loading="schemasLoading">
            {{ schemasLoading ? '连接中...' : '获取 Schema' }}
          </el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card shadow="never" style="margin-bottom: 20px;" v-if="schemas.length > 0">
      <template #header><span style="font-weight: 600;">导出范围</span></template>
      <el-form label-width="80px">
        <el-form-item label="Schema">
          <el-checkbox-group v-model="selectedSchemas">
            <el-checkbox v-for="s in schemas" :key="s" :value="s">{{ s }}</el-checkbox>
          </el-checkbox-group>
        </el-form-item>
        <el-form-item label="对象类型">
          <el-checkbox-group v-model="selectedTypes">
            <el-checkbox v-for="t in typeOptions" :key="t.key" :value="t.key">{{ t.label }}</el-checkbox>
          </el-checkbox-group>
        </el-form-item>
        <el-form-item label="TiDB 版">
          <el-switch v-model="includeTiDB" />
          <span style="margin-left: 12px; font-size: 13px; color: var(--tims-text-2);">
            同时导出 TiDB 转换版 tidb-tables.sql（参考脚本）
          </span>
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
