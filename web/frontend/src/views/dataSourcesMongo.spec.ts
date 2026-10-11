// M6: DataSourcesView 面向 MongoDB 开放——类型枚举 + 表单走 /sources 注册表
// 单一事实源（getSource('mongodb') = mongoMeta.Fields 含 camelCase authDb +
// tls switch）+ dialog 表单渲染 switch 分支。
// M7 ③: 类型枚举/标签/配色/tidb 目标端表迁移到 datasource 域注册表模块
// (composables/dataSourceTypes.ts)——本 spec 随迁：正面断言指向注册表模块，
// 反面断言（禁 mongodb 特判）仍盯 DataSourcesView；渲染行为面已由 mount 级行为
// 网 (dataSourcesViewBehavior.spec.ts) 更强覆盖（mongodb 字段集/tls switch/
// 卡片 primary tag/四类型枚举）。断言形=spec 精确匹配（禁页面盲扫，本仓惯例）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'DataSourcesView.vue'), 'utf8')

const registrySrc = () =>
  readFileSync(resolve(__dirname, '../composables/dataSourceTypes.ts'), 'utf8')

describe('DataSourcesView mongodb surface (M6, registry since M7)', () => {
  it('datasource type registry offers mongodb; labels know it', () => {
    const reg = registrySrc()
    expect(reg).toContain(`{ value: 'mongodb', label: 'MongoDB（对比）', shortLabel: 'MongoDB', tagType: 'primary' },`)
  })

  it('form fields stay registry-driven — no hardcoded mongodb field list', () => {
    // tidb keeps its local FieldSpec list (target-side, not a source
    // adapter, registry-independent by design); mongodb MUST go through the
    // /sources registry like postgres/mysql — the anti-special-casing red
    // line at the form layer. The registry module carries no mongodb entry
    // in localFormFields, and the view holds no mongodb branch at all.
    const reg = registrySrc()
    expect(reg).toMatch(/localFormFields[^=]*=\s*{\s*tidb:/)
    expect(reg).not.toContain(`mongodb:`)

    const src = viewSrc()
    expect(src).toContain(`dataSourceFormFields(type, getSource(type)?.fields)`)
    expect(src).not.toContain(`mongoFields`)
    expect(src).not.toContain(`if (type === 'mongodb')`)
    expect(src).not.toContain(`d.type === 'mongodb'`)
  })

  it('dialog renders switch-type fields (tls), not a text input fallback', () => {
    const src = viewSrc()
    expect(src).toContain(`v-if="f.type === 'switch'"`)
    expect(src).not.toContain(`v-else-if="f.type === 'switch'"`)
  })

  it('card badge colour comes from the registry helper, no inline ternary chain', () => {
    const src = viewSrc()
    expect(src).toContain(`:type="dataSourceTagType(d.type)"`)
    expect(src).not.toContain(`? 'warning'`)
  })
})
