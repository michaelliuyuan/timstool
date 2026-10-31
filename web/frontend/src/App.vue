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

// v3.20 方案A（云控制台风）：宽侧栏分组导航——概览 / 迁移 / 结构校验 /
// 数据同步 四组（样张口径）；路由与 API 零改动，CDC 门控逻辑保持
// （D3 #t53：未启用时整项隐藏而非禁用）。
const navSections = computed(() => {
  const syncItems = [
    { path: '/cdc', label: 'CDC 实时同步', icon: 'DataLine' },
    { path: '/incremental', label: '时间戳水位补齐', icon: 'Timer' },
  ]
  if (!cdcEnabled.value) syncItems.shift()
  return [
    {
      label: '概览',
      items: [
        { path: '/', label: '工作台', icon: 'Odometer' },
      ],
    },
    {
      label: '迁移',
      items: [
        { path: '/datasources', label: '数据源', icon: 'Coin' },
        { path: '/wizard', label: '新建迁移', icon: 'Connection' },
        { path: '/tasks', label: '任务监控', icon: 'Monitor' },
        { path: '/history', label: '迁移历史', icon: 'Clock' },
      ],
    },
    {
      label: '结构校验',
      items: [
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

const isActive = (path: string) =>
  path === '/' ? route.path === '/' : route.path === path || route.path.startsWith(path + '/')

// v3.20 方案A：浅色顶栏面包屑「工作台 / 当前页」，当前页取路由 meta.title；
// 工作台自身（/）只有一级，不重复渲染第二级。
const crumbTitle = computed(() => {
  if (route.path === '/') return ''
  return (route.meta.title as string) || ''
})
</script>

<template>
  <div class="tims-shell">
    <aside class="tims-sidenav">
      <div class="tims-logo" @click="router.push('/')" title="TiMS">
        <span class="tims-logo-ti">Ti</span><span class="tims-logo-ms">MS</span>
        <span class="tims-logo-sub">迁移工作台</span>
      </div>
      <nav class="tims-nav">
        <template v-for="section in navSections" :key="section.label || 'main'">
          <div class="tims-nav-group">{{ section.label }}</div>
          <button
            v-for="item in section.items"
            :key="item.path"
            class="tims-nav-item"
            :class="{ 'is-active': isActive(item.path) }"
            @click="router.push(item.path)"
          >
            <el-icon :size="17"><component :is="item.icon" /></el-icon>
            <span class="tims-nav-label">{{ item.label }}</span>
          </button>
        </template>
      </nav>
    </aside>

    <div class="tims-main-col">
      <header class="tims-topbar">
        <nav class="tims-crumb" aria-label="面包屑">
          <span class="tims-crumb-home">工作台</span>
          <template v-if="crumbTitle">
            <span class="tims-crumb-sep">/</span>
            <b class="tims-crumb-cur">{{ crumbTitle }}</b>
          </template>
        </nav>
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

/* --- v3.20 方案A：224px 宽侧栏（深色墨蓝保留；图标+单行文字+分组标题） --- */
.tims-sidenav {
  width: 224px;
  flex: none;
  background: var(--tims-ink);
  display: flex;
  flex-direction: column;
  align-items: stretch;
  padding: 0 12px 12px;
}
.tims-logo {
  display: flex;
  align-items: baseline;
  gap: 2px;
  padding: 18px 6px 14px;
  font-size: 22px;
  font-weight: 900;
  letter-spacing: 0.5px;
  cursor: pointer;
  user-select: none;
  white-space: nowrap;
}
.tims-logo-ti { color: var(--tims-brand); }
.tims-logo-ms { color: var(--tims-text-inv); }
.tims-logo-sub {
  font-size: 11px;
  font-weight: 400;
  color: var(--tims-text-inv-2);
  letter-spacing: 1px;
  margin-left: 8px;
}

.tims-nav {
  display: flex;
  flex-direction: column;
  gap: 2px;
  /* MC-19 A：侧栏内滚动兜底——高度不足时 nav 自身内滚（shell 100vh+overflow:hidden
      锁页滚，被裁项原本完全不可达）；内容不溢出时零视觉差。 */
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  overflow-x: hidden;
  scrollbar-width: thin;
  scrollbar-color: var(--tims-border-ink) transparent;
}
.tims-nav::-webkit-scrollbar { width: 4px; }
.tims-nav::-webkit-scrollbar-thumb { background: var(--tims-border-ink); border-radius: 2px; }
.tims-nav-group {
  margin: 12px 6px 4px;
  font-size: 11px;
  letter-spacing: 1.5px;
  color: var(--tims-text-inv-2);
  opacity: 0.65;
  border-top: 1px solid var(--tims-border-ink);
  padding-top: 10px;
  white-space: nowrap;
}
/* 概览组贴顶：首组不画分隔线（其余组以细线分节）。 */
.tims-nav-group:first-child {
  border-top: none;
  margin-top: 0;
  padding-top: 0;
}
.tims-nav-item {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: flex-start;
  gap: 10px;
  padding: 8px 10px;
  border: none;
  border-radius: var(--tims-radius-s);
  background: transparent;
  color: var(--tims-text-inv-2);
  cursor: pointer;
  white-space: nowrap;
  transition: background 0.15s ease, color 0.15s ease;
}
.tims-nav-item:hover { background: var(--tims-ink-3); color: var(--tims-text-inv); }
.tims-nav-item.is-active { background: var(--tims-ink-3); color: var(--tims-text-inv); font-weight: 600; }
.tims-nav-item.is-active::before {
  content: '';
  position: absolute;
  left: -12px;
  top: 8px;
  bottom: 8px;
  width: 3px;
  border-radius: 0 3px 3px 0;
  background: var(--tims-teal); /* 激活态随主操作色，红色仅保留危险语义 */
}
/* v3.20 宽侧栏单行标签：不再需要 #8 的两行等高档（min-height:71px 已随
   88px 窄栏方案退役），超出 224px 的长标签以省略号收敛。 */
.tims-nav-label {
  font-size: 13px;
  letter-spacing: 0.2px;
  line-height: 1.3;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* --- Main column --- */
.tims-main-col { flex: 1; display: flex; flex-direction: column; min-width: 0; }

/* v3.20 方案A：浅色细顶栏 + 面包屑（工作台 / 当前页），心跳与版本保留。 */
.tims-topbar {
  flex: none;
  height: 48px;
  background: var(--tims-card);
  border-bottom: 1px solid var(--tims-border);
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 24px;
}
.tims-crumb {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: var(--tims-font-sm);
  color: var(--tims-text-2);
  min-width: 0;
}
.tims-crumb-home { white-space: nowrap; }
.tims-crumb-sep { color: var(--tims-border); font-weight: 400; }
.tims-crumb-cur {
  color: var(--tims-text);
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.tims-topbar-right { display: flex; align-items: center; gap: 16px; flex: none; }

.tims-heartbeat {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--tims-font-xs);
  color: var(--tims-text-2);
}
.tims-heartbeat-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--tims-text-2);
}
.tims-heartbeat.is-idle .tims-heartbeat-dot { background: var(--tims-text-2); }
.tims-heartbeat.is-running .tims-heartbeat-dot {
  background: var(--tims-teal);
  box-shadow: 0 0 0 4px rgba(13, 132, 132, 0.15);
  animation: tims-pulse 1.6s ease-in-out infinite;
}
.tims-heartbeat.is-down .tims-heartbeat-dot { background: var(--tims-brand); }
@keyframes tims-pulse {
  0%, 100% { box-shadow: 0 0 0 0 rgba(15, 163, 163, 0.45); }
  50% { box-shadow: 0 0 0 5px rgba(15, 163, 163, 0); }
}

.tims-version { font-size: var(--tims-font-xs); color: var(--tims-text-2); opacity: 0.8; }

/* v3.20 方案A：工作区留白加大（样张 spacing 26×32）。 */
.tims-workspace {
  flex: 1;
  overflow: auto;
  background: var(--tims-work);
  padding: 26px 32px;
}

/* --- Responsive: collapse to icon rail, tighter workspace --- */
@media (max-width: 900px) {
  .tims-sidenav { width: 60px; padding: 0 6px 10px; }
  .tims-logo { justify-content: center; }
  .tims-logo-sub { display: none; }
  .tims-nav-item { flex-direction: column; gap: 4px; padding: 10px 0; }
  .tims-nav-label { display: none; }
  .tims-nav-group {
    text-align: center;
    letter-spacing: 0.5px;
    border-top: none;
    padding-top: 4px;
  }
  .tims-crumb-home { display: none; }
  .tims-crumb-sep { display: none; }
  .tims-workspace { padding: 14px; }
}

/* --- MC-19 B：高度自适应分档（宽栏 >900px 专用；页面放大=视口高收缩，
     既有响应式只看宽度，恰落盲区。压缩优先全量直显，极限档收紧内距）--- */
@media (max-height: 880px) and (min-width: 901px) {
  .tims-logo { padding: 10px 6px 10px; }
  .tims-nav { gap: 1px; }
  .tims-nav-group { margin: 8px 6px 3px; padding-top: 8px; }
  .tims-nav-item { padding: 6px 10px; }
}
@media (max-height: 680px) and (min-width: 901px) {
  .tims-logo { font-size: 20px; padding: 6px 6px 6px; }
  .tims-nav-group { margin: 6px 6px 2px; padding-top: 6px; }
  .tims-nav-item { padding: 4px 10px; }
}
</style>
