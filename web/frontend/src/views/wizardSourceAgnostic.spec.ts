// MS-10d 笔②姊妹锚：向导副标题通用形（10c 调整①同款口径）——类型集由
// DataSourcePicker 承载，副标题不再硬编码「PostgreSQL」；skip 四开关
// 全源有效（裁 b：零分域），cdc_chain 的 postgres gate 为 CapCDC 合法残面。
// 断言形=spec 精确匹配（禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'WizardView.vue'), 'utf8')

describe('WizardView source-agnostic subtitle + switch surface (MS-10d)', () => {
  it('subtitle is source-agnostic (no hardcoded PostgreSQL)', () => {
    const src = viewSrc()
    const header = src.slice(src.indexOf('<PageHeader'), src.indexOf('/>', src.indexOf('<PageHeader')))
    expect(header).toContain('配置源端数据库与目标端 TiDB')
    // The stale PG-only wording must be gone from the header line.
    expect(header).not.toContain('PostgreSQL')
  })

  it('the source picker carries the type set (postgres + mysql)', () => {
    const src = viewSrc()
    expect(src).toContain(`:types="['postgres', 'mysql']"`)
  })

  it('skip switches are exposed for every source (no per-type gating)', () => {
    const src = viewSrc()
    for (const opt of ['skip_precheck', 'skip_schema', 'skip_data', 'skip_validate']) {
      const toggle = `v-model="form.opts.${opt}"`
      expect(src).toContain(toggle)
      // No disabled= / v-if= source-type condition on the switch element.
      const idx = src.indexOf(toggle)
      const el = src.slice(src.lastIndexOf('<el-switch', idx), src.indexOf('>', idx))
      expect(el).not.toContain('disabled')
      expect(el).not.toContain('v-if')
    }
  })

  it('cdc_chain stays PG-gated (CapCDC: legit CDC-epic residue, not this batch)', () => {
    const src = viewSrc()
    expect(src).toContain(`form.opts.cdc_chain && effectiveSourceType.value === 'postgres'`)
    expect(src).toContain(`:disabled="effectiveSourceType !== 'postgres'"`)
  })
})
