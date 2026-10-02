import { ref } from 'vue'
import type { SourceMeta, Capabilities } from './sourceTypes'
import apiClient from '../api'

// Module-level cache: GET /api/v1/sources once per page load (doc §7.3). The
// source selector and the schema-driven form share it.
const sources = ref<SourceMeta[]>([])
const byName = ref<Record<string, SourceMeta>>({})
let loaded = false

export function useSourceSchema() {
  async function load(): Promise<void> {
    if (loaded) return
    try {
      const { data } = await apiClient.getSources()
      const list: SourceMeta[] = data.sources || []
      sources.value = list
      const map: Record<string, SourceMeta> = {}
      for (const s of list) map[s.name] = s
      byName.value = map
      loaded = true
    } catch {
      /* leave empty; the wizard falls back gracefully */
    }
  }

  function getSource(name: string): SourceMeta | undefined {
    return byName.value[name]
  }

  // MS-01 single truth for UI greying: read the capability matrix, not the
  // raw type string. Legacy default: a missing/empty kind is postgres with
  // ALL capabilities (old datasource profiles stay fully enabled).
  function capable(name: string, cap: keyof Capabilities): boolean {
    const meta = byName.value[name]
    if (!meta) return name === '' || name === 'postgres'
    return meta.capabilities?.[cap] === true
  }

  // Implemented source kinds for pickers (target-side tidb is excluded — it
  // lives in the datasource registry, not the source registry).
  function implementedKinds(): string[] {
    return sources.value.filter(s => s.implemented).map(s => s.name)
  }

  return { sources, load, getSource, capable, implementedKinds }
}
