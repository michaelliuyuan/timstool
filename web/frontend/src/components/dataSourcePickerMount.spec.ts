import { describe, it, expect, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ElementPlus from 'element-plus'

// M7 ① mount infra smoke: the first component-mount spec in this repo
// (jsdom + @vue/test-utils + ElementPlus plugin). DataSourcePicker is the
// smallest registry consumer — if this renders, the plumbing works.
// Plain `axios` calls are stubbed at the api module boundary; the picker
// only needs listDataSources via useDataSources.

vi.mock('../api', () => ({
  default: {
    listDataSources: vi.fn().mockResolvedValue({
      data: {
        datasources: [
          { id: 'ds-1', name: 'prod mongo', type: 'mongodb', fields: { host: '10.0.0.8' }, has_password: true },
          { id: 'ds-2', name: 'target tidb', type: 'tidb', fields: { host: '10.0.0.9' }, has_password: false },
        ],
      },
    }),
    getSources: vi.fn().mockResolvedValue({ data: { sources: [] } }),
  },
}))

import DataSourcePicker from './DataSourcePicker.vue'

function mountPicker(props: Record<string, unknown> = {}) {
  return mount(DataSourcePicker, {
    attachTo: document.body,
    global: { plugins: [ElementPlus] },
    props: { modelValue: '', ...props },
  })
}

describe('DataSourcePicker (mount)', () => {
  it('renders the select with saved datasources and the manual-entry hint', async () => {
    const w = mountPicker()
    await flushPromises()

    const hint = w.find('.ds-picker-hint')
    expect(hint.exists()).toBe(true)
    expect(hint.text()).toContain('密码只保存在服务端')

    const select = w.findComponent({ name: 'ElSelect' })
    expect(select.exists()).toBe(true)
    // no types filter → both mock datasources pass through, so the list is
    // non-empty and the regular placeholder shows
    expect(select.props('placeholder')).toBe('选择已保存的数据源')

    w.unmount()
  })

  it('filters options by the types prop and falls back when none match', async () => {
    // mongodb filter matches ds-1 only
    const w = mountPicker({ types: ['mongodb'] })
    await flushPromises()
    expect(w.findComponent({ name: 'ElSelect' }).props('placeholder')).toBe('选择已保存的数据源')

    // no mysql datasource exists → empty-list placeholder
    const w2 = mountPicker({ types: ['mysql'] })
    await flushPromises()
    expect(w2.findComponent({ name: 'ElSelect' }).props('placeholder')).toContain('暂无数据源')

    w.unmount()
    w2.unmount()
  })

  it('shows the selected datasource name with the credentials hint', async () => {
    const w = mountPicker({ modelValue: 'ds-1' })
    await flushPromises()

    const hint = w.find('.ds-picker-hint')
    expect(hint.text()).toContain('prod mongo')
    expect(hint.text()).toContain('凭据由服务端保管')

    w.unmount()
  })
})
