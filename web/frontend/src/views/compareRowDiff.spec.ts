// 行级差异（row-diff drill）FE 锚：报告面板的失败表可展开查看三类计数、
// 键列、样例键、截断披露与「下载差异明细」（只读 rowdiff 路由的同源直链）；
// note-only 摘要（无法对齐/零差异披露）原样呈现，不伪造计数。
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

class FakeWebSocket {
  onmessage: unknown = null
  onerror: unknown = null
  close() {}
}
class FakeResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}

const { mockGetCompareReport, completedTask, jsonReport } = vi.hoisted(() => {
  const completedTask = {
    id: 'rd0diff',
    name: '行级差异锚',
    status: 'completed' as const,
    source: {},
    target: {},
    mode: 'checksum',
    sample_ratio: 0.01,
    checksum_chunk_size: 50000,
    checksum_parallel: 0,
    parallel: 0,
    concurrency: 4,
    tables: [],
    tables_done: 3,
    tables_total: 3,
    created_at: '2026-01-01T00:00:00Z',
  }
  const jsonReport = {
    overall_status: 'fail',
    start_time: '2026-01-01T00:00:00Z',
    end_time: '2026-01-01T00:01:00Z',
    duration: '1m0.000s',
    tables: [
      {
        table_name: 'orders',
        status: 'fail',
        source_rows: 5,
        target_rows: 4,
        diff_rows: 3,
        error: 'checksum mismatch in 1/1 chunks',
        row_diffs: {
          key_columns: ['id'],
          content_diff: 1,
          src_only: 1,
          dst_only: 1,
          sample_keys: ['3', '4', '6'],
          file: 'orders-9f2c1b7a.csv',
          truncated: true,
        },
      },
      {
        table_name: 'logs',
        status: 'fail',
        source_rows: 2,
        target_rows: 2,
        diff_rows: 0,
        error: 'checksum mismatch in 1/1 chunks',
        row_diffs: {
          key_columns: ['id'],
          content_diff: 0,
          src_only: 0,
          dst_only: 0,
          note: '块校验不一致但行级对齐未发现差异（比对期间数据在变动，或两侧列集不同）',
        },
      },
      {
        table_name: 'users',
        status: 'pass',
        source_rows: 9,
        target_rows: 9,
      },
    ],
    stats: {
      total_tables: 3,
      pass_tables: 1,
      fail_tables: 2,
      warn_tables: 0,
      skip_tables: 0,
      total_source_rows: 16,
      total_target_rows: 15,
      total_diff_rows: 3,
    },
  }
  const mockGetCompareReport = vi.fn((id: string, format?: string) =>
    format === 'html'
      ? Promise.resolve({ data: '<!DOCTYPE html><html><body>x</body></html>' })
      : Promise.resolve({ data: jsonReport }),
  )
  return { mockGetCompareReport, completedTask, jsonReport }
})

vi.mock('../api', () => ({
  default: {
    getSources: vi.fn().mockResolvedValue({ data: { sources: [] } }),
    listDataSources: vi.fn().mockResolvedValue({ data: { datasources: [] } }),
    getCompareOptions: vi.fn().mockResolvedValue({ data: {} }),
    saveCompareOptions: vi.fn().mockResolvedValue({ data: { success: true } }),
    listCompares: vi.fn().mockResolvedValue({ data: [completedTask] }),
    getCompareReport: mockGetCompareReport,
    cancelCompare: vi.fn(),
    deleteCompare: vi.fn(),
    compareRowDiffUrl: (id: string, file: string) =>
      `/api/v1/compare/tasks/${id}/rowdiff/${encodeURIComponent(file)}`,
    // v2 行内容对照：本锚不消费（详见 compareRowDiffContent.spec.ts），提供
    // 空数组以防展开行触发的一次性懒加载落空
    getCompareRowDiffContent: vi.fn().mockResolvedValue({ data: [] }),
  },
}))

import CompareView from './CompareView.vue'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CompareView.vue'), 'utf8')

const apiSrc = () =>
  readFileSync(resolve(__dirname, '..', 'api', 'index.ts'), 'utf8')

async function openReportPanel() {
  const w = await mount(CompareView, {
    attachTo: document.body,
    global: { plugins: [ElementPlus] },
  })
  await flushPromises()
  const viewBtn = w.findAll('button').find(b => b.text().includes('查看'))
  if (!viewBtn) throw new Error('查看 button not rendered')
  await viewBtn.trigger('click')
  await flushPromises()
  return w
}

describe('CompareView row-diff drill (行级差异)', () => {
  beforeEach(() => {
    vi.stubGlobal('WebSocket', FakeWebSocket)
    vi.stubGlobal('ResizeObserver', FakeResizeObserver)
    vi.stubGlobal('URL', Object.assign(URL, {
      createObjectURL: vi.fn().mockReturnValue('blob:rd'),
      revokeObjectURL: vi.fn(),
    }))
    mockGetCompareReport.mockClear()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders the drilled table with the three-class summary after expand', async () => {
    const w = await openReportPanel()
    // 差异行数 shows the corrected three-class total (口径修正)
    expect(w.text()).toContain('orders')

    // expand the first row → the drill summary renders
    const expandIcons = w.findAll('.el-table__expand-icon')
    expect(expandIcons.length).toBeGreaterThan(0)
    await expandIcons[0].trigger('click')
    await flushPromises()
    const body = document.body.innerHTML
    expect(body).toContain('内容不一致 1')
    expect(body).toContain('仅源端 1')
    expect(body).toContain('仅目标端 1')
    expect(body).toContain('键列 id')
    // v2 统一样例键标签：差异键示例（前 10，共 N）
    expect(body).toContain('差异键示例（前 10，共 3）')
    // v2 裁定 2：截断 tag 泛化文案，无数字第二真相源（精确值只在后端 Suggestion）
    expect(body).toContain('明细已截断（三项计数仍精确）')
    expect(body).toContain('orders-9f2c1b7a.csv')
    w.unmount()
  })

  it('note-only summaries render the disclosure verbatim, never fake counts', async () => {
    const w = await openReportPanel()
    const expandIcons = w.findAll('.el-table__expand-icon')
    await expandIcons[1].trigger('click')
    await flushPromises()
    const body = document.body.innerHTML
    expect(body).toContain('行级对齐未发现差异')
    w.unmount()
  })

  it('下载差异明细 downloads via the same-origin rowdiff route', async () => {
    // capture the transient anchor's href/download inside the click spy —
    // the anchor is detached before any DOM query could see it
    const clicked: Array<{ href: string | null; download: string | null }> = []
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push({ href: this.getAttribute('href'), download: this.getAttribute('download') })
    })
    const w = await openReportPanel()
    const expandIcons = w.findAll('.el-table__expand-icon')
    await expandIcons[0].trigger('click')
    await flushPromises()

    const dlBtn = w.findAll('button').find(b => b.text().includes('下载差异明细'))
    if (!dlBtn) throw new Error('下载差异明细 button not rendered')
    await dlBtn.trigger('click')
    await flushPromises()

    expect(clicked).toHaveLength(1)
    expect(clicked[0].href).toBe('/api/v1/compare/tasks/rd0diff/rowdiff/orders-9f2c1b7a.csv')
    expect(clicked[0].download).toBe('orders-9f2c1b7a.csv')
    clickSpy.mockRestore()
    w.unmount()
  })

  it('view source anchors: expand column, download wiring, note guard', () => {
    const src = viewSrc()
    expect(src).toContain('type="expand"')
    expect(src).toContain('downloadRowDiff(row.row_diffs.file)')
    expect(src).toContain('apiClient.compareRowDiffUrl(t.id, file)')
    expect(src).toContain("row.row_diffs.note")
    expect(src).toContain('无行级差异信息')
  })

  it('api client exposes the rowdiff URL helper with encoded filename', () => {
    const src = apiSrc()
    const i = src.indexOf('compareRowDiffUrl')
    expect(i).toBeGreaterThan(-1)
    const slice = src.slice(i, i + 260)
    expect(slice).toContain('encodeURIComponent(file)')
    expect(slice).toContain('/compare/tasks/${id}/rowdiff/')
  })

  it('CompareRowDiffs type carries the summary contract', () => {
    const src = apiSrc()
    expect(src).toContain('interface CompareRowDiffs')
    for (const f of ['key_columns', 'content_diff', 'src_only', 'dst_only', 'sample_keys', 'file', 'truncated', 'note']) {
      expect(src).toContain(`${f}`)
    }
  })
})
