import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { ArrowDown } from '@element-plus/icons-vue'

// M7 ② behaviour safety net: mount-level fixtures for the DataSourcesView
// form across ALL FOUR datasource types, frozen BEFORE the registry-driven
// refactor (seq=199 card ③). Every fixture below is a faithful transcription
// of the backend /sources registry (postgres/mysql/mongodb adapters +
// tidb_meta.go) — if the backend contract drifts, these tests are supposed
// to scream, not silently follow.
//
// The refactor must keep this suite green byte-for-byte (snapshot gate:
// mysql/pg zero regression). vi.resetModules + dynamic import give each case
// fresh module-level composable caches.

const mocks = vi.hoisted(() => ({
  listDataSources: vi.fn(),
  createDataSource: vi.fn(),
  updateDataSource: vi.fn(),
  deleteDataSource: vi.fn(),
  testDataSource: vi.fn(),
  getSources: vi.fn(),
}))

vi.mock('../api', () => ({ default: mocks }))

// ---- faithful /sources registry mock (describe.go JSON contract) ----
const REGISTRY = [
  {
    name: 'postgres', displayName: 'PostgreSQL', shortName: 'PG', implemented: true, defaultPort: 5432,
    capabilities: { schema: true, data: true, cdc: true, compare: true, watermark: true, assess: true, ddl_export: true },
    fields: [
      { key: 'host', label: '主机地址', type: 'text', required: true, default: 'localhost', placeholder: 'localhost', group: 'common' },
      { key: 'port', label: '端口', type: 'number', required: true, default: 5432, group: 'common' },
      { key: 'user', label: '用户名', type: 'text', required: true, default: 'postgres', group: 'common' },
      { key: 'password', label: '密码', type: 'password', group: 'common' },
      { key: 'database', label: '数据库名', type: 'text', required: true, group: 'common' },
      { key: 'schema', label: 'Schema', type: 'text', default: 'public', group: 'source' },
      { key: 'sslmode', label: 'SSL模式', type: 'select', default: 'disable', group: 'source',
        options: [
          { label: '禁用', value: 'disable' },
          { label: '要求加密', value: 'require' },
          { label: '验证 CA', value: 'verify-ca' },
          { label: '完全验证', value: 'verify-full' },
        ] },
    ],
  },
  {
    name: 'mysql', displayName: 'MySQL', shortName: 'MySQL', implemented: true, defaultPort: 3306,
    capabilities: { schema: true, data: true, cdc: true, compare: true, watermark: true, assess: true, ddl_export: true },
    fields: [
      { key: 'host', label: '主机地址', type: 'text', required: true, default: 'localhost', placeholder: 'localhost', group: 'common' },
      { key: 'port', label: '端口', type: 'number', required: true, default: 3306, group: 'common' },
      { key: 'user', label: '用户名', type: 'text', required: true, default: 'root', group: 'common' },
      { key: 'password', label: '密码', type: 'password', group: 'common' },
      { key: 'database', label: '数据库名', type: 'text', required: true, group: 'common' },
      { key: 'charset', label: 'Charset', type: 'select', default: 'utf8mb4', group: 'source',
        options: [
          { label: 'utf8mb4', value: 'utf8mb4' },
          { label: 'utf8', value: 'utf8' },
          { label: 'latin1', value: 'latin1' },
          { label: 'gbk', value: 'gbk' },
        ] },
    ],
  },
  {
    name: 'tidb', displayName: 'TiDB', implemented: false, defaultPort: 4000,
    notImplMsg: 'TiDB 当前为目标端专用，作源端启用须专项实测后翻位',
    capabilities: { schema: false, data: false, cdc: false, compare: false, watermark: false, assess: false, ddl_export: false },
    fields: [
      { key: 'host', label: '主机地址', type: 'text', required: true, default: 'localhost', placeholder: 'localhost', group: 'common' },
      { key: 'port', label: '端口', type: 'number', required: true, default: 4000, group: 'common' },
      { key: 'user', label: '用户名', type: 'text', required: true, default: 'root', group: 'common' },
      { key: 'password', label: '密码', type: 'password', group: 'common' },
      { key: 'database', label: '数据库名', type: 'text', required: true, group: 'common' },
    ],
  },
  {
    name: 'mongodb', displayName: 'MongoDB', implemented: true, defaultPort: 27017,
    capabilities: { schema: false, data: false, cdc: false, compare: true, watermark: false, assess: false, ddl_export: false },
    fields: [
      { key: 'host', label: '主机地址', type: 'text', required: true, default: 'localhost', placeholder: 'localhost', group: 'common' },
      { key: 'port', label: '端口', type: 'number', required: true, default: 27017, group: 'common' },
      { key: 'user', label: '用户名', type: 'text', placeholder: '留空表示无认证', group: 'common' },
      { key: 'password', label: '密码', type: 'password', group: 'common' },
      { key: 'authDb', label: '认证库 (authSource)', type: 'text', placeholder: 'admin', help: '用于身份认证的库，留空时驱动按默认规则（admin/连接库）选择', group: 'source' },
      { key: 'tls', label: 'TLS/SSL', type: 'switch', default: false, help: '启用 TLS 连接', group: 'source' },
    ],
  },
]

// four cards, one per type; names fix the localeCompare order pg→mysql→tidb→mongodb
const CARDS = [
  { id: 'ds-pg', name: 'a-pg', type: 'postgres', fields: { host: 'pg.internal', port: 5432, database: 'appdb', user: 'postgres' }, has_password: true },
  { id: 'ds-my', name: 'b-my', type: 'mysql', fields: { host: 'my.internal', port: 3306 }, has_password: false },
  { id: 'ds-ti', name: 'c-ti', type: 'tidb', fields: { host: 'ti.internal', port: 4000, database: 'target' }, has_password: false },
  { id: 'ds-mo', name: 'd-mo', type: 'mongodb', fields: { host: 'mo.internal' }, has_password: true },
]

async function mountView() {
  const { default: DataSourcesView } = await import('./DataSourcesView.vue')
  const w = mount(DataSourcesView, {
    attachTo: document.body,
    global: {
      plugins: [ElementPlus],
      // the card "更多" dropdown uses the ArrowDown icon, globally registered
      // by main.ts in the app; tests mount without main.ts, so re-register it
      components: { ArrowDown },
    },
  })
  await flushPromises()
  return w
}

/** open the create dialog and return the in-body dialog element */
async function openCreate(w: Awaited<ReturnType<typeof mountView>>) {
  const btn = w.findAll('button').find(b => b.text() === '新建数据源')
  expect(btn, '新建数据源 button rendered').toBeTruthy()
  await btn!.trigger('click')
  await flushPromises()
  const dlg = document.querySelector('.el-dialog')
  expect(dlg, 'create dialog teleported to body').toBeTruthy()
  return dlg!
}

/** switch the dialog's type select by emitting its change event (no popper) */
async function switchType(w: Awaited<ReturnType<typeof mountView>>, type: string) {
  const selects = w.findAllComponents({ name: 'ElSelect' })
  expect(selects.length, 'type select rendered in dialog').toBeGreaterThan(0)
  selects[0].vm.$emit('change', type)
  await flushPromises()
}

/** labels of the dialog form items, in render order */
function dialogLabels(dlg: Element): string[] {
  return [...dlg.querySelectorAll('.el-form-item__label')].map(el => (el.textContent || '').trim())
}

/** the form-item element whose label is exactly `label` */
function fieldRow(dlg: Element, label: string): Element {
  const row = [...dlg.querySelectorAll('.el-form-item')]
    .find(r => (r.querySelector('.el-form-item__label')?.textContent || '').trim() === label)
  expect(row, `form item "${label}" rendered`).toBeTruthy()
  return row!
}

/** set a text input inside the form item labelled `label` */
async function setField(dlg: Element, label: string, value: string) {
  const input = fieldRow(dlg, label).querySelector('input') as HTMLInputElement
  input.value = value
  input.dispatchEvent(new Event('input'))
  await flushPromises()
}

beforeEach(() => {
  vi.resetModules()
  document.body.innerHTML = ''
  mocks.listDataSources.mockReset().mockResolvedValue({ data: { datasources: CARDS } })
  mocks.getSources.mockReset().mockResolvedValue({ data: { sources: REGISTRY } })
  mocks.createDataSource.mockReset().mockResolvedValue({ data: { id: 'new' } })
  mocks.updateDataSource.mockReset().mockResolvedValue({ data: {} })
  mocks.deleteDataSource.mockReset().mockResolvedValue({ data: {} })
  mocks.testDataSource.mockReset().mockResolvedValue({ data: { success: true, message: 'ok', version: 'x' } })
})

describe('DataSourcesView cards (mount)', () => {
  it('renders one card per datasource with type label and tag colour', async () => {
    const w = await mountView()

    const names = w.findAll('.ds-card-name').map(n => n.text())
    expect(names).toEqual(['a-pg', 'b-my', 'c-ti', 'd-mo'])

    const tags = w.findAll('.ds-card .el-tag')
    expect(tags.map(t => t.text())).toEqual(['PostgreSQL', '已存密码', 'MySQL', '无密码', 'TiDB', '无密码', 'MongoDB', '已存密码'])
    // colour tokens per type (tidb warning / mysql info / mongodb primary / default success)
    expect(tags[0].classes()).toContain('el-tag--success')
    expect(tags[2].classes()).toContain('el-tag--info')
    expect(tags[4].classes()).toContain('el-tag--warning')
    expect(tags[6].classes()).toContain('el-tag--primary')

    w.unmount()
  })

  it('shows the connection string host:port/database on the card', async () => {
    const w = await mountView()
    const conns = w.findAll('.ds-card-conn').map(c => c.text())
    expect(conns[0]).toBe('pg.internal:5432/appdb')
    expect(conns[3]).toBe('mo.internal') // mongodb without port/database renders bare host
    w.unmount()
  })
})

describe('DataSourcesView create dialog (mount)', () => {
  it('type dropdown offers exactly the four datasource types', async () => {
    const w = await mountView()
    const dlg = await openCreate(w)

    // the type select seeds postgres; its option slot holds the four kinds
    const typeSelect = w.findAllComponents({ name: 'ElSelect' })[0]
    expect(typeSelect.props('modelValue')).toBe('postgres')
    const optionLabels = typeSelect.findAllComponents({ name: 'ElOption' }).map(o => o.props('label'))
    expect(optionLabels).toEqual(['PostgreSQL', 'MySQL', 'TiDB（目标端）', 'MongoDB（对比）'])
    expect(dlg.textContent).toContain('PostgreSQL')

    w.unmount()
  })

  it('postgres form: seven fields in registry order with defaults', async () => {
    const w = await mountView()
    const dlg = await openCreate(w)

    expect(dialogLabels(dlg)).toEqual([
      '名称', '类型', '主机地址', '端口', '用户名', '密码', '数据库名', 'Schema', 'SSL模式',
    ])

    // port default seeds 5432; schema seeds public
    const num = w.getComponent({ name: 'ElInputNumber' })
    expect(num.props('modelValue')).toBe(5432)
    expect((fieldRow(dlg, 'Schema').querySelector('input') as HTMLInputElement).value).toBe('public')

    // NOTE current behaviour (frozen as-is): select-type fields (sslmode with
    // its four registry options) render through the generic text-input branch,
    // seeded with the default value. Rendering a real el-select for type=select
    // is an enhancement, out of the M7 zero-behaviour-change refactor scope.
    expect((fieldRow(dlg, 'SSL模式').querySelector('input') as HTMLInputElement).value).toBe('disable')

    w.unmount()
  })

  it('mysql form: six fields, schema/sslmode dropped, charset seeded utf8mb4', async () => {
    const w = await mountView()
    await openCreate(w)
    await switchType(w, 'mysql')
    const dlg = document.querySelector('.el-dialog')!

    expect(dialogLabels(dlg)).toEqual([
      '名称', '类型', '主机地址', '端口', '用户名', '密码', '数据库名', 'Charset',
    ])
    expect(w.getComponent({ name: 'ElInputNumber' }).props('modelValue')).toBe(3306)
    expect((fieldRow(dlg, 'Charset').querySelector('input') as HTMLInputElement).value).toBe('utf8mb4')

    w.unmount()
  })

  it('tidb form: target-side pd_addr/status_port fields present beyond registry five', async () => {
    const w = await mountView()
    await openCreate(w)
    await switchType(w, 'tidb')
    const dlg = document.querySelector('.el-dialog')!

    expect(dialogLabels(dlg)).toEqual([
      '名称', '类型', '主机地址', '端口', '用户名', '密码', '数据库名', 'PD 地址', '状态端口',
    ])
    expect(w.getComponent({ name: 'ElInputNumber' }).props('modelValue')).toBe(4000)
    expect(dlg.textContent).toContain('Lightning 物理导入')

    w.unmount()
  })

  it('tidb submit carries the target-only pd_addr/status_port keys (seq=204 ruling ②)', async () => {
    // pd_addr/status_port are target-side-only fields consumed by
    // dataSourceToTargetConfig (Lightning/CDC/compare/DDL flows); this case
    // proves the (b) partial refactor keeps them in the submitted payload —
    // the byte-stable field-key contract from the seq=204 ruling ③.
    const w = await mountView()
    await openCreate(w)
    await switchType(w, 'tidb')
    const dlg = document.querySelector('.el-dialog')!

    await setField(dlg, '名称', 'new-tidb')
    await setField(dlg, 'PD 地址', 'pd.internal:2379')
    await w.findAll('button').find(b => b.text() === '创建')!.trigger('click')
    await flushPromises()

    expect(mocks.createDataSource).toHaveBeenCalledTimes(1)
    const [body] = mocks.createDataSource.mock.calls[0]
    expect(body.type).toBe('tidb')
    expect(Object.keys(body.fields).sort()).toEqual([
      'database', 'host', 'password', 'pd_addr', 'port', 'status_port', 'user',
    ])
    expect(body.fields.pd_addr).toBe('pd.internal:2379')
    expect(body.fields.status_port).toBeUndefined() // left blank = not set

    w.unmount()
  })

  it('mongodb form: authDb input + tls renders as ElSwitch (seq=193③ debt)', async () => {
    const w = await mountView()
    await openCreate(w)
    await switchType(w, 'mongodb')
    const dlg = document.querySelector('.el-dialog')!

    expect(dialogLabels(dlg)).toEqual([
      '名称', '类型', '主机地址', '端口', '用户名', '密码', '认证库 (authSource)', 'TLS/SSL',
    ])
    expect(w.getComponent({ name: 'ElInputNumber' }).props('modelValue')).toBe(27017)

    const sw = w.findComponent({ name: 'ElSwitch' })
    expect(sw.exists(), 'tls field renders as a switch').toBe(true)
    expect(sw.props('modelValue')).toBe(false)
    expect(dlg.textContent).toContain('启用 TLS 连接')

    w.unmount()
  })

  it('blocks submit on empty name / empty host with a warning toast', async () => {
    const w = await mountView()
    await openCreate(w)

    const create = w.findAll('button').find(b => b.text() === '创建')!
    await create.trigger('click')
    await flushPromises()

    expect(mocks.createDataSource).not.toHaveBeenCalled()
    const toast = document.querySelector('.el-message')
    expect(toast?.textContent).toContain('请输入数据源名称')

    // fill name, then clear the seeded host default to trip the host guard
    const dlg0 = document.querySelector('.el-dialog')!
    await setField(dlg0, '名称', 'new-ds')
    await setField(dlg0, '主机地址', '')

    await create.trigger('click')
    await flushPromises()
    expect(mocks.createDataSource).not.toHaveBeenCalled()
    // the first toast lingers in jsdom (no clock advance), so scan all of them
    const toasts = [...document.querySelectorAll('.el-message')].map(t => t.textContent || '')
    expect(toasts.some(t => t.includes('请填写主机地址'))).toBe(true)

    w.unmount()
  })

  it('postgres submit sends the full seven-key field set with defaults', async () => {
    const w = await mountView()
    const dlg = await openCreate(w)

    await setField(dlg, '名称', 'new-pg')
    await setField(dlg, '主机地址', '10.9.8.7')

    await w.findAll('button').find(b => b.text() === '创建')!.trigger('click')
    await flushPromises()

    expect(mocks.createDataSource).toHaveBeenCalledTimes(1)
    const [body] = mocks.createDataSource.mock.calls[0]
    expect(body.type).toBe('postgres')
    expect(body.fields).toEqual({
      host: '10.9.8.7', port: 5432, user: 'postgres', password: '', database: '', schema: 'public', sslmode: 'disable',
    })

    w.unmount()
  })

  it('mongodb submit carries authDb/tls keys (registry key set)', async () => {
    const w = await mountView()
    await openCreate(w)
    await switchType(w, 'mongodb')
    const dlg = document.querySelector('.el-dialog')!

    await setField(dlg, '名称', 'new-mongo')

    await w.findAll('button').find(b => b.text() === '创建')!.trigger('click')
    await flushPromises()

    expect(mocks.createDataSource).toHaveBeenCalledTimes(1)
    const [body] = mocks.createDataSource.mock.calls[0]
    expect(body.type).toBe('mongodb')
    expect(Object.keys(body.fields).sort()).toEqual(['authDb', 'host', 'password', 'port', 'tls', 'user'])
    expect(body.fields.tls).toBe(false)
    expect(body.fields.authDb).toBe('')

    w.unmount()
  })
})

describe('DataSourcesView edit dialog (mount)', () => {
  it('restores stored fields, blanks password, locks the type select, and routes to update', async () => {
    const w = await mountView()

    // open edit on the postgres card via its dropdown command event
    const dropdowns = w.findAllComponents({ name: 'ElDropdown' })
    expect(dropdowns.length).toBe(4)
    dropdowns[0].vm.$emit('command', 'edit')
    await flushPromises()

    const dlg = document.querySelector('.el-dialog')!
    expect(dlg.textContent).toContain('编辑数据源')

    // type select is disabled while editing
    const typeSelect = w.findAllComponents({ name: 'ElSelect' })[0]
    expect(typeSelect.props('disabled')).toBe(true)

    // stored host restored; password blanked with the keep hint (placeholder)
    const hostInput = fieldRow(dlg, '主机地址').querySelector('input') as HTMLInputElement
    expect(hostInput.value).toBe('pg.internal')
    const pwdInput = fieldRow(dlg, '密码').querySelector('input') as HTMLInputElement
    expect(pwdInput.value).toBe('')
    expect(pwdInput.placeholder).toBe('留空保持不变')

    // save routes to updateDataSource with name + fields, no type
    await setField(dlg, '名称', 'renamed')
    await w.findAll('button').find(b => b.text() === '保存')!.trigger('click')
    await flushPromises()

    expect(mocks.updateDataSource).toHaveBeenCalledTimes(1)
    const [id, body] = mocks.updateDataSource.mock.calls[0]
    expect(id).toBe('ds-pg')
    expect(body.type).toBeUndefined()
    expect(body.name).toBe('renamed')
    expect(body.fields.host).toBe('pg.internal')
    expect(body.fields.password).toBe('') // blank = keep stored

    w.unmount()
  })
})
