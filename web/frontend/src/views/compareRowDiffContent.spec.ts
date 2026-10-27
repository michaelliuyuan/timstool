// 行内容对照（B-line content probe）FE 锚：展开钻取行懒加载
// {stem}.content.json（一次性缓存）、左右对照渲染（不等值高亮 / *_only
// 单侧「—」/ 截断标记）、页内首 50+加载更多、失败降级保留 CSV 下载、
// 无文件行不发请求；图例 tooltip 与超大表护栏截断标签口径。
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

const contentEntries = [
  {
    key: '3',
    category: 'content_diff' as const,
    src_row: { columns: ['id', 'name'], values: ['3', 'c'] },
    dst_row: { columns: ['id', 'name'], values: ['3', 'd'] },
  },
  {
    key: '4',
    category: 'src_only' as const,
    src_row: { columns: ['id', 'name'], values: ['4', 'x'] },
  },
  {
    key: '6',
    category: 'dst_only' as const,
    dst_row: { columns: ['id', 'name'], values: ['6', 'y'], truncated: true },
  },
]

const { mockGetCompareReport, mockGetContent, completedTask, taskTwin, jsonReport } = vi.hoisted(() => {
  const completedTask = {
    id: 'rd0diff',
    name: '内容对照锚',
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
  // taskTwin: 第二个任务比同一张表（content 文件名由表名派生、跨任务
  // 确定）——N-a 跨任务缓存隔离锚的对手方。
  const taskTwin = { ...completedTask, id: 'rd1diff', name: '跨任务缓存锚' }
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
  const mockGetContent = vi.fn(() => Promise.resolve({ data: contentEntries }))
  return { mockGetCompareReport, mockGetContent, completedTask, taskTwin, jsonReport }
})

vi.mock('../api', () => ({
  default: {
    getSources: vi.fn().mockResolvedValue({ data: { sources: [] } }),
    listDataSources: vi.fn().mockResolvedValue({ data: { datasources: [] } }),
    getCompareOptions: vi.fn().mockResolvedValue({ data: {} }),
    saveCompareOptions: vi.fn().mockResolvedValue({ data: { success: true } }),
    listCompares: vi.fn().mockResolvedValue({ data: [completedTask, taskTwin] }),
    getCompareReport: mockGetCompareReport,
    cancelCompare: vi.fn(),
    deleteCompare: vi.fn(),
    compareRowDiffUrl: (id: string, file: string) =>
      `/api/v1/compare/tasks/${id}/rowdiff/${encodeURIComponent(file)}`,
    getCompareRowDiffContent: mockGetContent,
  },
}))

import CompareView from './CompareView.vue'

const viewSrc = () => readFileSync(resolve(__dirname, 'CompareView.vue'), 'utf8')
const apiSrc = () => readFileSync(resolve(__dirname, '..', 'api', 'index.ts'), 'utf8')

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

async function expandRow(w: Awaited<ReturnType<typeof openReportPanel>>, idx: number) {
  const icons = w.findAll('.el-table__expand-icon')
  if (icons.length <= idx) throw new Error(`expand icon ${idx} not rendered`)
  await icons[idx].trigger('click')
  await flushPromises()
}

describe('CompareView row-diff content probe (行内容对照)', () => {
  beforeEach(() => {
    vi.stubGlobal('WebSocket', FakeWebSocket)
    vi.stubGlobal('ResizeObserver', FakeResizeObserver)
    vi.stubGlobal('URL', Object.assign(URL, {
      createObjectURL: vi.fn().mockReturnValue('blob:rd'),
      revokeObjectURL: vi.fn(),
    }))
    mockGetCompareReport.mockClear()
    mockGetContent.mockClear()
    mockGetContent.mockImplementation(() => Promise.resolve({ data: contentEntries }))
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('lazy-loads content.json once per expanded drilled row (cached)', async () => {
    const w = await openReportPanel()
    await expandRow(w, 0)
    expect(mockGetContent).toHaveBeenCalledTimes(1)
    expect(mockGetContent).toHaveBeenCalledWith('rd0diff', 'orders-9f2c1b7a.content.json')

    // collapse + re-expand → served from the one-shot cache, no refetch
    await expandRow(w, 0)
    await expandRow(w, 0)
    expect(mockGetContent).toHaveBeenCalledTimes(1)
    w.unmount()
  })

  it('renders the side-by-side table: highlight, single-side dash, truncation', async () => {
    const w = await openReportPanel()
    await expandRow(w, 0)
    const body = document.body.innerHTML
    // header + per-entry keys and category labels
    expect(body).toContain('行内容对照')
    expect(body).toContain('共 3 条')
    expect(body).toContain('内容不一致')
    expect(body).toContain('仅源端')
    expect(body).toContain('仅目标端')
    // unequal cell highlighted (name: c vs d), equal cell not
    const diffCells = document.querySelectorAll('.rd-val-diff')
    expect(diffCells.length).toBe(2) // 源端值 + 目标端值 of the differing column
    expect(diffCells[0].textContent).toBe('c')
    expect(diffCells[1].textContent).toBe('d')
    // *_only rows render「—」on the missing side
    expect(document.querySelectorAll('.rd-cmp td').length).toBeGreaterThan(0)
    const dashCells = Array.from(document.querySelectorAll('.rd-cmp td')).filter(td => td.textContent === '—')
    expect(dashCells.length).toBe(4) // src_only dst side + dst_only src side, 2 cols each
    // row-truncation marker rides the truncated dst_only row
    expect(body).toContain('行超 8KB 已截断')
    w.unmount()
  })

  it('pages in-page: first 50, 加载更多 up to the cap', async () => {
    const many = Array.from({ length: 60 }, (_, i) => ({
      key: `k${i}`,
      category: 'content_diff' as const,
      src_row: { columns: ['id'], values: [String(i)] },
      dst_row: { columns: ['id'], values: [String(i + 1)] },
    }))
    mockGetContent.mockImplementation(() => Promise.resolve({ data: many }))
    const w = await openReportPanel()
    await expandRow(w, 0)
    let body = document.body.innerHTML
    expect(body).toContain('前 50 / 共 60 条')
    expect((body.match(/rd-content-entry/g) || []).length).toBe(50)

    const more = w.findAll('button').find(b => b.text().includes('加载更多'))
    if (!more) throw new Error('加载更多 button not rendered')
    await more.trigger('click')
    await flushPromises()
    body = document.body.innerHTML
    expect(body).toContain('前 60 / 共 60 条')
    expect((body.match(/rd-content-entry/g) || []).length).toBe(60)
    w.unmount()
  })

  it('degrades on fetch failure: disclosure line + CSV download stays', async () => {
    mockGetContent.mockImplementation(() => Promise.reject({ response: { data: { error: 'boom' } } }))
    const w = await openReportPanel()
    await expandRow(w, 0)
    await flushPromises()
    const body = document.body.innerHTML
    expect(body).toContain('行内容对照加载失败')
    expect(body).toContain('boom')
    const dlBtn = w.findAll('button').find(b => b.text().includes('下载差异明细'))
    expect(dlBtn).toBeTruthy()
    w.unmount()
  })

  it('note-only rows never fetch (no artifact exists)', async () => {
    const w = await openReportPanel()
    await expandRow(w, 1) // logs row: note-only, no file
    expect(mockGetContent).not.toHaveBeenCalled()
    w.unmount()
  })

  it('aligns columns by NAME (union) — interleaved order + single-side columns (红队⑤)', async () => {
    const entry = {
      key: '9',
      category: 'content_diff' as const,
      // 列序交错夹具：dst 列序完全不同、缺 amount、多 remark——按列序
      // index 配对会逐位错位（id↔status 等），按名对齐才能照见真差异。
      src_row: { columns: ['id', 'amount', 'status'], values: ['9', '100', 'A'] },
      dst_row: { columns: ['status', 'id', 'remark'], values: ['B', '9', 'hi'] },
    }
    mockGetContent.mockImplementation(() => Promise.resolve({ data: [entry] }))
    const w = await openReportPanel()
    await expandRow(w, 0)

    const rows = Array.from(document.querySelectorAll('.rd-cmp tbody tr'))
    expect(rows.length).toBe(4) // union: id, amount, status, remark（源端序在前）
    const cells = (name: string) => {
      const tr = rows.find(r => (r as HTMLElement).querySelectorAll('td')[0]?.textContent === name)
      if (!tr) throw new Error(`column ${name} not rendered`)
      const tds = (tr as HTMLElement).querySelectorAll('td')
      return {
        src: tds[1]?.textContent ?? '',
        dst: tds[2]?.textContent ?? '',
        diff: tr.querySelectorAll('.rd-val-diff').length,
      }
    }
    expect(cells('id')).toEqual({ src: '9', dst: '9', diff: 0 })
    expect(cells('amount')).toEqual({ src: '100', dst: '—', diff: 0 }) // dst 缺列
    expect(cells('status')).toEqual({ src: 'A', dst: 'B', diff: 2 })   // 真差异（列序漂移下仍按名对齐）
    expect(cells('remark')).toEqual({ src: '—', dst: 'hi', diff: 0 })  // src 缺列
    w.unmount()
  })

  it('isolates the content cache across tasks on the SAME table (红队①)', async () => {
    const entriesB = [
      {
        key: '3',
        category: 'content_diff' as const,
        src_row: { columns: ['id', 'name'], values: ['3', 'zzz-B'] },
        dst_row: { columns: ['id', 'name'], values: ['3', 'w-B'] },
      },
    ]
    mockGetContent.mockImplementation((id: string) =>
      Promise.resolve({ data: id === 'rd1diff' ? entriesB : contentEntries }))

    const w = await openReportPanel()
    await expandRow(w, 0) // 任务 A（rd0diff）展开 orders
    expect(mockGetContent).toHaveBeenCalledTimes(1)
    expect(mockGetContent).toHaveBeenCalledWith('rd0diff', 'orders-9f2c1b7a.content.json')

    // 切到任务 B（rd1diff）——同一张表、同一个派生 content 文件名
    const viewBtns = w.findAll('button').filter(b => b.text().includes('查看'))
    expect(viewBtns.length).toBeGreaterThanOrEqual(2)
    await viewBtns[1].trigger('click')
    await flushPromises()
    await expandRow(w, 0)
    // 缓存键含任务维度 → 必须发第二次请求（带 B 的 id），绝不串 A 的旧行
    expect(mockGetContent).toHaveBeenCalledTimes(2)
    expect(mockGetContent).toHaveBeenLastCalledWith('rd1diff', 'orders-9f2c1b7a.content.json')
    expect(document.body.innerHTML).toContain('zzz-B') // 渲染的是 B 的数据
    expect(document.body.innerHTML).not.toContain('>c<') // A 的旧值不得串显

    // B 内部收起再展开 → 一次性缓存照常（不因任务维度而失效）
    await expandRow(w, 0)
    await expandRow(w, 0)
    expect(mockGetContent).toHaveBeenCalledTimes(2)
    w.unmount()
  })

  it('legend tooltips + generalized truncation label are wired in source', () => {
    const src = viewSrc()
    for (const want of [
      '两端按这些列识别同一行（主键/唯一索引）',
      '两端都有该行，但字段值不同',
      '源库有该行，目标库没有',
      '目标库有该行，源库没有',
      '明细已截断（三项计数仍精确）', // 裁定 2：泛化文案，无数字第二真相源
      '差异键示例（前 10，共',    // 统一样例键标签
      'getCompareRowDiffContent',
      'expand-change',
    ]) {
      expect(src).toContain(want)
    }
    // 裁定 2：FE 不得再造 100 万/10 万数字真相源
    expect(src).not.toContain('100 万行')
    expect(src).not.toContain('10 万行')
    // api client exposes the content GET with the encoded derived name
    const api = apiSrc()
    expect(api).toContain('getCompareRowDiffContent')
    expect(api).toContain('/compare/tasks/${id}/rowdiff-content/')
    expect(api).toContain('CompareRowDiffContentEntry')
  })
})
