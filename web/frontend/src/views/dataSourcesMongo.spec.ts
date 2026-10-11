// M6: DataSourcesView 面向 MongoDB 开放——类型枚举（手写 typeOptions 加入
// mongodb 行）、表单仍走 /sources 注册表单一事实源（fieldsForgetSource('mongodb')
// = mongoMeta.Fields 含 camelCase authDb + tls switch）、dialog 表单渲染补
// switch 分支（tls 是 mongoMeta 唯一的 switch 字段，此前掉进 text 兜底）。
// 断言形=spec 精确匹配（禁页面盲扫，本仓 vitest 惯例）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'DataSourcesView.vue'), 'utf8')

describe('DataSourcesView mongodb surface (M6)', () => {
  it('type enum offers mongodb; labels know it', () => {
    const src = viewSrc()
    expect(src).toContain(`{ value: 'mongodb', label: 'MongoDB（对比）' },`)
    expect(src).toContain(`mongodb: 'MongoDB'`)
  })

  it('form fields stay registry-driven — no hardcoded mongodb field list', () => {
    // tidb keeps its local FieldSpec list (target-side, not a source
    // adapter); mongodb MUST go through getSource like postgres/mysql —
    // the anti-special-casing red line at the form layer.
    const src = viewSrc()
    expect(src).toContain(`if (type === 'tidb') return tidbFields`)
    expect(src).toContain(`return getSource(type)?.fields || []`)
    expect(src).not.toContain(`mongoFields`)
    expect(src).not.toContain(`if (type === 'mongodb')`)
  })

  it('dialog renders switch-type fields (tls), not a text input fallback', () => {
    const src = viewSrc()
    expect(src).toContain(`v-if="f.type === 'switch'"`)
    expect(src).not.toContain(`v-else-if="f.type === 'switch'"`)
  })

  it('card badge distinguishes mongodb (primary tag)', () => {
    const src = viewSrc()
    expect(src).toContain(`d.type === 'mongodb' ? 'primary' : 'success'`)
  })
})
