// FEAT-CMP-TBL-DURATION FE 锚：
// ① 运行中：OnTableDone 广播（table_status/table_duration）落地即亮该表耗时
// ② 第二张表完成 → 列表累积（最新在前）
// ③ chunk 进度广播（无 table_status）→ 列表不动（门槛字段语义）
// ④ 完成态：结果表「耗时」列由后端填充自动点亮（C4=验证既有接线）
// ⑤ CSV 导出含每表耗时（既有列，后端填充即含值）
// ⑥ 旧任务（report.json 表行无 duration）→ 结果表/CSV 不炸（空值兼容）
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ElementPlus from 'element-plus'

const { state, mockGetCompareReport } = vi.hoisted(() => {
  const state: {
    task: Record<string, any>
    ws: any[]
    report: Record<string, any> | null
  } = { task: {}, ws: [], report: null }
  const mockGetCompareReport = vi.fn((id: string, format?: string) =>
    format === 'html'
      ? Promise.resolve({ data: '<!DOCTYPE html><html><body>TiMS 数据比对报告</body></html>' })
      : Promise.resolve({ data: state.report }),
  )
  return { state, mockGetCompareReport }
})

class FakeWebSocket {
  onmessage: unknown = null
  onerror: unknown = null
  close() {}
  constructor() {
    state.ws.push(this)
  }
}
class FakeResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}

function makeTask(over: Record<string, any>): Record<string, any> {
  return {
    id: 'durrun',
    name: '耗时锚',
    status: 'running',
    source: {},
    target: {},
    mode: 'checksum',
    sample_ratio: 0.01,
    checksum_chunk_size: 50000,
    concurrency: 4,
    tables: [],
    tables_done: 0,
    tables_total: 3,
    created_at: '2026-01-01T00:00:00Z',
    ...over,
  }
}

vi.mock('../api', () => ({
  default: {
    getSources: vi.fn().mockResolvedValue({ data: { sources: [] } }),
    listDataSources: vi.fn().mockResolvedValue({ data: { datasources: [] } }),
    getCompareOptions: vi.fn().mockResolvedValue({ data: {} }),
    saveCompareOptions: vi.fn().mockResolvedValue({ data: { success: true } }),
    listCompares: vi.fn(() => Promise.resolve({ data: [state.task] })),
    getCompareReport: mockGetCompareReport,
    cancelCompare: vi.fn(),
    deleteCompare: vi.fn(),
  },
}))

import CompareView from './CompareView.vue'

beforeEach(() => {
  state.ws = []
  state.task = makeTask({})
  state.report = null
  vi.stubGlobal('WebSocket', FakeWebSocket)
  vi.stubGlobal('ResizeObserver', FakeResizeObserver)
})
afterEach(() => {
  vi.unstubAllGlobals()
})

function fireWS(payload: Record<string, any>) {
  // use the LATEST socket: ⑥ remounts the view, and stale sockets from the
  // first (unmounted) instance must not swallow the broadcast
  const inst = state.ws[state.ws.length - 1]
  if (!inst) throw new Error('WS not connected')
  inst.onmessage({ data: JSON.stringify({ type: 'compare', task_id: 'durrun', ...payload }) })
}

async function openTask() {
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

function doneBoxText(w: ReturnType<typeof mount>) {
  const box = w.find('.done-tables-box')
  if (!box.exists()) throw new Error('已完成表 box not rendered')
  return box.text()
}

describe('compare per-table duration (FEAT-CMP-TBL-DURATION)', () => {
  it('① table-done WS broadcast lights the finished table name/status/duration immediately', async () => {
    const w = await openTask()
    fireWS({ status: 'running', tables_done: 1, tables_total: 3, current_table: 'orders', table_status: 'pass', table_duration: '12.500s', possibly_stuck: false })
    await flushPromises()
    const txt = doneBoxText(w)
    expect(txt).toContain('orders')
    expect(txt).toContain('12.500s')
    expect(txt).toContain('通过')
    w.unmount()
  })

  it('② second finished table accumulates, newest first', async () => {
    const w = await openTask()
    fireWS({ status: 'running', tables_done: 1, tables_total: 3, current_table: 'orders', table_status: 'pass', table_duration: '12.500s' })
    fireWS({ status: 'running', tables_done: 2, tables_total: 3, current_table: 'users', table_status: 'fail', table_duration: '1m2.000s' })
    await flushPromises()
    const txt = doneBoxText(w)
    expect(txt).toContain('orders')
    expect(txt).toContain('users')
    expect(txt).toContain('1m2.000s')
    expect(txt).toContain('失败')
    expect(txt.indexOf('users')).toBeLessThan(txt.indexOf('orders')) // 最新在前
    expect(w.findAll('.done-tables-row')).toHaveLength(2)
    w.unmount()
  })

  it('③ chunk-progress broadcasts (no table_status) never touch the list', async () => {
    const w = await openTask()
    fireWS({ status: 'running', tables_done: 1, tables_total: 3, current_table: 'orders', table_status: 'pass', table_duration: '12.500s' })
    fireWS({ status: 'running', current_table: 'orders', current_chunk: 4, total_chunks: 9, possibly_stuck: false })
    await flushPromises()
    expect(w.findAll('.done-tables-row')).toHaveLength(1)
    w.unmount()
  })

  it('④⑤ completed: result table lights per-table 耗时 and CSV carries it (pre-wired columns, backend-filled)', async () => {
    state.task = makeTask({ status: 'completed', mode: 'sample', tables_done: 2, tables_total: 2 })
    state.report = {
      overall_status: 'pass',
      start_time: '2026-01-01T00:00:00Z',
      end_time: '2026-01-01T00:02:00Z',
      duration: '2m0.000s',
      tables: [
        { table_name: 'slow_table', status: 'fail', source_rows: 1, target_rows: 2, diff_rows: 1, duration: '5m0.001s', error: 'boom', suggestion: '' },
        { table_name: 'fast_table', status: 'pass', source_rows: 9, target_rows: 9, diff_rows: 0, duration: '0.400s', error: '', suggestion: '' },
      ],
      stats: { total_tables: 2, pass_tables: 1, fail_tables: 1, warn_tables: 0, skip_tables: 0, total_source_rows: 10, total_target_rows: 11, total_diff_rows: 1 },
    }
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    vi.stubGlobal('URL', Object.assign(URL, {
      createObjectURL: vi.fn().mockReturnValue('blob:dur'),
      revokeObjectURL: vi.fn(),
    }))
    const w = await openTask()

    // ④ result table duration column (CompareView 耗时列 = 既有 prop 接线)
    const slowRow = w.findAll('tr').find(r => r.text().includes('slow_table'))
    if (!slowRow) throw new Error('slow_table row not rendered')
    expect(slowRow.text()).toContain('5m0.001s')
    const fastRow = w.findAll('tr').find(r => r.text().includes('fast_table'))
    if (!fastRow) throw new Error('fast_table row not rendered')
    expect(fastRow.text()).toContain('0.400s')

    // ⑤ CSV
    const csvBtn = w.findAll('button').find(b => b.text().includes('导出 CSV'))
    if (!csvBtn) throw new Error('导出 CSV button not rendered')
    await csvBtn.trigger('click')
    await flushPromises()
    const [blob] = (URL.createObjectURL as ReturnType<typeof vi.fn>).mock.calls[0] as Blob[]
    const text = await blob.text()
    const lines = text.split('\r\n')
    const header = lines.find(l => l.includes('表名') && l.includes('耗时'))
    const slowLine = lines.find(l => l.includes('slow_table'))
    const fastLine = lines.find(l => l.includes('fast_table'))
    if (!header || !slowLine || !fastLine) throw new Error('CSV header/table rows missing')
    expect(slowLine).toContain('5m0.001s')
    expect(fastLine).toContain('0.400s')

    clickSpy.mockRestore()
    w.unmount()
  })

  it('⑥ old report (tables without duration) renders without breaking; live list shows「—」for blank durations', async () => {
    state.task = makeTask({ status: 'completed', mode: 'sample', tables_done: 1, tables_total: 1 })
    state.report = {
      overall_status: 'pass',
      start_time: '2026-01-01T00:00:00Z',
      end_time: '2026-01-01T00:01:00Z',
      duration: '1m0.000s',
      tables: [
        // pre-feature report.json: no duration key on table rows
        { table_name: 'legacy_table', status: 'pass', source_rows: 7, target_rows: 7, diff_rows: 0, error: '', suggestion: '' },
      ],
      stats: { total_tables: 1, pass_tables: 1, fail_tables: 0, warn_tables: 0, skip_tables: 0, total_source_rows: 7, total_target_rows: 7, total_diff_rows: 0 },
    }
    const w = await openTask()
    const legacyRow = w.findAll('tr').find(r => r.text().includes('legacy_table'))
    if (!legacyRow) throw new Error('legacy_table row not rendered')
    expect(legacyRow.text()).toContain('7')
    w.unmount()

    // live list: a blank duration entry renders the honest dash, not a crash
    state.task = makeTask({ status: 'running' })
    const w2 = await openTask()
    fireWS({ status: 'running', tables_done: 1, tables_total: 3, current_table: 'legacy_table', table_status: 'warn', table_duration: '' })
    await flushPromises()
    const txt = doneBoxText(w2)
    expect(txt).toContain('legacy_table')
    expect(txt).toContain('—')
    w2.unmount()
  })
})
