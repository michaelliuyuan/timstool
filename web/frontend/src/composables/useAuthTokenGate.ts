// MC-10 C5 / MS-11f 笔① d / MC-12 P1: the shared 401 → token-prompt flow
// for gated operations. Single-flight flag guarantees no stacked dialogs
// (concurrent 401s share one prompt across every gated surface in this
// tab); each gated op carries a retriedAuth flag so a wrong token prompts
// EXACTLY once per user action — the second 401 surfaces as an error line
// instead of another dialog (MS-11c toast lesson, seq83 #4). The token
// itself lives in the api layer (sessionStorage — dies with the tab).
//
// MC-12 P1 three states, distinct by design:
//   ① confirm  → verify the candidate against POST /security/verify FIRST
//     (tentatively stored so the interceptor injects it); only a 200 keeps
//     the token and releases the retry — a wrong candidate can no longer
//     burn a real create/stop action.
//   ② wrong candidate → the prompt re-opens with an attempt counter, max 3
//     (no spin loop); a non-401 verify failure (403 unset / network / 5xx)
//     aborts with the server's own wording — those are not re-entry cases.
//   ③ cancel / empty input → an INFO line ("操作未执行"), never an error;
//     the final-failure error copy names the server-side config location.
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient, { setAuthToken } from '../api'

let tokenPromptOpen = false

const MAX_VERIFY_ATTEMPTS = 3

export async function promptForToken(): Promise<boolean> {
  if (tokenPromptOpen) return false
  tokenPromptOpen = true
  try {
    for (let attempt = 1; attempt <= MAX_VERIFY_ATTEMPTS; attempt++) {
      let value: string
      try {
        ;({ value } = await ElMessageBox.prompt(
          attempt === 1
            ? '该操作需要管理令牌（对应服务端 config.yaml 的 security.token）：'
            : `令牌验证未通过，请重新输入（第 ${attempt}/${MAX_VERIFY_ATTEMPTS} 次）：`,
          '身份验证',
          { type: 'warning', confirmButtonText: '确定', cancelButtonText: '取消', inputType: 'password' },
        ))
      } catch {
        // ③ cancel: user abort — info, not error; the caller surfaces the
        // original 401 line itself.
        ElMessage.info('已取消令牌输入，操作未执行')
        return false
      }
      const token = (value || '').trim()
      if (!token) {
        // ③ empty confirm ≈ cancel (same info state, no error toast).
        ElMessage.info('已取消令牌输入，操作未执行')
        return false
      }
      // ① verify before trusting: store tentatively (the interceptor
      // injects it on the probe), keep only on 200.
      setAuthToken(token)
      try {
        await apiClient.verifySecurityToken()
        return true
      } catch (e: any) {
        setAuthToken('')
        if (e?.response?.status !== 401) {
          // 403 (server token unset) / network / 5xx: re-entry cannot fix
          // these — surface the server's wording and stop.
          ElMessage.error(e?.response?.data?.error || '令牌验证请求失败，请稍后重试')
          return false
        }
        // ② wrong candidate: fall through to the next attempt; past the
        // cap the caller's 401 line plus our final hint land together.
      }
    }
    ElMessage.error('令牌验证失败：请核对服务端 config.yaml 的 security.token')
    return false
  } finally {
    tokenPromptOpen = false
  }
}
