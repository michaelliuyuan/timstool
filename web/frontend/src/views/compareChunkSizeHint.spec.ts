// #97「分块大小」单位/下限标注：比对页与向导的 checksum 窗口大小控件带单位
// 说明（每窗口行数，非批大小/字节数）与默认/下限提示（50000/1000）；比对页
// 载入已存参数时丢弃 <1000 的历史脏值。断言形 = spec 精确匹配（本仓 vitest
// 惯例，禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const read = (p: string) => readFileSync(resolve(__dirname, p), 'utf8')

describe('checksum chunk size unit/floor hint (#97)', () => {
  it('CompareView control carries unit + default + floor hint', () => {
    const src = read('CompareView.vue')
    expect(src).toContain(`:min="1000"`)
    expect(src).toContain(`单位为每窗口行数（非批大小/字节数）：默认 50000、最小 1000`)
  })

  it('CompareView drops sub-floor garbage when loading saved options', () => {
    const src = read('CompareView.vue')
    expect(src).toContain(`if (cs !== null && cs >= 1000) form.checksum_chunk_size = cs`)
  })

  it('WizardView control hint names unit + default + floor', () => {
    const src = read('WizardView.vue')
    expect(src).toContain(`:min="1000"`)
    expect(src).toContain(`每窗口行数（非批大小/字节数），默认 50000、最小 1000`)
    expect(src).not.toContain(`每块的行数`)
  })
})
