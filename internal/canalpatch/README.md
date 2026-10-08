# canalpatch — vendored go-mysql canal package (MS-11g 笔①)

Local copy of `github.com/go-mysql-org/go-mysql@v1.11.0` package `canal`
(8 non-test .go files + upstream LICENSE, Apache-2.0). All sibling packages
(mysql / replication / schema / client / dump / utils) still resolve from the
pinned upstream module — only this package is vendored.

## Why

Upstream `sync.go` silently skips Query(binlog) events it cannot classify:

1. **parse failures** (`sync.go` QueryEvent branch, upstream :142-149):
   logged at Error level, then skipped — the registered EventHandler's
   OnDDL is never called (CREATE TRIGGER, stored procedures, ...).
2. **zero table nodes** (parseable but outside parseStmt's seven-type
   whitelist): no OnDDL call AND no log line (CREATE/ALTER/DROP VIEW, ...).

Both shapes bypass the downstream DDL red-line gate (target-db halt /
non-target ignore — see internal/cdc/binlog_canal.go). This patch makes
them visible.

## Patch (additive, interface-assertion delivery)

Diff vs upstream v1.11.0 (byte-identical except the lines below):

- `handler.go`: adds `UnrecognizedQueryHandler` — an opt-in interface with
  `OnUnrecognizedQuery(header, nextPos, queryEvent, reason) error`.
- `sync.go` QueryEvent branch: parse-failure path and the
  zero-table-node-total path deliver the raw event to the handler via
  interface assertion. Handlers not implementing the interface keep the
  exact upstream behavior; savePos / table-cache / OnDDL seven-type
  semantics are untouched.

## Maintenance note

**go-mysql version bumps require manually syncing this copy.** The form
anchors in internal/cdc (TestCanalpatchFormAnchors) pin the two delivery
points — if a re-sync drops or moves them, those anchors fail and are the
drift alarm. Upstream fixes for the silent-skip behavior (if ever landed)
may let this package be retired in favor of the upstream event face.
