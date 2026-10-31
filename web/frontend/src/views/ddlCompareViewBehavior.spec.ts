// DDL 对比页行为锚（统一修复轮 D3）：空库对空空的运行期渲染。
// 后端对双空库返回的 tables 类不带 tables 字段（Category.Tables
// omitempty），汇总差异列的 row.tables.filter 会抛 TypeError 崩掉结果
// 卡。本锚走真实运行路径（mount → 设定两端 → 点运行 → 渲染汇总），
// 断言守卫 (row.tables || []) 生效：不崩、计数为 0。
import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { nextTick } from 'vue'

const { mockRun } = vi.hoisted(() => ({ mockRun: vi.fn() }))

vi.mock('../api', () => ({
  default: {
    listDataSources: vi.fn().mockResolvedValue({ data: { datasources: [] } }),
    ddlCompareSchemas: vi.fn().mockResolvedValue({ data: { schemas: [] } }),
    ddlCompareTasks: vi.fn().mockResolvedValue({ data: { tasks: [] } }),
    ddlCompareReportURL: (id: string, format: string) =>
      `/api/v1/ddl-compare/tasks/${id}/report?format=${format}`,
    ddlCompareRun: mockRun,
  },
}))

import DdlCompareView from './DdlCompareView.vue'

// 空对空结果：tables/indexes 类 omitempty 无明细字段；名称类 Names 为
// 空集对象（内部三列表亦 omitempty）。返回按 axios 响应信封 { data: ... }。
const emptyRunResponse = {
  data: {
    task_id: 'dcempty1',
    result: {
      generated_at: '2026-10-30T09:00:00Z',
      duration: '0.05s',
      overall: 'pass',
      options: { ignore_case: true, loose_types: true, max_objects: 20000 },
      source: { kind: 'source', type: 'postgres', host: 'pg', port: 5432, schema: 'public' },
      target: { kind: 'target', type: 'tidb', host: 'tidb', port: 4000, schema: 'db' },
      categories: [
        { key: 'tables', label: '表', source_count: 0, target_count: 0, matched: 0, verdict: 'pass' },
        { key: 'views', label: '视图', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
        { key: 'procedures', label: '存储过程', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
        { key: 'functions', label: '自定义函数', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
        { key: 'triggers', label: '触发器', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
        { key: 'sequences', label: '序列', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
        { key: 'users', label: '用户', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
        { key: 'privileges', label: '权限', source_count: 0, target_count: 0, matched: 0, verdict: 'pass',
          privileges: { comparable: true, users: [] } },
      ],
      warnings: [],
    },
  },
}

async function mountAndRun() {
  const w = mount(DdlCompareView, {
    attachTo: document.body,
    global: { plugins: [ElementPlus] },
  })
  await flushPromises()
  const vm = w.vm as any
  vm.sourceRef = 'src-ref'
  vm.targetRef = 'tgt-ref'
  vm.sourceSchema = 'public'
  vm.targetSchema = 'db'
  await nextTick()
  const btn = w.find('[data-testid="dc-run"]')
  expect((btn.element as HTMLButtonElement).disabled, 'run button enabled once both sides set').toBe(false)
  await btn.trigger('click')
  await flushPromises()
  return w
}

describe('ddlCompareView behavior (D3 empty-vs-empty)', () => {
  it('renders the zero-count summary without throwing when categories omit detail fields', async () => {
    mockRun.mockReset().mockResolvedValue(emptyRunResponse)
    const w = await mountAndRun()
    // The result card rendered at all (no render-level TypeError killed it).
    expect(w.find('[data-testid="dc-summary"]').exists(), 'summary table rendered').toBe(true)
    expect(w.text()).toContain('总体结论：通过')
    // Guarded zero counts in the tables summary row.
    const body = document.body.innerHTML
    expect(body).toContain('仅源 0')
    expect(body).toContain('仅目标 0')
    expect(body).toContain('结构不一致 0')
    w.unmount()
  })

  it('renders a populated run side by side (guard must not swallow real counts)', async () => {
    const populated = {
      data: {
        task_id: 'dcfull1',
        result: {
          ...emptyRunResponse.data.result,
          overall: 'fail',
          categories: [
            {
              key: 'tables', label: '表', source_count: 2, target_count: 1, matched: 0, verdict: 'fail',
              tables: [
                { table: 'orders', source_only: true, verdict: 'fail' },
                { table: 'users', verdict: 'pass' },
              ],
            },
            { key: 'views', label: '视图', source_count: 1, target_count: 2, matched: 1, verdict: 'fail',
              names: { source_only: ['v_old'], target_only: ['v_new'], common: ['v_users'] } },
            { key: 'procedures', label: '存储过程', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
            { key: 'functions', label: '自定义函数', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
            { key: 'triggers', label: '触发器', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
            { key: 'sequences', label: '序列', source_count: 1, target_count: 0, matched: 0, verdict: 'fail',
              names: { source_only: ['seq_users'] },
              note: '目标端无此对象类型（TiDB/MySQL 无 SEQUENCE 对象）' },
            { key: 'users', label: '用户', source_count: 1, target_count: 1, matched: 1, verdict: 'pass',
              names: { common: ['app'] } },
            { key: 'privileges', label: '权限', source_count: 1, target_count: 1, matched: 0, verdict: 'warn',
              note: '跨方言权限词汇表不相交，不做结论',
              privileges: { comparable: false, cross_dialect: true, users: [
                { user: 'app', source: ['SUPER'], target: ['Select_priv'], equal: false },
              ] } },
          ],
        },
      },
    }
    mockRun.mockReset().mockResolvedValue(populated)
    const w = await mountAndRun()
    const body = document.body.innerHTML
    expect(body).toContain('仅源 1')
    expect(body).toContain('结构不一致 0')
    expect(body).toContain('v_old')
    expect(body).toContain('目标端无此对象类型')
    expect(body).toContain('跨方言权限词汇表不相交，不做结论')
    expect(w.text()).toContain('总体结论：失败')
    w.unmount()
  })

  it('renders v1.4 hint (提示级) rows without failing the run (cross-dialect downgrade)', async () => {
    // 跨方言固有差异（排序规则词汇表/权限词汇表）→ verdict=hint：提示级
    // 徽标、结构不一致计数排除 hint、总体结论仍为通过。
    const hinted = {
      data: {
        task_id: 'dchint1',
        result: {
          ...emptyRunResponse.data.result,
          overall: 'pass',
          categories: [
            {
              key: 'tables', label: '表', source_count: 2, target_count: 2, matched: 2, verdict: 'hint',
              tables: [
                { table: 'users', verdict: 'hint',
                  pk_diff: { source: ['id'], target: ['id'], equal: true },
                  collation_diff: { source: 'en_US.UTF-8', target: 'utf8mb4_bin', equal: false,
                    note: '跨方言排序规则词汇表不相交，仅提示（不判错）' } },
                { table: 'orders', verdict: 'pass' },
              ],
            },
            { key: 'views', label: '视图', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
            { key: 'procedures', label: '存储过程', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
            { key: 'functions', label: '自定义函数', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
            { key: 'triggers', label: '触发器', source_count: 0, target_count: 0, matched: 0, verdict: 'pass', names: {} },
            { key: 'sequences', label: '序列', source_count: 1, target_count: 1, matched: 1, verdict: 'pass',
              names: { common: ['s1'] } },
            { key: 'users', label: '用户', source_count: 1, target_count: 1, matched: 1, verdict: 'pass',
              names: { common: ['app'] } },
            { key: 'privileges', label: '权限', source_count: 1, target_count: 1, matched: 0, verdict: 'hint',
              note: '跨方言权限词汇表不相交，不做结论',
              privileges: { comparable: false, cross_dialect: true, users: [
                { user: 'app', source: ['SUPER'], target: ['Select_priv'], equal: false },
              ] } },
          ],
        },
      },
    }
    mockRun.mockReset().mockResolvedValue(hinted)
    const w = await mountAndRun()
    const body = document.body.innerHTML
    // 提示级计入「提示」而排除出「结构不一致」。
    expect(body).toContain('结构不一致 0')
    expect(body).toContain('提示 1')
    // hint 徽标文案渲染（表行 + 类汇总行共用 verdictText）。
    expect(w.text()).toContain('提示')
    expect(w.text()).toContain('总体结论：通过')
    // 跨方言权限行渲染「提示（不判）」而非 warn 文案。
    expect(w.text()).toContain('提示（不判）')
    w.unmount()
  })
})
