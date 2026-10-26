// 第9单锚点（Mongo 水位化对比）：CompareCreateView 水位卡片 + API 类型。
// 卡片照搬 pg/mysql CompareView 模式（区间/单值切换、日期时间选择器、
// 跑前确认弹窗、知情提示文案、例外/全局✓ tag）；FE 激活/解析链与后端
// wmfilter/watermark.go 严格镜像（例外→全局→全量）。
// 水位列自动发现单（seq=39 ④）: 候选流落地——_id 默认项 + suggest API
// top-5（覆盖/分数 label、按类型直方图自动切 kind、warnings 披露）+ 手输。
// 断言形态 = spec exact match（族惯例）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8')

describe('compareMongoWatermark v3.19.32', () => {
  const s = src('CompareCreateView.vue')
  const api = src('../api/index.ts')

  it('watermark card: _id default candidate + kind radio + interval toggle', () => {
    expect(s).toContain('水位过滤（可选）')
    expect(s).toContain(`const wmColumnOptions = computed(() => ['_id', ...wmCandidates.value.map(c => c.column).filter(c => c !== '_id')])`)
    expect(s).toContain(`const wmKind = ref<'objectid' | 'datetime' | 'int'>('objectid')`)
    expect(s).toContain('<el-radio-button value="objectid">ObjectId</el-radio-button>')
    expect(s).toContain(':value="true">区间</el-radio-button>')
  })

  it('candidates are non-exhaustive (index + first-50 sample) and invalidated on srcDb change', () => {
    // 对抗席 c5: the disclosure must say candidates are inferred, manual
    // entry stays open, and the run-time probe remains the authority.
    expect(s).toContain('候选水位列基于索引与前 50 条采样文档推断、非穷尽；未列出的列可直接手输，运行期探针仍是权威')
    // 对抗席 d5: a database switch drops stale coverage labels.
    expect(s).toContain('watch(srcDb, () => {')
  })

  it('suggest flow: button gated on srcDb, API carries the source snapshot + database', () => {
    expect(s).toContain('获取候选水位列')
    expect(s).toContain(`:disabled="!srcDb"`)
    expect(s).toContain(`await apiClient.suggestMongoWatermark({ ...endpointOf('src'), database: srcDb.value })`)
    expect(s).toContain('未发现可比类型的候选水位列（date/int/long）；可手输列名')
    expect(api).toContain('suggestMongoWatermark: (source: CompareEndpoint & { database: string })')
    expect(api).toContain("'/mongocompare/suggest-watermark'")
  })

  it('candidate label carries coverage/score; kind derives from the type histogram', () => {
    expect(s).toContain('function wmColLabel(c: string): string')
    expect(s).toContain("'_id（默认，ObjectId 时间戳）'")
    expect(s).toContain('覆盖 ${cand.matched_tables.length}/${wmSuggestTotal.value || \'?\'} · ${cand.score.toFixed(0)} 分')
    expect(s).toContain(`if ((hist.datetime || 0) >= (hist.bigint || 0) + (hist.integer || 0)) return 'datetime'`)
    expect(s).toContain(`function wmKindForColumn(col: string): 'objectid' | 'datetime' | 'int'`)
  })

  it('candidate disclosure mirrors the pg page (warnings + unmatched tooltip)', () => {
    expect(s).toContain('v-if="activeWmCandidate"')
    expect(s).toContain('；{{ activeWmCandidate.warnings[0] }}')
    expect(s).toContain('缺该列 {{ activeWmCandidate.unmatched_tables.length }} 个集合（悬停查看，运行期将全量比对）')
  })

  it('datetime pickers ride the pg/mysql value-format convention', () => {
    expect(s).toContain(`v-model="wmConfig.value" type="datetime"`)
    expect(s).toContain('value-format="YYYY-MM-DD HH:mm:ss" placeholder="水位值 YYYY-MM-DD HH:mm:ss"')
    expect(s).toContain(`v-model="wmConfig.range_start" type="datetime"`)
    expect(s).toContain('placeholder="区间终点（不含）"')
  })

  it('resolution mirror: override → global → full, with the activation twin', () => {
    expect(s).toContain('function collWmActive(e: CollWmEntry | undefined): e is CollWmEntry')
    expect(s).toContain('wmEnabled.value')
    expect(s).toContain('（未配置例外的集合按此过滤）')
    expect(s).toContain('例外优先于全局（同一集合仅一条）；例外预检失败 = 该集合失败，不退化')
  })

  it('pre-run confirm dialog + informed-consent text', () => {
    expect(s).toContain("'水位过滤确认'")
    expect(s).toContain("'开始比对'")
    expect(s).toContain('两端按同一窗口过滤后比对')
    expect(s).toContain('水位前的旧文档改动、两端都已删除的文档检测不到')
    expect(s).toContain('缺字段文档两端一致排除')
    expect(s).toContain('区间为半开 [start, end)，终点不含')
    expect(s).toContain('自动退化为全量比对并在报告注明')
  })

  it('submit plumbs watermark/table_watermarks; base_mode is never a mongo dimension', () => {
    expect(s).toContain('watermark,')
    expect(s).toContain('table_watermarks: tableWatermarks,')
    expect(s).toContain('tableWatermarks = overrides.map(([table, e]) => ({')
    expect(s).not.toContain('base_mode')
  })

  it('range order compares in the type domain (ObjectId hex by byte order)', () => {
    // 对抗票 P2: "..0F" < "..0a" as raw strings but 0x0F > 0x0A as bytes
    // — the FE order gate case-folds ObjectId hex (lowercased ASCII order
    // == byte order); datetime stays fixed-width string order.
    expect(s).toContain(`const ordLt = (a: string, b: string) => (wmKind.value === 'objectid' ? a.toLowerCase() < b.toLowerCase() : wmKind.value === 'int' ? Number(a) < Number(b) : a < b)`)
    expect(s).toContain('if (!ordLt(g.range_start.trim(), g.range_end.trim()))')
    expect(s).not.toContain('g.range_start.trim() >= g.range_end.trim()')
  })

  it('api types: CompareTaskSpec carries both groups (no base_mode)', () => {
    expect(api).toContain('watermark?: { column: string; value?: string; op?: string; range_start?: string; range_end?: string }')
    expect(api).toContain("table_watermarks?: { table: string; column: string; value?: string; op?: string; range_start?: string; range_end?: string }[]")
  })
})
