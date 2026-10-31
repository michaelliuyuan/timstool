// v3.20 方案A（云控制台风）shell + theme anchors（leader 派单 14:09）:
// ① 224px 宽侧栏分组导航（概览/迁移/结构校验/数据同步），图标+单行文字，
//    #8 两行等高档（min-height:71px）随 88px 窄栏方案退役；
// ② 顶栏改浅色细面包屑（工作台 / 当前页，取 route.meta.title），心跳+版本徽章保留；
// ③ 工作区留白加大（26×32）；
// ④ ≤900px 响应式收敛为图标轨（标签隐藏）——护栏不动；
// ⑤ design-tokens.css：状态胶囊四档（pass/hint/warn/fail）、连接器类型色徽章
//    （PG #336791 / MySQL #E97F0F / TiDB 品牌红 / Mongo #12805C）、卡片标题色条、
//    表格斑马纹+悬浮态、el-tag 全站胶囊化、空状态通用样式。
// Assertion shape = spec exact match（沿用仓库口径，不做页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('v3.20 方案A: 宽侧栏分组导航（console shell）', () => {
  it('nav 分四组：概览 / 迁移 / 结构校验 / 数据同步；工作台挂在 / 下', () => {
    const s = src('App.vue')
    for (const g of ['概览', '迁移', '结构校验', '数据同步']) {
      expect(s).toContain(`label: '${g}'`)
    }
    expect(s).toContain(`{ path: '/', label: '工作台', icon: 'Odometer' }`)
  })

  it('CDC 特性门控保持（未启用时整项隐藏）', () => {
    const s = src('App.vue')
    expect(s).toContain(`if (!cdcEnabled.value) syncItems.shift()`)
    expect(s).toContain(`{ path: '/cdc', label: 'CDC 实时同步', icon: 'DataLine' }`)
  })

  it('侧栏 224px + 单行标签：两行等高档已退役，长标签省略号收敛不折行', () => {
    const s = src('App.vue')
    expect(s).toContain('width: 224px')
    expect(s).toContain('text-overflow: ellipsis;')
    expect(s).not.toContain('min-height: 71px')
    expect(s).not.toContain('-webkit-line-clamp')
  })

  // bf 返修（刘源 18:55 批复 B1.1 加档，样张 B11 pane，FE 3.20.2）：
  // 基础档 行距 8px / 行高 42px 档（padding 12px 12px）/ 图标 18 / 组头 teal 圆点；
  // ≤880 笔记本档同步放宽（gap 4 / 8px 12px / 14-6-6），≤680 最小回退档不动。
  it('B1.1 加档：nav gap 8px / item 12px 12px / 组头 22-8-10 + teal 圆点 / first-child 4px', () => {
    const s = src('App.vue')
    // 正锚：基础档新密度与圆点（B11 样张口径）。
    expect(s).toContain('<el-icon :size="18">')
    expect(s).toMatch(/\.tims-nav \{[^}]*gap: 8px;/)
    expect(s).toContain('gap: 14px;')
    expect(s).toContain('padding: 12px 12px;')
    expect(s).toContain('margin: 22px 8px 10px;')
    expect(s).toContain('.tims-nav-group:first-child { margin-top: 4px; }')
    expect(s).toContain('.tims-nav-group::before {')
    expect(s).toContain('border-radius: 50%;')
    expect(s).toContain('margin-right: 7px;')
    // 正锚：≤880 笔记本档放宽（MC-19 旧压缩档退役，1366×768 观感受益）。
    expect(s).toContain('.tims-nav { gap: 4px; }')
    expect(s).toContain('.tims-nav-item { padding: 8px 12px; }')
    expect(s).toContain('.tims-nav-group { margin: 14px 6px 6px; }')
    // 负锚：组头 border-top 分隔线不回归；B1/B1.1 旧密度值退场（nav gap 6px 用
    // 块作用域负锚——顶栏 .tims-heartbeat 的 gap:6px 与 nav 无关，勿用全局）。
    expect(s).not.toMatch(/\.tims-nav-group[^{]*\{[^}]*border-top/)
    expect(s).not.toContain(':size="17">')
    expect(s).not.toMatch(/\.tims-nav \{[^}]*gap: 6px;/)
    expect(s).not.toContain('padding: 10px 12px;')
    expect(s).not.toContain('margin: 18px 8px 8px;')
    expect(s).not.toContain('margin-top: 2px;')
    expect(s).not.toContain('.tims-nav { gap: 1px; }')
    expect(s).not.toContain('.tims-nav-item { padding: 6px 10px; }')
    expect(s).not.toContain('.tims-nav-group { margin: 8px 6px 3px; }')
    // 窄轨（≤900px）圆点隐藏，标签隐藏逻辑不变。
    expect(s).toContain('.tims-nav-group::before { display: none; }')
    expect(s).toContain('.tims-nav-label { display: none; }')
  })

  it('顶栏浅色面包屑（工作台 / 当前页）；心跳与版本徽章保留', () => {
    const s = src('App.vue')
    expect(s).toContain('class="tims-crumb"')
    expect(s).toContain(`route.meta.title as string`)
    expect(s).toContain(`if (route.path === '/') return ''`)
    // 心跳轮询 + 版本徽章（MS-11i pen1 构建注入形态）原样保留。
    expect(s).toContain(`tims-version tims-mono">V{{ appVersion }}`)
    expect(s).toContain('const appVersion = __APP_VERSION__')
    expect(s).toContain(`fetch('/api/v1/tasks')`)
    // 浅色顶栏：白底 + 浅描边（原深色顶栏底/描边退场；侧栏深色墨蓝保留）。
    const topbar = s.match(/\.tims-topbar \{[^}]*\}/)?.[0] || ''
    expect(topbar).toContain('background: var(--tims-card);')
    expect(topbar).toContain('border-bottom: 1px solid var(--tims-border);')
    expect(topbar).not.toContain('var(--tims-ink)')
  })

  it('工作区留白加大（样张 spacing 26×32）', () => {
    expect(src('App.vue')).toContain('padding: 26px 32px;')
  })

  it('≤900px 响应式收敛为图标轨（护栏保持）', () => {
    const s = src('App.vue')
    expect(s).toContain('@media (max-width: 900px)')
    expect(s).toContain('.tims-nav-label { display: none; }')
    expect(s).toContain('.tims-workspace { padding: 14px; }')
  })
})

describe('v3.20 方案A: 全站主题系统（design-tokens.css）', () => {
  const css = () => src('styles/design-tokens.css')

  it('状态胶囊四档：pass / hint / warn / fail（底色按样张四档）+ 中性 muted 档', () => {
    const c = css()
    for (const cls of ['.tims-pill--pass', '.tims-pill--hint', '.tims-pill--warn', '.tims-pill--fail']) {
      expect(c).toContain(cls)
    }
    expect(c).toContain('border-radius: 999px;')
    // bf 返修 裁定①：中性运行态/未决档 muted（灰底 AA 字，不参与四档结论语义）。
    expect(c).toContain('.tims-pill--muted { background: #f4f4f5; color: var(--tims-tag-info-text); }')
  })

  it('hint 提示蓝档令牌 + 连接器类型色（PG/MySQL/TiDB/Mongo）', () => {
    const c = css()
    expect(c).toContain('--tims-hint: #5873b0;')
    expect(c).toContain('--tims-hint-text: #4a619a;')
    expect(c).toContain('--tims-pg: #336791;')
    expect(c).toContain('--tims-mysql: #e97f0f;')
    expect(c).toContain('--tims-mongo: #12805c;')
    for (const cls of ['.tims-conn-dot.is-pg', '.tims-conn-dot.is-mysql', '.tims-conn-dot.is-tidb', '.tims-conn-dot.is-mongodb']) {
      expect(c).toContain(cls)
    }
    // TiDB 用品牌红令牌而非新色值（品牌一致性）。
    expect(c).toContain('.tims-conn-dot.is-tidb { background: var(--tims-brand); }')
  })

  it('el-tag 全站胶囊化（状态胶囊系统化落点）', () => {
    const c = css()
    expect(c).toContain('.el-tag { border-radius: 999px; font-weight: 600; }')
  })

  it('卡片标题色条：el-card 头部全局 teal 短条 + is-red 变体', () => {
    const c = css()
    expect(c).toContain('.el-card__header::before {')
    expect(c).toContain('.el-card__header.tims-card-title--red::before { background: var(--tims-brand); }')
  })

  it('表格斑马纹 + 悬浮态（全站 el-table 统一，$index 档 expand/fixed 不错位）', () => {
    const c = css()
    expect(c).toContain('.el-table .el-table__row--striped td.el-table__cell {')
    expect(c).toContain('--tims-table-zebra: #fafbfd;')
    expect(c).toContain('--tims-table-hover: #f2f6f6;')
    // 悬浮档：teal 一档淡底，stray nth-child 奇偶法已否（expand 行插队错位）。
    expect(c).not.toContain('.el-table__row:nth-child(even)')
  })

  it('空状态通用样式（tims-empty 插画+引导容器 + el-empty 收敛）', () => {
    const c = css()
    expect(c).toContain('.tims-empty {')
    expect(c).toContain('.tims-empty .tims-empty-title {')
    expect(c).toContain('.el-empty { padding: 30px 20px; }')
  })
})

// bf 返修（leader 15:06 裁定②）：hint 蓝收敛——info 档回归灰档中性，
// hint 蓝 = 跨方言提示语义专属（v3.20.x 四档口径），仅经专用类落位。
describe('裁定②: hint 蓝收敛（info 回归灰档；hint=跨方言提示专属）', () => {
  it('tokens: 全局 el-tag--info→hint 蓝映射已删（负锚）；专用类 .tims-tag-hint 落 hint 蓝', () => {
    const c = src('styles/design-tokens.css')
    // 负锚：info 档规则不得再耦合任何 hint 令牌（全局蓝映射退场）。
    expect(c).not.toMatch(/el-tag--info[^{}]*--tims-hint/)
    // 正锚：专用类是全站唯一染 hint 蓝的 el-tag 通道。
    expect(c).toContain('.el-tag.tims-tag-hint:not(.el-tag--dark) {')
    expect(c).toContain('--el-tag-bg-color: var(--tims-hint-soft);')
    expect(c).toContain('--el-tag-text-color: var(--tims-hint-text);')
    // info 档灰锚：既有 AA 灰字规则保留，中性标签（已创建/已取消等 20+ 处）回归灰。
    expect(c).toContain('.el-tag.el-tag--info:not(.el-tag--dark) { --el-tag-text-color: var(--tims-tag-info-text); }')
  })

  it('DdlCompareView: hint verdict 落专用类 tims-tag-hint（总体结论/类别/表行/历史四处）', () => {
    const s = src('views/DdlCompareView.vue')
    expect(s).toContain(`const verdictClass = (v: string) => (v === 'hint' ? 'tims-tag-hint' : '')`)
    expect(s).toContain(':class="verdictClass(result.overall)"')
    expect(s).toContain(':class="verdictClass(row.verdict)"')
    expect(s).toContain(`:class="!row.source_only && !row.target_only ? verdictClass(row.verdict) : ''"`)
    expect(s).toContain(':class="verdictClass(row.overall)"')
    // hint 档 EP 基型仍为 info（中性基型 + 专用类染蓝，两档解耦）。
    expect(s).toContain(`v === 'hint' ? 'info'`)
  })

  it('TaskList 等中性状态标签保持灰档 info（不染 hint 蓝）', () => {
    const s = src('views/TaskListView.vue')
    expect(s).toContain(`created: { type: 'info', label: '已创建' },`)
    expect(s).toContain(`cancelled: { type: 'info', label: '已取消' },`)
    // 中性标签不声明 hint 专用类（非蓝锚）。
    expect(s).not.toContain('tims-tag-hint')
  })
})

// B3 可折叠分组（刘源 21:08 点单「B3+4/5/6」，leader 21:08 派单）：组头
// button 化 + 计数徽章 + 箭头旋转；折叠态 localStorage 持久化；路由进入
// 已折叠组自动展开；≤900 图标轨折叠不适用（徽章/箭头隐藏、项目强制直显）。
describe('B3 侧栏可折叠分组', () => {
  it('组头 button 化：aria-expanded + 计数徽章 + 折叠箭头旋转 + 整组 v-show 收纳', () => {
    const s = src('App.vue')
    expect(s).toContain(`type="button"`)
    expect(s).toContain(`class="tims-nav-group"`)
    expect(s).toContain(`:aria-expanded="!collapsed[section.label]"`)
    expect(s).toContain(`:aria-controls="\`tims-nav-gp-\${si}\`"`)
    expect(s).toContain(`@click="toggleGroup(section.label)"`)
    expect(s).toContain(`{{ section.items.length }}`)
    expect(s).toContain(`:class="{ 'is-collapsed': collapsed[section.label] }"`)
    expect(s).toContain(`v-show="!collapsed[section.label]"`)
    expect(s).toContain(`transform: rotate(-90deg);`)
  })

  it('折叠态 localStorage 持久化（tims-nav-collapse 单键 JSON，损坏回落全展开）', () => {
    const s = src('App.vue')
    expect(s).toContain(`const NAV_COLLAPSE_KEY = 'tims-nav-collapse'`)
    expect(s).toContain(`localStorage.getItem(NAV_COLLAPSE_KEY)`)
    expect(s).toContain(`localStorage.setItem(NAV_COLLAPSE_KEY, JSON.stringify(collapsed.value))`)
  })

  it('路由进入已折叠组自动展开（活动项必须可见）；rail 模式折叠不适用强制直显', () => {
    const s = src('App.vue')
    expect(s).toContain(`s.items.some((i) => isActive(i.path))`)
    expect(s).toContain(`.tims-nav-count { display: none; }`)
    expect(s).toContain(`.tims-nav-arrow { display: none; }`)
    expect(s).toContain(`.tims-nav-items { display: flex !important; }`)
  })

  // 差异①回归守卫（te 实证 / leader 21:27 裁决，FE 3.22.1）：整页刷新时
  // useRoute() 初值是 START_LOCATION（path='/'）——自动展开首跑判定必须挂
  // router.isReady() 就绪后；无 immediate 的 watch 管后续导航。旧形态
  // `{ immediate: true }` 会在非概览路由 reload 时误展概览组并覆写持久化。
  it('差异①守卫：初始路由判定挂 router.isReady()，无 immediate 瞬态误展', () => {
    const s = src('App.vue')
    expect(s).toContain(`const expandActiveGroup = () => {`)
    expect(s).toContain(`router.isReady().then(expandActiveGroup)`)
    expect(s).toContain(`watch(() => route.path, expandActiveGroup)`)
    expect(s).not.toContain(`{ immediate: true }`)
  })
})
