// coll-select ②锚点（详情页集合结果表格）：TaskDetailView 内嵌集合级
// 结果表 + CSV 导出。数据源 = summary.json 逐字节产物（与「下载 JSON」
// 同一字节流，零新读端点）；过滤视图导出；部分选择的 selected/matched
// 披露对（引擎 Stats）直出。断言形态 = spec exact match（族惯例）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('taskDetailMongoColls v3.19.33', () => {
  const s = src('TaskDetailView.vue')

  it('loads the verbatim summary.json artifact once per finalized run', () => {
    expect(s).toContain(`await apiClient.getTaskReport(taskId, 'json')`)
    expect(s).toContain('mcSummary.value = JSON.parse(data as string) as McSummary')
    // In-flight guard (正评席 N2): a slow request must not overlap the 5s poll.
    expect(s).toContain('if (mcSummary.value || mcSummaryLoading.value) return')
  })

  it('watch(task): finalized states fetch, restart/resume clears the stale view', () => {
    expect(s).toContain("if (isCompare.value && (t.status === 'completed' || t.status === 'failed')) {")
    expect(s).toContain('// A restart/resume invalidates the artifact view until the new round')
    expect(s).toContain('mcSummary.value = null')
  })

  it('result table: per-collection columns incl. watermark state and hash mismatch', () => {
    expect(s).toContain('<span>集合结果</span>')
    expect(s).toContain('<el-table-column prop="coll" label="集合名" min-width="220" show-overflow-tooltip />')
    expect(s).toContain('<el-tag v-else-if="row.wm_state === \'degraded\'" type="warning" size="small">已退化</el-tag>')
    expect(s).toContain('{{ row.checks?.count?.src ?? \'-\' }} / {{ row.checks?.count?.dst ?? \'-\' }}')
    expect(s).toContain('function mcHashMismatch(row: McCollectionRow): number')
  })

  it('expand row carries the five-level detail (schema/index/count/hash/diff)', () => {
    expect(s).toContain('<el-descriptions :column="2" border size="small" style="padding: 8px;">')
    expect(s).toContain("Array.isArray(row.checks?.schema?.diff) ? row.checks.schema.diff.join('、')")
    expect(s).toContain('共 {{ row.checks?.hash?.length ?? 0 }} 桶，不一致 {{ mcHashMismatch(row) }} 桶')
    expect(s).toContain('明细见 diff 目录 ndjson')
  })

  it('toolbar: overall filter + name search + CSV export of the FILTERED view', () => {
    expect(s).toContain('const mcOverallFilter = ref(\'all\')')
    expect(s).toContain('rows = rows.filter(r => (r.coll || \'\').toLowerCase().includes(q))')
    expect(s).toContain('function mcExportCsv()')
    expect(s).toContain("for (const r of mcFilteredRows.value) {")
  })

  it('CSV: BOM for Excel UTF-8 Chinese + formula-injection guard + quoted cells', () => {
    expect(s).toContain("new Blob(['\\uFEFF' + lines.join('\\r\\n')], { type: 'text/csv;charset=utf-8' })")
    expect(s).toContain("a.download = `mongo-collections-${taskId}.csv`")
    // 对抗席 F2/N1: = + - @ tab/CR cells get a leading single quote FIRST,
    // then the ordinary quote-wrap decision runs (early return would
    // misalign columns for `=a,b`-style values).
    expect(s).toContain("if (/^[=+\\-\\t\\r@]/.test(c)) c = `'${c}`")
    expect(s).toContain('function mcCsvCell(raw: string): string')
  })

  it('partial-selection disclosure pair renders from engine Stats', () => {
    expect(s).toContain('mcSummary.stats.selected_collections !== undefined')
    expect(s).toContain('部分选择：已选 {{ mcSummary.stats.selected_collections }} · 实际比对 {{ mcSummary.stats.matched_collections ?? \'-\' }}')
  })
})
