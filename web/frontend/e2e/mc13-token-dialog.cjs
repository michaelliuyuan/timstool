// MC-13 工单2 回归层：真实浏览器 click 触发 401 → 三态令牌弹框全链。
// 历史盲区：jsdom 单测直接调 submit()（无参，默认 retriedAuth=false 生效），
// 测不出 bare @click="submit" 把 MouseEvent 当首参传入导致弹框永不出现的坑
// （CompareCreateView 401 直落错误 toast）。本用例必须真实 click——这正是
// 它存在的意义，禁改回 dispatch/直接调函数。
//
// 前置：一个带 security.token 的本地 web 实例（真 bundle，非 vite dev）：
//   npm run build && go build -o tims13.exe .
//   ./tims13.exe web -c <config.yaml with security.token> -p 18080
// 运行：
//   node mc13-token-dialog.cjs                # 默认 http://127.0.0.1:8080
//   E2E_BASE_URL=http://127.0.0.1:18080 node mc13-token-dialog.cjs
// 断言链：① 401 后弹框必须出现且先于任何错误 toast（旧 bug 位）
//        ② 错 token → verify 401 → 同框重输带「第 2/3 次」计数
//        ③ 取消 → info 态「已取消令牌输入，操作未执行」（非 error）
const assert = require('node:assert')
const { chromium } = require('playwright')

const BASE = process.env.E2E_BASE_URL || 'http://127.0.0.1:8080'

async function main() {
  const browser = await chromium.launch()
  const page = await browser.newPage({ viewport: { width: 1920, height: 1080 } })
  const seen = { create401: false }
  page.on('response', (r) => {
    if (r.url().includes('/api/v1/mongocompare/tasks') && r.request().method() === 'POST' && r.status() === 401) {
      seen.create401 = true
    }
  })

  await page.goto(`${BASE}/mongocompare/new`, { waitUntil: 'domcontentloaded', timeout: 45000 })
  await page.waitForSelector('text=源端（被对比方）', { timeout: 30000 })
  await page.waitForTimeout(800)

  // 手输库名（双轨红线：手输一等公民，不依赖库列表拉取）
  await page.locator('.db-browse-row input').first().fill('testdb')
  // tgtSameAsSrc 初值 true：目标库名自动同源端，无需第二输入

  // 前置态：无任何弹框
  assert.equal(await page.locator('.el-message-box').count(), 0, 'pre-click: no dialog yet')

  // 真实 click —— 回归点本体
  await page.getByRole('button', { name: '创建并启动对比' }).click()

  // ① 401 → 弹框必须出现（旧 bug：永不出现，直落 unauthorized toast）
  const box = page.locator('.el-message-box')
  await box.waitFor({ state: 'visible', timeout: 8000 })
  assert.ok(seen.create401, 'POST /api/v1/mongocompare/tasks got 401')
  await box.getByText('该操作需要管理令牌').waitFor({ state: 'visible', timeout: 3000 })
  assert.equal(
    await page.locator('.el-message--error').count(), 0,
    'dialog-first: no error toast before the prompt (old-bug discriminator)',
  )

  // ② 错 token：verify 401 → 同框重输，带计数
  await box.locator('input').fill('wrong-token-mc13')
  await box.getByRole('button', { name: '确定' }).click()
  await box.getByText('第 2/3 次').waitFor({ state: 'visible', timeout: 8000 })

  // ③ 取消 = info 态（非 error）
  await box.getByRole('button', { name: '取消' }).last().click()
  await page
    .locator('.el-message--info', { hasText: '已取消令牌输入，操作未执行' })
    .waitFor({ state: 'visible', timeout: 8000 })

  await browser.close()
  console.log('MC-13 token-dialog e2e: PASS (401→dialog→wrong-token counter→cancel info)')
}

main().catch((e) => {
  console.error('MC-13 token-dialog e2e: FAIL —', e.message)
  process.exitCode = 1
})
