<script setup lang="ts">
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import apiClient from '../api'

// MC-11 database browse+pick (single-select, symmetric for both compare
// sides): the 「获取数据库列表」 button fetches the list through
// POST /sources/meta — the payload prop decides datasource_id vs manual
// fields, so the caller owns the connection shape. The engine's v1 contract
// is single-db-per-side, so picking is a radio that fills the manual input
// (dual-track: manual entry stays first-class). System databases are
// filtered client-side (admin/config/local — the engine's skip set,
// engine.go:142). A failed fetch degrades to manual input only — it must
// never block task creation.
const props = defineProps<{
  modelValue: string
  payload: () => { datasource_id?: string; source?: string; fields?: Record<string, any> }
}>()
const emit = defineEmits<{ (e: 'update:modelValue', v: string): void }>()

const loading = ref(false)
const dbs = ref<string[] | null>(null)

const SYSTEM_DBS = new Set(['admin', 'config', 'local'])

async function fetchDbs() {
  loading.value = true
  try {
    const { data } = await apiClient.browseSourceDatabases(props.payload())
    const names = (data.entries || [])
      .map((e) => e.name)
      .filter((n) => !SYSTEM_DBS.has(n))
    if (!names.length) {
      ElMessage.info('未发现可对比的用户库（系统库已过滤）')
      dbs.value = null
      return
    }
    dbs.value = names.sort()
  } catch (e: any) {
    // Degrade to manual input (non-blocking by design).
    ElMessage.error(e.response?.data?.error || '获取数据库列表失败，可手动输入库名')
    dbs.value = null
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div>
    <el-input
      :model-value="modelValue"
      placeholder="例如 orders"
      style="max-width: 320px; vertical-align: middle;"
      @update:model-value="emit('update:modelValue', $event)"
    />
    <el-button :loading="loading" style="margin-left: 8px;" @click="fetchDbs">获取数据库列表</el-button>
    <div
      v-if="dbs"
      data-testid="mc11-db-list"
      style="margin-top: 8px; max-width: 360px; max-height: 180px; overflow-y: auto; border: 1px solid var(--el-border-color-lighter); border-radius: 4px; padding: 8px 12px;"
    >
      <el-radio-group
        :model-value="modelValue"
        style="display: flex; flex-direction: column; gap: 4px;"
        @update:model-value="emit('update:modelValue', $event)"
      >
        <el-radio v-for="d in dbs" :key="d" :value="d">{{ d }}</el-radio>
      </el-radio-group>
    </div>
  </div>
</template>
