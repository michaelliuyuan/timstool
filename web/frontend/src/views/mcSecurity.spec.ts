// MC-10 C5 FE 锚：mongocompare 门（create + compare 生命周期动作）401 单框
// 重试。断言形=源文本精确匹配（禁页面盲扫），形制仿 cdcSecurity.spec.ts。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const gateSrc = () => readFileSync(resolve(__dirname, '../composables/useAuthTokenGate.ts'), 'utf8')
const createSrc = () => readFileSync(resolve(__dirname, 'CompareCreateView.vue'), 'utf8')
const detailSrc = () => readFileSync(resolve(__dirname, 'TaskDetailView.vue'), 'utf8')

describe('shared token gate composable (MC-10 C5)', () => {
  it('single-flight prompt writing the shared api-layer token', () => {
    const src = gateSrc()
    expect(src).toContain(`let tokenPromptOpen = false`)
    expect(src).toContain(`if (tokenPromptOpen) return false`)
    expect(src).toContain(`setAuthToken(token)`)
  })
})

describe('CompareCreateView 401 token prompt (MC-10 C5)', () => {
  it('the create→start flow carries the one-shot retry path', () => {
    const src = createSrc()
    expect(src).toContain(`async function submit(retriedAuth = false)`)
    expect(src).toContain(`e.response?.status === 401 && !retriedAuth && await promptForToken()`)
    expect(src).toContain(`return submit(true)`)
    // 403 (server token unset) surfaces as the plain error line — no prompt
    expect(src).not.toContain(`status === 403`)
  })
})

describe('TaskDetailView lifecycle actions (MC-10 C5)', () => {
  it('every gated action carries the 401 retry path', () => {
    const src = detailSrc()
    // pause/resume (shared action), compare restart (start/resume), cancel, delete
    for (const anchor of [
      `async function action(actionName: string, retriedAuth = false)`,
      `async function restartCompare(resume: boolean, retriedAuth = false)`,
      `async function cancelTask(retriedAuth = false)`,
      `async function deleteTask(retriedAuth = false)`,
    ]) {
      expect(src).toContain(anchor)
    }
    const branches = src.split(`e.response?.status === 401 && !retriedAuth && await promptForToken()`).length - 1
    expect(branches).toBe(4)
  })

  it('403 surfaces as a plain error line — no prompt can fix a server-side gap', () => {
    const src = detailSrc()
    expect(src).not.toContain(`status === 403`)
  })
})
