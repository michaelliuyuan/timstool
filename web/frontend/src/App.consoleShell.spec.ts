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

  it('状态胶囊四档：pass / hint / warn / fail（底色按样张四档）', () => {
    const c = css()
    for (const cls of ['.tims-pill--pass', '.tims-pill--hint', '.tims-pill--warn', '.tims-pill--fail']) {
      expect(c).toContain(cls)
    }
    expect(c).toContain('border-radius: 999px;')
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

  it('el-tag 全站胶囊化（状态胶囊系统化落点；info 档转 hint 蓝）', () => {
    const c = css()
    expect(c).toContain('.el-tag { border-radius: 999px; font-weight: 600; }')
    expect(c).toContain('.el-tag.el-tag--info:not(.el-tag--dark) {')
    expect(c).toContain('--el-tag-text-color: var(--tims-hint-text);')
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
