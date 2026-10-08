// MS-11c 布局锚：CDC 页面三分 Tab 重构 + Hero 状态条 + 合并卡 + 预检折叠/
// 徽标 + 位点推进小图。断言形=源文本精确匹配（禁页面盲扫）；srcIsMySQL
// 文案分派与 MS-11a 竞态三面闭环的恒等面由 cdcSourceAgnostic.spec.ts 守护，
// 本文件只锚新增结构面。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CDCView.vue'), 'utf8')

describe('CDCView layout (MS-11c pens 1+2)', () => {
  it('A hero bar inlines start/stop/refresh and carries the four-state skin', () => {
    const src = viewSrc()
    expect(src).toContain(`class="status-card hero" :class="cardState"`)
    // start keeps the precheck disable semantics + tooltip (tab wording)
    expect(src).toContain(`:disabled="busy || isActive || !precheckPassed"`)
    expect(src).toContain(`请先在「启动预检」Tab 重新检查`)
    expect(src).toContain(`@click="confirmStopCDC">停止 CDC</el-button>`)
    expect(src).toContain(`<el-button @click="refresh">刷新</el-button>`)
    // hero summary strip: lag/throughput/uptime
    expect(src).toContain(`<span>延迟 {{ stats.lag_seconds?.toFixed(1) || '0' }}s</span>`)
    expect(src).toContain(`<span>运行 {{ formatUptime(stats.uptime_seconds) }}</span>`)
  })

  it('B stats grid sits right under the hero (pipeline strip stays above it)', () => {
    const src = viewSrc()
    const heroIdx = src.indexOf('status-card hero')
    const statsIdx = src.indexOf('class="stats-grid"')
    const stripIdx = src.indexOf('<DataPipelineStrip')
    expect(stripIdx).toBeLessThan(heroIdx)
    expect(heroIdx).toBeLessThan(statsIdx)
  })

  it('C three tabs: overview default, connection+config, precheck', () => {
    const src = viewSrc()
    expect(src).toContain(`<el-tabs v-model="activeTab" class="cdc-tabs">`)
    expect(src).toContain(`ref<'overview' | 'conn' | 'precheck'>('overview')`)
    expect(src).toContain(`<el-tab-pane label="概览" name="overview">`)
    expect(src).toContain(`<el-tab-pane label="连接与配置" name="conn">`)
    expect(src).toContain(`<el-tab-pane name="precheck">`)
  })

  it('C merged position card unifies checkpoint/config/slot with dedup guards', () => {
    const src = viewSrc()
    expect(src).toContain(`<h3>位点 / 复制槽 / 进程</h3>`)
    expect(src).toContain(`hasPositionInfo`)
    // dedup: the slotView checkpoint row hides when it equals the live checkpoint
    expect(src).toContain(`(!checkpoint || slotView.checkpoint.lsn !== checkpoint.lsn)`)
    // no duplicate 运行时长 row (stats-grid 运行时间 owns that datum)
    expect(src).not.toContain(`运行时长:</span>`)
  })

  it('C SyncCompareCard lives in the connection tab, still behind connCfg', () => {
    const src = viewSrc()
    const tabIdx = src.indexOf(`label="连接与配置"`)
    const cmpIdx = src.indexOf(`<SyncCompareCard v-if="connCfg" current="cdc"`)
    expect(tabIdx).toBeGreaterThan(-1)
    expect(cmpIdx).toBeGreaterThan(tabIdx)
  })

  it('pen2: precheck folds ok items — warn/fail always rendered first', () => {
    const src = viewSrc()
    expect(src).toContain(`v-for="it in precheckAttentionItems"`)
    expect(src).toContain(`v-for="it in precheckOkItems"`)
    expect(src).toContain(`showOkItems = !showOkItems`)
    expect(src).toContain(`显示通过项（${'${precheckOkItems.length}'}）`)
    // the folded ok rows render the plain ok shape, never the no-PK fix button
    expect(src).toContain(`const precheckAttentionItems = computed(() => (precheck.value?.items || []).filter(it => it.level !== 'ok'))`)
  })

  it('pen2: precheck tab badge + fail auto-activate (warn-only never steals focus)', () => {
    const src = viewSrc()
    expect(src).toContain(`<span v-if="precheck && !precheckPassed" class="tab-dot"`)
    expect(src).toContain(`watch(precheck, p => {`)
    expect(src).toContain(`activeTab.value = 'precheck'`)
  })

  it('pen2: 位点推进 sparkline parses PG LSN and MySQL file:pos, skips junk', () => {
    const src = viewSrc()
    expect(src).toContain(`<SparkLine v-if="positionHistory.length > 1" :data="positionHistory" :height="34" />`)
    expect(src).toContain(`h * 0x100000000 + l`)
    expect(src).toContain(`* 1e9 + pos`)
    expect(src).toContain(`if (v === null) return`)
    expect(src).toContain(`pushPosition(statusRes?.lsn || cpRes?.lsn)`)
  })
})

describe('CDCView pen4 fixes (adversarial P2/P3 closure)', () => {
  it('P2-⑤: hero meta is neutral until connCfg arrives — no forked-word first-frame flash', () => {
    const src = viewSrc()
    expect(src).toContain(`<div class="status-meta" v-if="connCfg">`)
    expect(src).toContain(`<div class="status-meta" v-else>连接配置加载中…</div>`)
    // the forked literals still live INSIDE the connCfg-gated block
    const gateIdx = src.indexOf(`<div class="status-meta" v-if="connCfg">`)
    const forkIdx = src.indexOf(`{{ srcIsMySQL ? '同步位点' : 'LSN' }}: {{ status.lsn }}`)
    expect(forkIdx).toBeGreaterThan(gateIdx)
  })

  it('P2-①: fail auto-activate fires only on the no-fail→fail transition and never after the operator leaves', () => {
    const src = viewSrc()
    expect(src).toContain(`if (!precheckFailActive && !userLeftPrecheckTab) {`)
    expect(src).toContain(`precheckFailActive = true`)
    expect(src).toContain(`watch(activeTab, t => {`)
    // fail-clear resets the episode so a NEW fail may activate again
    expect(src).toContain(`precheckFailActive = false`)
    expect(src).toContain(`userLeftPrecheckTab = false`)
  })

  it('P2-② + P3-d: tab dot and ok-fold are accessible', () => {
    const src = viewSrc()
    expect(src).toContain(`class="tab-dot" :class="precheckHasFail ? 'fail' : 'warn'" role="status"`)
    expect(src).toContain(`:aria-expanded="showOkItems" aria-controls="precheck-ok-list"`)
    expect(src).toContain(`id="precheck-ok-list"`)
  })

  it('P2-④: runPrecheck failure is visible — stale result labeled, never dressed as fresh', () => {
    const src = viewSrc()
    expect(src).toContain(`precheckError.value = e?.response?.data?.error || e?.message || '网络错误'`)
    expect(src).toContain(`显示的是上次结果，请重新检查`)
    // success clears the banner
    expect(src).toContain(`precheckError.value = ''`)
  })

  it('P3-a/P3-b: parsePosition gates finite/non-negative and reads the post-dot rotation index only', () => {
    const src = viewSrc()
    expect(src).toContain(`(!Number.isFinite(n) || n < 0 ? null : n)`)
    expect(src).toContain(`lsn.slice(0, idx).split('.').pop()`)
  })
})

describe('CDCView pen5 closure (merged-card flash + transient button)', () => {
  it('pen5: merged-card forked 位点 label gates on connCfg — no "LSN:" flash on mysql chains', () => {
    const src = viewSrc()
    expect(src).toContain(`<div class="detail-row" v-if="connCfg && checkpoint && checkpoint.lsn">`)
    const gateIdx = src.indexOf(`<div class="detail-row" v-if="connCfg && checkpoint && checkpoint.lsn">`)
    const forkIdx = src.indexOf(`{{ srcIsMySQL ? '同步位点:' : 'LSN:' }}`)
    expect(forkIdx).toBeGreaterThan(gateIdx)
  })

  it('pen5: no-PK assist button stays hidden in the pre-connCfg transient', () => {
    const src = viewSrc()
    expect(src).toContain(`noPKTables.length && connCfg && !srcIsMySQL`)
  })

  it('pen5: fail predicates pinned — auto-activate and badge ride level === "fail", never warn', () => {
    const src = viewSrc()
    expect(src).toContain(`.some(it => it.level === 'fail')`)
    expect(src).toContain(`const precheckHasFail = computed(() => (precheck.value?.items || []).some(it => it.level === 'fail'))`)
  })
})
