// MC-12 题2A FE 锚：DbBrowseSelect 卡片化（观感升级）。断言形=源文本精确
// 匹配（禁页面盲扫，本仓 vitest 惯例）。MC-11 行为红线（单选回填/系统库
// 过滤/失败降级非阻塞/手输双轨）由 compareCreateDbBrowse.spec.ts 五锚继续
// 把守，此处只锚新增观感面+红线复断言。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const compSrc = () => readFileSync(resolve(__dirname, 'DbBrowseSelect.vue'), 'utf8')
const apiSrc = () => readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')

describe('DbBrowseSelect card list (MC-12 题2A)', () => {
  it('header row: 共 N 库 badge + refresh icon re-pull', () => {
    const src = compSrc()
    expect(src).toContain('{{ countText }}')
    expect(src).toContain('共 ${total} 库')
    expect(src).toContain('匹配 ${dbs.value?.length || 0} / 共 ${total} 库')
    expect(src).toContain(':icon="Refresh"')
    expect(src).toContain('@click="fetchDbs"')
  })

  it('card container: light rounded + el-scrollbar slim scroll', () => {
    const src = compSrc()
    expect(src).toContain('<el-scrollbar max-height="220px">')
    expect(src).toContain('background: var(--el-fill-color-lighter)')
    expect(src).toContain('border-radius: 8px')
  })

  it('radio selected state highlights the row border', () => {
    const src = compSrc()
    expect(src).toContain(':class="{ selected: modelValue === d }"')
    expect(src).toContain('.db-row.selected {')
    expect(src).toContain('border-color: var(--el-color-primary)')
  })

  it('long db names ellipsize with a tooltip', () => {
    const src = compSrc()
    expect(src).toContain('<el-tooltip :content="d"')
    expect(src).toContain('text-overflow: ellipsis')
  })

  it('right edge shows human-readable size_on_disk; empty 库 renders gray 空', () => {
    const src = compSrc()
    expect(src).toContain('function humanSize(e?: DbEntry): string')
    expect(src).toContain("const units = ['B', 'KB', 'MB', 'GB', 'TB']")
    expect(src).toContain(`if (!e || e.empty) return '空'`)
    expect(src).toContain(`'db-size-empty': dbMeta[d]?.empty`)
    // the backend already ships the fields (MC-11 wire contract)
    const api = apiSrc()
    expect(api).toContain('size_on_disk?: number; empty?: boolean')
  })

  it('filter input appears only past 8 entries', () => {
    const src = compSrc()
    expect(src).toContain('(allDbs.value?.length || 0) > 8')
    expect(src).toContain('v-model="filterText"')
    expect(src).toContain('placeholder="过滤库名"')
  })

  it('red lines restated: single-select + system-db filter + non-blocking degrade', () => {
    const src = compSrc()
    expect(src).not.toContain('el-checkbox')
    expect(src).not.toContain('multiple')
    expect(src).toContain(`new Set(['admin', 'config', 'local'])`)
    expect(src).toContain('可手动输入库名')
    expect(src).toContain(':model-value="modelValue"')
  })
})
