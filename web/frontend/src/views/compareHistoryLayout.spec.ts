// 第8单锚点（2026-10-24 20:02 令，比对历史表布局再平衡）：
// 创建时间列由唯一弹性（UX②A 的 min-width=170，宽屏独占数百 px 富余）
// 回归定宽 180——族先例 TaskListView 同为 toLocaleString 全时间戳 +
// width=180，生产已验证不折行；名称列恢复弹性 min-width=220 +
// show-overflow-tooltip，宽屏富余宽度全部由名称承接（长名省略、悬停全名），
// ID/模式/状态/表进度/操作维持定宽，前列不再相对拥挤。
// 断言形态 = spec exact match（沿用 MS-11i 锚点方式，不做页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('compareHistoryLayout v3.19.30', () => {
  const s = src('CompareView.vue')

  it('名称 is the flexible slack-taker (min-width 220 + tooltip), not fixed', () => {
    expect(s).toContain('prop="name" label="名称" min-width="220" show-overflow-tooltip')
    expect(s).not.toContain('prop="name" label="名称" width="220"')
  })

  it('创建时间 back to fixed 180 (TaskListView family precedent), no longer flexes', () => {
    expect(s).toContain('label="创建时间" width="180" class-name="hide-sm"')
    expect(s).not.toContain('label="创建时间" min-width=')
  })

  it('front pinned columns stay put: ID 90 / 模式 130 / 状态 90 / 表进度 120 / 操作 140', () => {
    expect(s).toContain('prop="id" label="ID" width="90" class-name="hide-sm"')
    expect(s).toContain('label="模式" width="130"')
    expect(s).toContain('label="状态" width="90"')
    expect(s).toContain('label="表进度" width="120" class-name="hide-sm"')
    expect(s).toContain('label="操作" width="140"')
  })
})
