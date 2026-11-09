// A1 任务队列（排队中）：状态徽标新增「排队中」+ 位次、排队任务可取消、
// 在飞行任务时轮询保活 —— 均为纯展示/入口层改动，不动既有字段语义，
// 旧 FE（3.19.35）收到未知 status='queued' 时按原文回显不崩（statusText
// 的 `|| s` 兜底既有）。本 spec 沿用仓库惯例：对视图源码做确定性扫描。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const read = (p: string) => readFileSync(resolve(__dirname, p), 'utf8')

describe('compare task queue (A1 排队中)', () => {
  it('status maps know queued (badge + text, unknown statuses still fall through)', () => {
    const src = read('CompareView.vue')
    expect(src).toContain(`s === 'queued' ? 'warning'`)
    expect(src).toContain(`queued: '排队中'`)
    // 旧字段语义不动：未知状态兜底仍存在（3.19.35 容错锚点）
    expect(src).toContain(`}[s] || s`)
    // 列表/详情都要展示排队位次，格式统一为「第 N 位」
    expect(src).toContain('（第 ${queuePosition(row)} 位）')
    expect(src).toContain('（第 ${queuePosition(activeTask)} 位）')
  })

  it('queued rows show the FIFO position (queue_seq based, legacy fallback)', () => {
    const src = read('CompareView.vue')
    expect(src).toContain(`function queuePosition(`)
    expect(src).toContain(`row.status === 'queued' ?`)
    // 严格 FIFO 序由 queue_seq 决定，缺省才退化为 created_at
    expect(src).toContain(`queue_seq`)
  })

  it('queued tasks expose the cancel action (immediate dequeue semantics)', () => {
    const src = read('CompareView.vue')
    expect(src).toContain(`activeTask.status === 'running' || activeTask.status === 'queued'`)
    expect(src).toContain(`'已取消排队'`)
    expect(src).toContain(`'确认取消该排队中的比对任务？任务将移出队列，立即生效。'`)
  })

  it('poll keeps refreshing while ANY row is not settled (queued→running→terminal)', () => {
    // P2-3：只盯当前状态会在「排队→运行→终态」跨轮次 + 丢 WS 终帧时卡在排队中。
    // 改为只要有任一行未落终态就继续刷新。
    const src = read('CompareView.vue')
    expect(src).toContain(`const settled = (s?: string) =>`)
    expect(src).toContain(`tasks.value.every((t) => settled(t.status))`)
  })

  it('poll/report also handle a queued predecessor reaching completed', () => {
    // 排队任务可能在一次轮询间隔内跑完；终帧丢失时报告仍要自动加载。
    const src = read('CompareView.vue')
    expect(src).toContain(`prevStatus === 'running' || prevStatus === 'queued'`)
  })

  it('create discloses queue admission (202) instead of silence', () => {
    const src = read('CompareView.vue')
    expect(src).toContain(`if (data.status === 'queued') ElMessage.success('已有比对任务进行中，本任务已加入队列')`)
  })

  it('api type carries the queued status + refs + queue_seq', () => {
    const src = read('../api/index.ts')
    expect(src).toContain(`'queued' | 'running' | 'completed' | 'failed' | 'cancelled'`)
    expect(src).toContain(`source_ref?: string`)
    expect(src).toContain(`queue_seq?: number`)
  })
})
