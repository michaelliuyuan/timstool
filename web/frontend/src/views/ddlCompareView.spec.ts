// ddlCompareView v3.19.40 结构锚（ddlSectionLabel.spec.ts 家族）：
// DDL 对比页的结构性约束——精确断言关键结构字面量，禁页面级盲扫。
// ① 源端 picker 限 postgres+mysql；目标端 picker 限 tidb+mysql（后端
//    srcCapable/目标类型门的 FE 镜像）；
// ② 八类对象面：结果区按 categories 渲染，名称类三栏（仅源/仅目标/一致），
//    权限类跨方言分轨 + 序列「目标端无此对象类型」注解（v1.3）；
// ③ 运行走 apiClient.ddlCompareRun（S1-UI-08 统一客户端约定）；
// ④ 报告下载走 ddlCompareReportURL（读路由，免 token，新窗口打开）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const src = readFileSync(resolve(__dirname, 'DdlCompareView.vue'), 'utf8')
const apiSrc = readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')
const routerSrc = readFileSync(resolve(__dirname, '../router/index.ts'), 'utf8')

describe('ddlCompareView v3.19.40 structure anchors', () => {
  it('source picker is restricted to postgres+mysql', () => {
    expect(src).toContain(`<DataSourcePicker v-model="sourceRef" :types="['postgres', 'mysql']" />`)
  })

  it('target picker is restricted to tidb+mysql', () => {
    expect(src).toContain(`<DataSourcePicker v-model="targetRef" :types="['tidb', 'mysql']" />`)
  })

  it('runs through the unified api client (ddlCompareRun)', () => {
    expect(src).toContain('apiClient.ddlCompareRun(')
  })

  it('probes schemas per side (ddlCompareSchemas)', () => {
    expect(src).toContain(`@click="loadSchemas('source')"`)
    expect(src).toContain(`@click="loadSchemas('target')"`)
    expect(src).toContain('apiClient.ddlCompareSchemas(side, ref)')
  })

  it('downloads reports via ddlCompareReportURL (read-route GET)', () => {
    expect(src).toContain("apiClient.ddlCompareReportURL(resultTaskID.value, format)")
  })

  it('renders the eight-category summary table from result.categories', () => {
    expect(src).toContain(':data="result.categories"')
    expect(src).toContain('序列')
    expect(src).toContain('权限')
  })

  it('summary diff counts guard a missing tables field (empty-vs-empty run)', () => {
    // D3: 双空库运行时 tables 类经 omitempty 无 tables 字段，汇总列守卫
    // (row.tables || [])；运行期行为见 ddlCompareViewBehavior.spec.ts。
    expect(src).toContain('(row.tables || []).filter((t: any) => t.source_only)')
    expect(src).toContain('(row.tables || []).filter((t: any) => t.target_only)')
    expect(src).toContain("(row.tables || []).filter((t: any) => !t.source_only && !t.target_only && t.verdict !== 'pass' && t.verdict !== 'hint')")
    expect(src).toContain(":data=\"cat.tables || []\"")
  })

  it('name categories render the three-pane layout (source-only / target-only / common)', () => {
    expect(src).toContain('仅源端')
    expect(src).toContain('仅目标端')
    expect(src).toContain('两边一致')
  })

  it('privileges face renders the V1 cross-dialect disclosure from category.note (single source)', () => {
    expect(src).toContain(':title="privilegeCategory.note"')
    expect(src).toContain('privilegeRows(privilegeCategory)')
  })

  it('sequences target-no-type annotation renders distinctly (V2)', () => {
    expect(src).toContain('目标端无此对象类型')
  })

  it('v1.3 dropped the index_by_name / loose_types switches (index class removed)', () => {
    expect(src).not.toContain('indexByName')
    expect(src).not.toContain('looseTypes')
  })

  it('api client exposes the four ddl-compare endpoints', () => {
    expect(apiSrc).toContain('ddlCompareSchemas:')
    expect(apiSrc).toContain('ddlCompareRun:')
    expect(apiSrc).toContain('ddlCompareTasks:')
    expect(apiSrc).toContain('ddlCompareReportURL:')
  })

  it('router registers /ddl-compare with the DDL 对比 title', () => {
    expect(routerSrc).toContain("path: '/ddl-compare'")
    expect(routerSrc).toContain("title: 'DDL 对比'")
  })
})
