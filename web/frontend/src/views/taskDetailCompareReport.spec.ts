// MC-14 对比报告下载锚（spec 精确匹配口径，禁页面盲扫）：
// - 对比任务终态（completed/failed）显示「下载对比报告」+「下载 JSON」，
//   cancelled 刻意排除（取消跑无 summary.json，点了必 404）；
// - 迁移路径按钮门控与文件名零变化（回归锚）；
// - json 走 responseType text（verbatim 工件字节级下载）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'TaskDetailView.vue'), 'utf8')

const apiSrc = () =>
  readFileSync(resolve(__dirname, '..', 'api', 'index.ts'), 'utf8')

describe('TaskDetailView compare report download (MC-14)', () => {
  it('compare tasks show the HTML + JSON download pair in terminal states', () => {
    const src = viewSrc()
    const gate = 'v-if="isCompare && (task.status === \'completed\' || task.status === \'failed\')"'
    expect(src).toContain(gate)
    expect(src).toContain('下载对比报告')
    expect(src).toContain('下载 JSON')
    // Each compare button wires its handler inside its own element (the
    // @click precedes the label in the template).
    const g1 = src.indexOf(gate)
    const htmlBtn = src.slice(g1, src.indexOf('</el-button>', g1) + 12)
    expect(htmlBtn).toContain('@click="downloadReport"')
    expect(htmlBtn).toContain('下载对比报告')
    const g2 = src.indexOf(gate, g1 + 1)
    const jsonBtn = src.slice(g2, src.indexOf('</el-button>', g2) + 12)
    expect(jsonBtn).toContain('@click="downloadCompareSummary"')
    expect(jsonBtn).toContain('下载 JSON')
  })

  it('cancelled compare runs never see the buttons (no summary.json exists)', () => {
    const src = viewSrc()
    // The compare gate enumerates exactly completed/failed — assert the
    // literal does not include cancelled between the parens.
    const m = src.match(/v-if="isCompare && \(([^)]+)\)"/g) || []
    expect(m.length).toBeGreaterThan(0)
    for (const g of m) expect(g).not.toContain('cancelled')
  })

  it('migration download button and filename stay untouched (regression anchor)', () => {
    const src = viewSrc()
    expect(src).toContain('v-if="!isCompare && (task.status === \'completed\' || task.status === \'failed\' || task.status === \'cancelled\')"')
    expect(src).toContain('`report-${taskId}.html`')
  })

  it('download filenames branch by task family', () => {
    const src = viewSrc()
    expect(src).toContain('isCompare.value ? `compare-report-${taskId}.html` : `report-${taskId}.html`')
    expect(src).toContain('`compare-summary-${taskId}.json`')
  })

  it('report downloads extract the backend error from a text body', () => {
    const src = viewSrc()
    expect(src).toContain('function reportDownloadError')
    expect(src).toContain('reportDownloadError(e)')
  })

  it('api client serves both report faces as text (verbatim artifact bytes)', () => {
    const src = apiSrc()
    const i = src.indexOf('getTaskReport')
    expect(i).toBeGreaterThan(-1)
    expect(src.slice(i, i + 260)).toContain("responseType: 'text'")
    expect(src.slice(i, i + 260)).not.toContain("'json' ? 'json'")
  })
})
