// MS-10d 笔②姊妹锚：向导副标题通用形（10c 调整①同款口径）——类型集由
// DataSourcePicker 承载，副标题不再硬编码「PostgreSQL」；skip 四开关
// 全源有效（裁 b：零分域）；cdc_chain 门=cdcChainCapable computed
// （MS-11 笔④翻面：postgres+mysql）。
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

  it('cdc_chain is gated by the cdcChainCapable computed (postgres + mysql, MS-11 pen 4)', () => {
    const src = viewSrc()
    // The single gate: both the switch and the submit payload ride the computed.
    expect(src).toContain(`const cdcChainCapable = computed(() => ['postgres', 'mysql'].includes(effectiveSourceType.value))`)
    expect(src).toContain(`:disabled="!cdcChainCapable"`)
    expect(src).toContain(`cdc_chain: form.opts.cdc_chain && cdcChainCapable.value,`)
    // The stale PG-only cdc_chain gate forms must be gone (the list-tables
    // postgres routing at the fetch layer is unrelated and stays).
    expect(src).not.toContain(`form.opts.cdc_chain && effectiveSourceType.value === 'postgres'`)
    expect(src).not.toContain(`:disabled="effectiveSourceType !== 'postgres'"`)
    expect(src).not.toContain(`form.opts.cdc_chain && effectiveSourceType === 'postgres'`)
  })
})
