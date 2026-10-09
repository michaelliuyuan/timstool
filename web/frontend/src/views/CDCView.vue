<template>
  <div class="cdc-container tims-page">
    <PageHeader title="CDC 实时同步" subtitle="基于数据库日志的推式实时同步管道，自动捕获 INSERT/UPDATE/DELETE，常驻运行" />

    <!-- Module disabled (cdc.enable=false) -->
    <!-- P1 巡检修复 #7：div → el-card，白底/边框/圆角由 el-card 皮肤提供 -->
    <el-card class="disabled-card" v-if="disabled">
      <h3>CDC 模块未启用</h3>
      <p>当前部署未开启 CDC 实时同步（<code>cdc.enable: false</code>）。</p>
      <p class="hint">如需使用：在 config.yaml 设置 <code>cdc.enable: true</code>，或用 <code>timstool cdc --enable-cdc</code> 启动。</p>
    </el-card>

    <template v-else>
    <!-- D: signature pipeline strip stays above the hero bar -->
    <DataPipelineStrip
      v-if="connCfg"
      :source="srcIsMySQL ? 'MySQL' : 'PostgreSQL'"
      :status="pipelineStatus"
      :badges="pipelineBadges"
    />

    <!-- MS-11c A: hero status bar — state dot + word + lsn/lag/uptime summary
         + inline start/stop/refresh actions (absorbs the old control card and
         status card; four-state colors run through hero + tab badges). -->
    <div class="status-card hero" :class="cardState">
      <div class="hero-main">
        <div class="status-indicator">
          <span class="status-dot" :class="{ active: status.running || inStartup }"></span>
          <span class="status-text">{{ cardLabel }}</span>
          <span v-if="control && control.restarts > 0" class="control-restarts hero-restarts">自动重启 {{ control.restarts }} 次</span>
        </div>
        <!-- MS-11c pen4 P2-⑤: hero meta stays neutral until connCfg arrives —
             the forked wording (同步位点/LSN, MySQL/PG) must never flash on a
             first frame whose source type is not yet known. -->
        <div class="status-meta" v-if="connCfg">
          <span v-if="status.running && status.lsn">{{ srcIsMySQL ? '同步位点' : 'LSN' }}: {{ status.lsn }}</span>
          <span v-else-if="inStartup">CDC 启动中…（control 通道确认运行，等待 CDC 连接 {{ srcIsMySQL ? 'MySQL' : 'PG' }}/TiDB 写首条状态，约 8-90s）</span>
          <span v-else>{{ status.message || 'CDC 未运行，点右侧「启动 CDC」开始' }}</span>
        </div>
        <div class="status-meta" v-else>连接配置加载中…</div>
        <div class="status-meta hero-summary" v-if="stats">
          <span>延迟 {{ stats.lag_seconds?.toFixed(1) || '0' }}s</span>
          <span>吞吐 {{ stats.throughput_rps?.toFixed(1) || '0' }}/s</span>
          <span>运行 {{ formatUptime(stats.uptime_seconds) }}</span>
        </div>
        <div class="status-meta" v-if="status.fatal_error" style="color: #fff; opacity: 0.95;">
          ⚠️ {{ status.fatal_error }}
        </div>
      </div>
      <div class="hero-actions">
        <el-button type="success" :disabled="busy || isActive || !precheckPassed" @click="startCDC" :title="precheckPassed ? '' : '预检未通过或有未完成的预检项，请先在「启动预检」Tab 重新检查'">{{ startLabel }}</el-button>
        <el-button type="danger" :disabled="busy || !canStop" @click="confirmStopCDC">停止 CDC</el-button>
        <el-button @click="refresh">刷新</el-button>
        <span class="auto-refresh hero-refresh">自动刷新: {{ refreshInterval }}s</span>
      </div>
      <div v-if="controlMsg" class="control-msg hero-msg" :class="{ error: controlError }">{{ controlMsg }}</div>
    </div>

    <!-- MS-11f 笔① c: anomaly-stop alarms — CDC stopped with no audited
         operator stop (the 2h-silent external stop this batch roots out).
         Newest first; the ring lives in the web process and clears on its
         restart. -->
    <div v-if="alarmRows.length" class="alarm-card">
      <div v-for="(a, i) in alarmRows" :key="i" class="alarm-row">
        <span class="alarm-time">{{ formatAlarmTS(a.ts) }}</span>
        <span class="alarm-text">{{ a.message }}</span>
      </div>
    </div>

    <!-- B: stats grid right under the hero -->
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
      <!-- MS-11c D: 位点推进小图（SparkLine 复用，与吞吐并列）— series advances
           as the checkpoint/LSN position moves; hidden until 2 samples. -->
      <div class="stat-item">
        <div class="stat-value">{{ positionLatest || '-' }}</div>
        <div class="stat-label">位点推进</div>
        <SparkLine v-if="positionHistory.length > 1" :data="positionHistory" :height="34" />
        <!-- MS-11e G: parsed-but-unsafe (>2^53) position — sampling paused; the
             flag clears again once a safe sample lands (pen7). -->
        <div v-if="positionPrecisionRisk" class="stat-label" style="color: var(--el-color-warning);">位点超出 2^53 安全整数范围，推进小图暂停采样</div>
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

    <!-- MS-11c C: three tabs — overview / connection+config / precheck -->
    <el-tabs v-model="activeTab" class="cdc-tabs">
      <!-- Tab ①: overview (default) -->
      <el-tab-pane label="概览" name="overview">
        <!-- merged card: checkpoint + config + slot (three cards → one, fields deduped) -->
        <el-card class="detail-card" v-if="hasPositionInfo">
          <h3>位点 / 复制槽 / 进程</h3>
          <!-- MS-11c pen5: the forked 位点 label gates on connCfg too — a MySQL
               chain must never flash "LSN:" before /cdc/config lands (the
               checkpoint poll races it independently). -->
          <div class="detail-row" v-if="connCfg && checkpoint && checkpoint.lsn">
            <span class="detail-label">{{ srcIsMySQL ? '同步位点:' : 'LSN:' }}</span>
            <code>{{ checkpoint.lsn }}</code>
          </div>
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
          <div class="detail-row" v-if="checkpoint && checkpoint.lsn">
            <span class="detail-label">更新时间:</span>
            <span>{{ checkpoint.updated_at ? new Date(checkpoint.updated_at).toLocaleString() : '-' }}</span>
          </div>
          <!-- A3 live slot fields (PG-only surface; mysql clears slotView) -->
          <div class="detail-row" v-if="slotView && slotView.slot.exists && slotView.slot.restart_lsn">
            <span class="detail-label">restart_lsn:</span>
            <code>{{ slotView.slot.restart_lsn }}</code>
            <span v-if="slotView.slot.active" class="tag-ok">active</span>
            <span v-else class="tag-warn">inactive</span>
          </div>
          <div class="detail-row" v-if="slotView && slotView.slot.exists">
            <span class="detail-label">滞留 WAL:</span>
            <code :class="{ 'lag-warn': (slotView.slot.lag_bytes || 0) > 1073741824 }">{{ formatBytes(slotView.slot.lag_bytes) }}</code>
            <span class="detail-hint">（当前 LSN {{ slotView.slot.current_lsn }}；CDC 停止期间持续增长）</span>
          </div>
          <div class="detail-row" v-if="slotView && slotView.checkpoint && slotView.checkpoint.exists && slotView.checkpoint.lsn && (!checkpoint || slotView.checkpoint.lsn !== checkpoint.lsn)">
            <span class="detail-label">checkpoint:</span>
            <code>{{ slotView.checkpoint.lsn }}</code>
            <span class="detail-hint">（{{ Math.floor(slotView.checkpoint_age_seconds || 0) }}s 前更新）</span>
          </div>
        </el-card>

        <!-- Recent error -->
        <el-card class="error-card" v-if="stats && stats.last_error">
          <h3>最近错误</h3>
          <pre>{{ stats.last_error }}</pre>
        </el-card>
      </el-tab-pane>

      <!-- Tab ②: connection & config -->
      <el-tab-pane label="连接与配置" name="conn">
        <!-- S1-UI-06: which-one-to-use card (moved into the config tab, MS-11c) -->
        <SyncCompareCard v-if="connCfg" current="cdc" :cdc-source-req="srcIsMySQL ? 'binlog（ROW 格式 + REPLICATION 权限）' : undefined" />

        <!-- Connection card (A1): the live config.yaml the CDC child uses.
             MS-11h（seq188 需求/seq192 落地令）：源/目标双栏对照卡（流向感）+
             CDC 参数标签行 + 操作区统一收纳；编辑态与展示态同构（原位变输入框）。
             功能零变动红线：三动作与既有校验语义原样，仅布局呈现变化。 -->
        <el-card class="detail-card" v-if="connCfg">
          <h3>连接信息（CDC 实际使用）</h3>
          <div class="detail-row">
            <span class="detail-label">配置文件:</span>
            <code>{{ connCfg.cfg_file }}</code>
          </div>

          <div class="conn-cards">
            <div class="conn-side">
              <div class="conn-side-head">
                <span class="conn-side-title">源端数据库</span>
                <span class="conn-side-type">{{ srcIsMySQL ? 'MySQL · binlog' : 'PostgreSQL' }}</span>
              </div>
              <div class="conn-field"><span>主机</span><el-input v-if="editingConn" v-model="editForm.source.host" size="small" /><code v-else>{{ connCfg.source.host }}</code></div>
              <div class="conn-field"><span>端口</span><el-input-number v-if="editingConn" v-model="editForm.source.port" :min="1" :max="65535" controls-position="right" size="small" class="conn-field-num" /><code v-else>{{ connCfg.source.port }}</code></div>
              <div class="conn-field"><span>用户</span><el-input v-if="editingConn" v-model="editForm.source.user" size="small" /><code v-else>{{ connCfg.source.user }}</code></div>
              <div class="conn-field"><span>密码</span><el-input v-if="editingConn" v-model="editForm.source.password" type="password" show-password placeholder="留空保持不变" size="small" /><code v-else :class="{ 'conn-nopwd': !connCfg.has_password }">{{ connCfg.has_password ? '●●●●●●' : '（未设密码）' }}</code></div>
              <div class="conn-field"><span>数据库</span><el-input v-if="editingConn" v-model="editForm.source.database" size="small" /><code v-else>{{ connCfg.source.database }}</code></div>
              <div class="conn-field" v-if="editingConn || connCfg.source.schema"><span>Schema</span><el-input v-if="editingConn" v-model="editForm.source.schema" size="small" /><code v-else>{{ connCfg.source.schema }}</code></div>
            </div>
            <div class="conn-flow" aria-hidden="true">→</div>
            <div class="conn-side">
              <div class="conn-side-head">
                <span class="conn-side-title">目标端</span>
                <span class="conn-side-type">TiDB</span>
              </div>
              <div class="conn-field"><span>主机</span><el-input v-if="editingConn" v-model="editForm.target.host" size="small" /><code v-else>{{ connCfg.target.host }}</code></div>
              <div class="conn-field"><span>端口</span><el-input-number v-if="editingConn" v-model="editForm.target.port" :min="1" :max="65535" controls-position="right" size="small" class="conn-field-num" /><code v-else>{{ connCfg.target.port }}</code></div>
              <div class="conn-field"><span>用户</span><el-input v-if="editingConn" v-model="editForm.target.user" size="small" /><code v-else>{{ connCfg.target.user }}</code></div>
              <div class="conn-field"><span>密码</span><el-input v-if="editingConn" v-model="editForm.target.password" type="password" show-password placeholder="留空保持不变" size="small" /><code v-else>●●●●●●</code></div>
              <div class="conn-field"><span>数据库</span><el-input v-if="editingConn" v-model="editForm.target.database" size="small" /><code v-else>{{ connCfg.target.database }}</code></div>
            </div>
          </div>

          <div class="conn-params">
            <div class="conn-param"><span class="conn-param-k">模式</span><code>{{ connCfg.cdc.mode }}</code></div>
            <div class="conn-param"><span class="conn-param-k">位点续传</span><code><template v-if="srcIsMySQL">binlog 采集（file:pos 位点续传）</template><template v-else-if="connCfg.cdc.slot_name">slot={{ connCfg.cdc.slot_name }} · pub={{ connCfg.cdc.publication }}</template></code></div>
            <div class="conn-param"><span class="conn-param-k">并发</span><code>{{ connCfg.cdc.parallel }}</code></div>
            <div class="conn-param"><span class="conn-param-k">冲突策略</span><code>{{ connCfg.cdc.conflict_strategy }}</code></div>
            <div class="conn-param"><span class="conn-param-k">DDL</span><code>{{ connCfg.cdc.sync_ddl ? '同步' : '不同步' }}</code></div>
          </div>

          <div class="conn-actions" v-if="!editingConn">
            <el-button type="primary" @click="startEditConn">编辑连接</el-button>
            <el-button @click="importConn" :disabled="busy || isActive">从最近迁移任务导入</el-button>
            <el-divider direction="vertical" />
            <span class="ds-import-label">从数据源导入：</span>
            <el-select v-model="dsSourceRef" class="ds-import-select" placeholder="源端数据源" size="small">
              <el-option label="源端数据源" value="" />
              <el-option v-for="d in dsByType(['postgres', 'mysql'])" :key="d.id" :value="d.id" :label="d.name" />
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

          <div class="conn-actions" v-if="editingConn">
            <el-button type="success" @click="saveConn" :disabled="savingConn" :loading="savingConn">{{ savingConn ? '保存中…' : '保存' }}</el-button>
            <el-button type="danger" plain @click="editingConn = false">取消</el-button>
            <span v-if="connMsg" class="control-msg" :class="{ error: connError }">{{ connMsg }}</span>
          </div>
        </el-card>
      </el-tab-pane>

      <!-- Tab ③: startup precheck — ok items folded, warn/fail always shown;
           red-dot badge + auto-activate while the precheck has not passed. -->
      <el-tab-pane name="precheck">
        <template #label>
          启动预检
          <span v-if="precheck && !precheckPassed" class="tab-dot" :class="precheckHasFail ? 'fail' : 'warn'" role="status" :aria-label="precheckHasFail ? '预检存在未通过项' : '预检存在警告项'" :title="precheckHasFail ? '预检存在未通过项' : '预检存在警告项'"></span>
        </template>
        <el-card class="detail-card" v-if="precheck">
          <h3>启动预检 <el-button size="small" style="margin-left: 8px;" @click="runPrecheck" :disabled="checking" :loading="checking">{{ checking ? '检查中…' : '重新检查' }}</el-button></h3>
          <div v-for="it in precheckAttentionItems" :key="it.item" class="precheck-row" :class="it.level">
            <span class="precheck-dot">{{ it.level === 'ok' ? '✓' : it.level === 'warn' ? '!' : '✗' }}</span>
            <span class="precheck-label">{{ it.label }}</span>
            <span class="precheck-detail">{{ it.detail }}</span>
            <!-- REPLICA IDENTITY is a PG-only mechanism — the mysql no-PK precheck
                 warn carries its own guidance (add a PK for row-accurate UPDATE/DELETE). -->
            <!-- MS-11c pen5: connCfg-gated — the PG-only mechanism button must
                 not appear in the pre-connCfg transient either. -->
            <el-button v-if="it.item === 'no_pk_tables' && it.level === 'warn' && noPKTables.length && connCfg && !srcIsMySQL"
              size="small" style="margin-left: auto; flex: none;"
              @click="openNoPKFix">修复（REPLICA IDENTITY FULL）</el-button>
          </div>
          <!-- MS-11c pen4 P2-④: a failed re-run must be visible, never silently
               keep the stale result dressed as fresh. -->
          <div v-if="precheckError" class="control-msg error" style="margin-bottom: 8px;">预检请求失败（{{ precheckError }}），显示的是上次结果，请重新检查</div>
          <!-- MS-11c: ok items folded by default — only warn/fail need eyes. -->
          <div v-if="precheckOkItems.length" class="precheck-fold">
            <el-button link size="small" :aria-expanded="showOkItems" aria-controls="precheck-ok-list" @click="showOkItems = !showOkItems">{{ showOkItems ? '收起通过项' : `显示通过项（${precheckOkItems.length}）` }}</el-button>
            <div v-if="showOkItems" id="precheck-ok-list">
              <div v-for="it in precheckOkItems" :key="it.item" class="precheck-row ok">
                <span class="precheck-dot">✓</span>
                <span class="precheck-label">{{ it.label }}</span>
                <span class="precheck-detail">{{ it.detail }}</span>
              </div>
            </div>
          </div>
          <div class="resume-box">
            <strong>断点/起点：</strong>{{ precheck.conclusion }}
            <span v-if="precheck.checkpoint.exists"> · checkpoint 文件 {{ precheck.checkpoint.file }}（更新于 {{ precheck.checkpoint.updated_at ? new Date(precheck.checkpoint.updated_at).toLocaleString() : '-' }}）</span>
          </div>
          <div class="control-actions" style="margin-top: 8px;" v-if="precheck.checkpoint.exists && !isActive">
            <el-button type="danger" plain size="small" @click="resetCheckpoint">重置断点（危险）</el-button>
          </div>
        </el-card>
      </el-tab-pane>
    </el-tabs>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient, { setAuthToken } from '../api'
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
  // MS-11f 笔① c: anomaly-stop alarm ring (running→stopped with no audited
  // operator stop) — absent while no alarm has fired in this web process.
  alarms?: CDCAlarmRow[]
}

interface CDCAlarmRow {
  ts: string
  message: string
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

// MS-11c C: three-tab layout — overview is the default landing tab.
const activeTab = ref<'overview' | 'conn' | 'precheck'>('overview')

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

// MS-11c ②: precheck attention split — ok items fold away, warn/fail stay.
const precheckAttentionItems = computed(() => (precheck.value?.items || []).filter(it => it.level !== 'ok'))
const precheckOkItems = computed(() => (precheck.value?.items || []).filter(it => it.level === 'ok'))
const precheckHasFail = computed(() => (precheck.value?.items || []).some(it => it.level === 'fail'))
const showOkItems = ref(false)

// MS-11c ②: a failing precheck auto-activates the precheck tab (warn-only
// never steals focus — operator judgement stays on the overview).
// MS-11c pen4 P2-①: hijack suppression — only the no-fail→fail TRANSITION
// fires, and once the operator manually leaves the tab this fail episode
// never drags them back (reset when fail clears).
let precheckFailActive = false
let userLeftPrecheckTab = false
watch(precheck, p => {
  const hasFail = !!(p && (p.items || []).some(it => it.level === 'fail'))
  if (!hasFail) {
    precheckFailActive = false
    userLeftPrecheckTab = false
    return
  }
  if (!precheckFailActive && !userLeftPrecheckTab) {
    precheckFailActive = true
    activeTab.value = 'precheck'
  }
})
watch(activeTab, t => {
  if (t !== 'precheck') userLeftPrecheckTab = true
})

// MS-11a: source-aware copy. Same truth as the REPLICA IDENTITY guard in the
// precheck tab — connCfg.source.type. PG branch keeps every string byte-identical.
const srcIsMySQL = computed(() => connCfg.value?.source?.type === 'mysql')

// MS-11a pen 4c: any source switch (saveConn / importConn / importFromDS all
// funnel through loadConnConfig) re-runs the slot fetch — the mysql branch
// clears a stale PG slot card immediately instead of waiting a poll cycle.
watch(srcIsMySQL, () => refreshSlot())

async function loadConnConfig() {
  try {
    const { data } = await apiClient.getCDCConfig()
    connCfg.value = data
  } catch (e) {
    // MS-11a pen 4b: a silent failure here would freeze the page on PG-shaped
    // copy forever — make it visible once instead.
    console.error('load CDC config failed', e)
    ElMessage.warning('CDC 配置加载失败，页面显示可能不准确')
  }
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

async function saveConn(retriedAuth = false) {
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
    if (e.response?.status === 401 && !retriedAuth && await promptForToken()) {
      savingConn.value = false
      return saveConn(true)
    }
    connError.value = true
    connMsg.value = e.response?.data?.error || e.message || '保存失败'
  } finally {
    savingConn.value = false
  }
}

async function importConn(retriedAuth = false) {
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
    if (e.response?.status === 401 && !retriedAuth && await promptForToken()) {
      return importConn(true)
    }
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

async function importFromDS(retriedAuth = false) {
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
    if (e.response?.status === 401 && !retriedAuth && await promptForToken()) {
      importingDS.value = false
      return importFromDS(true)
    }
    connError.value = true
    connMsg.value = e.response?.data?.error || e.message || '导入失败'
  } finally {
    importingDS.value = false
  }
}

const precheckError = ref('')

async function runPrecheck() {
  checking.value = true
  try {
    const { data } = await apiClient.cdcPrecheck()
    precheck.value = data
    precheckError.value = ''
  } catch (e: any) {
    // MS-11c pen4 P2-④: never swallow — the visible stale result must be
    // labeled stale, and the red banner lives in the precheck tab.
    precheckError.value = e?.response?.data?.error || e?.message || '网络错误'
  } finally {
    checking.value = false
  }
}

async function refreshSlot() {
  // Slot view is a PG-only surface; skip the fetch for MySQL chains.
  if (srcIsMySQL.value) {
    slotView.value = null
    return
  }
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

async function executeNoPKFix(tables: string[], retriedAuth = false) {
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
    if (e.response?.status === 401 && !retriedAuth && await promptForToken()) {
      return executeNoPKFix(tables, true)
    }
    ElMessage.error('执行失败：' + (e.response?.data?.error || e.message))
  } finally {
    noPKBusy.value = false
  }
}

async function resetCheckpoint(retriedAuth = false) {
  let answer: string
  try {
    const { value } = await ElMessageBox.prompt(
      `危险操作：删除 checkpoint 断点文件。重启后将从${srcIsMySQL.value ? ' checkpoint 记录的 binlog 位点重放' : ' slot restart_lsn 重放'}（宁重放不丢数据）。输入 DELETE 确认：`,
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
    if (e.response?.status === 401 && !retriedAuth && await promptForToken()) {
      return resetCheckpoint(true)
    }
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

// MS-11c D: 位点推进 history — parse the source-native position string into a
// monotonic number so the sparkline advances as replication moves: PG LSN
// "X/Y" hex → byte offset; MySQL "file:pos" → file index * 1e9 + pos; bare
// integers pass through. Unparseable strings skip the sample (no fake flat).
const positionHistory = ref<number[]>([])
const positionLatest = ref('')
// MS-11e G: a position string that PARSES but exceeds Number.MAX_SAFE_INTEGER
// cannot enter the sparkline (silent precision loss = fake flat/wrong slope);
// the raw string display is unaffected — we only flag the skipped sampling.
const positionPrecisionRisk = ref(false)
function parsePosition(lsn?: string): number | null {
  if (!lsn) return null
  // MS-11c pen4 P3-a: every branch funnels into the finite/non-negative gate —
  // Infinity (overflow) and bare negatives never enter the history.
  // MS-11e G: unsafe integers (>2^53) are rejected too — parseInt keeps
  // parsing past the safe range and the low bits rot silently.
  const accept = (n: number): number | null => {
    if (!Number.isFinite(n) || n < 0) return null
    if (!Number.isSafeInteger(n)) {
      positionPrecisionRisk.value = true
      return null
    }
    // MS-11e pen7: a SAFE sample clears the risk flag — the banner says
    // "paused", so it must not outlive the pause.
    positionPrecisionRisk.value = false
    return n
  }
  if (lsn.includes('/')) {
    const [hi, lo] = lsn.split('/')
    const h = parseInt(hi, 16), l = parseInt(lo, 16)
    if (Number.isNaN(h) || Number.isNaN(l)) return null
    return accept(h * 0x100000000 + l)
  }
  const idx = lsn.lastIndexOf(':')
  if (idx > 0) {
    // MS-11c pen4 P3-b: only the digits after the LAST '.' are the rotation
    // index (mysql-bin.000123 — the "2" in "bin2" is not part of it).
    const file = parseInt((lsn.slice(0, idx).split('.').pop() || '').replace(/\D/g, ''), 10)
    const pos = parseInt(lsn.slice(idx + 1), 10)
    if (Number.isNaN(pos)) return null
    return accept((Number.isNaN(file) ? 0 : file) * 1e9 + pos)
  }
  return accept(parseInt(lsn, 10))
}
function pushPosition(lsn?: string) {
  const v = parsePosition(lsn)
  // MS-11e pen7: an unparseable/unsafe sample skips the history push, but
  // the latest-position readout still advances — freezing it at the last
  // safe value misrepresents "latest".
  if (lsn) positionLatest.value = lsn
  if (v === null) return
  positionHistory.value.push(v)
  if (positionHistory.value.length > 60) positionHistory.value.shift()
}

const pipelineStatus = computed<'running' | 'warn' | 'stopped'>(() => {
  if (isActive.value) return stats.value && stats.value.failed > 0 ? 'warn' : 'running'
  return 'stopped'
})
const pipelineBadges = computed(() => {
  const b: { label: string; value: string }[] = []
  if (status.value.lsn) b.push({ label: srcIsMySQL.value ? 'Binlog' : 'LSN', value: status.value.lsn })
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

// MS-11f 笔① c: anomaly-stop alarm rows, newest first (server ring is
// oldest-first).
const alarmRows = computed<CDCAlarmRow[]>(() => [...(status.value.alarms || [])].reverse())

function formatAlarmTS(ts: string): string {
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString()
}

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

// MS-11f 笔① d: 401 → token prompt. Single-flight flag guarantees no
// stacked dialogs; each gated op carries a retriedAuth flag so a wrong
// token prompts EXACTLY once per user action — the second 401 surfaces as
// an error line instead of another dialog (MS-11c toast lesson, seq83 #4).
let tokenPromptOpen = false

async function promptForToken(): Promise<boolean> {
  if (tokenPromptOpen) return false
  tokenPromptOpen = true
  try {
    const { value } = await ElMessageBox.prompt(
      '该操作需要管理令牌（对应服务端 config.yaml 的 security.token）：',
      '身份验证',
      { type: 'warning', confirmButtonText: '确定', cancelButtonText: '取消', inputType: 'password' },
    )
    const token = (value || '').trim()
    if (!token) return false
    setAuthToken(token)
    return true
  } catch {
    return false
  } finally {
    tokenPromptOpen = false
  }
}

const controlState = computed(() => control.value?.state || 'stopped')
const isActive = computed(() => ['running', 'starting', 'adopted'].includes(controlState.value))
const canStop = computed(() => ['running', 'starting', 'adopted'].includes(controlState.value))
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

// MS-11c ①: merged position card visibility — checkpoint lsn, live slot
// fields, or any config-from-status row (slot/publication/pid) present.
const hasPositionInfo = computed(() =>
  !!(checkpoint.value?.lsn || status.value.slot || status.value.publication || status.value.pid ||
    (slotView.value?.slot?.exists)))

async function callCDC(action: 'start' | 'stop', retriedAuth = false) {
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
    // MS-11f: 401 → one token prompt + retry; a 403 (server token unset)
    // falls through as a plain error line — no browser prompt can fix a
    // server-side config gap.
    if (e.response?.status === 401 && !retriedAuth && await promptForToken()) {
      busy.value = false
      return callCDC(action, true)
    }
    controlError.value = true
    controlMsg.value = e.response?.data?.error || e.response?.data?.message || e.message || '请求失败'
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
    // MS-11c D: feed the 位点推进 sparkline (status lsn first, checkpoint as
    // the fallback view when the status file has not caught up).
    pushPosition(statusRes?.lsn || cpRes?.lsn)
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
/* F-02 datasource import row（MS-11h：并 conn-actions 行内，wrapper 类清除，
   label/arrow/select 三子类保留） */
.ds-import-label { font-size: var(--tims-font-xs); color: var(--tims-text-2, #909399); } /* P1 巡检修复 #7 字号归一 */
.ds-import-arrow { color: var(--tims-text-2, #909399); }
.ds-import-select { width: 180px; }

.cdc-container {
  /* width & centering come from the shared .tims-page class */
}
.cdc-container code { word-break: break-all; } /* #t2 375 视口长 LSN/配置串防御性换行 */

/* MS-11c A: hero status bar — old status-card skin, now a flex row with the
   start/stop/refresh actions inlined on the right. */
.status-card {
  border-radius: var(--tims-radius); padding: 20px 24px; margin-bottom: 16px;
  box-shadow: var(--tims-shadow);
  display: flex; align-items: center; gap: 24px; flex-wrap: wrap;
}
.status-card.running { background: linear-gradient(135deg, #0b7a7a, #0d8484); color: #fff; }
.status-card.starting { background: linear-gradient(135deg, #2c4a8f, #3d5ea3); color: #fff; }
.status-card.stopped { background: #eef0f6; color: var(--tims-text-2); }
.status-card.halted { background: linear-gradient(135deg, #c02f2f, #d03a3a); color: #fff; }
.status-card.stale { background: linear-gradient(135deg, #9a5700, #a86200); color: #fff; }
.hero-main { flex: 1 1 320px; min-width: 0; }
.status-indicator { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
.status-dot { width: 12px; height: 12px; border-radius: 50%; background: #d9d9d9; flex: none; }
.status-dot.active { background: #fff; animation: pulse 2s infinite; }
.status-text { font-size: 18px; font-weight: 600; }
.hero-restarts { background: rgba(255, 255, 255, 0.18); padding: 1px 8px; border-radius: 10px; }
.status-meta { font-size: var(--tims-font-sm); opacity: 0.85; }
.hero-summary { display: flex; gap: 16px; margin-top: 4px; }
.hero-actions { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
.hero-refresh { flex: none; }
.hero-msg { width: 100%; }

@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.5; }
}

.stats-grid {
  display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px;
  margin-bottom: 16px;
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

/* MS-11c C: tab layout */
.cdc-tabs { margin-top: 4px; }
.cdc-tabs :deep(.el-tabs__content) { overflow: visible; } /* confirm/prompt popups inside tabs */
.tab-dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-left: 6px; vertical-align: middle; }
.tab-dot.fail { background: #f5222d; }
.tab-dot.warn { background: #faad14; }

/* P1 巡检修复 #9：tag 文字换 AA 深色（#389e0d/#d48806 不足 4.5:1）；胶囊圆角保留 */
.control-restarts { font-size: 12px; color: var(--tims-tag-warning-text); }
.control-actions { display: flex; gap: 12px; align-items: center; }
.control-msg { font-size: var(--tims-font-sm); color: var(--tims-ok-text); } /* P1 巡检修复 #9：#52c41a → AA 绿；字号 #7 */
.control-msg.error { color: #cf1322; }
.stopped .control-msg { color: var(--tims-ok-text); }
.stopped .control-msg.error { color: #cf1322; }

/* P1 巡检修复 #7：div → el-card，保留 dashed 边框语义；灰阶/字号统一 */
.disabled-card { border-style: dashed; text-align: center; color: var(--tims-text-2); }

/* MS-11f 笔① c: anomaly-stop alarm rows — same semantic-red family as the
   error card (#cf1322 / #fff1f0 AA contrast), mono timestamp. */
.alarm-card {
  background: #fff1f0; border: 1px solid #ffccc7; border-radius: var(--tims-radius);
  padding: 10px 16px; margin-bottom: 16px;
}
.alarm-row { display: flex; gap: 12px; align-items: baseline; padding: 3px 0; }
.alarm-time { font-family: var(--tims-font-mono); font-size: 12px; color: #cf1322; flex-shrink: 0; }
.alarm-text { font-size: var(--tims-font-sm); color: #cf1322; }
.disabled-card :deep(.el-card__body) { padding: 32px 24px; }
.disabled-card h3 { font-size: var(--tims-font-md); color: var(--tims-text); margin-bottom: 12px; }
.disabled-card p { font-size: 14px; margin: 6px 0; }
.disabled-card .hint { color: var(--tims-text-2); font-size: var(--tims-font-sm); }
.disabled-card code { background: #f0f0f0; padding: 2px 6px; border-radius: 4px; font-size: 12px; }
.auto-refresh { font-size: 12px; color: var(--tims-text-2); } /* P1 巡检修复 #7 灰阶统一 */

/* MS-11h（seq192 落地令）：源/目标双栏对照卡 + 参数标签行 + 操作区统一。
   旧 conn-grid/conn-edit 编辑表单形态由双栏卡原位编辑取代（类清除）。 */
.conn-cards { display: grid; grid-template-columns: 1fr 32px 1fr; gap: 0 12px; align-items: stretch; margin: 12px 0 4px; }
.conn-side { border: 1px solid var(--el-border-color-light, #e4e7ed); border-radius: 8px; padding: 10px 14px 12px; background: var(--el-fill-color-extra-light, #fafbfc); }
.conn-side-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 6px; padding-bottom: 6px; border-bottom: 1px dashed var(--el-border-color-lighter, #ebeef5); }
.conn-side-title { font-weight: 600; font-size: 13px; }
.conn-side-type { font-size: var(--tims-font-xs, 12px); color: var(--tims-text-2, #909399); border: 1px solid var(--el-border-color-light, #e4e7ed); border-radius: 4px; padding: 0 6px; line-height: 18px; }
.conn-field { display: grid; grid-template-columns: 56px 1fr; gap: 8px; align-items: center; padding: 3px 0; font-size: 12px; min-height: 28px; }
.conn-field > span { color: var(--tims-text-2, #909399); }
.conn-field-num { width: 100%; }
.conn-nopwd { color: #cf1322; }
.conn-flow { display: flex; align-items: center; justify-content: center; color: var(--tims-text-2, #909399); font-size: 18px; }
.conn-params { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 10px; }
.conn-param { display: inline-flex; align-items: baseline; gap: 6px; border: 1px solid var(--el-border-color-light, #e4e7ed); border-radius: 6px; padding: 3px 10px; font-size: 12px; background: var(--el-bg-color, #fff); }
.conn-param-k { color: var(--tims-text-2, #909399); }
.conn-actions { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin-top: 12px; padding-top: 12px; border-top: 1px solid var(--el-border-color-lighter, #ebeef5); }
@media (max-width: 900px) {
  .conn-cards { grid-template-columns: 1fr; }
  .conn-flow { transform: rotate(90deg); padding: 2px 0; }
}

/* A2 precheck panel */
.precheck-fold { margin-top: 4px; border-bottom: 1px dashed #f0f0f0; }
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
