// 比对页水位列区间条件（任务书）：单值/区间互斥切换仅对 datetime/date 列
// 开放（int/text 隐藏区间）；区间模式隐藏 op 下拉、改双 date-picker，提示
// 半开区间 [start, end) + timestamptz UTC；payload 为 range_start/range_end；
// options 往返持久化区间。断言形=spec 精确匹配（本仓 vitest 惯例，禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CompareView.vue'), 'utf8')
const apiSrc = () =>
  readFileSync(resolve(__dirname, '../api/index.ts'), 'utf8')

describe('compare watermark range conditions (任务书)', () => {
  it('API inline watermark types carry the optional range fields', () => {
    const src = apiSrc()
    expect(src).toContain(`range_start?: string; range_end?: string`)
  })

  it('wmConfig gains range state: range_mode/range_start/range_end', () => {
    const src = viewSrc()
    expect(src).toContain(`range_mode: false,`)
    expect(src).toContain(`range_start: '',`)
    expect(src).toContain(`range_end: '',`)
  })

  it('range is only offered for datetime/date columns (wmRangeEligible)', () => {
    const src = viewSrc()
    expect(src).toContain(`wmRangeEligible`)
    expect(src).toContain(`wmKindInferred.value === 'datetime' || wmKindInferred.value === 'date'`)
    // 单值/区间切换仅在列类型允许时渲染。
    expect(src).toContain(`el-radio-group v-if="wmRangeEligible"`)
    expect(src).toContain(`<el-radio-button :value="false">单值</el-radio-button>`)
    expect(src).toContain(`<el-radio-button :value="true">区间</el-radio-button>`)
  })

  it('switching to a non-eligible column exits range mode (deadlock guard)', () => {
    const src = viewSrc()
    // 切到 int/text 列时 radio 组隐藏，watcher 必须自动退出区间模式防死锁。
    expect(src).toContain(`watch(wmRangeEligible, (eligible) => {`)
    expect(src).toContain(`if (!eligible) wmConfig.range_mode = false`)
  })

  it('range mode hides the op dropdown and uses dual date-pickers', () => {
    const src = viewSrc()
    expect(src).toContain(`<template v-if="wmConfig.range_mode">`)
    expect(src).toContain(`<template v-else>`)
    expect(src).toContain(`v-model="wmConfig.range_start"`)
    expect(src).toContain(`v-model="wmConfig.range_end"`)
    // op 下拉只在非区间（v-else）分支内。
    expect(src).toContain(`v-model="wmConfig.op" style="width: 90px;"`)
  })

  it('range payload uses range_start/range_end (no value/op)', () => {
    const src = viewSrc()
    expect(src).toContain(`range_start: wmConfig.range_start.trim(), range_end: wmConfig.range_end.trim()`)
  })

  it('options save/apply round-trips the interval group', () => {
    const src = viewSrc()
    expect(src).toContain(`range_start: wmConfig.range_start, range_end: wmConfig.range_end`)
    expect(src).toContain(`wmConfig.range_start = w.range_start`)
    expect(src).toContain(`wmConfig.range_end = w.range_end`)
    expect(src).toContain(`wmConfig.range_mode = true`)
  })

  it('interval validation: both endpoints required, format, start >= end', () => {
    const src = viewSrc()
    expect(src).toContain(`区间起点与终点都必须填写`)
    expect(src).toContain(`区间起点格式应为 YYYY-MM-DD HH:mm:ss`)
    expect(src).toContain(`半开区间 [start, end)`)
  })

  it('half-open interval hint disclosed in range mode', () => {
    const src = viewSrc()
    expect(src).toContain(`区间为半开 [start, end)，终点行不包含。`)
  })
})
