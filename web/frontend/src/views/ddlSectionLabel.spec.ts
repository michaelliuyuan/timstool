// MS-10c 笔⑨姊妹锚（assessDynamicLabel.spec.ts 家族，DDL 导出面）：
// DDL 导出页手工形区段标签采通用形「源数据库」——类型由 DataSourcePicker
// 承载（刘源调整①同款口径），区段文案不得写死类型括注。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'DDLExportView.vue'), 'utf8')

describe('DDLExportView generic section label (MS-10c commit 9)', () => {
  it('source section divider is generic (no type annotation)', () => {
    const src = viewSrc()
    expect(src).toContain('>源数据库</el-divider>')
  })

  it('divider does not hardcode or ternary-dispatch a source type', () => {
    const src = viewSrc()
    const dividers = src.match(/<el-divider[^>]*>[^<]*<\/el-divider>/g) ?? []
    for (const d of dividers) {
      expect(d, `divider leaks a type: ${d}`).not.toContain('PostgreSQL')
      expect(d, `divider leaks a type: ${d}`).not.toContain('effectiveSourceType')
      expect(d, `divider leaks a type: ${d}`).not.toContain('（postgres')
    }
  })
})
