// MC-13 门3 样式三缺陷复测（本地真 bundle 实例 + /sources/meta 路由 mock）：
// ①按钮换行：输入与「获取数据库列表」按钮同 y、同行（row 不 wrap）
// ②10px 错位：.db-card 左缘 == .db-browse-row 左缘（无漂移）
// ③7px 右溢出：.db-card/.db-row/.el-scrollbar 右缘 == 根 .db-browse 右缘（宽 460，border-box）
const assert = require('node:assert')
const { chromium } = require('playwright')

const BASE = process.env.E2E_BASE_URL || 'http://127.0.0.1:18080'
const entries = []
for (let i = 1; i <= 10; i++) entries.push({ name: `db${String(i).padStart(2, '0')}`, size_on_disk: i * 1048576 })

async function main() {
  const browser = await chromium.launch()
  const page = await browser.newPage({ viewport: { width: 1920, height: 1080 } })
  await page.route('**/api/v1/sources/meta', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ source: 'mongodb', level: 'database', entries, count: entries.length }) }),
  )
  await page.goto(`${BASE}/mongocompare/new`, { waitUntil: 'domcontentloaded', timeout: 45000 })
  const srcCard = page.locator('.el-card').filter({ hasText: '源端（被对比方）' }).first()
  await srcCard.waitFor({ timeout: 30000 })
  await page.waitForTimeout(600)

  const root = srcCard.locator('.db-browse').first()
  const row = srcCard.locator('.db-browse-row').first()
  const input = row.locator('.el-input').first()
  const btn = row.getByRole('button', { name: '获取数据库列表' })

  // ① 同行不换行
  const rb = await row.boundingBox(); const ib = await input.boundingBox(); const bb = await btn.boundingBox()
  assert.ok(rb && ib && bb, 'row/input/btn boxes exist')
  assert.equal(await row.evaluate((e) => getComputedStyle(e).flexWrap), 'nowrap', 'row flex-wrap=nowrap')
  assert.ok(Math.abs(ib.y - bb.y) < 1 && Math.abs(ib.height - bb.height) < 2, `input/btn same row (y ${ib.y} vs ${bb.y})`)
  assert.ok(bb.x > ib.x + ib.width - 1, 'btn sits right of input on the same line')

  // 拉列表出卡片
  await btn.click()
  await srcCard.locator('[data-testid=mc11-db-list]').waitFor({ timeout: 10000 })
  await page.waitForTimeout(400)

  const card = srcCard.locator('[data-testid=mc11-db-list]').first()
  const radio = srcCard.locator('.db-radio-group').first()
  const oneRow = srcCard.locator('.db-row').first()
  const scroll = srcCard.locator('.db-card .el-scrollbar').first()
  const gb = await root.boundingBox(); const cb = await card.boundingBox()
  const rnb = await radio.boundingBox(); const orb = await oneRow.boundingBox(); const sb = await scroll.boundingBox()

  // 根宽 460
  assert.ok(Math.abs(gb.width - 460) < 1.5, `root width ${gb.width} ~= 460`)
  // ② 左缘对齐（无 10px 错位）
  assert.ok(Math.abs(cb.x - rb.x) < 1, `card left ${cb.x} == row left ${rb.x}`)
  // ③ 右缘不溢出：card 本体右缘==根右缘；内部件（radio/row/scrollbar，含于
  // card padding）只须不超根右缘（旧缺陷=超出 7px，非内缩）
  for (const [name, b] of [['card', cb], ['radio-group', rnb], ['row-item', orb], ['scrollbar', sb]]) {
    assert.ok(b.x + b.width <= gb.x + gb.width + 1, `${name} right edge ${b.x + b.width} overflows root right ${gb.x + gb.width}`)
  }
  assert.ok(Math.abs(cb.x + cb.width - (gb.x + gb.width)) < 1, `card right edge exactly aligns root (got ${cb.x + cb.width} vs ${gb.x + gb.width})`)
  const boxSizing = await card.evaluate((e) => getComputedStyle(e).boxSizing + ' / ' + getComputedStyle(e).width)
  assert.ok(boxSizing.startsWith('border-box'), `card box-sizing: ${boxSizing}`)
  // 红线抽查：过滤框（>8 库）+ 选中填充
  assert.ok((await srcCard.locator('[data-testid=mc11-db-filter]').count()) === 1, 'filter input shown (>8 dbs)')
  await oneRow.click(); await page.waitForTimeout(300)
  const v = await row.locator('input').inputValue()
  assert.equal(v, 'db01', 'radio pick fills manual input (dual-track)')

  await page.screenshot({ path: 'C:/Users/刘源/AppData/Local/Temp/opencode/mc13-style-retest.png', fullPage: true })
  await browser.close()
  console.log('MC-13 style retest: PASS (no wrap / no 10px offset / no 7px overflow; 460 border-box aligned; redlines ok)')
}
main().catch((e) => { console.error('MC-13 style retest: FAIL —', e.message); process.exitCode = 1 })
