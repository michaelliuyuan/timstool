import { ref } from 'vue'
import type { SourceMeta } from './sourceTypes'
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

  return { sources, load, getSource }
}
