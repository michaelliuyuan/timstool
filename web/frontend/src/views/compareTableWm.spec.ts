// 比对页例外表水位（P1A/P2A/P3A/P4A）：每表生效水位解析链=例外→全局→
// 全量兜底；例外预检失败该表 FAIL；无全局+有例外仍激活水位模式；表列
// 表行内「水位」列三态渲染（全局✓绿/例外蓝/全量灰）+ 弹窗编辑 + 开跑
// 前摘要确认。断言形=spec 精确匹配（本仓 vitest 惯例，禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CompareView.vue'), 'utf8')
const apiSrc = () =>
  readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')
const goWmSrc = () =>
  readFileSync(resolve(__dirname, '../../../../internal/validator/wmfilter.go'), 'utf8')
const goValidatorSrc = () =>
  readFileSync(resolve(__dirname, '../../../../internal/validator/validator.go'), 'utf8')

describe('compare per-table watermark overrides (P1A-P4A)', () => {
  it('API types carry table_watermarks on create + options', () => {
    const src = apiSrc()
    expect(src).toContain(`table_watermarks?: { table: string; column: string; value?: string; op?: string; range_start?: string; range_end?: string }[]`)
    // 三处（CreateCompareRequest + CompareOptions + 第9单 CompareTaskSpec mongo 侧）
    expect(src.match(/table_watermarks\?:/g)?.length).toBe(3)
  })

  it('frontend resolution chain mirrors the engine: override → global → full', () => {
    const src = viewSrc()
    expect(src).toContain(`function wmStateFor(name: string)`)
    expect(src).toContain(`if (twmActive(e)) return { kind: 'override', entry: e }`)
    expect(src).toContain(`if (globalWmActive.value) return { kind: 'global' }`)
    expect(src).toContain(`return { kind: 'full' }`)
    // 激活镜像：与后端 wmActive 同规则（列 + 单值/完整区间）
    expect(src).toContain(`const col = e.column.trim() !== ''`)
    expect(src).toContain(`return col && (single || fullRange)`)
  })

  it('engine resolver documented in the same terms (display-vs-execution drift guard)', () => {
    const src = goWmSrc()
    expect(src).toContain(`wmOverrideFor`)
    expect(src).toContain(`hasActiveWMOverrides`)
    expect(src).toContain(`例外水位列：`)
    expect(src).toContain(`水位过滤%s %s %s %s`)
  })

  it('BE activation expression anchored verbatim (twmActive mirror must track it)', () => {
    const src = goWmSrc()
    // Obs1: 前端 twmActive 的激活镜像必须锚定后端 wmActive 的激活表达式本体
    expect(src).toContain(`func wmActive(wm *config.WatermarkFilter) bool`)
    expect(src).toContain(`wm.Column != "" && (wm.Value != "" || (wm.RangeStart != "" && wm.RangeEnd != ""))`)
  })

  it('override precheck FAIL rows skip the zero-count scope note (Obs2)', () => {
    // 生产者/识别者同源 + 盖戳点守卫：例外预检 FAIL 行不盖「（源 0 行/目标 0 行）」注
    expect(goWmSrc()).toContain(`func isWmExFail(tr reporter.TableReport) bool`)
    expect(goWmSrc()).toContain(`const wmExFailPrefix = "例外水位列："`)
    expect(goValidatorSrc()).toContain(`if !isWmExFail(tr)`)
  })

  it('table list renders the tri-state watermark column with inline actions', () => {
    const src = viewSrc()
    expect(src).toContain('<el-table-column label="水位" width="230">')
    expect(src).toContain('<el-tag size="small" type="primary">例外</el-tag>')
    expect(src).toContain('@click="openTwmDialog(row.name)">编辑</el-button>')
    expect(src).toContain('<el-tag size="small" type="success">全局✓</el-tag>')
    expect(src).toContain('<el-tag size="small" type="info">全量</el-tag>')
    expect(src).toContain('设为例外')
  })

  it('override editor dialog: metadata-aided column select, dual form, no-index warn', () => {
    const src = viewSrc()
    expect(src).toContain('v-model="twmDialog"')
    expect(src).toContain(':title="`例外水位 — ${twmTable}`"')
    expect(src).toContain(':disabled="!c.comparable"')
    expect(src).toContain('不可比类型')
    expect(src).toContain('无索引：例外过滤将对该表做全表扫描')
    expect(src).toContain('v-model="twmForm.range_mode"')
    expect(src).toContain('清除例外')
  })

  it('pre-run summary dialog discloses every table effective watermark (>10 collapse)', () => {
    const src = viewSrc()
    expect(src).toContain('confirmTableWmsBeforeRun')
    expect(src).toContain('：例外 ${wmCondShort(st.entry!)}')
    expect(src).toContain('：全局 ${globalWmCondShort.value}')
    expect(src).toContain('：全量比对')
    expect(src).toContain('等共 ${lines.length} 张表')
  })

  it('create payload + options memory carry the ACTIVE overrides only', () => {
    const src = viewSrc()
    expect(src).toContain(`table_watermarks: Object.entries(tableWms.value)`)
    expect(src).toContain(`.filter(([, e]) => twmActive(e))`)
    expect(src).toContain(`applyCompareOptions`)
    expect(src).toContain(`Array.isArray(opts.table_watermarks)`)
  })
})
