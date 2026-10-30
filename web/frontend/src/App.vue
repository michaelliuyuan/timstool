<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'

const router = useRouter()
const route = useRoute()

// Build-stamped app version (vite define from package.json — MS-11i pen1).
// Bridge binding: template identifiers resolve lexically, so the global
// __APP_VERSION__ must be pulled into setup scope to render.
const appVersion = __APP_VERSION__

// Optional modules: CDC is hidden unless /features reports it enabled (D3 #t53).
const cdcEnabled = ref(false)

// Global task heartbeat: green pulse when any migration task is running,
// gray otherwise. Polled every 5s alongside the features probe.
const runningCount = ref(0)
const backendUp = ref(true)
let heartbeatTimer: number | undefined

async function pollHeartbeat() {
  try {
    const r = await fetch('/api/v1/tasks')
    backendUp.value = r.ok
    if (r.ok) {
      const tasks = await r.json()
      runningCount.value = Array.isArray(tasks) ? tasks.filter((t: any) => t.status === 'running').length : 0
    }
  } catch {
    backendUp.value = false
  }
}

onMounted(async () => {
  try {
    const r = await fetch('/api/v1/features')
    if (r.ok) cdcEnabled.value = !!(await r.json())?.cdc?.enabled
  } catch {
    // default: CDC hidden
  }
  pollHeartbeat()
  heartbeatTimer = window.setInterval(pollHeartbeat, 5000)
})
onUnmounted(() => window.clearInterval(heartbeatTimer))

// S1-UI-06: nav grouped — a "数据同步" section keeps the two sync modules
// (CDC real-time vs timestamp-watermark backfill) visibly adjacent and named
// apart. Routes/API untouched.
const navSections = computed(() => {
  const syncItems = [
    { path: '/cdc', label: 'CDC 实时同步', icon: 'DataLine' },
    { path: '/incremental', label: '时间戳水位补齐', icon: 'Timer' },
  ]
  if (!cdcEnabled.value) syncItems.shift()
  return [
    {
      label: '',
      items: [
        { path: '/datasources', label: '数据源', icon: 'Coin' },
        { path: '/wizard', label: '新建迁移', icon: 'Connection' },
        { path: '/tasks', label: '任务监控', icon: 'Monitor' },
        { path: '/history', label: '迁移历史', icon: 'Clock' },
        { path: '/compare', label: '数据比对', icon: 'Grid' },
        { path: '/mongocompare/new', label: 'MongoDB 对比', icon: 'Switch' },
        { path: '/assess', label: '兼容评估', icon: 'DataAnalysis' },
        { path: '/ddl-export', label: 'DDL 导出', icon: 'Download' },
        { path: '/ddl-compare', label: 'DDL 对比', icon: 'Document' },
      ],
    },
    { label: '数据同步', items: syncItems },
  ]
})

const isActive = (path: string) => route.path === path || route.path.startsWith(path + '/')
</script>

<template>
  <div class="tims-shell">
    <aside class="tims-sidenav">
      <div class="tims-logo" @click="router.push('/wizard')" title="TiMS">
        <span class="tims-logo-ti">Ti</span><span class="tims-logo-ms">MS</span>
      </div>
      <nav class="tims-nav">
        <template v-for="section in navSections" :key="section.label || 'main'">
          <div v-if="section.label" class="tims-nav-group">{{ section.label }}</div>
          <button
            v-for="item in section.items"
            :key="item.path"
            class="tims-nav-item"
            :class="{ 'is-active': isActive(item.path) }"
            @click="router.push(item.path)"
          >
            <el-icon :size="18"><component :is="item.icon" /></el-icon>
            <span class="tims-nav-label">{{ item.label }}</span>
          </button>
        </template>
      </nav>
    </aside>

    <div class="tims-main-col">
      <header class="tims-topbar">
        <div class="tims-topbar-title">异构数据迁移工作台</div>
        <div class="tims-topbar-right">
          <span class="tims-heartbeat" :class="runningCount > 0 ? 'is-running' : backendUp ? 'is-idle' : 'is-down'" :title="backendUp ? `${runningCount} 个任务运行中` : '后端不可达'">
            <i class="tims-heartbeat-dot"></i>
            <span class="tims-mono">{{ runningCount > 0 ? `${runningCount} 运行` : backendUp ? '空闲' : '离线' }}</span>
          </span>
          <span class="tims-version tims-mono">V{{ appVersion }}</span>
        </div>
      </header>
      <main class="tims-workspace">
        <router-view />
      </main>
    </div>
  </div>
</template>

<style scoped>
.tims-shell {
  display: flex;
  height: 100vh;
  overflow: hidden;
}

/* --- Left narrow nav --- */
.tims-sidenav {
  width: 88px;
  flex: none;
  background: var(--tims-ink);
  display: flex;
  flex-direction: column;
  align-items: stretch;
  padding: 14px 0 10px;
}
.tims-logo {
  text-align: center;
  font-size: 22px;
  font-weight: 900;
  letter-spacing: 0.5px;
  cursor: pointer;
  padding: 6px 0 16px;
  user-select: none;
}
.tims-logo-ti { color: var(--tims-brand); }
.tims-logo-ms { color: var(--tims-text-inv); }

.tims-nav {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 0 10px;
  /* MC-19 A：侧栏内滚动兜底——高度不足时 nav 自身内滚（shell 100vh+overflow:hidden
      锁页滚，被裁项原本完全不可达）；内容不溢出时零视觉差。 */
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  scrollbar-width: thin;
  scrollbar-color: var(--tims-border-ink) transparent;
}
.tims-nav::-webkit-scrollbar { width: 4px; }
.tims-nav::-webkit-scrollbar-thumb { background: var(--tims-border-ink); border-radius: 2px; }
.tims-nav-group {
  margin-top: 10px;
  padding: 2px 0 4px;
  text-align: center;
  font-size: var(--tims-font-xs);
  letter-spacing: 1px;
  color: var(--tims-text-inv-2);
  opacity: 0.65;
  border-top: 1px solid var(--tims-border-ink);
}
.tims-nav-item {
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 4px;
  padding: 10px 4px 8px;
  /* #8 侧栏导航等高：长标签（MongoDB 对比 / CDC 实时同步 / 时间戳水位补齐）
     在 88px 侧栏内必然折为两行（71px），nowrap 会溢出侧栏宽，故取「全员
     min-height 对齐两行档」方案——短标签项垂直居中撑到同高，文案零改动。 */
  min-height: 71px;
  border: none;
  border-radius: var(--tims-radius-s);
  background: transparent;
  color: var(--tims-text-inv-2);
  cursor: pointer;
  transition: background 0.15s ease, color 0.15s ease;
}
.tims-nav-item:hover { background: var(--tims-ink-3); color: var(--tims-text-inv); }
.tims-nav-item.is-active { background: var(--tims-ink-3); color: var(--tims-text-inv); }
.tims-nav-item.is-active::before {
  content: '';
  position: absolute;
  left: -10px;
  top: 10px;
  bottom: 8px;
  width: 3px;
  border-radius: 0 3px 3px 0;
  background: var(--tims-teal); /* 激活态随主操作色，红色仅保留危险语义 */
}
.tims-nav-label { font-size: 12px; letter-spacing: 0.2px; line-height: 1.3; text-align: center; }

/* --- Main column --- */
.tims-main-col { flex: 1; display: flex; flex-direction: column; min-width: 0; }

.tims-topbar {
  flex: none;
  height: 52px;
  background: var(--tims-ink);
  border-bottom: 1px solid var(--tims-border-ink);
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 20px;
}
.tims-topbar-title {
  color: var(--tims-text-inv);
  font-size: 14px;
  font-weight: 600;
  letter-spacing: 1px;
}
.tims-topbar-right { display: flex; align-items: center; gap: 16px; }

.tims-heartbeat {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--tims-font-xs);
  color: var(--tims-text-inv-2);
}
.tims-heartbeat-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #6b7280;
}
.tims-heartbeat.is-idle .tims-heartbeat-dot { background: var(--tims-text-inv-2); }
.tims-heartbeat.is-running .tims-heartbeat-dot {
  background: var(--tims-teal);
  animation: tims-pulse 1.6s ease-in-out infinite;
}
.tims-heartbeat.is-down .tims-heartbeat-dot { background: var(--tims-brand); }
@keyframes tims-pulse {
  0%, 100% { box-shadow: 0 0 0 0 rgba(15, 163, 163, 0.45); }
  50% { box-shadow: 0 0 0 5px rgba(15, 163, 163, 0); }
}

.tims-version { font-size: var(--tims-font-xs); color: var(--tims-text-inv-2); opacity: 0.8; }

.tims-workspace {
  flex: 1;
  overflow: auto;
  background: var(--tims-work);
  padding: 22px 26px;
}

/* --- Responsive: collapse to icon rail, tighter workspace --- */
@media (max-width: 900px) {
  .tims-sidenav { width: 60px; }
  /* #8 等高仅限带标签的宽侧栏；窄栏图标轨无折行，min-height 解除。 */
  .tims-nav-item { padding: 10px 0; min-height: 0; }
  .tims-nav-label { display: none; }
  .tims-topbar-title { display: none; }
  .tims-workspace { padding: 14px; }
}

/* --- MC-19 B：高度自适应分档（宽轨 >900px 专用；页面放大=视口高收缩，
     既有响应式只看宽度，恰落盲区。压缩优先全量直显，极限档回落 A 滚动）--- */
@media (max-height: 880px) and (min-width: 901px) {
  .tims-logo { padding: 4px 0 10px; }
  .tims-nav { gap: 3px; }
  .tims-nav-group { margin-top: 6px; }
  .tims-nav-item { min-height: 58px; padding: 7px 4px 6px; gap: 3px; }
  .tims-nav-label { line-height: 1.2; }
}
@media (max-height: 680px) and (min-width: 901px) {
  .tims-logo { font-size: 20px; padding: 2px 0 8px; }
  .tims-nav { gap: 2px; }
  .tims-nav-item { min-height: 50px; padding: 5px 4px 4px; gap: 2px; }
}
</style>
