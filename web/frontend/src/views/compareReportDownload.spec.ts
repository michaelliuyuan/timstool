// MC-16 数据比对报告下载锚（mount + api mock 口径）：
// - 面板加载走显式 format='json'（服务端默认已是 html 家族报告）；
// - 「下载报告」按钮触发 format='html' 的服务端请求并走 blob 下载
//   （compare-report-{id}.html，单一真源；客户端拼装 exportHtml 已删）；
// - CSV 导出按钮原样保留（回归锚）。
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ElementPlus, { ElMessage } from 'element-plus'
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

// vi.hoisted: the mock factory is hoisted above module init, so every value
// it closes over must be created here too (vitest hoisting rules).
const { mockGetCompareReport, completedTask, jsonReport } = vi.hoisted(() => {
  const completedTask = {
    id: 'mc16dl',
    name: 'MC-16 下载锚',
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
    tables_done: 3,
    tables_total: 3,
    created_at: '2026-01-01T00:00:00Z',
  }
  const jsonReport = {
    overall_status: 'pass',
    start_time: '2026-01-01T00:00:00Z',
    end_time: '2026-01-01T00:01:00Z',
    duration: '1m0.000s',
    tables: [],
    stats: {
      total_tables: 3,
      pass_tables: 3,
      fail_tables: 0,
      warn_tables: 0,
      skip_tables: 0,
      total_source_rows: 100,
      total_target_rows: 100,
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

const apiSrc = () =>
  readFileSync(resolve(__dirname, '..', 'api', 'index.ts'), 'utf8')

async function mountView() {
  return mount(CompareView, {
    attachTo: document.body,
    global: { plugins: [ElementPlus] },
  })
}

async function openReportPanel() {
  const w = await mountView()
  await flushPromises()
  // History table「查看」→ viewTask → loadReport(id, 'json').
  const viewBtn = w.findAll('button').find(b => b.text().includes('查看'))
  if (!viewBtn) throw new Error('查看 button not rendered')
  await viewBtn.trigger('click')
  await flushPromises()
  return w
}

describe('CompareView report download (MC-16)', () => {
  beforeEach(() => {
    vi.stubGlobal('WebSocket', FakeWebSocket)
    vi.stubGlobal('ResizeObserver', FakeResizeObserver)
    vi.stubGlobal('URL', Object.assign(URL, {
      createObjectURL: vi.fn().mockReturnValue('blob:mc16'),
      revokeObjectURL: vi.fn(),
    }))
    mockGetCompareReport.mockClear()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('the report panel loads with the explicit json format', async () => {
    await openReportPanel()
    expect(mockGetCompareReport).toHaveBeenCalledWith('mc16dl', 'json')
  })

  it('下载报告 fires a format=html server request and downloads the blob', async () => {
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const w = await openReportPanel()

    const dlBtn = w.findAll('button').find(b => b.text().includes('下载报告'))
    if (!dlBtn) throw new Error('下载报告 button not rendered')
    await dlBtn.trigger('click')
    await flushPromises()

    expect(mockGetCompareReport).toHaveBeenCalledWith('mc16dl', 'html')
    const [blob] = (URL.createObjectURL as ReturnType<typeof vi.fn>).mock.calls[0] as Blob[]
    expect(blob).toBeInstanceOf(Blob)
    expect(blob.type).toContain('text/html')
    expect(clickSpy).toHaveBeenCalled()
    clickSpy.mockRestore()
    w.unmount()
  })

  it('client-side HTML assembly is gone; CSV export stays (regression)', () => {
    const src = viewSrc()
    expect(src).not.toContain('function exportHtml')
    expect(src).not.toContain('由 TimsTool 生成')
    expect(src).toContain('@click="exportCsv"')
    expect(src).toContain('@click="downloadReport"')
    expect(src).toContain('`compare-report-${t.id}.html`')
  })

  it('api client branches the html face to responseType text', () => {
    const src = apiSrc()
    const i = src.indexOf('getCompareReport')
    const slice = src.slice(i, i + 400)
    expect(slice).toContain("format === 'html'")
    expect(slice).toContain("responseType: 'text'")
    expect(slice).toContain("params: { format }")
  })

  // MC-17 ④: format is required — the MC-16 default flip means a
  // parameterless call would fetch HTML into a JSON-typed consumer.
  it('api client signature requires the format argument (MC-17)', () => {
    const src = apiSrc()
    const i = src.indexOf('getCompareReport')
    const slice = src.slice(i, i + 400)
    expect(slice).toContain("(id: string, format: 'html' | 'json')")
    expect(slice).not.toContain('format?:')
  })

  // MC-17 ③: the html face is responseType 'text' — the server's JSON error
  // body arrives as a raw string; the catch must surface the server cause
  // (404 dual-cause wording) instead of falling through to "undefined".
  it('download failure surfaces the server cause from a text error body (MC-17)', async () => {
    const msgSpy = vi.spyOn(ElMessage, 'error').mockImplementation(() => ({}) as never)
    const w = await openReportPanel()
    mockGetCompareReport.mockRejectedValueOnce({
      response: {
        status: 404,
        data: '{"error":"compare report not found — the task has not produced report.json (unfinished, failed before the compare completed, or the artifact was cleared)"}',
      },
      message: 'Request failed with status code 404',
    })
    const dlBtn = w.findAll('button').find(b => b.text().includes('下载报告'))
    if (!dlBtn) throw new Error('下载报告 button not rendered')
    await dlBtn.trigger('click')
    await flushPromises()
    const shown = msgSpy.mock.calls.map(c => String(c[0])).join('\n')
    expect(shown).toContain('unfinished')
    expect(shown).not.toContain('undefined')
    msgSpy.mockRestore()
    w.unmount()
  })

  // MC-17 ③ companion: a NON-JSON text body must fall back to e.message —
  // never a JSON.parse crash and never "undefined".
  it('a non-JSON text error body falls back to e.message (MC-17)', async () => {
    const msgSpy = vi.spyOn(ElMessage, 'error').mockImplementation(() => ({}) as never)
    const w = await openReportPanel()
    mockGetCompareReport.mockRejectedValueOnce({
      response: { status: 502, data: '<html>bad gateway' },
      message: 'Request failed with status code 502',
    })
    const dlBtn = w.findAll('button').find(b => b.text().includes('下载报告'))
    if (!dlBtn) throw new Error('下载报告 button not rendered')
    await dlBtn.trigger('click')
    await flushPromises()
    const shown = msgSpy.mock.calls.map(c => String(c[0])).join('\n')
    expect(shown).toContain('Request failed with status code 502')
    expect(shown).not.toContain('undefined')
    msgSpy.mockRestore()
    w.unmount()
  })
})
