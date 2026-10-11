import { ref } from 'vue'
import type { DataSource } from '../api'
import apiClient from '../api'

// Module-level cache of the datasource registry (F-02). Views refresh it
// after create/update/delete so every picker on the page sees fresh data.
const datasources = ref<DataSource[]>([])
let loaded = false
let loading: Promise<void> | null = null

export function useDataSources() {
  async function load(force = false): Promise<void> {
    if (loaded && !force) return
    if (loading) return loading
    loading = (async () => {
      try {
        const { data } = await apiClient.listDataSources()
        datasources.value = (data.datasources || []).slice().sort((a, b) => a.name.localeCompare(b.name))
        loaded = true
      } catch {
        /* leave empty; pickers fall back to manual entry */
      } finally {
        loading = null
      }
    })()
    return loading
  }

  function byType(types?: string[]): DataSource[] {
    if (!types || types.length === 0) return datasources.value
    return datasources.value.filter(d => types.includes(d.type))
  }

  function get(id: string): DataSource | undefined {
    return datasources.value.find(d => d.id === id)
  }

  return { datasources, load, byType, get }
}
