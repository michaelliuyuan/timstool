// 刘源 seq768 调整姊妹锚（TestNoHardcodedPGReportLabels 家族，评估面）：
// 评估报告的源库列标签必须走 srcLabelShort 动态分派，副标题不得写死类型
// 集列举——今后增源零文案维护。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'AssessView.vue'), 'utf8')

describe('AssessView dynamic source label (MS-10c)', () => {
  it('report source column label is dispatched via srcLabelShort', () => {
    const src = viewSrc()
    expect(src).toContain(':label="srcLabelShort"')
    expect(src).not.toContain('label="PG"')
  })

  it('srcLabelShort dispatches mysql → MySQL, default → PG', () => {
    const src = viewSrc()
    expect(src).toContain(`=== 'mysql' ? 'MySQL' : 'PG'`)
  })

  it('subtitle does not enumerate source types', () => {
    const src = viewSrc()
    const m = src.match(/PageHeader[^/]*subtitle="([^"]*)"/)
    expect(m, 'PageHeader subtitle not found').toBeTruthy()
    expect(m![1]).not.toContain('MySQL')
    expect(m![1]).not.toContain('PostgreSQL')
  })
})
