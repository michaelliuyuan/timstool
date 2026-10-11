import { describe, it, expect } from 'vitest'
import { isCompareTask, parseCompareResult } from './index'
import type { Task } from './index'

// Fixtures mirror the wire contract exactly: task responses carry a
// server-derived `kind` and NEVER a config_json field (credentials are hidden
// server-side; welding the seq=89 gap — fixtures must not self-certify a
// contract the backend does not serve).

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: 't1',
    name: 'task',
    status: 'created',
    kind: 'migration',
    phase: '',
    progress: 0,
    tables_total: 0,
    tables_done: 0,
    rows_total: 0,
    rows_done: 0,
    error: '',
    result_json: '',
    created_at: '2026-10-11T00:00:00Z',
    started_at: null,
    finished_at: null,
    updated_at: '2026-10-11T00:00:00Z',
    ...overrides,
  }
}

describe('isCompareTask — wire-contract kind discrimination', () => {
  it('returns true for kind="compare" (no config_json in fixture, as served)', () => {
    expect(isCompareTask(makeTask({ kind: 'compare' }))).toBe(true)
  })
  it('returns false for kind="migration"', () => {
    expect(isCompareTask(makeTask({ kind: 'migration' }))).toBe(false)
  })
  it('returns false when kind is absent (stale cached response) — falls back to migration rendering', () => {
    const t = makeTask()
    delete (t as Partial<Task>).kind
    expect(isCompareTask(t)).toBe(false)
  })
  it('returns false for null/undefined task objects', () => {
    expect(isCompareTask(null)).toBe(false)
    expect(isCompareTask(undefined)).toBe(false)
  })
})

describe('parseCompareResult — single-parse result discrimination', () => {
  it('returns the compare payload for a clean result_json', () => {
    const t = makeTask({
      kind: 'compare',
      result_json: '{"kind":"compare","overall":"equal","counts":{"equal":2,"diff":0,"src_only":0,"dst_only":0},"duration_sec":1,"summary_path":"s","diff_dir":"d"}',
    })
    const r = parseCompareResult(t)
    expect(r?.overall).toBe('equal')
    expect(r?.counts.equal).toBe(2)
  })
  it('returns null for migration result_json (missing kind)', () => {
    const t = makeTask({ result_json: '{"total": 3}' })
    expect(parseCompareResult(t)).toBeNull()
  })
  it('returns null for empty result or null task', () => {
    expect(parseCompareResult(makeTask())).toBeNull()
    expect(parseCompareResult(null)).toBeNull()
  })
})
