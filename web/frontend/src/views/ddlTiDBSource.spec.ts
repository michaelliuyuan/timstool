// MS-12 锚（ddlSectionLabel.spec.ts 家族，source-scan 形）：
// DDL 导出页接入 TiDB 源——picker 三类型（postgres/mysql/tidb）、
// TiDB 源隐藏 IncludeTiDB 开关（源已是 TiDB，转换版不适用——服务端
// 忽略并记 manifest 台账，docs/MS12-TIDB-DIALECT-MAP.md）。
// 锚形约束：精确字面/结构断言，禁页面级盲扫；负锚退役旧双类型
// picker 与无守卫的开关挂载。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = readFileSync(resolve(__dirname, 'DDLExportView.vue'), 'utf8')

describe('DDL export TiDB source (MS-12)', () => {
  it('picker accepts postgres + mysql + tidb', () => {
    expect(src).toContain(`:types="['postgres', 'mysql', 'tidb']"`)
  })

  it('retires the two-type picker', () => {
    expect(src).not.toContain(`:types="['postgres', 'mysql']"`)
  })

  it('manual-entry hint covers TiDB', () => {
    expect(src).toContain('MySQL / TiDB 源请使用上方数据源引用')
  })

  it('IncludeTiDB switch is guarded by sourceIsTiDB', () => {
    // The computed wiring: datasource-registry type read, not a label scan.
    expect(src).toContain("getDataSource(sourceRef.value)?.type === 'tidb'")
    // The switch item hides for a TiDB source.
    expect(src).toContain('<el-form-item v-if="!sourceIsTiDB" label="TiDB 版">')
    // The disclosure shown instead for a TiDB source.
    expect(src).toContain('TiDB 源无需转换版——tidb-tables.sql 不生成')
  })

  it('the switch itself stays a plain toggle for non-TiDB sources', () => {
    // The el-switch binding is untouched (MS-10c shape) — the guard lives
    // on the form item, not by rewiring the switch model.
    expect(src).toContain('<el-switch v-model="includeTiDB" />')
  })
})
