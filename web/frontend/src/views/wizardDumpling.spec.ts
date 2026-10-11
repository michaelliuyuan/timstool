// MS-11p FE anchors: the Dumpling export switch surface in the wizard.
// 断言形=spec 精确匹配（禁页面盲扫），沿 wizardSourceAgnostic 先例。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'WizardView.vue'), 'utf8')

describe('WizardView dumpling export switch surface (MS-11p)', () => {
  it('renders the dumpling switch ONLY for MySQL sources (PG never sees it)', () => {
    const src = viewSrc()
    // Both dumpling form-items ride the effectiveSourceType === 'mysql' gate.
    expect(src).toContain(`<el-form-item v-if="effectiveSourceType === 'mysql'" label="使用 Dumpling 导出">`)
    expect(src).toContain(`<el-form-item v-if="effectiveSourceType === 'mysql' && form.opts.use_dumpling" label="Dumpling 路径">`)
  })

  it('step gate: enabled dumpling cannot advance past step 3 unvalidated', () => {
    const src = viewSrc()
    expect(src).toContain(
      `if (activeStep.value === 3 && form.opts.use_dumpling && effectiveSourceType.value === 'mysql' && !dumplingValidated.value) {`,
    )
    // The gate blocks with return (mirror of the Lightning gate).
    const idx = src.indexOf(`!dumplingValidated.value) {`)
    const block = src.slice(idx, src.indexOf('}', src.indexOf('return', idx)))
    expect(block).toContain('return')
  })

  it('summary page carries the dumpling row (+ path row only when enabled)', () => {
    const src = viewSrc()
    expect(src).toContain(`<el-descriptions-item label="使用 Dumpling">{{ form.opts.use_dumpling && effectiveSourceType === 'mysql' ? '是' : '否' }}</el-descriptions-item>`)
    expect(src).toContain(`<el-descriptions-item v-if="form.opts.use_dumpling && effectiveSourceType === 'mysql'" label="Dumpling 路径" :span="2">`)
    // The path row falls back to 自动发现 wording when nothing is set.
    expect(src).toContain(`{{ dumplingResolvedPath || form.opts.dumpling_path || '自动发现' }}`)
  })

  it('submit payload rides the MySQL guard (a non-MySQL task never carries dumpling opts)', () => {
    const src = viewSrc()
    expect(src).toContain(`use_dumpling: form.opts.use_dumpling && effectiveSourceType.value === 'mysql',`)
    expect(src).toContain(`dumpling_path: form.opts.use_dumpling && effectiveSourceType.value === 'mysql' ? (dumplingResolvedPath.value || form.opts.dumpling_path.trim()) : '',`)
  })

  it('validation face shows the live --version probe result (探真)', () => {
    const src = viewSrc()
    expect(src).toContain('验证通过：{{ dumplingResolvedPath }}（{{ dumplingVersion }}）')
    expect(src).toContain('--version 探真')
  })
})
