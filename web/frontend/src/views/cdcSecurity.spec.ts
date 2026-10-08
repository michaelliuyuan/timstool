// MS-11f 笔① FE 锚：token 拦截器（sessionStorage 不落 localStorage）、
// 401 单框重试（每动作至多一框，二次 401 直显错误）、403 只显提示不弹框、
// 异常宕停告警行渲染（最新在前）。断言形=源文本精确匹配（禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const apiSrc = () => readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')
const viewSrc = () => readFileSync(resolve(__dirname, 'CDCView.vue'), 'utf8')

describe('api client token plane (MS-11f)', () => {
  it('injects X-Auth-Token from sessionStorage on every request', () => {
    const src = apiSrc()
    expect(src).toContain(`const AUTH_TOKEN_HEADER = 'X-Auth-Token'`)
    expect(src).toContain(`api.interceptors.request.use((config) => {`)
    expect(src).toContain(`if (token) config.headers[AUTH_TOKEN_HEADER] = token`)
  })

  it('stores the token in sessionStorage — never the persistent web-store API', () => {
    const src = apiSrc()
    expect(src).toContain(`sessionStorage.setItem(AUTH_TOKEN_KEY, token)`)
    expect(src).toContain(`sessionStorage.removeItem(AUTH_TOKEN_KEY)`)
    // the token must die with the tab: no persistent-store reads/writes
    expect(src).not.toContain(`localStorage.setItem`)
    expect(src).not.toContain(`localStorage.getItem`)
    expect(src).not.toContain(`localStorage.removeItem`)
  })
})

describe('CDCView 401 token prompt (MS-11f 笔① d)', () => {
  it('one prompt per user action: single-flight flag + retriedAuth depth guard', () => {
    const src = viewSrc()
    // single-flight: concurrent 401s share one dialog (no stacking)
    expect(src).toContain(`let tokenPromptOpen = false`)
    expect(src).toContain(`if (tokenPromptOpen) return false`)
    // depth guard: the retry re-enters the op with retriedAuth=true; a second
    // 401 falls through to the error line, never a second dialog
    expect(src).toContain(`e.response?.status === 401 && !retriedAuth && await promptForToken()`)
    expect(src).toContain(`return callCDC(action, true)`)
  })

  it('every gated op carries the 401 retry path', () => {
    const src = viewSrc()
    // start/stop, PUT config, both imports, checkpoint reset — six faces
    for (const anchor of [
      `async function callCDC(action: 'start' | 'stop', retriedAuth = false)`,
      `async function saveConn(retriedAuth = false)`,
      `async function importConn(retriedAuth = false)`,
      `async function importFromDS(retriedAuth = false)`,
      `async function resetCheckpoint(retriedAuth = false)`,
    ]) {
      expect(src).toContain(anchor)
    }
    // and each one's catch wires the same 401 branch
    const branches = src.split(`e.response?.status === 401 && !retriedAuth && await promptForToken()`).length - 1
    expect(branches).toBe(5)
  })

  it('403 surfaces as a plain error line — no prompt can fix a server-side gap', () => {
    const src = viewSrc()
    // the error extraction reads the server's error field (403 remediation
    // wording lands verbatim); no 403-specific prompt branch exists
    expect(src).toContain(`e.response?.data?.error || e.response?.data?.message || e.message || '请求失败'`)
    expect(src).not.toContain(`status === 403`)
  })
})

describe('CDCView anomaly alarms (MS-11f 笔① c)', () => {
  it('renders the alarm ring newest-first right under the hero', () => {
    const src = viewSrc()
    expect(src).toContain(`const alarmRows = computed<CDCAlarmRow[]>(() => [...(status.value.alarms || [])].reverse())`)
    const heroIdx = src.indexOf(`class="status-card hero"`)
    const alarmIdx = src.indexOf(`class="alarm-card"`)
    const statsIdx = src.indexOf(`class="stats-grid"`)
    expect(heroIdx).toBeGreaterThan(-1)
    expect(alarmIdx).toBeGreaterThan(heroIdx)
    expect(statsIdx).toBeGreaterThan(alarmIdx)
    // each row carries ts + message (the /cdc/status .alarms contract)
    expect(src).toContain(`alarms?: CDCAlarmRow[]`)
    expect(src).toContain(`{{ formatAlarmTS(a.ts) }}`)
    expect(src).toContain(`{{ a.message }}`)
  })
})
