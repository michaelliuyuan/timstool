// coll-select ①锚点（集合级选择）：创建页集合选择器 + spec 字段 +
// list-collections API。默认全选、视图禁选、部分选择才携带 collections
// 字段（全选省略 = legacy 整库语义、payload 字节级不变）。
// 断言形态 = spec exact match（族惯例）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('compareMongoCollSelect v3.19.33', () => {
  const s = src('CompareCreateView.vue')
  const api = src('../api/index.ts')

  it('picker: load button gated on srcDb, table with view-disabled selection', () => {
    expect(s).toContain('加载集合列表')
    expect(s).toContain('async function loadColls()')
    // 对抗席 F1: database rides INSIDE the source snapshot (suggest twin).
    expect(s).toContain(`await apiClient.listMongoCollections({ ...endpointOf('src'), database: srcDb.value })`)
    expect(s).toContain(':selectable="(row: CollEntry) => !row.is_view"')
    expect(s).toContain('<el-tag v-if="row.is_view" type="warning" size="small">视图</el-tag>')
    expect(s).toContain('该数据库没有可比集合（或均为视图，或数据库名不存在）')
  })

  it('submit guards: loading-state refusal (F4) + orphan watermark override refusal (F3)', () => {
    expect(s).toContain('集合列表加载中，请稍候再提交')
    expect(s).toContain('例外水位指向未勾选的集合')
  })

  it('default = all pickable collections checked (requirement: 默认全选)', () => {
    expect(s).toContain(`for (const row of collEntries.value) {`)
    expect(s).toContain('if (!row.is_view) t.toggleRowSelection(row, true)')
  })

  it('all/none helpers + selection counter', () => {
    expect(s).toContain('function selectAllColls()')
    expect(s).toContain('function clearCollSelection()')
    expect(s).toContain('已选 {{ collSelectedNames.length }} / 可选 {{ pickableColls.length }}')
  })

  it('partial pick only: omitted field when everything selected (legacy byte-identical), empty pick refused', () => {
    expect(s).toContain('if (collSelectedNames.value.length !== pickableColls.value.length) {')
    expect(s).toContain('collections = [...collSelectedNames.value]')
    expect(s).toContain('已加载集合列表但未勾选任何集合：请至少勾选一个，或点「全选」比对整库')
    expect(s).toContain('collections,')
    expect(api).toContain('collections?: string[]')
  })

  it('srcDb switch invalidates the picker (no stale selection rides into the spec)', () => {
    expect(s).toContain('collEntries.value = []')
    expect(s).toContain('collSelectedNames.value = []')
  })

  it('disclosure line states the semantics (partial vs whole-database default)', () => {
    expect(s).toContain('默认全选 = 对比库内全部集合；部分勾选则仅比对所选集合，目标端未勾选的集合不报差异')
  })

  it('api: listMongoCollections POSTs the source snapshot carrying the database (F1 contract)', () => {
    expect(api).toContain('listMongoCollections: (source: CompareEndpoint & { database: string })')
    expect(api).toContain("'/mongocompare/list-collections'")
    expect(api).toContain("'/mongocompare/list-collections', { source },")
  })
})
