// MC-12 P1/P2 FE 锚：令牌弹框三态分立 + 弹框内即时验证。断言形=源文本
// 精确匹配（禁页面盲扫，本仓 vitest 惯例，对齐 mcSecurity/cdcSecurity）。
// 三态：①confirm→先 verify（暂存→探针→200 才真存+放行）②错令牌→续框
// 重输，上限 3 次防循环；非 401（403 未配置/网络/5xx）非重输面，直显服务
// 端文案终止 ③取消/空输→info 态"操作未执行"（非 error）；最终失败文案
// 落到服务端 config 位置。single-flight 防叠框（MS-11f）不破。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const gateSrc = () => readFileSync(resolve(__dirname, 'useAuthTokenGate.ts'), 'utf8')
const apiSrc = () => readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')

describe('token dialog immediate verification (MC-12 P2)', () => {
  it('api wrapper posts the same gated path as destructive ops', () => {
    const src = apiSrc()
    expect(src).toContain(`verifySecurityToken: () => api.post<{ status: string }>('/security/verify')`)
  })

  it('state ① confirm verifies BEFORE trusting: tentative store, probe, keep only on 200', () => {
    const src = gateSrc()
    expect(src).toContain(`await apiClient.verifySecurityToken()`)
    // tentative store feeds the interceptor; a rejection strips it again
    expect(src).toContain(`setAuthToken(token)`)
    expect(src).toContain(`setAuthToken('')`)
  })
})

describe('token dialog three states (MC-12 P1)', () => {
  it('state ② wrong candidate re-opens with an attempt counter, capped at 3', () => {
    const src = gateSrc()
    expect(src).toContain(`const MAX_VERIFY_ATTEMPTS = 3`)
    expect(src).toContain('令牌验证未通过，请重新输入（第 ${attempt}/${MAX_VERIFY_ATTEMPTS} 次）：')
    expect(src).toContain(`attempt <= MAX_VERIFY_ATTEMPTS`)
  })

  it('state ② non-401 verify failures abort with the server wording — not re-entry cases', () => {
    const src = gateSrc()
    expect(src).toContain(`if (e?.response?.status !== 401) {`)
    expect(src).toContain(`e?.response?.data?.error || '令牌验证请求失败，请稍后重试'`)
  })

  it('state ③ cancel/empty shows info (not error) and names the un-executed op', () => {
    const src = gateSrc()
    expect(src).toContain(`ElMessage.info('已取消令牌输入，操作未执行')`)
  })

  it('final failure copy lands on the server-side config location', () => {
    const src = gateSrc()
    expect(src).toContain(`ElMessage.error('令牌验证失败：请核对服务端 config.yaml 的 security.token')`)
  })

  it('single-flight survives the rewrite (MS-11f, no stacked dialogs)', () => {
    const src = gateSrc()
    expect(src).toContain(`let tokenPromptOpen = false`)
    expect(src).toContain(`if (tokenPromptOpen) return false`)
  })
})
