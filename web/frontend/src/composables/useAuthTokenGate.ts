// MC-10 C5 / MS-11f 笔① d: the shared 401 → token-prompt flow for gated
// operations. Single-flight flag guarantees no stacked dialogs (concurrent
// 401s share one prompt across every gated surface in this tab); each
// gated op carries a retriedAuth flag so a wrong token prompts EXACTLY
// once per user action — the second 401 surfaces as an error line instead
// of another dialog (MS-11c toast lesson, seq83 #4). The token itself
// lives in the api layer (sessionStorage — dies with the tab).
import { ElMessageBox } from 'element-plus'
import { setAuthToken } from '../api'

let tokenPromptOpen = false

export async function promptForToken(): Promise<boolean> {
  if (tokenPromptOpen) return false
  tokenPromptOpen = true
  try {
    const { value } = await ElMessageBox.prompt(
      '该操作需要管理令牌（对应服务端 config.yaml 的 security.token）：',
      '身份验证',
      { type: 'warning', confirmButtonText: '确定', cancelButtonText: '取消', inputType: 'password' },
    )
    const token = (value || '').trim()
    if (!token) return false
    setAuthToken(token)
    return true
  } catch {
    return false
  } finally {
    tokenPromptOpen = false
  }
}
