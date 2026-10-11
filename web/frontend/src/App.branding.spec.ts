// MS-10d pen 6 FE anchors (leader ruling seq926, 刘源 ask): the sidenav
// footer literal "PG → TiDB" is removed (MySQL sources are first-class -
// the footnote was stale). MS-11i pen1: the topbar version badge is now
// BUILD-INJECTED from package.json via vite define (__APP_VERSION__) —
// hardcoded V3.x literals are banned (the V3.18-stale rot this fixes).
// Assertion shape = spec exact match (no page blind-scan).
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const appSrc = () => readFileSync(resolve(__dirname, 'App.vue'), 'utf8')

describe('App shell branding (MS-10d pen 6 + MS-11i pen1)', () => {
  it('sidenav footer "PG → TiDB" footnote is gone (template + dead CSS)', () => {
    const src = appSrc()
    expect(src).not.toContain('PG → TiDB')
    expect(src).not.toContain('tims-sidenav-foot')
  })

  it('topbar version badge is build-injected, not a hardcoded literal (MS-11i pen1)', () => {
    const src = appSrc()
    expect(src).toContain('const appVersion = __APP_VERSION__')
    expect(src).toContain('tims-version tims-mono">V{{ appVersion }}')
    // No hardcoded V3.x literal may ever return to the template.
    expect(src).not.toMatch(/V3\.\d+/)
  })

  it('package.json version is a real semver the build stamps (stale-0.0.0 guard)', () => {
    const pkg = JSON.parse(readFileSync(resolve(__dirname, '../package.json'), 'utf8'))
    expect(pkg.version).toMatch(/^3\.\d+\.\d+$/)
    expect(pkg.version).not.toBe('0.0.0')
  })

  it('vite define stamps __APP_VERSION__ from package.json (single source)', () => {
    const vite = readFileSync(resolve(__dirname, '../vite.config.ts'), 'utf8')
    expect(vite).toContain('__APP_VERSION__: JSON.stringify(pkg.version)')
    // The plugin-vue import must stay default-form: plugin-vue 6.x exports no
    // named "vue" — a named import hard-fails strict ESM config loading.
    expect(vite).toContain(`import vue from '@vitejs/plugin-vue'`)
    expect(vite).not.toContain(`import { vue } from '@vitejs/plugin-vue'`)
  })
})
