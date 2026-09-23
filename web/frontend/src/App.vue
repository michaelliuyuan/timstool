<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'

const router = useRouter()
const route = useRoute()

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

const navItems = computed(() => {
  const items = [
    { path: '/wizard', label: '新建迁移', icon: 'Connection' },
    { path: '/tasks', label: '任务监控', icon: 'Monitor' },
    { path: '/history', label: '迁移历史', icon: 'Clock' },
    { path: '/compare', label: '数据比对', icon: 'Grid' },
    { path: '/assess', label: '兼容评估', icon: 'DataAnalysis' },
    { path: '/ddl-export', label: 'DDL 导出', icon: 'Download' },
  ]
  if (cdcEnabled.value) items.push({ path: '/cdc', label: 'CDC 增量', icon: 'DataLine' })
  return items
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
        <button
          v-for="item in navItems"
          :key="item.path"
          class="tims-nav-item"
          :class="{ 'is-active': isActive(item.path) }"
          @click="router.push(item.path)"
        >
          <el-icon :size="18"><component :is="item.icon" /></el-icon>
          <span class="tims-nav-label">{{ item.label }}</span>
        </button>
      </nav>
      <div class="tims-sidenav-foot tims-mono">PG → TiDB</div>
    </aside>

    <div class="tims-main-col">
      <header class="tims-topbar">
        <div class="tims-topbar-title">异构数据迁移工作台</div>
        <div class="tims-topbar-right">
          <span class="tims-heartbeat" :class="runningCount > 0 ? 'is-running' : backendUp ? 'is-idle' : 'is-down'" :title="backendUp ? `${runningCount} 个任务运行中` : '后端不可达'">
            <i class="tims-heartbeat-dot"></i>
            <span class="tims-mono">{{ runningCount > 0 ? `${runningCount} 运行` : backendUp ? '空闲' : '离线' }}</span>
          </span>
          <span class="tims-version tims-mono">V2.2</span>
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

.tims-nav { display: flex; flex-direction: column; gap: 4px; padding: 0 10px; }
.tims-nav-item {
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
  padding: 10px 4px 8px;
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
  background: var(--tims-brand);
}
.tims-nav-label { font-size: 12px; letter-spacing: 0.2px; }

.tims-sidenav-foot {
  margin-top: auto;
  text-align: center;
  font-size: 10px;
  color: var(--tims-text-inv-2);
  opacity: 0.6;
}

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
  font-size: 11.5px;
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

.tims-version { font-size: 11px; color: var(--tims-text-inv-2); opacity: 0.8; }

.tims-workspace {
  flex: 1;
  overflow: auto;
  background: var(--tims-work);
  padding: 22px 26px;
}

/* --- Responsive: collapse to icon rail, tighter workspace --- */
@media (max-width: 900px) {
  .tims-sidenav { width: 60px; }
  .tims-nav-item { padding: 10px 0; }
  .tims-nav-label { display: none; }
  .tims-topbar-title { display: none; }
  .tims-workspace { padding: 14px; }
}
</style>
