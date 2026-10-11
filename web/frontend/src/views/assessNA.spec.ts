// MS-10b2 item 3 姊妹锚（裁③渲染层口径）：Applicable=false 的维度在
// AssessView 渲染为灰卡 N/A（不画假 100 进度条、不标兼容级别）；
// Applicable 判定为精确 === false（遗留载荷缺省该键=适用，零扰）。
// 断言形=spec 精确匹配（禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'AssessView.vue'), 'utf8')

describe('AssessView N/A dimension rendering (MS-10b2 item 3)', () => {
  it('DimResult declares the optional applicable field (absent = applicable)', () => {
    const src = viewSrc()
    expect(src).toContain('applicable?: boolean')
  })

  it('N/A rows render the gray note instead of a progress bar', () => {
    const src = viewSrc()
    // Exact tri-state check: only an explicit false is N/A.
    expect(src).toContain('v-if="row.applicable === false"')
    expect(src).toContain('N/A 不适用')
    // The progress bar must be the v-else branch (never rendered for N/A).
    const naBranch = src.slice(
      src.indexOf('v-if="row.applicable === false"'),
      src.indexOf('<el-progress', src.indexOf('v-if="row.applicable === false"'))
    )
    expect(naBranch).toContain('#9AA3AF')
    // v-else rides the el-progress tag itself (next line) - N/A rows
    // can never reach the bar.
    const progressIdx = src.indexOf('<el-progress', src.indexOf('v-if="row.applicable === false"'))
    expect(src.slice(progressIdx, progressIdx + 120)).toContain('v-else')
  })

  it('N/A rows render a dash, never a compatibility level', () => {
    const src = viewSrc()
    const compatCol = src.slice(src.indexOf('label="兼容性"'))
    expect(compatCol.slice(0, 400)).toContain('row.applicable === false')
  })
})
