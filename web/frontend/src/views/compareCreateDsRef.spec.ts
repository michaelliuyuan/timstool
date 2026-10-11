// M6: 对比页引用已存数据源——CompareCreateView 两侧 DataSourcePicker
// （types=['mongodb']，选数据源免填表单、连接测试走数据源测试端点）、
// endpointOf 产出 datasource_id 快照引用 + 手动路径 tls 透传（此前表单
// 有 TLS 开关但 DTO 不带，开关形同虚设）；DataSourcePicker/API 类型同步。
// 断言形=spec 精确匹配（禁页面盲扫，本仓 vitest 惯例）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CompareCreateView.vue'), 'utf8')
const pickerSrc = () =>
  readFileSync(resolve(__dirname, '../components/DataSourcePicker.vue'), 'utf8')
const apiSrc = () =>
  readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')
const registrySrc = () =>
  readFileSync(resolve(__dirname, '../composables/dataSourceTypes.ts'), 'utf8')

describe('CompareCreateView datasource reference (M6)', () => {
  it('both sides carry a DataSourcePicker restricted to mongodb', () => {
    const src = viewSrc()
    expect(src).toContain(`<DataSourcePicker v-model="srcRef" :types="['mongodb']" />`)
    expect(src).toContain(`<DataSourcePicker v-model="tgtRef" :types="['mongodb']" />`)
  })

  it('selecting a datasource hides the manual form (both sides)', () => {
    const src = viewSrc()
    expect(src).toContain(`<template v-if="!srcRef">`)
    expect(src).toContain(`<template v-if="!tgtRef">`)
  })

  it('referenced side tests through the datasource endpoint (server-side credentials)', () => {
    const src = viewSrc()
    expect(src).toContain(`await apiClient.testDataSource(refId)`)
  })

  it('endpointOf emits datasource_id for referenced sides', () => {
    const src = viewSrc()
    expect(src).toContain(`if (refId) return { datasource_id: refId, database: '' }`)
  })

  it('manual path passes tls through to the compare DTO (was silently dropped)', () => {
    const src = viewSrc()
    expect(src).toContain(`if (model.tls) ep.tls = true`)
  })

  it('picker labels come from the datasource registry, which knows mongodb', () => {
    // M7 ③: the picker's local label map moved to the datasource-domain
    // registry; the picker itself must hold no per-type knowledge.
    expect(pickerSrc()).toContain(`dataSourceTypeLabel(d.type)`)
    expect(pickerSrc()).not.toContain(`mongodb:`)
    expect(registrySrc()).toContain(`shortLabel: 'MongoDB'`)
  })

  it('CompareEndpoint DTO carries tls + datasource_id', () => {
    const src = apiSrc()
    expect(src).toContain(`tls?: boolean`)
    expect(src).toContain(`datasource_id?: string`)
  })
})
