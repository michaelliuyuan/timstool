<template>
  <div class="cdc-container tims-page">
    <PageHeader title="CDC 实时同步" subtitle="基于数据库日志的推式实时管道，自动捕获 INSERT/UPDATE/DELETE，常驻运行" />

    <!-- S1-UI-06: which-one-to-use card (shared with the watermark backfill page) -->
    <SyncCompareCard current="cdc" />

    <!-- Module disabled (cdc.enable=false) -->
    <!-- P1 巡检修复 #7：div → el-card，白底/边框/圆角由 el-card 皮肤提供 -->
    <el-card class="disabled-card" v-if="disabled">
      <h3>CDC 模块未启用</h3>
      <p>当前部署未开启 CDC 实时同步（<code>cdc.enable: false</code>）。</p>
      <p class="hint">如需使用：在 config.yaml 设置 <code>cdc.enable: true</code>，或用 <code>pg2tidb cdc --enable-cdc</code> 启动。</p>
    </el-card>

    <template v-else>
    <!-- Signature pipeline strip -->
    <DataPipelineStrip
      :status="pipelineStatus"
      :badges="pipelineBadges"
    />

    <!-- Connection card (A1): the live config.yaml the CDC child uses -->
    <!-- P1 巡检修复 #7：div → el-card（下同） -->
    <el-card class="detail-card" v-if="connCfg">
      <h3>连接信息（CDC 实际使用）</h3>
      <div class="detail-row">
        <span class="detail-label">配置文件:</span>
        <code>{{ connCfg.cfg_file }}</code>
      </div>
      <div class="detail-row">
        <span class="detail-label">源端:</span>
        <code>{{ connCfg.source.host }}:{{ connCfg.source.port }}/{{ connCfg.source.database }} · {{ connCfg.source.user }}<span v-if="!connCfg.has_password" style="color:#cf1322;">（未设密码）</span></code>
      </div>
      <div class="detail-row">
        <span class="detail-label">目标端:</span>
        <code>{{ connCfg.target.host }}:{{ connCfg.target.port }}/{{ connCfg.target.database }} · {{ connCfg.target.user }}</code>
      </div>
      <div class="detail-row">
        <span class="detail-label">CDC 参数:</span>
        <code>{{ connCfg.cdc.mode }} · slot={{ connCfg.cdc.slot_name }} · pub={{ connCfg.cdc.publication }} · parallel={{ connCfg.cdc.parallel }} · 冲突={{ connCfg.cdc.conflict_strategy }} · DDL={{ connCfg.cdc.sync_ddl ? '同步' : '不同步' }}</code>
      </div>
      <div class="control-actions" style="margin-top: 10px;">
        <el-button v-if="!editingConn" @click="startEditConn">编辑连接</el-button>
        <el-button @click="importConn" :disabled="busy || isActive || editingConn">从最近迁移任务导入</el-button>
      </div>
      <!-- F-02 D4: one-click import from saved datasources -->
      <div class="control-actions ds-import" v-if="!editingConn">
        <span class="ds-import-label">从数据源导入：</span>
        <el-select v-model="dsSourceRef" class="ds-import-select" placeholder="源端（PG 数据源）" size="small">
          <el-option label="源端（PG 数据源）" value="" />
          <el-option v-for="d in dsByType(['postgres'])" :key="d.id" :value="d.id" :label="d.name" />
        </el-select>
        <span class="ds-import-arrow">→</span>
        <el-select v-model="dsTargetRef" class="ds-import-select" placeholder="目标端（TiDB 数据源）" size="small">
          <el-option label="目标端（TiDB 数据源）" value="" />
          <el-option v-for="d in dsByType(['tidb'])" :key="d.id" :value="d.id" :label="d.name" />
        </el-select>
        <el-button size="small" @click="importFromDS" :disabled="busy || isActive || !dsSourceRef || !dsTargetRef || importingDS" :loading="importingDS">
          {{ importingDS ? '导入中…' : '导入' }}
        </el-button>
      </div>
      <!-- Inline edit form (A1 manual edit) -->
      <div v-if="editingConn" class="conn-edit">
        <div class="conn-edit-section">源端（PostgreSQL）</div>
        <div class="conn-grid">
          <label>主机<el-input v-model="editForm.source.host" size="small" /></label>
          <label>端口<el-input-number v-model="editForm.source.port" :min="1" :max="65535" controls-position="right" size="small" class="conn-grid-num" /></label>
          <label>用户名<el-input v-model="editForm.source.user" size="small" /></label>
          <label>密码<el-input v-model="editForm.source.password" type="password" show-password placeholder="留空保持不变" size="small" /></label>
          <label>数据库<el-input v-model="editForm.source.database" size="small" /></label>
          <label>Schema<el-input v-model="editForm.source.schema" size="small" /></label>
        </div>
        <div class="conn-edit-section">目标端（TiDB）</div>
        <div class="conn-grid">
          <label>主机<el-input v-model="editForm.target.host" size="small" /></label>
          <label>端口<el-input-number v-model="editForm.target.port" :min="1" :max="65535" controls-position="right" size="small" class="conn-grid-num" /></label>
          <label>用户名<el-input v-model="editForm.target.user" size="small" /></label>
          <label>密码<el-input v-model="editForm.target.password" type="password" show-password placeholder="留空保持不变" size="small" /></label>
          <label>数据库<el-input v-model="editForm.target.database" size="small" /></label>
        </div>
        <div class="control-actions" style="margin-top: 8px;">
          <el-button type="success" @click="saveConn" :disabled="savingConn" :loading="savingConn">{{ savingConn ? '保存中…' : '保存' }}</el-button>
          <el-button type="danger" plain @click="editingConn = false">取消</el-button>
        </div>
        <div v-if="connMsg" class="control-msg" :class="{ error: connError }">{{ connMsg }}</div>
      </div>
    </el-card>

    <!-- Precheck panel (A2): run before Start; fail items block -->
    <el-card class="detail-card" v-if="precheck">
      <h3>启动预检 <el-button size="small" style="margin-left: 8px;" @click="runPrecheck" :disabled="checking" :loading="checking">{{ checking ? '检查中…' : '重新检查' }}</el-button></h3>
      <div v-for="it in precheck.items" :key="it.item" class="precheck-row" :class="it.level">
        <span class="precheck-dot">{{ it.level === 'ok' ? '✓' : it.level === 'warn' ? '!' : '✗' }}</span>
        <span class="precheck-label">{{ it.label }}</span>
        <span class="precheck-detail">{{ it.detail }}</span>
        <el-button v-if="it.item === 'no_pk_tables' && it.level === 'warn' && noPKTables.length"
          size="small" style="margin-left: auto; flex: none;"
          @click="openNoPKFix">修复（REPLICA IDENTITY FULL）</el-button>
      </div>
      <div class="resume-box">
        <strong>断点/起点：</strong>{{ precheck.conclusion }}
        <span v-if="precheck.checkpoint.exists"> · checkpoint 文件 {{ precheck.checkpoint.file }}（更新于 {{ precheck.checkpoint.updated_at ? new Date(precheck.checkpoint.updated_at).toLocaleString() : '-' }}）</span>
      </div>
      <div class="control-actions" style="margin-top: 8px;" v-if="precheck.checkpoint.exists && !isActive">
        <el-button type="danger" plain size="small" @click="resetCheckpoint">重置断点（危险）</el-button>
      </div>
    </el-card>

    <!-- Control panel: one-click start/stop (#t55) -->
    <!-- P1 巡检修复 #7：div → el-card -->
    <el-card class="control-card">
      <div class="control-head">
        <span class="control-badge" :class="controlState">{{ controlLabel }}</span>
        <span v-if="control && control.restarts > 0" class="control-restarts">自动重启 {{ control.restarts }} 次</span>
      </div>
      <div class="control-actions">
        <el-button type="success" :disabled="busy || isActive || !precheckPassed" @click="startCDC" :title="precheckPassed ? '' : '预检未通过或有未完成的预检项，请先点击预检面板重新检查'">{{ startLabel }}</el-button>
        <el-button type="danger" :disabled="busy || !canStop" @click="confirmStopCDC">停止 CDC</el-button>
      </div>
      <div v-if="controlMsg" class="control-msg" :class="{ error: controlError }">{{ controlMsg }}</div>
    </el-card>

    <!-- Status Card -->
    <div class="status-card" :class="cardState">
      <div class="status-indicator">
        <span class="status-dot" :class="{ active: status.running || inStartup }"></span>
        <span class="status-text">{{ cardLabel }}</span>
      </div>
      <div class="status-meta">
        <span v-if="status.running && status.lsn">LSN: {{ status.lsn }}</span>
        <span v-else-if="inStartup">CDC 启动中…（control 通道确认运行，等待 CDC 连接 PG/TiDB 写首条状态，约 8-90s）</span>
        <span v-else>{{ status.message || 'CDC 未运行，点上方「启动 CDC」开始' }}</span>
      </div>
      <div class="status-meta" v-if="status.fatal_error" style="color: #fff; opacity: 0.95;">
        ⚠️ {{ status.fatal_error }}
      </div>
    </div>

    <!-- Stats Grid -->
    <div class="stats-grid" v-if="stats">
      <div class="stat-item">
        <div class="stat-value">{{ formatNumber(stats.source_events) }}</div>
        <div class="stat-label">源端事件</div>
      </div>
      <div class="stat-item">
        <div class="stat-value">{{ formatNumber(stats.applied) }}</div>
        <div class="stat-label">已应用</div>
      </div>
      <div class="stat-item">
        <div class="stat-value" :class="{ error: stats.failed > 0 }">{{ formatNumber(stats.failed) }}</div>
        <div class="stat-label">失败</div>
      </div>
      <div class="stat-item">
        <div class="stat-value">{{ formatNumber(stats.skipped) }}</div>
        <div class="stat-label">已跳过</div>
      </div>
      <div class="stat-item">
        <div class="stat-value">{{ stats.throughput_rps?.toFixed(1) || '0' }}/s</div>
        <div class="stat-label">吞吐量</div>
        <SparkLine v-if="throughputHistory.length > 1" :data="throughputHistory" :height="34" />
      </div>
      <div class="stat-item">
        <div class="stat-value">{{ stats.lag_seconds?.toFixed(1) || '0' }}s</div>
        <div class="stat-label">延迟(秒)</div>
      </div>
      <div class="stat-item">
        <div class="stat-value">{{ formatUptime(stats.uptime_seconds) }}</div>
        <div class="stat-label">运行时间</div>
      </div>
      <div class="stat-item">
        <div class="stat-value">{{ formatNumber(stats.batches) }}</div>
        <div class="stat-label">批次数</div>
      </div>
    </div>

    <!-- Checkpoint Card -->
    <el-card class="detail-card" v-if="checkpoint && checkpoint.lsn">
      <h3>检查点</h3>
      <div class="detail-row">
        <span class="detail-label">LSN:</span>
        <code>{{ checkpoint.lsn }}</code>
      </div>
      <div class="detail-row" v-if="status.slot">
        <span class="detail-label">Slot:</span>
        <code>{{ status.slot }}</code>
      </div>
      <div class="detail-row">
        <span class="detail-label">更新时间:</span>
        <span>{{ checkpoint.updated_at ? new Date(checkpoint.updated_at).toLocaleString() : '-' }}</span>
      </div>
    </el-card>

    <!-- Config Card (from status: slot/publication/pid) -->
    <el-card class="detail-card" v-if="status.slot || status.publication">
      <h3>配置</h3>
      <div class="detail-row" v-if="status.slot">
        <span class="detail-label">Slot:</span>
        <code>{{ status.slot }}</code>
      </div>
      <div class="detail-row" v-if="status.publication">
        <span class="detail-label">Publication:</span>
        <code>{{ status.publication }}</code>
      </div>
      <div class="detail-row" v-if="status.pid">
        <span class="detail-label">PID:</span>
        <code>{{ status.pid }}</code>
      </div>
      <div class="detail-row" v-if="status.uptime_seconds">
        <span class="detail-label">运行时长:</span>
        <span>{{ formatUptime(status.uptime_seconds) }}</span>
      </div>
    </el-card>

    <!-- Live slot / lag card (A3): restart_lsn + retained WAL + checkpoint age -->
    <el-card class="detail-card" v-if="slotView && slotView.slot.exists">
      <h3>Slot 与延迟</h3>
      <div class="detail-row">
        <span class="detail-label">restart_lsn:</span>
        <code>{{ slotView.slot.restart_lsn }}</code>
        <span v-if="slotView.slot.active" class="tag-ok">active</span>
        <span v-else class="tag-warn">inactive</span>
      </div>
      <div class="detail-row">
        <span class="detail-label">滞留 WAL:</span>
        <code :class="{ 'lag-warn': (slotView.slot.lag_bytes || 0) > 1073741824 }">{{ formatBytes(slotView.slot.lag_bytes) }}</code>
        <span class="detail-hint">（当前 LSN {{ slotView.slot.current_lsn }}；CDC 停止期间持续增长）</span>
      </div>
      <div class="detail-row" v-if="slotView.checkpoint.exists">
        <span class="detail-label">checkpoint:</span>
        <code>{{ slotView.checkpoint.lsn }}</code>
        <span class="detail-hint">（{{ Math.floor(slotView.checkpoint_age_seconds || 0) }}s 前更新）</span>
      </div>
    </el-card>

    <!-- Error display -->
    <!-- P1 巡检修复 #7：div → el-card -->
    <el-card class="error-card" v-if="stats && stats.last_error">
      <h3>最近错误</h3>
      <pre>{{ stats.last_error }}</pre>
    </el-card>

    <!-- Refresh button -->
    <div class="actions">
      <el-button @click="refresh">刷新</el-button>
      <span class="auto-refresh">自动刷新: {{ refreshInterval }}s</span>
    </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient from '../api'
import DataPipelineStrip from '../components/DataPipelineStrip.vue'
import SparkLine from '../components/SparkLine.vue'
import PageHeader from '../components/PageHeader.vue'
import SyncCompareCard from '../components/SyncCompareCard.vue'
import { useDataSources } from '../composables/useDataSources'

// Mirrors the web API contract (docs/cdc-web-monitoring-contract.md, #t48 B).
interface CDCStatus {
  available?: boolean
  enabled?: boolean // false when the CDC module is off (cdc.enable=false)
  running: boolean
  state?: string // not_running | running | stale | halted
  message?: string
  lsn?: string
  slot?: string
  publication?: string
  pid?: number
  uptime_seconds?: number
  fatal_error?: string
  control?: CDCControl
}

// CDCSupervisor control view (#t55 CONTROL channel).
interface CDCControl {
  state: string // stopped|starting|running|stopping|failed|adopted
  pid: number
  restarts: number
  uptime_seconds: number
  adopted: boolean
}

interface CDCStats {
  source_events: number
  applied: number
  failed: number
  skipped: number
  batches: number
  throughput_rps: number
  lag_seconds: number
  uptime_seconds: number
  last_error?: string
}

interface CDCCheckpoint {
  lsn: string
  updated_at: string
}

// A1: live connection config (config.yaml the CDC child loads).
interface CDCConnConfig {
  cfg_file: string
  source: { type: string; host: string; port: number; user: string; database: string; schema: string; sslmode: string }
  target: { host: string; port: number; user: string; database: string }
  cdc: { enable: boolean; mode: string; slot_name: string; publication: string; sync_ddl: boolean; conflict_strategy: string; parallel: number; checkpoint_file: string }
  has_password: boolean
}

// A2/A3: precheck panel + resume point.
interface CDCPrecheck {
  items: { item: string; label: string; level: 'ok' | 'warn' | 'fail'; detail: string }[]
  checkpoint: { exists: boolean; lsn?: string; file?: string; updated_at?: string }
  slot: { exists: boolean; active?: boolean; restart_lsn?: string; lag_bytes?: number; current_lsn?: string }
  conclusion: string
  warn_only: boolean
  no_pk_tables_list?: string[]
}

interface CDCSlotView {
  slot: { exists: boolean; active?: boolean; restart_lsn?: string; lag_bytes?: number; current_lsn?: string }
  checkpoint: { exists: boolean; lsn?: string }
  checkpoint_age_seconds?: number
}

const connCfg = ref<CDCConnConfig | null>(null)
const editingConn = ref(false)
const savingConn = ref(false)
const connMsg = ref('')
const connError = ref(false)
const editForm = ref({
  source: { host: '', port: 5432, user: '', password: '', database: '', schema: '' },
  target: { host: '', port: 4000, user: '', password: '', database: '' },
})

const precheck = ref<CDCPrecheck | null>(null)
const checking = ref(false)
const slotView = ref<CDCSlotView | null>(null)

const precheckPassed = computed(() => precheck.value?.warn_only === true)

async function loadConnConfig() {
  try {
    const { data } = await apiClient.getCDCConfig()
    connCfg.value = data
  } catch {}
}

function startEditConn() {
  if (!connCfg.value) return
  editForm.value.source = {
    host: connCfg.value.source.host, port: connCfg.value.source.port, user: connCfg.value.source.user,
    password: '', database: connCfg.value.source.database, schema: connCfg.value.source.schema,
  }
  editForm.value.target = {
    host: connCfg.value.target.host, port: connCfg.value.target.port, user: connCfg.value.target.user,
    password: '', database: connCfg.value.target.database,
  }
  editingConn.value = true
  connMsg.value = ''
}

async function saveConn() {
  savingConn.value = true
  connMsg.value = ''
  connError.value = false
  try {
    // Only include non-empty passwords: empty means "keep stored" server-side.
    const source: Record<string, any> = { ...editForm.value.source }
    if (!source.password) delete source.password
    const target: Record<string, any> = { ...editForm.value.target }
    if (!target.password) delete target.password
    const { data: j } = await apiClient.saveCDCConfig(source, target)
    connMsg.value = j.message || '已保存'
    editingConn.value = false
    await loadConnConfig()
    await runPrecheck()
  } catch (e: any) {
    connError.value = true
    connMsg.value = e.response?.data?.error || e.message || '保存失败'
  } finally {
    savingConn.value = false
  }
}

async function importConn() {
  try {
    await ElMessageBox.confirm('确认把最近一次成功迁移任务的源/目标连接导入 config.yaml？当前 CDC 使用的连接将被覆盖（cdc 配置段不变）。', '确认', { type: 'warning' })
  } catch { return }
  connMsg.value = ''
  connError.value = false
  try {
    const { data: j } = await apiClient.importCDCConfig()
    connMsg.value = j.message || '已导入'
    await loadConnConfig()
    await runPrecheck()
  } catch (e: any) {
    connError.value = true
    connMsg.value = e.response?.data?.error || e.message || '导入失败'
  }
}

// F-02 D4: write datasource credentials into config.yaml (supervisor chain
// untouched; takes effect on next CDC start/restart).
const { load: loadDataSources, byType: dsByType } = useDataSources()
const dsSourceRef = ref('')
const dsTargetRef = ref('')
const importingDS = ref(false)

async function importFromDS() {
  if (!dsSourceRef.value || !dsTargetRef.value) return
  try {
    await ElMessageBox.confirm('确认把所选数据源的连接写入 config.yaml？当前 CDC 使用的连接将被覆盖（cdc 配置段不变）。', '确认', { type: 'warning' })
  } catch { return }
  importingDS.value = true
  connMsg.value = ''
  connError.value = false
  try {
    const { data: j } = await apiClient.importCDCFromDataSource(dsSourceRef.value, dsTargetRef.value)
    connMsg.value = j.message || '已导入'
    await loadConnConfig()
    await runPrecheck()
  } catch (e: any) {
    connError.value = true
    connMsg.value = e.response?.data?.error || e.message || '导入失败'
  } finally {
    importingDS.value = false
  }
}

async function runPrecheck() {
  checking.value = true
  try {
    const { data } = await apiClient.cdcPrecheck()
    precheck.value = data
  } catch {} finally {
    checking.value = false
  }
}

async function refreshSlot() {
  try {
    const { data } = await apiClient.cdcSlot()
    slotView.value = data
  } catch {}
}

// No-PK assist (P1): generate / copy / execute ALTER TABLE ... REPLICA IDENTITY
// FULL so UPDATE/DELETE can sync for tables without a primary key.
const noPKTables = computed(() => precheck.value?.no_pk_tables_list || [])
const noPKBusy = ref(false)

function noPKStatements(tables: string[]): string {
  return tables.map(t => `ALTER TABLE ${t} REPLICA IDENTITY FULL;`).join('\n')
}

async function copyNoPKSQL() {
  try {
    await navigator.clipboard.writeText(noPKStatements(noPKTables.value))
    ElMessage.success('ALTER 语句已复制，请在源端以表 owner / superuser 手动执行')
  } catch {
    ElMessageBox.alert('复制失败，请手动复制：\n' + noPKStatements(noPKTables.value), '提示', { type: 'warning' })
  }
}

async function openNoPKFix() {
  const tables = noPKTables.value
  if (!tables.length) return
  const stmts = noPKStatements(tables)
  try {
    await ElMessageBox.confirm(
      `将对 ${tables.length} 张无主键表启用整行复制：\n\n${stmts}\n\n` +
      '执行需要当前用户是表 owner 或 superuser。确定执行？（取消则改为复制 SQL 手动执行）',
      '确认',
      { type: 'warning', customStyle: { whiteSpace: 'pre-wrap' } as any },
    )
  } catch {
    await copyNoPKSQL()
    return
  }
  await executeNoPKFix(tables)
}

async function executeNoPKFix(tables: string[]) {
  noPKBusy.value = true
  try {
    const { data: j } = await apiClient.runReplicaIdentity(tables)
    const lines: string[] = []
    for (const res of j.results || []) {
      lines.push(res.ok ? `✓ ${res.table}` : `✗ ${res.table}：${res.error}（SQL：${res.sql}）`)
    }
    ElMessageBox.alert(
      lines.join('\n') + '\n\n' + (j.ok ? '全部成功，正在重新预检…' : '部分失败，失败的表请复制 SQL 手动执行'),
      j.ok ? '执行成功' : '部分失败',
      { type: j.ok ? 'success' : 'warning', customStyle: { whiteSpace: 'pre-wrap' } as any },
    )
    if (j.ok || (j.results || []).some((x: any) => x.ok)) await runPrecheck()
  } catch (e: any) {
    ElMessage.error('执行失败：' + (e.response?.data?.error || e.message))
  } finally {
    noPKBusy.value = false
  }
}

async function resetCheckpoint() {
  let answer: string
  try {
    const { value } = await ElMessageBox.prompt(
      '危险操作：删除 checkpoint 断点文件。重启后将从 slot restart_lsn 重放（宁重放不丢数据）。输入 DELETE 确认：',
      '危险操作',
      { type: 'warning', confirmButtonText: '确认', cancelButtonText: '取消' },
    )
    answer = value
  } catch { return }
  if (answer !== 'DELETE') return
  try {
    const { data: j } = await apiClient.resetCDCCheckpoint()
    ElMessage.success(j.message || '已重置')
    await runPrecheck()
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || e.message || '重置失败')
  }
}

function formatBytes(b?: number): string {
  if (!b) return '0B'
  if (b >= 1073741824) return (b / 1073741824).toFixed(1) + 'GB'
  if (b >= 1048576) return (b / 1048576).toFixed(1) + 'MB'
  if (b >= 1024) return (b / 1024).toFixed(1) + 'KB'
  return b + 'B'
}


const status = ref<CDCStatus>({ running: false })
const stats = ref<CDCStats | null>(null)
const checkpoint = ref<CDCCheckpoint | null>(null)
const refreshInterval = ref(5)
let timer: ReturnType<typeof setInterval> | null = null

// Throughput sparkline history (capped ring buffer)
const throughputHistory = ref<number[]>([])
function pushThroughput(v: number | undefined) {
  throughputHistory.value.push(v ?? 0)
  if (throughputHistory.value.length > 60) throughputHistory.value.shift()
}

const pipelineStatus = computed<'running' | 'warn' | 'stopped'>(() => {
  if (isActive.value) return stats.value && stats.value.failed > 0 ? 'warn' : 'running'
  return 'stopped'
})
const pipelineBadges = computed(() => {
  const b: { label: string; value: string }[] = []
  if (status.value.lsn) b.push({ label: 'LSN', value: status.value.lsn })
  if (stats.value) {
    b.push({ label: '吞吐', value: (stats.value.throughput_rps?.toFixed(1) || '0') + '/s' })
    b.push({ label: '延迟', value: (stats.value.lag_seconds?.toFixed(1) || '0') + 's' })
    b.push({ label: '已应用', value: String(stats.value.applied ?? 0) })
  }
  return b
})

const statusState = computed(() => {
  switch (status.value.state) {
    case 'running': return 'running'
    case 'halted': return 'halted'
    case 'stale': return 'stale'
    default: return 'stopped'
  }
})

const statusLabel = computed(() => {
  switch (status.value.state) {
    case 'running': return '运行中'
    case 'halted': return '已停止 (halt)'
    case 'stale': return '状态过期 (进程可能已崩溃)'
    default: return status.value.running ? '运行中' : '未运行'
  }
})

// CDC is an optional module: when disabled (cdc.enable=false) the dashboard is
// replaced by a notice and polling stops (D3 #t53).
const disabled = computed(() => status.value.enabled === false)

// One-click start/stop (CONTROL channel, #t55).
const control = ref<CDCControl | null>(null)
const busy = ref(false)
const controlMsg = ref('')
const controlError = ref(false)

const controlState = computed(() => control.value?.state || 'stopped')
const isActive = computed(() => ['running', 'starting', 'adopted'].includes(controlState.value))
const canStop = computed(() => ['running', 'starting', 'adopted'].includes(controlState.value))
const controlLabel = computed(() => {
  switch (controlState.value) {
    case 'running': return '运行中'
    case 'starting': return '启动中…'
    case 'stopping': return '停止中…'
    case 'failed': return '已失败（崩溃超限）'
    case 'adopted': return '运行中（领养）'
    default: return '未启动'
  }
})
const startLabel = computed(() => {
  if (busy.value) return '处理中…'
  return controlState.value === 'failed' ? '重新启动 CDC' : '启动 CDC'
})

// Startup-window polish (#t55): the supervisor (control channel) knows CDC is
// running before the READ channel (status file) gets its first write — ~8-90s
// while CDC connects to PG/TiDB + creates the replication slot. During that
// window, show "启动中" instead of a misleading stale/未运行.
const inStartup = computed(() => isActive.value && !status.value.running)
const cardState = computed(() => (inStartup.value ? 'starting' : statusState.value))
const cardLabel = computed(() => (inStartup.value ? '启动中…（control 通道确认运行）' : statusLabel.value))

async function callCDC(action: 'start' | 'stop') {
  busy.value = true
  controlMsg.value = ''
  controlError.value = false
  try {
    const { data: j, status } = await apiClient.cdcControl(action)
    if (!j || !j.ok) {
      controlError.value = true
      controlMsg.value = j?.message || ('操作失败 HTTP ' + status)
    } else {
      controlMsg.value = j.message || ('CDC 已' + (action === 'start' ? '启动' : '停止'))
    }
    await refresh()
  } catch (e: any) {
    controlError.value = true
    controlMsg.value = e.response?.data?.message || e.message || '请求失败'
  } finally {
    busy.value = false
  }
}

function startCDC() {
  // A2: precheck gates start — fail items must be resolved first. Warn-only
  // is allowed (operator judgement, e.g. no-PK tables).
  if (!precheckPassed.value) {
    controlError.value = true
    controlMsg.value = '预检未通过（红色项），请先处理后再启动'
    return
  }
  return callCDC('start')
}
async function confirmStopCDC() {
  try {
    await ElMessageBox.confirm('确认停止 CDC 实时同步？进行中的同步将中断。', '确认', { type: 'warning' })
  } catch { return }
  await callCDC('stop')
}

async function refresh() {
  try {
    const statusRes = await apiClient.cdcStatus().then(r => r.data).catch(() => null)
    if (statusRes) {
      status.value = statusRes
      control.value = statusRes.control ?? null
    }
    // Optional module disabled (cdc.enable=false): stop polling, surface notice.
    if (statusRes && statusRes.enabled === false) {
      if (timer) { clearInterval(timer); timer = null }
      return
    }
    // /stats and /checkpoint return {} (empty object) when not_running — treat as no data.
    const [statsRes, cpRes] = await Promise.all([
      apiClient.cdcStats().then(r => r.data).catch(() => null),
      apiClient.cdcCheckpoint().then(r => r.data).catch(() => null),
    ])
    if (statsRes && statsRes.source_events !== undefined) {
      stats.value = statsRes
      pushThroughput(statsRes.throughput_rps)
    }
    if (cpRes && cpRes.lsn) checkpoint.value = cpRes
    // A3: live slot lag alongside business stats.
    await refreshSlot()
  } catch {
    // silently ignore fetch errors
  }
}

function formatNumber(n: number): string {
  if (n === undefined || n === null) return '0'
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M'
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K'
  return String(n)
}

function formatUptime(seconds: number): string {
  if (!seconds) return '0s'
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = Math.floor(seconds % 60)
  if (h > 0) return `${h}h ${m}m`
  if (m > 0) return `${m}m ${s}s`
  return `${s}s`
}

onMounted(() => {
  refresh()
  loadConnConfig()
  runPrecheck()
  loadDataSources()
  timer = setInterval(refresh, refreshInterval.value * 1000)
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
/* F-02 datasource import row */
.ds-import {
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.ds-import-label { font-size: var(--tims-font-xs); color: var(--tims-text-2, #909399); } /* P1 巡检修复 #7 字号归一 */
.ds-import-arrow { color: var(--tims-text-2, #909399); }
.ds-import-select { width: 180px; }

.cdc-container {
  /* width & centering come from the shared .tims-page class */
}
.cdc-container code { word-break: break-all; } /* #t2 375 视口长 LSN/配置串防御性换行 */

.status-card {
  border-radius: var(--tims-radius); padding: 24px; margin-bottom: 24px;
  box-shadow: var(--tims-shadow);
}
/* #t2 对比度二轮：状态横幅渐变两档白字均 ≥4.5 */
.status-card.running { background: linear-gradient(135deg, #0b7a7a, #0d8484); color: #fff; }
.status-card.starting { background: linear-gradient(135deg, #2c4a8f, #3d5ea3); color: #fff; }
.status-card.stopped { background: #eef0f6; color: var(--tims-text-2); }
.status-card.halted { background: linear-gradient(135deg, #c02f2f, #d03a3a); color: #fff; }
.status-card.stale { background: linear-gradient(135deg, #9a5700, #a86200); color: #fff; }
.status-indicator { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
.status-dot { width: 12px; height: 12px; border-radius: 50%; background: #d9d9d9; }
.status-dot.active { background: #fff; animation: pulse 2s infinite; }
.status-text { font-size: 18px; font-weight: 600; }
.status-meta { font-size: var(--tims-font-sm); opacity: 0.85; }

@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.5; }
}

.stats-grid {
  display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px;
  margin-bottom: 24px;
}
.stat-item {
  background: var(--tims-card); border-radius: var(--tims-radius); padding: 16px 18px; text-align: left;
  border: 1px solid var(--tims-border);
  box-shadow: var(--tims-shadow);
}
.stat-value {
  font-family: var(--tims-font-mono); font-weight: 500; font-size: var(--tims-font-xl); /* #t2 P2 字号归一：26→22 大数值档 */
  letter-spacing: -0.5px; color: var(--tims-text);
}
.stat-value.error { color: var(--tims-brand); }
.stat-label { font-size: 12px; color: var(--tims-text-2); margin-top: 4px; letter-spacing: 0.4px; }

/* P1 巡检修复 #7：div → el-card，白底/边框/圆角/阴影由全局 .el-card 皮肤提供，此处仅保留间距 */
.detail-card { margin-bottom: 16px; }
.detail-card h3 { font-size: var(--tims-font-md); margin-bottom: 12px; color: var(--tims-text); } /* P1 巡检修复 #7 字号/灰阶 */
.detail-row { display: flex; align-items: center; padding: 6px 0; font-size: 14px; }
.detail-label { width: 100px; color: var(--tims-text-2); } /* P1 巡检修复 #7 灰阶统一 */
code { background: #f0f0f0; padding: 2px 8px; border-radius: 4px; font-size: var(--tims-font-sm); } /* P1 巡检修复 #7 字号归一 */

/* P1 巡检修复 #7：div → el-card，仅覆盖语义色底/边框色（单层框）；#cf1322 深红达标保留 */
.error-card { background: #fff1f0; border-color: #ffccc7; margin-bottom: 16px; }
.error-card :deep(.el-card__body) { padding: 16px; }
.error-card h3 { font-size: var(--tims-font-md); color: #cf1322; margin-bottom: 8px; }
.error-card pre { font-size: var(--tims-font-sm); color: #cf1322; white-space: pre-wrap; word-break: break-all; }

.actions {
  display: flex; align-items: center; gap: 16px; margin-top: 16px;
}

/* P1 巡检修复 #7：div → el-card，外观由 el-card 提供，仅保留间距与纵向布局 */
.control-card { margin-bottom: 24px; }
.control-card :deep(.el-card__body) { display: flex; flex-direction: column; gap: 12px; }
.control-head { display: flex; align-items: center; gap: 12px; }
.control-badge {
  font-size: var(--tims-font-sm); font-weight: 600; padding: 4px 12px; border-radius: 12px;
  background: #f0f0f0; color: var(--tims-text-2); /* P1 巡检修复 #7 字号/灰阶；胶囊圆角保留 */
}
.control-badge.running, .control-badge.adopted { background: #f6ffed; color: var(--tims-ok-text); } /* P1 巡检修复 #9：#389e0d(3.37:1) → AA 绿 */
.control-badge.starting, .control-badge.stopping { background: #e6f7ff; color: #1890ff; }
.control-badge.failed { background: #fff1f0; color: #cf1322; }
.control-restarts { font-size: 12px; color: var(--tims-tag-warning-text); } /* P1 巡检修复 #9：#faad14(≈2.2:1) → AA */
.control-actions { display: flex; gap: 12px; align-items: center; }
.control-msg { font-size: var(--tims-font-sm); color: var(--tims-ok-text); } /* P1 巡检修复 #9：#52c41a → AA 绿；字号 #7 */
.control-msg.error { color: #cf1322; }

/* P1 巡检修复 #7：div → el-card，保留 dashed 边框语义；灰阶/字号统一 */
.disabled-card { border-style: dashed; text-align: center; color: var(--tims-text-2); }
.disabled-card :deep(.el-card__body) { padding: 32px 24px; }
.disabled-card h3 { font-size: var(--tims-font-md); color: var(--tims-text); margin-bottom: 12px; }
.disabled-card p { font-size: 14px; margin: 6px 0; }
.disabled-card .hint { color: var(--tims-text-2); font-size: var(--tims-font-sm); }
.disabled-card code { background: #f0f0f0; padding: 2px 6px; border-radius: 4px; font-size: 12px; }
.auto-refresh { font-size: 12px; color: var(--tims-text-2); } /* P1 巡检修复 #7 灰阶统一 */

/* A1 connection edit form */
.conn-edit { margin-top: 12px; padding: 12px; background: #fafafa; border-radius: 8px; }
.conn-edit-section { font-size: var(--tims-font-sm); font-weight: 600; color: var(--tims-text); margin: 8px 0; } /* P1 巡检修复 #7 字号/灰阶 */
.conn-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 8px; }
.conn-grid label { display: flex; flex-direction: column; font-size: 12px; color: var(--tims-text-2); gap: 2px; } /* P1 巡检修复 #7 灰阶统一 */
.conn-grid-num { width: 100%; }

/* A2 precheck panel */
.precheck-row { display: flex; align-items: baseline; gap: 8px; padding: 6px 0; font-size: var(--tims-font-sm); border-bottom: 1px dashed #f0f0f0; } /* P1 巡检修复 #7 字号归一 */
.precheck-dot { width: 18px; height: 18px; border-radius: 50%; color: #fff; font-size: 12px; display: inline-flex; align-items: center; justify-content: center; flex: none; align-self: center; }
.precheck-row.ok .precheck-dot { background: #52c41a; }
.precheck-row.warn .precheck-dot { background: #faad14; }
.precheck-row.fail .precheck-dot { background: #f5222d; }
.precheck-label { font-weight: 600; color: var(--tims-text); flex: none; width: 150px; } /* P1 巡检修复 #7 灰阶统一 */
.precheck-detail { color: var(--tims-text-2); word-break: break-all; } /* P1 巡检修复 #7 灰阶统一 */
/* P1 巡检修复 #7 圆角/字号 + #9：#389e0d(3.37:1) → AA 绿 */
.resume-box { margin-top: 12px; padding: 10px 12px; background: #f6ffed; border: 1px solid #b7eb8f; border-radius: var(--tims-radius-s); font-size: var(--tims-font-sm); color: var(--tims-ok-text); }

/* A3 slot card */
/* P1 巡检修复 #9：tag 文字换 AA 深色（#389e0d/#d48806 不足 4.5:1）；胶囊圆角保留 */
.tag-ok { font-size: 12px; padding: 1px 8px; border-radius: 10px; background: #f6ffed; color: var(--tims-ok-text); margin-left: 8px; }
.tag-warn { font-size: 12px; padding: 1px 8px; border-radius: 10px; background: #fff7e6; color: var(--tims-tag-warning-text); margin-left: 8px; }
.detail-hint { font-size: 12px; color: var(--tims-text-2); margin-left: 8px; } /* P1 巡检修复 #7 灰阶统一 */
.lag-warn { color: #cf1322; font-weight: 700; }
</style>
