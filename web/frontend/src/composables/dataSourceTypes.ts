import type { FieldSpec } from './sourceTypes'

// M7 ③ datasource-domain type registry — the single frontend module that
// knows which datasource KINDS exist for the management surface (cards,
// pickers, create dialog) and how each renders. Before this module the same
// knowledge was hand-written twice (DataSourcesView + DataSourcePicker) as
// local label maps plus a tidb field list.
//
// Division of truth with the backend /sources registry:
//   - WHICH kinds are creatable as datasources, their dropdown labels (with
//     the role suffixes 目标端/对比) and card tag colours are UI-domain facts
//     and live HERE. The backend datasource gate (webapi/datasource.go
//     dataSourceTypes map) is the API-side whitelist; the two are kept in
//     step by the mount behaviour spec (dataSourcesViewBehavior.spec.ts)
//     and datasource_mongo_test.go on the Go side.
//   - WHICH connection fields a kind needs comes from the source registry
//     (getSource(type).fields) for postgres/mysql/mongodb. tidb is
//     target-only: the registry describes a 5-field target meta, but the
//     datasource form has always shown the full Lightning-era form with
//     pd_addr/status_port — the complete local spec lives here and the
//     registry tidb meta is deliberately NOT merged, so the form survives a
//     /sources outage unchanged (zero behaviour change vs the pre-M7 form).

export interface DataSourceTypeSpec {
  /** datasource type key, e.g. "postgres" */
  value: string
  /** label in the create-dialog type dropdown (role suffix included) */
  label: string
  /** compact label on cards / picker options (matches registry displayName) */
  shortLabel: string
  /** el-tag colour token on the datasource card */
  tagType: 'success' | 'info' | 'warning' | 'primary'
}

/** The four creatable datasource kinds, in dropdown order. */
export const dataSourceTypeSpecs: DataSourceTypeSpec[] = [
  { value: 'postgres', label: 'PostgreSQL', shortLabel: 'PostgreSQL', tagType: 'success' },
  { value: 'mysql', label: 'MySQL', shortLabel: 'MySQL', tagType: 'info' },
  { value: 'tidb', label: 'TiDB（目标端）', shortLabel: 'TiDB', tagType: 'warning' },
  { value: 'mongodb', label: 'MongoDB（对比）', shortLabel: 'MongoDB', tagType: 'primary' },
]

/**
 * Full local form spec for kinds whose datasource form is registry-independent.
 * tidb (seq=204 ruling ①): pd_addr/status_port are TARGET-side-only fields
 * (consumed by dataSourceToTargetConfig for Lightning/CDC/compare/DDL) that
 * the source registry deliberately does not carry — its tidb meta is a
 * 5-field target stub (Implemented=false). Registry-izing the tidb form must
 * wait for dedicated target-side profile modelling (M8+ design topic); this
 * local table is NOT to be "completed away" — dropping it regresses two
 * production fields.
 */
export const localFormFields: Record<string, FieldSpec[]> = {
  tidb: [
    { key: 'host', label: '主机地址', type: 'text', required: true, default: 'localhost', placeholder: 'localhost', group: 'common' },
    { key: 'port', label: '端口', type: 'number', required: true, default: 4000, group: 'common' },
    { key: 'user', label: '用户名', type: 'text', required: true, default: 'root', group: 'common' },
    { key: 'password', label: '密码', type: 'password', group: 'common' },
    { key: 'database', label: '数据库名', type: 'text', required: true, group: 'common' },
    { key: 'pd_addr', label: 'PD 地址', type: 'text', placeholder: 'host:2379', help: '可选。PD 地址，主要用于 Lightning 物理导入；SQL 端口经代理时务必填真实 PD 端口，留空则导入时按 主机:2379 推断', group: 'common' },
    { key: 'status_port', label: '状态端口', type: 'number', placeholder: '10080', help: '可选。TiDB 状态端口（默认 10080），主要用于 Lightning 导入与连通探测；留空表示未设置', group: 'common' },
  ],
}

/** Dropdown options for the create dialog ({value,label} pairs, registry order). */
export function dataSourceTypeOptions(): { value: string; label: string }[] {
  return dataSourceTypeSpecs.map(s => ({ value: s.value, label: s.label }))
}

/** Compact type label for cards/pickers; unknown types fall back to the raw key. */
export function dataSourceTypeLabel(type: string): string {
  return dataSourceTypeSpecs.find(s => s.value === type)?.shortLabel || type
}

/** Card tag colour token; unknown types fall back to success (legacy default). */
export function dataSourceTagType(type: string): string {
  return dataSourceTypeSpecs.find(s => s.value === type)?.tagType || 'success'
}

/**
 * Form fields for one datasource kind: table-driven — a local (registry-
 * independent) spec wins, otherwise the /sources registry fields, else none.
 */
export function dataSourceFormFields(type: string, registryFields?: FieldSpec[]): FieldSpec[] {
  return localFormFields[type] ?? registryFields ?? []
}
