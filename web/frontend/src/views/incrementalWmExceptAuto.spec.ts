// 时间戳水位补齐-例外表自动铺底（FEAT-WM-AUTO-EX）：批量应用水位候选后，
// 候选覆盖不了的表（缺列 / 表名特殊字符 / 超批量上限溢出）自动列入
// 「例外表（单独配置）」。只增不覆盖用户行；大小写折叠去重（镜像 BE
// strings.ToLower 去重与批量勾选）；再应用先清未动过的自动行再重铺；
// 用户一编辑即升格手动行；空水位列保存前点名阻断（BE `invalid
// watermark column %q` 400 的 FE 先行镜像）；超 WM_AUTO_EXCEPT_MAX(50)
// 上限部分显式披露、绝不静默丢弃。
// 断言形=spec 精确匹配（本仓 vitest 惯例，禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () => readFileSync(resolve(__dirname, 'IncrementalView.vue'), 'utf8')
const goIncSrc = () => readFileSync(resolve(__dirname, '../../../../internal/webapi/incremental.go'), 'utf8')

describe('incremental watermark exception auto-population (FEAT-WM-AUTO-EX)', () => {
  it('auto rows carry an in-memory flag that never reaches the submit body', () => {
    const src = viewSrc()
    expect(src).toContain('_autoExcept?: boolean')
    expect(src).toContain('_autoExcept: true')
    // 提交体按字段逐个构造，标记天然不入请求（标记不持久化）
    expect(src).toContain('tables: rows.map(t => ({ table: t.table, watermark_column: t.watermark_column, initial_watermark: t.initial_watermark || \'\' }))')
    // 手动添加的行永不带标记；编辑回填行同样不带
    expect(src).toContain("form.tables.push({ table: '', watermark_column: '', initial_watermark: '', columns: [] })")
    expect(src).toContain("form.tables = j.tables.map(t => ({ table: t.table, watermark_column: t.watermark_column, initial_watermark: t.initial_watermark || '', columns: [] }))")
  })

  it('apply routes every non-batch table (missing/badName/overflow) into the sync', () => {
    const src = viewSrc()
    expect(src).toContain('syncAutoExceptRows(c, badNames, overflowed ? safe : [])')
    expect(src).toContain("...(c.unmatched_tables || []).map(name => ({ name, kind: 'missing' as const }))")
    expect(src).toContain("...badNames.map(name => ({ name, kind: 'badName' as const }))")
    expect(src).toContain("...overflowTables.map(name => ({ name, kind: 'overflow' as const }))")
    // 两处既有 warning 文案改指例外区（不再是「请去例外区」的纯提示）
    expect(src).toContain('已列入例外区逐表配置')
    expect(src).toContain('超限表已列入例外区（可手选或分批）')
  })

  it('re-sync drops only untouched auto rows; dup-guard folds case like the BE', () => {
    const src = viewSrc()
    expect(src).toContain('form.tables = form.tables.filter(t => !t._autoExcept)')
    expect(src).toContain('...form.tables.map(t => t.table.trim().toLowerCase()),')
    expect(src).toContain('...batchTables.value.map(t => t.trim().toLowerCase()),')
    expect(src).toContain("const key = p.name.toLowerCase()")
    // BE 镜像锚（verbatim）：大小写折叠去重 + 空水位列 400
    const go = goIncSrc()
    expect(go).toContain('if seen[strings.ToLower(t.Table)] {')
    expect(go).toContain('if !incIdentifierOK(t.WatermarkColumn) {')
    expect(go).toContain('return fmt.Errorf("invalid watermark column %q", t.WatermarkColumn)')
  })

  it('cap 50 with explicit truncation disclosure, never silent drops', () => {
    const src = viewSrc()
    expect(src).toContain('const WM_AUTO_EXCEPT_MAX = 50')
    expect(src).toContain('if (added.length >= WM_AUTO_EXCEPT_MAX) {')
    // 截断计数只计超限跳过：已在配置中的 dup 跳过不得混入 truncated 披露
    expect(src).toContain('if (seen.has(key)) continue // already configured — not a truncation')
    expect(src).toContain('truncated++ // cap-skipped only; never count dup-skips here')
    expect(src).toContain('autoExceptMeta.value = total > 0 ? { ...counts, truncated } : null')
    expect(src).toContain('另有 {{ autoExceptMeta.truncated }} 表超过单次铺底上限 {{ WM_AUTO_EXCEPT_MAX }} 未自动列入')
    expect(src).toContain('表超铺底上限未列入')
  })

  it('summary line + per-row tag disclose the auto-listed set', () => {
    const src = viewSrc()
    expect(src).toContain('已自动列入例外 {{ form.tables.filter(t => t._autoExcept).length }} 表（缺列 {{ autoExceptMeta.missing }} · 特殊名 {{ autoExceptMeta.badName }} · 超限 {{ autoExceptMeta.overflow }}）')
    expect(src).toContain('<el-tag v-if="t._autoExcept" type="info" size="small" effect="plain" style="margin-left: 4px;">自动列入</el-tag>')
    expect(src).toContain('已自动列入下方例外区，请逐表选择各自水位列')
  })

  it('any user edit promotes the row to manual — re-sync never touches it', () => {
    const src = viewSrc()
    expect(src).toContain('function touchRow(i: number)')
    expect(src).toContain('if (row?._autoExcept) row._autoExcept = false')
    // 升格挂点：表名/水位列选择（onTableChanged/onColumnPicked）+ 手输水位列
    // + 三种初始水位输入（datetime/date/text）＝ 6 处 touchRow(i)
    expect((src.match(/touchRow\(i\)/g) || []).length).toBe(6)
  })

  it('source switch (and dialog reset) never leaks stale auto rows', () => {
    const src = viewSrc()
    // resetTablePicker 是 source_ref watch / openCreate / openEdit 三路公共清理点
    expect(src).toContain('form.tables = form.tables.filter(t => !t._autoExcept)')
    expect(src).toContain('autoExceptMeta.value = null')
  })

  it('save blocks empty watermark columns and names the offending tables', () => {
    const src = viewSrc()
    expect(src).toContain('未选水位列（例外区自动列入的表需逐表选择水位列后再保存）')
    expect(src).toContain("emptyWm.length > 0")
  })
})
