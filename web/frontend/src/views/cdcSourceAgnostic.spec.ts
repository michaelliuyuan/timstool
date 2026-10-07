// MS-11 笔④翻面锚：CDC 面向 MySQL 开放——数据源导入类型集 postgres+mysql、
// 连接编辑区源端标签通用形、CDC 参数行 slot/pub 段按存在性条件渲染
// （MySQL 无 slot/publication 概念）、no-PK 修复（REPLICA IDENTITY FULL）
// 为 PG-only 机制对 MySQL 隐藏。断言形=spec 精确匹配（禁页面盲扫）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const viewSrc = () =>
  readFileSync(resolve(__dirname, 'CDCView.vue'), 'utf8')

describe('CDCView source surface (MS-11 pen 4)', () => {
  it('datasource import offers postgres + mysql sources', () => {
    const src = viewSrc()
    expect(src).toContain(`dsByType(['postgres', 'mysql'])`)
    // The stale PG-only picker forms must be gone.
    expect(src).not.toContain(`dsByType(['postgres'])`)
    expect(src).not.toContain(`源端（PG 数据源）`)
  })

  it('connection edit section label is source-generic', () => {
    const src = viewSrc()
    expect(src).toContain(`源端数据库（PostgreSQL / MySQL）`)
    expect(src).not.toContain(`源端（PostgreSQL）`)
  })

  it('slot/pub segment renders only when a slot is configured (mysql has none)', () => {
    const src = viewSrc()
    expect(src).toContain(`<template v-if="connCfg.cdc.slot_name"> · slot={{ connCfg.cdc.slot_name }} · pub={{ connCfg.cdc.publication }}</template>`)
  })

  it('REPLICA IDENTITY no-PK assist is hidden for mysql sources (PG-only mechanism)', () => {
    const src = viewSrc()
    expect(src).toContain(`noPKTables.length && connCfg?.source?.type !== 'mysql'`)
  })
})
