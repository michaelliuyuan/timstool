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

  it('CDC params row forks by source: mysql shows binlog wording, slot/pub stays PG-only', () => {
    const src = viewSrc()
    // MS-11a: mysql branch shows the binlog wording; PG branch keeps the
    // original slot/pub segment byte-identical.
    expect(src).toContain(`<template v-if="srcIsMySQL"> · binlog 采集（file:pos 位点续传）</template><template v-else-if="connCfg.cdc.slot_name"> · slot={{ connCfg.cdc.slot_name }} · pub={{ connCfg.cdc.publication }}</template>`)
  })

  it('REPLICA IDENTITY no-PK assist is hidden for mysql sources (PG-only mechanism)', () => {
    const src = viewSrc()
    // MS-11c pen4 P3-c: 并轨单真源 — the guard rides srcIsMySQL itself
    // (same computed the copy forks use), not a second type probe.
    expect(src).toContain(`noPKTables.length && !srcIsMySQL`)
    expect(src).not.toContain(`connCfg?.source?.type !== 'mysql'`)
  })
})

// MS-11a 笔②锚：CDC 页源感知文案——MySQL 链七断言 + PG 恒等四面。
// 断言形=源文本精确匹配（禁页面盲扫）；PG 恒等面=分叉的 PG 侧字面逐字保留。
const cmpSrc = () =>
  readFileSync(resolve(__dirname, '../components/SyncCompareCard.vue'), 'utf8')

describe('CDCView source-aware copy (MS-11a pens 1+2)', () => {
  it('A1 pipeline strip source is dynamic: MySQL for mysql chains', () => {
    const src = viewSrc()
    expect(src).toContain(`:source="srcIsMySQL ? 'MySQL' : 'PostgreSQL'"`)
  })

  it('A2 mysql chain shows binlog wording, never slot/pub', () => {
    const src = viewSrc()
    expect(src).toContain(`<template v-if="srcIsMySQL"> · binlog 采集（file:pos 位点续传）</template>`)
    // The slot/pub segment is gated behind the PG (v-else-if) branch only.
    expect(src).not.toContain(`<template v-if="connCfg.cdc.slot_name">`)
  })

  it('A3 no bare LSN literal on mysql chains — status/checkpoint/badge all forked', () => {
    const src = viewSrc()
    expect(src).toContain(`{{ srcIsMySQL ? '同步位点' : 'LSN' }}: {{ status.lsn }}`)
    expect(src).toContain(`{{ srcIsMySQL ? '同步位点:' : 'LSN:' }}`)
    expect(src).toContain(`label: srcIsMySQL.value ? 'Binlog' : 'LSN'`)
  })

  it('A4 startup hint names the actual source', () => {
    const src = viewSrc()
    expect(src).toContain(`{{ srcIsMySQL ? 'MySQL' : 'PG' }}/TiDB 写首条状态`)
  })

  it('A5 reset-checkpoint warning forks: mysql replays from checkpoint binlog position', () => {
    const src = viewSrc()
    expect(src).toContain(`srcIsMySQL.value ? ' checkpoint 记录的 binlog 位点重放' : ' slot restart_lsn 重放'`)
  })

  it('A6 compare card CDC row passes the mysql binlog requirement', () => {
    const src = viewSrc()
    expect(src).toContain(`:cdc-source-req="srcIsMySQL ? 'binlog（ROW 格式 + REPLICATION 权限）' : undefined"`)
    const c = cmpSrc()
    expect(c).toContain(`<td>{{ cdcSourceReq }}</td>`)
  })

  it('A7 datasource import placeholder has no PG-only form', () => {
    const src = viewSrc()
    expect(src).toContain(`placeholder="源端数据源"`)
    expect(src).not.toContain(`源端（PG/MySQL 数据源）`)
    expect(src).not.toContain(`源端（PG 数据源）`)
  })

  it('PG identity 1/4: slot/pub segment keeps the original literal in the PG branch', () => {
    const src = viewSrc()
    expect(src).toContain(`<template v-else-if="connCfg.cdc.slot_name"> · slot={{ connCfg.cdc.slot_name }} · pub={{ connCfg.cdc.publication }}</template>`)
  })

  it('PG identity 2/4: startup hint PG side is the original wording', () => {
    const src = viewSrc()
    expect(src).toContain(`control 通道确认运行，等待 CDC 连接 {{ srcIsMySQL ? 'MySQL' : 'PG' }}/TiDB 写首条状态，约 8-90s`)
  })

  it('PG identity 3/4: reset warning PG side is the original slot restart_lsn wording', () => {
    const src = viewSrc()
    expect(src).toContain(`' slot restart_lsn 重放'`)
  })

  it('PG identity 4/4: SyncCompareCard default prop is the original PG text (watermark page untouched)', () => {
    const c = cmpSrc()
    expect(c).toContain(`{ cdcSourceReq: '逻辑复制 slot（wal_level=logical、复制权限）' }`)
  })

  it('pen4a: no first-frame PG flash — pipeline strip and compare card render only after connCfg loads', () => {
    const src = viewSrc()
    expect(src).toContain(`<SyncCompareCard v-if="connCfg" current="cdc"`)
    expect(src).toContain(`<DataPipelineStrip\n      v-if="connCfg"`)
  })

  it('pen4c: source switches re-run the slot fetch — stale PG slot card cleared on switch to mysql', () => {
    const src = viewSrc()
    expect(src).toContain(`watch(srcIsMySQL, () => refreshSlot())`)
  })
})
