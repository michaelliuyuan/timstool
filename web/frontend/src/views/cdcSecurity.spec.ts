// MS-11f 笔① FE 锚（令牌门移除后）：认证平面已整体下线 —— api 层不再有
// X-Auth-Token 拦截器/会话存储，视图不再有 401 弹框重试路径；异常宕停
// 告警行渲染（最新在前）继续保留。断言形=源文本精确匹配（禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const apiSrc = () => readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')
const viewSrc = () => readFileSync(resolve(__dirname, 'CDCView.vue'), 'utf8')
const createSrc = () => readFileSync(resolve(__dirname, 'CompareCreateView.vue'), 'utf8')
const detailSrc = () => readFileSync(resolve(__dirname, 'TaskDetailView.vue'), 'utf8')

describe('api client auth plane removed', () => {
  it('no token header, no session storage, no verify probe', () => {
    const src = apiSrc()
    expect(src).not.toContain(`X-Auth-Token`)
    expect(src).not.toContain(`sessionStorage`)
    expect(src).not.toContain(`verifySecurityToken`)
    expect(src).not.toContain(`/security/verify`)
    expect(src).not.toContain(`api.interceptors.request.use`)
  })
})

describe('views carry no token-prompt flow', () => {
  it('CDCView: no composable import, no 401 retry branches', () => {
    const src = viewSrc()
    expect(src).not.toContain(`useAuthTokenGate`)
    expect(src).not.toContain(`promptForToken`)
    expect(src).not.toContain(`retriedAuth`)
    expect(src).not.toContain(`status === 401`)
  })

  it('CompareCreateView / TaskDetailView: same', () => {
    for (const src of [createSrc(), detailSrc()]) {
      expect(src).not.toContain(`useAuthTokenGate`)
      expect(src).not.toContain(`promptForToken`)
      expect(src).not.toContain(`retriedAuth`)
      expect(src).not.toContain(`status === 401`)
    }
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
