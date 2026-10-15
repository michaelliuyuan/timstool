// MC-11: 对比创建页源端/目标端库名 browse+单选回填。断言形=spec 精确匹配
// （禁页面盲扫，本仓 vitest 惯例，对齐 compareCreateDsRef.spec.ts）。
// 覆盖四件：API 封装 / 两侧接线与 payload 形 / 系统库前端过滤（对齐
// engine.go:142 跳过集）/ 单选回填+失败降级不阻塞（v1 单库契约不动）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CompareCreateView.vue'), 'utf8')
const compSrc = () =>
  readFileSync(resolve(__dirname, '../components/DbBrowseSelect.vue'), 'utf8')
const apiSrc = () =>
  readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')

describe('CompareCreateView database browse (MC-11)', () => {
  it('api wrapper posts /sources/meta with database empty (databases level)', () => {
    const src = apiSrc()
    expect(src).toContain(`browseSourceDatabases`)
    expect(src).toContain(`'/sources/meta'`)
    expect(src).toContain(`{ source: 'mongodb', database: '', ...payload }`)
    expect(src).toContain(`datasource_id?: string`)
  })

  it('both sides wire DbBrowseSelect; datasource ref folds id, manual rides fields', () => {
    const src = viewSrc()
    expect(src).toContain(`<DbBrowseSelect`)
    expect(src).toContain(`v-model="srcDb"`)
    expect(src).toContain(`v-model="tgtDb"`)
    expect(src).toContain(`{ datasource_id: srcRef } : { fields: { ...srcModel } }`)
    expect(src).toContain(`{ datasource_id: tgtRef } : { fields: { ...tgtModel } }`)
    // Manual input stays dual-track on both sides (no bare el-input replaced path lost).
    expect(src).not.toContain(`<el-input v-model="srcDb"`)
  })

  it('component filters the engine system-skip set client-side', () => {
    const src = compSrc()
    expect(src).toContain(`new Set(['admin', 'config', 'local'])`)
    expect(src).toContain(`engine.go:142`)
  })

  it('single-select radio fills the manual input (v1 single-db contract untouched)', () => {
    const src = compSrc()
    expect(src).toContain(`<el-radio-group`)
    expect(src).toContain(`<el-radio v-for="d in dbs" :key="d" :value="d"`)
    // No multi-select surface: the ruling is 单选回填, batch multi-select is a
    // separate increment.
    expect(src).not.toContain(`el-checkbox`)
    expect(src).not.toContain(`multiple`)
  })

  it('failed browse degrades to manual input, never blocks (loading + toast)', () => {
    const src = compSrc()
    expect(src).toContain(`:loading="loading"`)
    expect(src).toContain(`获取数据库列表`)
    expect(src).toContain(`ElMessage.error`)
    expect(src).toContain(`可手动输入库名`)
    // Manual input remains rendered regardless of browse state.
    expect(src).toContain(`:model-value="modelValue"`)
  })
})
