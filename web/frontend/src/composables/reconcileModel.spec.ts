import { describe, it, expect } from 'vitest'
import { reconcileModel } from './reconcileModel'
import type { SourceMeta } from './sourceTypes'

// Fixtures mirror the real backend SourceMeta (internal/source/describe.go),
// served by GET /api/v1/sources. They are the single source of truth for the
// connection form, so the unit test asserts reconcileModel against the actual
// field contracts (PG 7 fields / MySQL 6 fields).

const pg: SourceMeta = {
  name: 'postgres',
  displayName: 'PostgreSQL',
  implemented: true,
  defaultPort: 5432,
  fields: [
    { key: 'host', label: 'Host', type: 'text', required: true, default: 'localhost', group: 'common' },
    { key: 'port', label: 'Port', type: 'number', required: true, default: 5432, group: 'common' },
    { key: 'user', label: 'User', type: 'text', required: true, default: 'postgres', group: 'common' },
    { key: 'password', label: 'Password', type: 'password', group: 'common' },
    { key: 'database', label: 'Database', type: 'text', required: true, group: 'common' },
    { key: 'schema', label: 'Schema', type: 'text', default: 'public', group: 'source' },
    { key: 'sslmode', label: 'SSL Mode', type: 'select', default: 'disable', group: 'source' },
  ],
  capabilities: { schema: true, data: true, cdc: true },
}

const mysql: SourceMeta = {
  name: 'mysql',
  displayName: 'MySQL',
  implemented: true,
  defaultPort: 3306,
  fields: [
    { key: 'host', label: 'Host', type: 'text', required: true, default: 'localhost', group: 'common' },
    { key: 'port', label: 'Port', type: 'number', required: true, default: 3306, group: 'common' },
    { key: 'user', label: 'User', type: 'text', required: true, default: 'root', group: 'common' },
    { key: 'password', label: 'Password', type: 'password', group: 'common' },
    { key: 'database', label: 'Database', type: 'text', required: true, group: 'common' },
    { key: 'charset', label: 'Charset', type: 'select', default: 'utf8mb4', group: 'source' },
  ],
  capabilities: { schema: true, data: true, cdc: false },
}

describe('reconcileModel — doc §7.2 source-switch rules', () => {
  it('FL-UT-01: preserves common keys (host/user/password/database)', () => {
    const next = reconcileModel(
      { host: 'h1', port: 5432, user: 'u1', password: 'p1', database: 'd1', schema: 's1', sslmode: 'require' },
      pg,
      mysql,
    )
    expect(next.host).toBe('h1')
    expect(next.user).toBe('u1')
    expect(next.password).toBe('p1')
    expect(next.database).toBe('d1')
  })

  it('FL-UT-02: drops source-specific keys not in new schema (pg->mysql: no schema/sslmode)', () => {
    const next = reconcileModel(
      { host: 'h', port: 5432, user: 'u', password: 'p', database: 'd', schema: 's', sslmode: 'require' },
      pg,
      mysql,
    )
    expect(next).not.toHaveProperty('schema')
    expect(next).not.toHaveProperty('sslmode')
  })

  it('FL-UT-03: port default-swap when old port == old-source default (5432 -> 3306)', () => {
    const next = reconcileModel(
      { host: 'h', port: 5432, user: 'u', password: 'p', database: 'd', schema: 's' },
      pg,
      mysql,
    )
    expect(next.port).toBe(3306)
  })

  it('FL-UT-04: port user-value retained when non-default (5433 stays)', () => {
    const next = reconcileModel(
      { host: 'h', port: 5433, user: 'u', password: 'p', database: 'd', schema: 's' },
      pg,
      mysql,
    )
    expect(next.port).toBe(5433)
  })

  it('FL-UT-05: port empty/undefined -> new source default (3306)', () => {
    const a = reconcileModel({ host: 'h', user: 'u', password: 'p', database: 'd' }, pg, mysql)
    expect(a.port).toBe(3306)
    const b = reconcileModel({ host: 'h', port: '', user: 'u', password: 'p', database: 'd' }, pg, mysql)
    expect(b.port).toBe(3306)
  })

  it('FL-UT-06: fills new-source field defaults (charset=utf8mb4; reverse schema/sslmode)', () => {
    const toMysql = reconcileModel(
      { host: 'h', port: 5432, user: 'u', password: 'p', database: 'd', schema: 's', sslmode: 'require' },
      pg,
      mysql,
    )
    expect(toMysql.charset).toBe('utf8mb4')
    const toPg = reconcileModel(
      { host: 'h', port: 3306, user: 'u', password: 'p', database: 'd', charset: 'latin1' },
      mysql,
      pg,
    )
    expect(toPg.schema).toBe('public')
    expect(toPg.sslmode).toBe('disable')
  })

  it('FL-UT-07 / FL-REQ invariant: output keys === exactly newMeta.fields keys (no stale keys leak)', () => {
    // pg -> mysql: schema/sslmode and any stray key must NOT appear
    const next = reconcileModel(
      { host: 'h', port: 5432, user: 'u', password: 'p', database: 'd', schema: 's', sslmode: 'require', stray: 'X' },
      pg,
      mysql,
    )
    expect(Object.keys(next).sort()).toEqual(['charset', 'database', 'host', 'password', 'port', 'user'])
    // reverse mysql -> pg: charset and any stray key must NOT appear
    const back = reconcileModel(
      { host: 'h', port: 3306, user: 'u', password: 'p', database: 'd', charset: 'utf8mb4', stray: 'Y' },
      mysql,
      pg,
    )
    expect(Object.keys(back).sort()).toEqual(['database', 'host', 'password', 'port', 'schema', 'sslmode', 'user'])
  })
})
