// A2 多库批量任务 (#102) FE 锚：
// - 创建面：多库开关 + 库名/通配多选 + 排除清单 + 口径提示；开关关闭或
//   列表为空时 payload 不携带 databases/exclude_databases（旧单库字节兼容）；
// - 进度面：「库 X/Y · 当前库 · 表 done/total」与整体百分比合成；
// - 结果面：跨库汇总表（task.database_results）+ 汇总 CSV/报告下载（服务端
//   工件）+ 查看 按库加载单库报告（?db=）；
// - api 契约：getCompareReport 的 db 选择器、compareSummaryCsvUrl、
//   rowdiff 的 ?db= 作用域、CompareTask/CompareDatabaseResult 类型。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = () => readFileSync(resolve(__dirname, 'CompareView.vue'), 'utf8')
const apiSrc = () => readFileSync(resolve(__dirname, '..', 'api', 'index.ts'), 'utf8')

describe('compareMultiDb A2 (#102) — create face', () => {
  it('multi-db switch + selection/exclusion tag inputs + wildcard hint', () => {
    const s = src()
    expect(s).toContain('const multiDb = ref(false)')
    expect(s).toContain('const multiDbs = ref<string[]>([])')
    expect(s).toContain('const excludeDbs = ref<string[]>([])')
    expect(s).toContain('v-model="multiDb"')
    expect(s).toContain('v-model="multiDbs"')
    expect(s).toContain('v-model="excludeDbs"')
    // 口径提示：通配符集合 + 目标同名库 + 串行执行（资源安全）
    expect(s).toContain('通配符支持 * 与 ?')
    expect(s).toContain('目标 TiDB 需已建同名库')
    expect(s).toContain('库与库之间串行执行')
  })

  it('empty selection under an ON switch is rejected before submit', () => {
    const s = src()
    expect(s).toContain('多库批量已开启，请至少填写一个库名或通配模式')
  })

  it('payload carries databases/exclude_databases ONLY when the switch is on with entries (legacy byte-compat)', () => {
    const s = src()
    expect(s).toContain("...(multiDb.value && dbSel.length > 0 ? {")
    expect(s).toContain('databases: dbSel')
    expect(s).toContain('exclude_databases: excludeDbs.value')
  })
})

describe('compareMultiDb A2 (#102) — progress + summary faces', () => {
  it('running line prefixes 「库 X/Y（当前：db）」 on multi-db tasks', () => {
    const s = src()
    expect(s).toContain('multiProgress')
    expect(s).toContain('库 {{ activeTask.databases_done || 0 }} / {{ activeTask.databases_total }}')
    expect(s).toContain('（当前：{{ activeTask.current_database }}）')
  })

  it('overall progress = finished databases + current db fraction, clamped at 100', () => {
    const s = src()
    expect(s).toContain('const done = (t.databases_done || 0) + perDb')
    expect(s).toContain('Math.min(100, Math.round((done / t.databases_total) * 100))')
  })

  it('WS broadcast consumes the A2 counters (absent keys leave single-db untouched)', () => {
    const s = src()
    expect(s).toContain('activeTask.value.databases_done = data.databases_done ?? activeTask.value.databases_done')
    expect(s).toContain('activeTask.value.current_database = data.current_database ?? activeTask.value.current_database')
  })

  it('history 表进度 column shows the 库 X/Y prefix for multi-db rows', () => {
    const s = src()
    expect(s).toContain('库 {{ row.databases_done || 0 }}/{{ row.databases_total }} · ')
  })

  it('cross-db summary table renders database_results with per-db verdicts + downloads + 查看', () => {
    const s = src()
    expect(s).toContain('v-if="activeTask.database_results?.length"')
    expect(s).toContain('跨库汇总（{{ activeTask.database_results.length }} 个库）')
    expect(s).toContain('@click="downloadSummaryCsv"')
    expect(s).toContain('@click="downloadSummaryReport"')
    expect(s).toContain('@click="viewDbReport(row.database)"')
    expect(s).toContain('dbStatusTag(row.status)')
    // loop-only verdict rows get their own vocabulary
    expect(s).toContain("'执行失败'")
    expect(s).toContain("'已取消'")
  })

  it('multi-db tasks never fetch the top-level json face into the report table', () => {
    const s = src()
    expect(s).toContain('const isMultiTask = (t: CompareTask | null | undefined) => !!t && (t.databases_total ?? 0) > 0')
    expect(s).toContain('if (isMultiTask(t)) {')
  })

  it('per-db report tag discloses the open database', () => {
    const s = src()
    expect(s).toContain('v-if="activeDb"')
    expect(s).toContain('库 {{ activeDb }}')
  })

  it('rowdiff downloads + content cache carry the viewed database', () => {
    const s = src()
    expect(s).toContain('apiClient.compareRowDiffUrl(t.id, file, activeDb.value || undefined)')
    expect(s).toContain('apiClient.getCompareRowDiffContent(t.id, file, activeDb.value || undefined)')
    expect(s).toContain("`${activeTask.value?.id ?? ''}#${activeDb.value}#${table}#${rdf?.file ?? ''}`")
  })
})

describe('compareMultiDb A2 (#102) — api contract', () => {
  it('CompareTask carries the A2 fields; CompareDatabaseResult exported', () => {
    const a = apiSrc()
    expect(a).toContain('databases?: string[]')
    expect(a).toContain('exclude_databases?: string[]')
    expect(a).toContain('resolved_databases?: string[]')
    expect(a).toContain('databases_done?: number')
    expect(a).toContain('databases_total?: number')
    expect(a).toContain('current_database?: string')
    expect(a).toContain('database_results?: CompareDatabaseResult[]')
    expect(a).toContain("export interface CompareDatabaseResult")
    expect(a).toContain("'pass' | 'warn' | 'fail' | 'error' | 'cancelled'")
  })

  it('CreateCompareRequest gains databases/exclude_databases (optional = legacy absent)', () => {
    const a = apiSrc()
    expect(a).toContain('databases?: string[]')
    expect(a).toContain('exclude_databases?: string[]')
  })

  it('getCompareReport signature: format union + db selector; csv summary URL helper', () => {
    const a = apiSrc()
    expect(a).toContain("(id: string, format: 'html' | 'json' | 'csv', db?: string)")
    expect(a).toContain('compareSummaryCsvUrl')
    expect(a).toContain('`/api/v1/compare/tasks/${id}/report?format=csv`')
  })

  it('rowdiff URL + content GET accept the db scope', () => {
    const a = apiSrc()
    expect(a).toContain('compareRowDiffUrl: (id: string, file: string, db?: string)')
    expect(a).toContain('(db ? `?db=${encodeURIComponent(db)}`')
    expect(a).toContain('getCompareRowDiffContent: (id: string, file: string, db?: string)')
  })
})
