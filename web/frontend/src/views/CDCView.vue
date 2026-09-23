<template>
  <div class="cdc-container">
    <h1>CDC 增量同步</h1>
    <p class="subtitle">PostgreSQL → TiDB 实时增量同步监控</p>

    <!-- Module disabled (cdc.enable=false) -->
    <div class="disabled-card" v-if="disabled">
      <h3>🚫 CDC 模块未启用</h3>
      <p>当前部署未开启 CDC 增量同步（<code>cdc.enable: false</code>）。</p>
      <p class="hint">如需使用：在 config.yaml 设置 <code>cdc.enable: true</code>，或用 <code>pg2tidb cdc --enable-cdc</code> 启动。</p>
    </div>

    <template v-else>
    <!-- Connection card (A1): the live config.yaml the CDC child uses -->
    <div class="detail-card" v-if="connCfg">
      <h3>🔌 连接信息（CDC 实际使用）</h3>
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
        <button class="btn-refresh" @click="startEditConn" v-if="!editingConn">编辑连接</button>
        <button class="btn-refresh" @click="importConn" :disabled="busy || isActive || editingConn">从最近迁移任务导入</button>
      </div>
      <!-- Inline edit form (A1 manual edit) -->
      <div v-if="editingConn" class="conn-edit">
        <div class="conn-edit-section">源端（PostgreSQL）</div>
        <div class="conn-grid">
          <label>主机<input v-model="editForm.source.host" /></label>
          <label>端口<input type="number" v-model.number="editForm.source.port" /></label>
          <label>用户名<input v-model="editForm.source.user" /></label>
          <label>密码<input type="password" v-model="editForm.source.password" placeholder="留空保持不变" /></label>
          <label>数据库<input v-model="editForm.source.database" /></label>
          <label>Schema<input v-model="editForm.source.schema" /></label>
        </div>
        <div class="conn-edit-section">目标端（TiDB）</div>
        <div class="conn-grid">
          <label>主机<input v-model="editForm.target.host" /></label>
          <label>端口<input type="number" v-model.number="editForm.target.port" /></label>
          <label>用户名<input v-model="editForm.target.user" /></label>
          <label>密码<input type="password" v-model="editForm.target.password" placeholder="留空保持不变" /></label>
          <label>数据库<input v-model="editForm.target.database" /></label>
        </div>
        <div class="control-actions" style="margin-top: 8px;">
          <button class="btn-start" @click="saveConn" :disabled="savingConn">{{ savingConn ? '保存中…' : '保存' }}</button>
          <button class="btn-stop" @click="editingConn = false">取消</button>
        </div>
        <div v-if="connMsg" class="control-msg" :class="{ error: connError }">{{ connMsg }}</div>
      </div>
    </div>

    <!-- Precheck panel (A2): run before Start; fail items block -->
    <div class="detail-card" v-if="precheck">
      <h3>🩺 启动预检 <button class="btn-refresh" style="margin-left: 8px; padding: 2px 10px; font-size: 12px;" @click="runPrecheck" :disabled="checking">{{ checking ? '检查中…' : '重新检查' }}</button></h3>
      <div v-for="it in precheck.items" :key="it.item" class="precheck-row" :class="it.level">
        <span class="precheck-dot">{{ it.level === 'ok' ? '✓' : it.level === 'warn' ? '!' : '✗' }}</span>
        <span class="precheck-label">{{ it.label }}</span>
        <span class="precheck-detail">{{ it.detail }}</span>
      </div>
      <div class="resume-box">
        <strong>断点/起点：</strong>{{ precheck.conclusion }}
        <span v-if="precheck.checkpoint.exists"> · checkpoint 文件 {{ precheck.checkpoint.file }}（更新于 {{ precheck.checkpoint.updated_at ? new Date(precheck.checkpoint.updated_at).toLocaleString() : '-' }}）</span>
      </div>
      <div class="control-actions" style="margin-top: 8px;" v-if="precheck.checkpoint.exists && !isActive">
        <button class="btn-stop" @click="resetCheckpoint">重置断点（危险）</button>
      </div>
    </div>

    <!-- Control panel: one-click start/stop (#t55) -->
    <div class="control-card">
      <div class="control-head">
        <span class="control-badge" :class="controlState">{{ controlLabel }}</span>
        <span v-if="control && control.restarts > 0" class="control-restarts">自动重启 {{ control.restarts }} 次</span>
      </div>
      <div class="control-actions">
        <button class="btn-start" :disabled="busy || isActive || !precheckPassed" @click="startCDC" :title="precheckPassed ? '' : '预检未通过或有未完成的预检项，请先点击预检面板重新检查'">{{ startLabel }}</button>
        <button class="btn-stop" :disabled="busy || !canStop" @click="confirmStopCDC">停止 CDC</button>
      </div>
      <div v-if="controlMsg" class="control-msg" :class="{ error: controlError }">{{ controlMsg }}</div>
    </div>

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
    <div class="detail-card" v-if="checkpoint && checkpoint.lsn">
      <h3>📌 检查点</h3>
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
    </div>

    <!-- Config Card (from status: slot/publication/pid) -->
    <div class="detail-card" v-if="status.slot || status.publication">
      <h3>⚙️ 配置</h3>
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
    </div>

    <!-- Live slot / lag card (A3): restart_lsn + retained WAL + checkpoint age -->
    <div class="detail-card" v-if="slotView && slotView.slot.exists">
      <h3>📡 Slot 与延迟</h3>
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
    </div>

    <!-- Error display -->
    <div class="error-card" v-if="stats && stats.last_error">
      <h3>⚠️ 最近错误</h3>
      <pre>{{ stats.last_error }}</pre>
    </div>

    <!-- Refresh button -->
    <div class="actions">
      <button @click="refresh" class="btn-refresh">🔄 刷新</button>
      <span class="auto-refresh">自动刷新: {{ refreshInterval }}s</span>
    </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'

const API_BASE = '/api/v1/cdc'

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
    const r = await fetch(API_BASE + '/config')
    if (r.ok) connCfg.value = await r.json()
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
    const r = await fetch(API_BASE + '/config', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ source, target }),
    })
    const j = await r.json().catch(() => ({}))
    if (!r.ok) { connError.value = true; connMsg.value = j.error || '保存失败' }
    else {
      connMsg.value = j.message || '已保存'
      editingConn.value = false
      await loadConnConfig()
      await runPrecheck()
    }
  } catch (e: any) {
    connError.value = true
    connMsg.value = '请求失败: ' + e
  } finally {
    savingConn.value = false
  }
}

async function importConn() {
  if (!confirm('确认把最近一次成功迁移任务的源/目标连接导入 config.yaml？当前 CDC 使用的连接将被覆盖（cdc 配置段不变）。')) return
  connMsg.value = ''
  connError.value = false
  try {
    const r = await fetch(API_BASE + '/config/import', { method: 'POST' })
    const j = await r.json().catch(() => ({}))
    if (!r.ok) { connError.value = true; connMsg.value = j.error || '导入失败' }
    else {
      connMsg.value = j.message || '已导入'
      await loadConnConfig()
      await runPrecheck()
    }
  } catch (e: any) {
    connError.value = true
    connMsg.value = '请求失败: ' + e
  }
}

async function runPrecheck() {
  checking.value = true
  try {
    const r = await fetch(API_BASE + '/precheck')
    if (r.ok) precheck.value = await r.json()
  } catch {} finally {
    checking.value = false
  }
}

async function refreshSlot() {
  try {
    const r = await fetch(API_BASE + '/slot')
    if (r.ok) slotView.value = await r.json()
  } catch {}
}

async function resetCheckpoint() {
  const answer = prompt('危险操作：删除 checkpoint 断点文件。重启后将从 slot restart_lsn 重放（宁重放不丢数据）。输入 DELETE 确认：')
  if (answer !== 'DELETE') return
  try {
    const r = await fetch(API_BASE + '/checkpoint/reset', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: 'DELETE' }),
    })
    const j = await r.json().catch(() => ({}))
    if (!r.ok) alert(j.error || '重置失败')
    else alert(j.message || '已重置')
    await runPrecheck()
  } catch (e: any) {
    alert('请求失败: ' + e)
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
    const r = await fetch(API_BASE + '/' + action, { method: 'POST' })
    const j = await r.json().catch(() => ({}))
    if (!r.ok || !j.ok) {
      controlError.value = true
      controlMsg.value = j.message || '操作失败 HTTP ' + r.status
    } else {
      controlMsg.value = j.message || ('CDC 已' + (action === 'start' ? '启动' : '停止'))
    }
    await refresh()
  } catch (e) {
    controlError.value = true
    controlMsg.value = '请求失败: ' + e
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
function confirmStopCDC() {
  if (!confirm('确认停止 CDC 增量同步？进行中的同步将中断。')) return
  return callCDC('stop')
}

async function refresh() {
  try {
    const statusRes = await fetch(API_BASE + '/status').then(r => r.json()).catch(() => null)
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
      fetch(API_BASE + '/stats').then(r => r.json()).catch(() => null),
      fetch(API_BASE + '/checkpoint').then(r => r.json()).catch(() => null),
    ])
    if (statsRes && statsRes.source_events !== undefined) stats.value = statsRes
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
  timer = setInterval(refresh, refreshInterval.value * 1000)
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.cdc-container {
  max-width: 1000px;
  margin: 0 auto;
  padding: 24px;
  font-family: -apple-system, BlinkMacSystemFont, 'PingFang SC', sans-serif;
}
h1 { font-size: 24px; color: #1a1a2e; margin-bottom: 4px; }
.subtitle { color: #666; font-size: 14px; margin-bottom: 24px; }

.status-card {
  border-radius: 12px; padding: 24px; margin-bottom: 24px;
  box-shadow: 0 2px 8px rgba(0,0,0,0.08);
}
.status-card.running { background: linear-gradient(135deg, #52c41a, #73d13d); color: #fff; }
.status-card.starting { background: linear-gradient(135deg, #1890ff, #69b1ff); color: #fff; }
.status-card.stopped { background: #f5f5f5; color: #666; }
.status-card.halted { background: linear-gradient(135deg, #f5222d, #ff7875); color: #fff; }
.status-card.stale { background: linear-gradient(135deg, #faad14, #ffc53d); color: #fff; }
.status-indicator { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
.status-dot { width: 12px; height: 12px; border-radius: 50%; background: #d9d9d9; }
.status-dot.active { background: #fff; animation: pulse 2s infinite; }
.status-text { font-size: 18px; font-weight: 600; }
.status-meta { font-size: 13px; opacity: 0.85; }

@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.5; }
}

.stats-grid {
  display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px;
  margin-bottom: 24px;
}
.stat-item {
  background: #fff; border-radius: 12px; padding: 20px; text-align: center;
  box-shadow: 0 2px 8px rgba(0,0,0,0.06);
}
.stat-value { font-size: 28px; font-weight: 700; color: #1a1a2e; }
.stat-value.error { color: #f5222d; }
.stat-label { font-size: 13px; color: #666; margin-top: 4px; }

.detail-card {
  background: #fff; border-radius: 12px; padding: 20px; margin-bottom: 16px;
  box-shadow: 0 2px 8px rgba(0,0,0,0.06);
}
.detail-card h3 { font-size: 16px; margin-bottom: 12px; color: #1a1a2e; }
.detail-row { display: flex; align-items: center; padding: 6px 0; font-size: 14px; }
.detail-label { width: 100px; color: #666; }
code { background: #f0f0f0; padding: 2px 8px; border-radius: 4px; font-size: 13px; }

.error-card {
  background: #fff1f0; border: 1px solid #ffccc7; border-radius: 12px;
  padding: 16px; margin-bottom: 16px;
}
.error-card h3 { font-size: 16px; color: #cf1322; margin-bottom: 8px; }
.error-card pre { font-size: 13px; color: #cf1322; white-space: pre-wrap; word-break: break-all; }

.actions {
  display: flex; align-items: center; gap: 16px; margin-top: 16px;
}

.control-card {
  background: #fff; border-radius: 12px; padding: 20px; margin-bottom: 24px;
  box-shadow: 0 2px 8px rgba(0,0,0,0.06);
  display: flex; flex-direction: column; gap: 12px;
}
.control-head { display: flex; align-items: center; gap: 12px; }
.control-badge {
  font-size: 13px; font-weight: 600; padding: 4px 12px; border-radius: 12px;
  background: #f0f0f0; color: #666;
}
.control-badge.running, .control-badge.adopted { background: #f6ffed; color: #389e0d; }
.control-badge.starting, .control-badge.stopping { background: #e6f7ff; color: #1890ff; }
.control-badge.failed { background: #fff1f0; color: #cf1322; }
.control-restarts { font-size: 12px; color: #faad14; }
.control-actions { display: flex; gap: 12px; }
.btn-start, .btn-stop {
  padding: 8px 20px; border: none; border-radius: 8px; font-size: 14px; cursor: pointer;
}
.btn-start { background: #52c41a; color: #fff; }
.btn-start:hover { opacity: 0.9; }
.btn-start:disabled { background: #d9d9d9; cursor: not-allowed; }
.btn-stop { background: #ff4d4f; color: #fff; }
.btn-stop:hover { opacity: 0.9; }
.btn-stop:disabled { background: #d9d9d9; cursor: not-allowed; }
.control-msg { font-size: 13px; color: #52c41a; }
.control-msg.error { color: #cf1322; }

.disabled-card {
  background: #fff; border: 1px dashed #d9d9d9; border-radius: 12px;
  padding: 32px 24px; text-align: center; color: #666;
}
.disabled-card h3 { font-size: 18px; color: #1a1a2e; margin-bottom: 12px; }
.disabled-card p { font-size: 14px; margin: 6px 0; }
.disabled-card .hint { color: #999; font-size: 13px; }
.disabled-card code { background: #f0f0f0; padding: 2px 6px; border-radius: 4px; font-size: 12px; }
.btn-refresh {
  padding: 8px 20px; border: none; border-radius: 8px;
  background: #1a1a2e; color: #fff; font-size: 14px; cursor: pointer;
}
.btn-refresh:hover { opacity: 0.85; }
.auto-refresh { font-size: 12px; color: #999; }

/* A1 connection edit form */
.conn-edit { margin-top: 12px; padding: 12px; background: #fafafa; border-radius: 8px; }
.conn-edit-section { font-size: 13px; font-weight: 600; color: #1a1a2e; margin: 8px 0; }
.conn-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 8px; }
.conn-grid label { display: flex; flex-direction: column; font-size: 12px; color: #666; gap: 2px; }
.conn-grid input { padding: 6px 8px; border: 1px solid #d9d9d9; border-radius: 6px; font-size: 13px; }

/* A2 precheck panel */
.precheck-row { display: flex; align-items: baseline; gap: 8px; padding: 6px 0; font-size: 13px; border-bottom: 1px dashed #f0f0f0; }
.precheck-dot { width: 18px; height: 18px; border-radius: 50%; color: #fff; font-size: 12px; display: inline-flex; align-items: center; justify-content: center; flex: none; align-self: center; }
.precheck-row.ok .precheck-dot { background: #52c41a; }
.precheck-row.warn .precheck-dot { background: #faad14; }
.precheck-row.fail .precheck-dot { background: #f5222d; }
.precheck-label { font-weight: 600; color: #1a1a2e; flex: none; width: 150px; }
.precheck-detail { color: #666; word-break: break-all; }
.resume-box { margin-top: 12px; padding: 10px 12px; background: #f6ffed; border: 1px solid #b7eb8f; border-radius: 8px; font-size: 13px; color: #389e0d; }

/* A3 slot card */
.tag-ok { font-size: 12px; padding: 1px 8px; border-radius: 10px; background: #f6ffed; color: #389e0d; margin-left: 8px; }
.tag-warn { font-size: 12px; padding: 1px 8px; border-radius: 10px; background: #fff7e6; color: #d48806; margin-left: 8px; }
.detail-hint { font-size: 12px; color: #999; margin-left: 8px; }
.lag-warn { color: #cf1322; font-weight: 700; }
</style>
