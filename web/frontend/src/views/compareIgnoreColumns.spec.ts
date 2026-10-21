// 比对页排除字段（ignore_columns）：比对配置区提供多值输入（el-select
// multiple + allow-create），排除列表在 saveCompareParams/applyCompareOptions
// 往返、随 createCompare payload 携带；API 类型携带 ignore_columns。断言形=
// spec 精确匹配（本仓 vitest 惯例，禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CompareView.vue'), 'utf8')
const apiSrc = () =>
  readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')

describe('compare ignore columns (任务书)', () => {
  it('API compare types carry ignore_columns', () => {
    const src = apiSrc()
    expect(src).toContain(`ignore_columns?: string[]`)
  })

  it('view gains ignoreCols state (multi-tag input)', () => {
    const src = viewSrc()
    expect(src).toContain(`const ignoreCols = ref<string[]>([])`)
    expect(src).toContain(`v-model="ignoreCols" multiple allow-create`)
    expect(src).toContain(`如 created_at, updated_at`)
  })

  it('create payload carries the trimmed ignore list', () => {
    const src = viewSrc()
    expect(src).toContain(`ignore_columns: ignoreCols.value.map((c) => c.trim()).filter((c) => c !== '')`)
  })

  it('options save/apply round-trips the ignore list', () => {
    const src = viewSrc()
    expect(src).toContain(`ignore_columns: ignoreCols.value,`)
    expect(src).toContain(`if (Array.isArray(opts.ignore_columns)) {`)
    expect(src).toContain(`ignoreCols.value = [...opts.ignore_columns]`)
  })

  it('exclusion semantics disclosed in the UI', () => {
    const src = viewSrc()
    expect(src).toContain(`两端注定不一致的列可在此排除`)
    expect(src).toContain(`排除列命中某表主键时该表自动保留该列`)
  })
})
