// 比对页水位列改 suggest 式 Top-N 候选（任务书①②）：比对页复用 suggest
// 打分器（按所选表子集评分）、allow-create 手输兜底、missing_tables 显式
// 披露（部分缺失 warning / 全缺失独立 error / 无候选提示手输），运行期
// 缺列表全量比对回退（Plan A，报告逐表注明）+ 报告 skip 独立灰标签。
// 断言形=spec 精确匹配（本仓 vitest 惯例，禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CompareView.vue'), 'utf8')
const apiSrc = () =>
  readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')

describe('compare watermark Top-N candidates (任务书①②)', () => {
  it('suggestWatermark API carries the optional tables subset + missing_tables', () => {
    const src = apiSrc()
    expect(src).toContain(`suggestWatermark: (ref: string, tables?: string[])`)
    expect(src).toContain(`{ source_ref: ref, tables: tables || undefined }`)
    expect(src).toContain(`missing_tables?: string[]`)
  })

  it('compare page rides the suggest scorer over the SELECTED tables', () => {
    const src = viewSrc()
    expect(src).toContain(`await apiClient.suggestWatermark(sourceRef.value, selectedTables.value)`)
    expect(src).toContain(`获取候选水位列`)
    // 严格交集逻辑已删：不再调 columnsBatch，不再有误导性「交集为空」。
    expect(src).not.toContain(`apiClient.columnsBatch`)
    expect(src).not.toContain(`交集为空`)
  })

  it('candidate select keeps manual entry via allow-create', () => {
    const src = viewSrc()
    expect(src).toContain(`filterable allow-create default-first-option clearable`)
  })

  it('missing tables disclosed: partial warning, all-missing hard error, no-candidate hint', () => {
    const src = viewSrc()
    expect(src).toContain(`张表在源库不存在，已排除：`)
    expect(src).toContain(`张表在源库均不存在，无法获取候选水位列`)
    expect(src).toContain(`所选表均无可比类型列，请手输列名或调整表选择`)
  })

  it('candidate info panel shows coverage + unmatched tables (runtime fallback note)', () => {
    const src = viewSrc()
    expect(src).toContain(`覆盖 {{ activeWmCandidate.matched_tables.length }}/{{ wmSuggestTotal || selectedTables.length }} 张表`)
    expect(src).toContain(`缺该列 {{ activeWmCandidate.unmatched_tables.length }} 张表`)
    expect(src).toContain(`运行期将全量比对`)
  })

  it('manual (no sourceRef) mode hint says runtime full-comparison fallback', () => {
    const src = viewSrc()
    expect(src).toContain(`运行期缺列表将全量比对`)
  })

  it('report skip status renders as its own gray tag, never fail', () => {
    const src = viewSrc()
    expect(src).toContain(`skip: 'info'`)
    expect(src).toContain(`skip: '跳过'`)
    // 错误/建议列兜底展示 suggestion（skip 行的原因披露）。
    expect(src).toContain(`{{ row.error ? row.error : row.suggestion }}`)
  })
})
