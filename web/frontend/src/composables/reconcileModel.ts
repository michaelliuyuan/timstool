import type { SourceMeta } from './sourceTypes'

// FieldValues is the dynamic connection model: a map of field key -> value.
// It contains exactly the current source's field keys (no stale keys from a
// previous source).
export type FieldValues = Record<string, any>

// Common keys preserved verbatim across a source switch (present for every
// source). Port is reconciled separately (default-swap rule).
const COMMON_KEYS = ['host', 'user', 'password', 'database']

// reconcileModel applies the doc §7.2 source-switch rules, returning a NEW model
// that contains exactly newMeta's fields — the core fix for "切换后表单不联动"
// and the FL-REQ zero-tolerance item (no stale source-specific keys pollute the
// SourceConfig/DSN):
//   1. common keys (host/user/password/database) preserved from oldModel
//   2. port: if oldModel.port equals oldMeta.defaultPort (or is empty) → new
//      source defaultPort; otherwise the user's value is preserved
//   3. source-specific keys not in the new schema are dropped (never carried)
//   4. new-schema keys absent from the model get their declared default
//
// Pure & standalone so #t78 can unit-test it directly.
export function reconcileModel(
  oldModel: FieldValues,
  oldMeta: SourceMeta,
  newMeta: SourceMeta,
): FieldValues {
  const next: FieldValues = {}
  for (const f of newMeta.fields) {
    const had = oldModel[f.key] !== undefined
    if (f.key === 'port') {
      const oldPort = oldModel['port']
      const swap =
        oldPort === undefined || oldPort === '' || oldPort === oldMeta.defaultPort
      next['port'] = swap ? newMeta.defaultPort : oldPort
    } else if (COMMON_KEYS.includes(f.key) && had) {
      next[f.key] = oldModel[f.key]
    } else if (had) {
      next[f.key] = oldModel[f.key]
    } else if (f.default !== undefined && f.default !== null) {
      next[f.key] = f.default
    } else {
      next[f.key] = ''
    }
  }
  return next
}
