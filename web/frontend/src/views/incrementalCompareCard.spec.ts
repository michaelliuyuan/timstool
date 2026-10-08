// MS-11f 笔② FE 锚：水位页 SyncCompareCard 源端要求传参——无 prop 时恒显
// PG 专属文案（mysql 链部署误导），一行传通用联合文案（MS-11a 笔①同款形）。
// 组件默认 prop 不动（PG 恒等 4/4 锚在 cdcSourceAgnostic.spec.ts 保持有效）。
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const incSrc = () => readFileSync(resolve(__dirname, 'IncrementalView.vue'), 'utf8')
const cmpSrc = () => readFileSync(resolve(__dirname, '../components/SyncCompareCard.vue'), 'utf8')

describe('watermark page compare card (MS-11f pen2)', () => {
  it('passes a source-generic CDC requirement — never the PG-only default', () => {
    const src = incSrc()
    expect(src).toContain(
      `:cdc-source-req="'逻辑复制 slot（PG 源）/ binlog ROW 格式（MySQL 源）'"`,
    )
  })

  it('component default prop stays byte-identical (CDCView PG branch relies on it)', () => {
    const c = cmpSrc()
    expect(c).toContain(`{ cdcSourceReq: '逻辑复制 slot（wal_level=logical、复制权限）' }`)
    expect(c).toContain(`<td>{{ cdcSourceReq }}</td>`)
  })
})
