<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import PageHeader from '../components/PageHeader.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import { useDataSources } from '../composables/useDataSources'

// F-02: the connection is either a saved datasource (postgres, server-side
// credentials) or the legacy inline form. The localStorage memory is retired
// (passwords must not live in the browser).

interface Finding {
  dimension: string
  object_type: string
  object_name: string
  level: string
  pg_detail: string
  tidb_detail: string
  suggestion: string
  auto_fix: boolean
  ddl?: string
  tidb_ddl?: string
}

interface DimResult {
  dimension: string
  total: number
  score: number
  findings: Finding[]
}

interface AssessReport {
  score: number
  level: string
  dimension_results: DimResult[]
  all_findings: Finding[]
  summary: Record<string, number>
}

const loading = ref(false)
const report = ref<AssessReport | null>(null)
const htmlReportUrl = ref('')
const ddlDialogVisible = ref(false)
const ddlDialogTitle = ref('')
const ddlDialogContent = ref('')

const sourceRef = ref('')
const { load: loadDataSources } = useDataSources()
// P1-1: one-shot migration — the retired localStorage key historically stored
// the inline sourceForm including a plaintext password; drop it so the value
// can never be read back.
onMounted(() => {
  localStorage.removeItem('pg2tidb-assess-source')
  loadDataSources()
})

const sourceForm = ref({
  host: '',
  port: 5432,
  user: 'postgres',
  password: '',
  database: '',
  schema: 'public'
})

const testingSource = ref(false)
const sourceTestResult = ref<{ success: boolean; message: string; version?: string } | null>(null)

// 测试连接（S1-UI-04）：ref 模式服务端解析（/test-connection source_ref），
// 手动模式走 {source, fields}，与向导/比对页体验一致。
async function testSourceConn() {
  testingSource.value = true
  sourceTestResult.value = null
  try {
    const body = sourceRef.value
      ? { source_ref: sourceRef.value }
      : { source: 'postgres', fields: { ...sourceForm.value } }
    const resp = await fetch('/api/v1/test-connection', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    })
    const data = await resp.json()
    if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`)
    sourceTestResult.value = data
    if (data.success) ElMessage.success('连接成功')
    else ElMessage.error(`连接失败: ${data.message}`)
  } catch (e: any) {
    sourceTestResult.value = { success: false, message: e.message }
    ElMessage.error(`连接测试失败: ${e.message}`)
  } finally {
    testingSource.value = false
  }
}

const levelEmoji: Record<string, string> = {
  compatible: '●',
  convertible: '▲',
  manual_needed: '◆',
  incompatible: '✕'
}

const levelName: Record<string, string> = {
  compatible: '兼容',
  convertible: '可转换',
  manual_needed: '需手动',
  incompatible: '不兼容'
}

const dimName: Record<string, string> = {
  data_type: '数据类型',
  structure: '表结构',
  index: '索引',
  view: '视图',
  function: '函数',
  trigger: '触发器',
  custom_type: '自定义类型',
  extension: '扩展',
  sequence: '序列'
}

const scoreColor = computed(() => {
  if (!report.value) return '#52c41a'
  const s = report.value.score
  if (s >= 90) return '#52c41a'
  if (s >= 70) return '#faad14'
  if (s >= 40) return '#fa8c16'
  return '#f5222d'
})

const problems = computed(() => {
  if (!report.value) return []
  return report.value.all_findings
    .filter(f => f.level !== 'compatible')
    .sort((a, b) => levelOrder(a.level) - levelOrder(b.level))
})

function levelOrder(level: string): number {
  const order: Record<string, number> = { incompatible: 0, manual_needed: 1, convertible: 2 }
  return order[level] ?? 3
}

function badgeClass(level: string): string {
  const map: Record<string, string> = {
    convertible: 'warning',
    manual_needed: 'warning',
    incompatible: 'danger'
  }
  return map[level] || 'info'
}

async function runAssess() {
  if (!sourceRef.value && (!sourceForm.value.host || !sourceForm.value.database)) {
    ElMessage.warning('请选择数据源或填写主机和数据库名')
    return
  }
  loading.value = true
  report.value = null
  htmlReportUrl.value = ''

  try {
    const body = sourceRef.value
      ? { source_ref: sourceRef.value }
      : { ...sourceForm.value }
    const resp = await fetch('/api/v1/assess', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    })
    if (!resp.ok) {
      const err = await resp.json()
      throw new Error(err.error || `HTTP ${resp.status}`)
    }
    report.value = await resp.json()
    ElMessage.success('评估完成')
  } catch (e: any) {
    ElMessage.error(e.message || '评估失败')
  } finally {
    loading.value = false
  }
}

async function downloadHTML() {
  if (!sourceRef.value && !sourceForm.value.host) return
  loading.value = true

  try {
    const body = sourceRef.value
      ? { source_ref: sourceRef.value, format: 'html' }
      : { ...sourceForm.value, format: 'html' }
    const resp = await fetch('/api/v1/assess', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    })
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
    const html = await resp.text()
    const blob = new Blob([html], { type: 'text/html' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'compatibility-assessment-report.html'
    a.click()
    URL.revokeObjectURL(url)
  } catch (e: any) {
    ElMessage.error(e.message || '下载失败')
  } finally {
    loading.value = false
  }
}

function showDDL(finding: Finding) {
  ddlDialogTitle.value = finding.object_name + ' — DDL'
  const parts: string[] = []
  if (finding.ddl) {
    parts.push('-- PG DDL')
    parts.push(finding.ddl)
  }
  if (finding.tidb_ddl) {
    parts.push('')
    parts.push('-- TiDB 建议DDL')
    parts.push(finding.tidb_ddl)
  }
  ddlDialogContent.value = parts.length > 0 ? parts.join('\n') : '暂无 DDL'
  ddlDialogVisible.value = true
}

function copyDDL() {
  navigator.clipboard.writeText(ddlDialogContent.value)
  ElMessage.success('已复制到剪贴板')
}
</script>

<template>
  <div class="tims-page">
    <PageHeader title="兼容评估" subtitle="扫描 PostgreSQL → TiDB 迁移兼容性风险" />
    <!-- Connection Form -->
    <el-card shadow="never" style="margin-bottom: 20px;">
      <template #header>
        <span style="font-weight: 600;">数据源配置</span>
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
            <el-col :span="12">
              <el-form-item label="Schema">
                <el-input v-model="sourceForm.schema" />
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
          <el-button type="primary" style="margin-left: 12px;" @click="runAssess" :loading="loading">
            {{ loading ? '评估中...' : '开始评估' }}
          </el-button>
        </el-form-item>
      </el-form>
    </el-card>

    <!-- Report -->
    <template v-if="report">
      <!-- Score Card -->
      <el-card shadow="never" style="margin-bottom: 20px;" :body-style="{ padding: '0' }">
        <div :style="{
          background: `linear-gradient(135deg, ${scoreColor}, ${scoreColor}dd)`,
          padding: '32px', textAlign: 'center', color: '#fff', borderRadius: '8px'
        }">
          <div style="font-size: 56px; font-weight: 900; line-height: 1;">{{ report.score.toFixed(1) }}</div>
          <div style="font-size: 16px; margin-top: 8px; opacity: 0.9;">总体兼容性评分</div>
          <div style="margin-top: 10px;">
            <span :style="{
              display: 'inline-block', padding: '4px 16px', borderRadius: '20px',
              fontSize: '14px', background: 'rgba(255,255,255,0.2)'
            }">{{ levelEmoji[report.level] }} {{ levelName[report.level] || report.level }}</span>
          </div>
        </div>
      </el-card>

      <!-- Summary Cards -->
      <el-row :gutter="16" style="margin-bottom: 20px;">
        <el-col :span="6" v-for="(key, idx) in ['compatible', 'convertible', 'manual_needed', 'incompatible']" :key="key">
          <el-card shadow="never" :body-style="{ textAlign: 'center', padding: '20px' }">
            <div :style="{ fontSize: '32px', fontWeight: 700, color: ['#52c41a','#faad14','#fa8c16','#f5222d'][idx] }">
              {{ report.summary[key] || 0 }}
            </div>
            <div style="font-size: 13px; color: #666; margin-top: 4px;">
              {{ ['兼容', '可转换', '需手动', '不兼容'][idx] }}
            </div>
          </el-card>
        </el-col>
      </el-row>

      <!-- Dimension Scores -->
      <el-card shadow="never" style="margin-bottom: 20px;">
        <template #header>
            <span style="font-weight: 600;">维度评分</span>
        </template>
        <el-table :data="report.dimension_results" stripe size="default">
          <el-table-column label="维度" width="140">
            <template #default="{ row }">{{ dimName[row.dimension] || row.dimension }}</template>
          </el-table-column>
          <el-table-column prop="total" label="对象数" width="100" align="center" />
          <el-table-column label="得分" width="220">
            <template #default="{ row }">
              <el-progress
                :percentage="Math.round(row.score)"
                :color="row.score >= 90 ? '#52c41a' : row.score >= 70 ? '#faad14' : row.score >= 40 ? '#fa8c16' : '#f5222d'"
                :stroke-width="10"
              />
            </template>
          </el-table-column>
          <el-table-column label="兼容性" width="120">
            <template #default="{ row }">
              {{ levelEmoji[
                row.score >= 90 ? 'compatible' : row.score >= 70 ? 'convertible' : row.score >= 40 ? 'manual_needed' : 'incompatible'
              ] }}
            </template>
          </el-table-column>
        </el-table>
      </el-card>

      <!-- Problems -->
      <el-card shadow="never" v-if="problems.length > 0">
        <template #header>
          <div style="display: flex; justify-content: space-between; align-items: center;">
            <span style="font-weight: 600;">需要处理的项目（{{ problems.length }} 项）</span>
            <el-button size="small" @click="downloadHTML" :loading="loading">下载 HTML 报告</el-button>
          </div>
        </template>
        <el-table :data="problems" stripe size="small" max-height="500">
          <el-table-column type="index" width="50" />
          <el-table-column prop="object_type" label="类型" width="80" />
          <el-table-column prop="object_name" label="对象" min-width="200" show-overflow-tooltip />
          <el-table-column label="级别" width="110">
            <template #default="{ row }">
              <el-tag :type="badgeClass(row.level)" size="small">
                {{ levelEmoji[row.level] }} {{ levelName[row.level] }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="pg_detail" label="PG" width="140" show-overflow-tooltip />
          <el-table-column prop="tidb_detail" label="TiDB" width="120" show-overflow-tooltip />
          <el-table-column prop="suggestion" label="建议" min-width="250" show-overflow-tooltip />
          <el-table-column label="DDL" width="80" align="center">
            <template #default="{ row }">
              <el-button v-if="row.ddl" link type="primary" size="small" @click="showDDL(row)">查看</el-button>
              <span v-else style="color: #ccc;">-</span>
            </template>
          </el-table-column>
        </el-table>
      </el-card>
    </template>

    <!-- DDL Dialog -->
    <el-dialog v-model="ddlDialogVisible" :title="ddlDialogTitle" width="700px">
      <el-input type="textarea" :model-value="ddlDialogContent" :rows="18" readonly style="font-family: monospace;" />
      <template #footer>
        <el-button @click="copyDDL">复制</el-button>
        <el-button @click="ddlDialogVisible = false">关闭</el-button>
      </template>
    </el-dialog>
  </div>
</template>
