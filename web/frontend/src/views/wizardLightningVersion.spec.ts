// MS-11q FE anchors: the Lightning validation face upgrades to the live
// --version probe display (探真), mirroring the MS-11p dumpling face.
// 断言形=spec 精确匹配（禁页面盲扫），沿 wizardDumpling 先例。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'WizardView.vue'), 'utf8')

describe('WizardView lightning validation probe face (MS-11q)', () => {
  it('validation face shows the live --version probe result (探真), mirroring dumpling', () => {
    const src = viewSrc()
    expect(src).toContain('验证通过：{{ lightningResolvedPath }}（{{ lightningVersion }}）')
    expect(src).toContain('--version 探真')
  })

  it('api type carries the optional version field for validate-lightning', () => {
    const api = readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')
    expect(api).toContain(`api.post<{ success: boolean; message: string; resolved_path: string; version?: string }>('/validate-lightning'`)
  })

  it('version state resets with every invalidation (edit path / toggle switch / request error)', () => {
    const src = viewSrc()
    // All invalidation paths clear lightningVersion alongside the flag:
    // catch + path-changed + switch-changed bare resets (the success/fail
    // split rides the ternary asserted below).
    const resets = src.match(/lightningVersion\.value = ''/g) || []
    expect(resets.length).toBe(3)
    expect(src).toContain(`lightningVersion.value = data.success ? (data.version || '') : ''`)
  })

  it('dumpling face stays byte-identical (red line: no dumpling drift)', () => {
    const src = viewSrc()
    expect(src).toContain('验证通过：{{ dumplingResolvedPath }}（{{ dumplingVersion }}）')
    expect(src).toContain(`dumplingVersion.value = data.success ? (data.version || '') : ''`)
  })
})
