// MS-10d pen 6 FE anchors (leader ruling seq926, 刘源 ask): the sidenav
// footer literal "PG → TiDB" is removed (MySQL sources are first-class -
// the footnote was stale) and the topbar version badge no longer hardcodes
// the stale "V3.9". Assertion shape = spec exact match (no page blind-scan).
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const appSrc = () => readFileSync(resolve(__dirname, 'App.vue'), 'utf8')

describe('App shell branding (MS-10d pen 6)', () => {
  it('sidenav footer "PG → TiDB" footnote is gone (template + dead CSS)', () => {
    const src = appSrc()
    expect(src).not.toContain('PG → TiDB')
    expect(src).not.toContain('tims-sidenav-foot')
  })

  it('topbar version badge shows V3.18, not the stale V3.9', () => {
    const src = appSrc()
    expect(src).toContain('tims-version tims-mono">V3.18')
    expect(src).not.toContain('V3.9')
  })
})
