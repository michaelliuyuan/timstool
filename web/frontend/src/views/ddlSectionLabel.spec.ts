// MS-10c 笔⑨姊妹锚（assessDynamicLabel.spec.ts 家族）：
// DDL/评估/对比三页的手工形区段标签统一通用形「源数据库」——类型由
// DataSourcePicker 承载（刘源调整①同款口径，leader seq797 ①②③ 三处顺收）。
// 锚形约束：精确断言区段标签文本==「源数据库」，禁页面级盲扫——
// AssessView「手工连接仅支持 PostgreSQL」hint=合法 PG-only 结构性说明字面。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const srcOf = (f: string) =>
  readFileSync(resolve(__dirname, f), 'utf8')

const genericDivider = /<el-divider content-position="left">源数据库<\/el-divider>/
const annotatedDivider = /<el-divider[^>]*>源数据库（/

describe('generic source-section divider (MS-10c commit 9, sites 1-3)', () => {
  it.each(['DDLExportView.vue', 'AssessView.vue', 'CompareView.vue'])(
    '%s divider is exactly 源数据库 (no type annotation)',
    (f) => {
      const src = srcOf(f)
      expect(src, `${f} lacks the generic divider`).toMatch(genericDivider)
      expect(src, `${f} still has an annotated divider`).not.toMatch(annotatedDivider)
    },
  )
})
