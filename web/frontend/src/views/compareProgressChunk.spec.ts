// #t95 FE 小单锚：运行中比对任务的进度区消费后端广播的
// current_chunk/total_chunks/possibly_stuck（「慢与卡可区分」最后展示一公里）：
// ① checksum 模式运行中：current_table 旁展示「chunk i/N」且随 WS 广播递增
// ② quick/sample 模式（无 chunk 字段）：不显示 chunk
// ③ PossiblyStuck=true：醒目提示 + current_chunk 佐证同显
// ④ PossiblyStuck=false：不显示提示
// ⑤ 状态文案/徽章回归不变（比对中/已完成 等 tag 文案不受影响）
// ⑥ 粘滞清标：后端进度广播恒携 possibly_stuck（红队 seq99 ①）——卡住提示
//    展示中收到 progress 广播（possibly_stuck:false）即清标落屏（?? 不再粘滞）
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ElementPlus from 'element-plus'

const { state } = vi.hoisted(() => {
  const state: {
    task: Record<string, any>
    ws: any[]
  } = { task: {}, ws: [] }
  return { state }
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
    id: 'chunkrun',
    name: '进度锚',
    status: 'running',
    source: {},
    target: {},
    mode: 'checksum',
    sample_ratio: 0.01,
    checksum_chunk_size: 50000,
    checksum_parallel: 0,
    parallel: 0,
    concurrency: 4,
    tables: [],
    tables_done: 1,
    tables_total: 4,
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
    getCompareReport: vi.fn().mockResolvedValue({ data: null }),
    cancelCompare: vi.fn(),
    deleteCompare: vi.fn(),
    compareRowDiffUrl: vi.fn(),
    getCompareRowDiffContent: vi.fn().mockResolvedValue({ data: [] }),
  },
}))

import CompareView from './CompareView.vue'

beforeEach(() => {
  state.ws = []
  state.task = makeTask({})
  vi.stubGlobal('WebSocket', FakeWebSocket)
  vi.stubGlobal('ResizeObserver', FakeResizeObserver)
})
afterEach(() => {
  vi.unstubAllGlobals()
})

function fireWS(payload: Record<string, any>) {
  const inst = state.ws[0]
  if (!inst) throw new Error('WS not connected')
  inst.onmessage({ data: JSON.stringify({ type: 'compare', task_id: 'chunkrun', ...payload }) })
}

async function openRunningTask() {
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

describe('compare progress chunk/PossiblyStuck (FE 小单)', () => {
  it('① checksum running: chunk i/N beside current_table, increments via WS broadcast', async () => {
    state.task = makeTask({ current_table: 'orders', current_chunk: 3, total_chunks: 16, possibly_stuck: false })
    const w = await openRunningTask()
    expect(w.text()).toContain('当前：orders，chunk 3/16')

    fireWS({ current_table: 'orders', current_chunk: 8, total_chunks: 16 })
    await flushPromises()
    expect(w.text()).toContain('chunk 8/16')
    w.unmount()
  })

  it('② quick/sample mode without chunk fields: no chunk display', async () => {
    state.task = makeTask({ mode: 'quick', current_table: 'orders', name: '进度锚' })
    const w = await openRunningTask()
    const body = w.text()
    expect(body).toContain('当前：orders')
    expect(body).not.toContain('chunk ')
    w.unmount()
  })

  it('③ PossiblyStuck=true: warning hint with chunk evidence', async () => {
    state.task = makeTask({ current_table: 'orders', current_chunk: 7, total_chunks: 16, possibly_stuck: true })
    const w = await openRunningTask()
    expect(w.text()).toContain('疑似卡住')
    expect(w.text()).toContain('当前卡在：orders，chunk 7/16')
    w.unmount()
  })

  it('④ PossiblyStuck=false: no stuck hint', async () => {
    state.task = makeTask({ current_table: 'orders', current_chunk: 3, total_chunks: 16, possibly_stuck: false })
    const w = await openRunningTask()
    expect(w.text()).not.toContain('疑似卡住')
    w.unmount()
  })

  it('⑤ status wording/tag unchanged (比对中 badge, no auto-action)', async () => {
    state.task = makeTask({ current_chunk: 3, total_chunks: 16 })
    const w = await openRunningTask()
    expect(w.text()).toContain('比对中')
    // 纯展示：无取消/超时按钮自动触发——取消按钮仍为手动唯一动作
    const cancelBtn = w.findAll('button').find(b => b.text().includes('取消'))
    expect(cancelBtn).toBeTruthy()
    w.unmount()
  })

  it('⑥ possibly_stuck clears on progress broadcast (no ?? stickiness)', async () => {
    state.task = makeTask({ current_table: 'orders', current_chunk: 7, total_chunks: 16, possibly_stuck: true })
    const w = await openRunningTask()
    expect(w.text()).toContain('疑似卡住')

    // Backend OnTableDone/OnChunkProgress now carry possibly_stuck explicitly;
    // the field must be the authoritative value, not merged from a stale true.
    fireWS({ current_table: 'orders', current_chunk: 9, total_chunks: 16, possibly_stuck: false })
    await flushPromises()
    expect(w.text()).not.toContain('疑似卡住')
    expect(w.text()).toContain('chunk 9/16')
    w.unmount()
  })
})
