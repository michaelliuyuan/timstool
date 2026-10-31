<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
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
        { path: '/pipeline', label: '管线拓扑', icon: 'Share' },
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

// B3 可折叠分组（刘源 21:08 点单）：组头可点折叠（button 语义，原生 Enter/Space
// 触发 click）+ 计数徽章 + 箭头旋转过渡；默认全展开=原行为；折叠态 localStorage
// 持久化；路由进入已折叠组自动展开——活动项必须可见。
const NAV_COLLAPSE_KEY = 'tims-nav-collapse'
const collapsed = ref<Record<string, boolean>>({})
try {
  const saved = JSON.parse(localStorage.getItem(NAV_COLLAPSE_KEY) || '{}')
  if (saved && typeof saved === 'object') collapsed.value = saved
} catch {
  // 存储损坏回落全展开（默认态）
}
function persistCollapse() {
  try {
    localStorage.setItem(NAV_COLLAPSE_KEY, JSON.stringify(collapsed.value))
  } catch {
    // 隐私模式等写失败：仅本会话内生效
  }
}
const toggleGroup = (label: string) => {
  collapsed.value = { ...collapsed.value, [label]: !collapsed.value[label] }
  persistCollapse()
}
watch(
  () => route.path,
  () => {
    const sec = navSections.value.find((s) => s.items.some((i) => isActive(i.path)))
    if (sec && collapsed.value[sec.label]) {
      collapsed.value = { ...collapsed.value, [sec.label]: false }
      persistCollapse()
    }
  },
  { immediate: true },
)

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
        <template v-for="(section, si) in navSections" :key="section.label || 'main'">
          <button
            type="button"
            class="tims-nav-group"
            :aria-expanded="!collapsed[section.label]"
            :aria-controls="`tims-nav-gp-${si}`"
            @click="toggleGroup(section.label)"
          >
            <span class="tims-nav-group-label">{{ section.label }}</span>
            <span class="tims-nav-count" aria-hidden="true">{{ section.items.length }}</span>
            <span class="tims-nav-arrow" :class="{ 'is-collapsed': collapsed[section.label] }" aria-hidden="true">▾</span>
          </button>
          <div class="tims-nav-items" :id="`tims-nav-gp-${si}`" v-show="!collapsed[section.label]">
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
          </div>
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

/* --- v3.20 方案A：224px 宽侧栏（深色墨蓝保留；图标+单行文字+分组标题；
    B1.1 加档 3.20.2：行距 8px / 行高 42px 档 / 图标 18 / 组头 teal 圆点） --- */
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
  gap: 8px;
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
/* B3：组头升级为可点折叠按钮（原静态分组标题；布局=圆点+标签+计数徽章+箭头）。 */
.tims-nav-group {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  margin: 22px 8px 10px;
  padding: 0;
  border: none;
  background: transparent;
  font: inherit;
  font-size: 11px;
  letter-spacing: 1.5px;
  color: var(--tims-text-inv-2);
  opacity: 0.65;
  white-space: nowrap;
  text-align: left;
  cursor: pointer;
}
.tims-nav-group:hover { opacity: 0.9; }
.tims-nav-group-label { flex: 1; overflow: hidden; text-overflow: ellipsis; }
/* 计数徽章：弱化样式（组内项数，仅宽屏折叠形态披露）。 */
.tims-nav-count {
  flex: none;
  min-width: 16px;
  height: 16px;
  line-height: 16px;
  padding: 0 4px;
  border-radius: 8px;
  background: var(--tims-ink-3);
  font-size: 10px;
  letter-spacing: 0;
  text-align: center;
}
/* 折叠箭头：展开 ▾ 朝下，折叠态旋转 -90° 朝右（过渡 0.15s）。 */
.tims-nav-arrow {
  flex: none;
  font-size: 10px;
  line-height: 1;
  transition: transform 0.15s ease;
}
.tims-nav-arrow.is-collapsed { transform: rotate(-90deg); }
/* B3：组内项容器（折叠 v-show 整组收纳；间距承接 nav gap 8px）。 */
.tims-nav-items {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
/* B1.1 加档：组头分隔线退役，teal 4px 圆点替代（容器 opacity 下实收≈.59，
   样张 .75×.9≈.68 同档观感）。 */
.tims-nav-group::before {
  content: '';
  display: inline-block;
  flex: none;
  width: 4px;
  height: 4px;
  border-radius: 50%;
  background: var(--tims-teal);
  opacity: 0.9;
  margin-right: 7px;
  vertical-align: 2px;
}
/* 概览组贴顶：首组微距 4px（B1.1 样张）。 */
.tims-nav-group:first-child { margin-top: 4px; }
.tims-nav-item {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: flex-start;
  gap: 14px;
  padding: 12px 12px;
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
  /* B1：窄轨组头居中，teal 圆点隐藏（标签隐藏逻辑不变）。B3：折叠不适用
     图标轨——徽章/箭头隐藏，折叠残留的 v-show 内联 display 被 !important
     压制，项目强制直显。 */
  .tims-nav-group {
    justify-content: center;
    text-align: center;
    letter-spacing: 0.5px;
  }
  .tims-nav-group::before { display: none; }
  .tims-nav-count { display: none; }
  .tims-nav-arrow { display: none; }
  .tims-nav-items { display: flex !important; }
  .tims-crumb-home { display: none; }
  .tims-crumb-sep { display: none; }
  .tims-workspace { padding: 14px; }
}

/* --- MC-19 B：高度自适应分档（宽栏 >900px 专用；页面放大=视口高收缩，
     既有响应式只看宽度，恰落盲区。压缩优先全量直显，极限档收紧内距）--- */
@media (max-height: 880px) and (min-width: 901px) {
  .tims-logo { padding: 10px 6px 10px; }
  .tims-nav { gap: 4px; }
  .tims-nav-group { margin: 14px 6px 6px; }
  .tims-nav-items { gap: 4px; }
  .tims-nav-item { padding: 8px 12px; }
}
@media (max-height: 680px) and (min-width: 901px) {
  .tims-logo { font-size: 20px; padding: 6px 6px 6px; }
  .tims-nav-group { margin: 6px 6px 2px; }
  .tims-nav-item { padding: 4px 10px; }
}
</style>
