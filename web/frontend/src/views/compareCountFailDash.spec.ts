// BUG-CMP-COUNT-P0 P0-B 语义锚（失败行数「—」）：
// - status=fail 且 error 非空 → 表格三列（源行数/目标行数/差异行数）
//   渲染「—」而非误导性 0（本单正是「0 行假象」误导了排障方向）；
// - pass 行照常渲染数字；
// - CSV 导出同步该口径（fail+error 行三计数列「—」），汇总 totals
//   口径不动（服务端单一真源，记档披露）。
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

// jsdom lacks the browser APIs the view touches at mount/render time.
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
    id: 'cmpct',
    name: 'P0-B 失败行数锚',
    status: 'completed' as const,
    source: {},
    target: {},
    mode: 'sample',
    sample_ratio: 0.01,
    checksum_chunk_size: 50000,
    checksum_parallel: 0,
    parallel: 0,
    concurrency: 4,
    tables: [],
    tables_done: 2,
    tables_total: 2,
    created_at: '2026-01-01T00:00:00Z',
  }
  // The failed table mirrors the incident shape: count timed out, the
  // backend left the row-count fields at zero and carried the error.
  const jsonReport = {
    overall_status: 'fail',
    start_time: '2026-01-01T00:00:00Z',
    end_time: '2026-01-01T00:01:00Z',
    duration: '1m0.000s',
    tables: [
      {
        table_name: 'huge_cold_table',
        status: 'fail',
        source_rows: 0,
        target_rows: 0,
        diff_rows: 0,
        duration: '5m0.001s',
        error: 'source count: query timeout (5m0s): context deadline exceeded',
        suggestion: '',
      },
      {
        table_name: 'healthy_table',
        status: 'pass',
        source_rows: 1234,
        target_rows: 1234,
        diff_rows: 0,
        duration: '0.1s',
        error: '',
        suggestion: 'ok',
      },
    ],
    stats: {
      total_tables: 2,
      pass_tables: 1,
      fail_tables: 1,
      warn_tables: 0,
      skip_tables: 0,
      // Totals keep the raw sums (disclosed caveat — NOT part of this fix).
      total_source_rows: 1234,
      total_target_rows: 1234,
      total_diff_rows: 0,
    },
  }
  const mockGetCompareReport = vi.fn((id: string, format?: string) =>
    format === 'html'
      ? Promise.resolve({ data: '<!DOCTYPE html><html><body>TiMS 数据比对报告</body></html>' })
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
  },
}))

import CompareView from './CompareView.vue'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CompareView.vue'), 'utf8')

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

function rowOf(w: ReturnType<typeof mount>, tableName: string) {
  const tr = w.findAll('tr').find(r => r.text().includes(tableName))
  if (!tr) throw new Error(`row ${tableName} not rendered`)
  return tr.findAll('td').map(td => td.text())
}

describe('CompareView failed-count dash (BUG-CMP-COUNT-P0 P0-B)', () => {
  beforeEach(() => {
    vi.stubGlobal('WebSocket', FakeWebSocket)
    vi.stubGlobal('ResizeObserver', FakeResizeObserver)
    vi.stubGlobal('URL', Object.assign(URL, {
      createObjectURL: vi.fn().mockReturnValue('blob:cmpct'),
      revokeObjectURL: vi.fn(),
    }))
    mockGetCompareReport.mockClear()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('fail+error rows render「—」in all three count columns, pass rows keep numbers', async () => {
    const w = await openReportPanel()

    const failCells = rowOf(w, 'huge_cold_table')
    expect(failCells.filter(c => c === '—')).toHaveLength(3)
    // The misleading zeros are gone from the count columns.
    expect(failCells).not.toContain('0')

    const passCells = rowOf(w, 'healthy_table')
    expect(passCells.filter(c => c === '—')).toHaveLength(0)
    expect(passCells).toContain('1234')

    w.unmount()
  })

  it('CSV export writes「—」for the failed row and numbers for the pass row', async () => {
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const w = await openReportPanel()

    const csvBtn = w.findAll('button').find(b => b.text().includes('导出 CSV'))
    if (!csvBtn) throw new Error('导出 CSV button not rendered')
    await csvBtn.trigger('click')
    await flushPromises()

    const [blob] = (URL.createObjectURL as ReturnType<typeof vi.fn>).mock.calls[0] as Blob[]
    expect(blob).toBeInstanceOf(Blob)
    const text = await blob.text()
    const lines = text.split('\r\n')
    const failLine = lines.find(l => l.includes('huge_cold_table'))
    const passLine = lines.find(l => l.includes('healthy_table'))
    if (!failLine || !passLine) throw new Error('table rows missing from CSV')
    expect(failLine).toContain('—,—,—')
    expect(failLine).not.toMatch(/,0,0,0,/)
    expect(passLine).toContain('1234,1234,0')

    clickSpy.mockRestore()
    w.unmount()
  })

  it('source shape: the dash rule is a render-time rule (schema untouched, totals untouched)', () => {
    const src = viewSrc()
    expect(src).toContain('function countCellsUnknown')
    expect(src).toContain("t.status === 'fail' && !!t.error")
    // Count columns template-rendered (no more bare prop passthrough).
    expect(src).toContain(`countCell(row, 'source_rows')`)
    expect(src).toContain(`countCell(row, 'target_rows')`)
    expect(src).toContain('countCellsUnknown(row)">—')
    // Totals keep the raw sums verbatim.
    expect(src).toContain('r.stats.total_source_rows, r.stats.total_target_rows, r.stats.total_diff_rows')
  })
})
