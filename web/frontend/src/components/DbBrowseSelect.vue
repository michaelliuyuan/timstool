<script setup lang="ts">
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
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
//
// MC-12 卡片化（观感升级，行为红线不动）：头部「共 N 库」徽标+刷新 icon
// 重新拉取；浅底圆角+el-scrollbar 细滚动条；radio 选中态边框高亮；长库名
// ellipsis+tooltip；右缘人类可读 size（entries 自带 size_on_disk，空库灰显
// 「空」）；列表>8 才现过滤输入框（兼得搜索，保一屏勾选语义）。
type DbEntry = { name: string; size_on_disk?: number; empty?: boolean }

const props = defineProps<{
  modelValue: string
  payload: () => { datasource_id?: string; source?: string; fields?: Record<string, any> }
}>()
const emit = defineEmits<{ (e: 'update:modelValue', v: string): void }>()

const loading = ref(false)
const allDbs = ref<DbEntry[] | null>(null)
const dbMeta = ref<Record<string, DbEntry>>({})
const filterText = ref('')

const SYSTEM_DBS = new Set(['admin', 'config', 'local'])

// dbs stays a NAME list (the radio's :value binds the db name, never an
// entry object); the current filter shrinks the view, never the pick set.
const dbs = computed<string[] | null>(() => {
  const list = allDbs.value
  if (!list) return null
  const q = filterText.value.trim().toLowerCase()
  return (q ? list.filter((e) => e.name.toLowerCase().includes(q)) : list).map((e) => e.name)
})

// The filter input only earns its space past 8 entries.
const showFilter = computed(() => (allDbs.value?.length || 0) > 8)

const countText = computed(() => {
  const total = allDbs.value?.length || 0
  return filterText.value.trim() ? `匹配 ${dbs.value?.length || 0} / 共 ${total} 库` : `共 ${total} 库`
})

// Human-readable size for the right edge: B→KB→MB→GB→TB, one decimal
// below 100; empty 库 renders as a gray 「空」 instead of a number.
function humanSize(e?: DbEntry): string {
  if (!e || e.empty) return '空'
  const b = e.size_on_disk ?? 0
  if (!b) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = b
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${i === 0 || v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

async function fetchDbs() {
  loading.value = true
  try {
    const { data } = await apiClient.browseSourceDatabases(props.payload())
    const entries = (data.entries || [])
      .filter((e) => !SYSTEM_DBS.has(e.name))
      .sort((a, b) => a.name.localeCompare(b.name))
    if (!entries.length) {
      ElMessage.info('未发现可对比的用户库（系统库已过滤）')
      allDbs.value = null
      return
    }
    allDbs.value = entries
    dbMeta.value = Object.fromEntries(entries.map((e) => [e.name, e]))
    filterText.value = ''
  } catch (e: any) {
    // Degrade to manual input (non-blocking by design).
    ElMessage.error(e.response?.data?.error || '获取数据库列表失败，可手动输入库名')
    allDbs.value = null
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
    <div v-if="dbs" data-testid="mc11-db-list" class="db-card">
      <div class="db-card-head">
        <el-tag size="small" type="info" effect="plain">{{ countText }}</el-tag>
        <el-tooltip content="重新拉取库列表" placement="top">
          <el-button
            size="small"
            text
            :icon="Refresh"
            :loading="loading"
            aria-label="重新拉取库列表"
            @click="fetchDbs"
          />
        </el-tooltip>
      </div>
      <el-input
        v-if="showFilter"
        v-model="filterText"
        size="small"
        clearable
        placeholder="过滤库名"
        data-testid="mc11-db-filter"
        class="db-filter"
      />
      <el-scrollbar max-height="220px">
        <el-radio-group
          :model-value="modelValue"
          class="db-radio-group"
          @update:model-value="emit('update:modelValue', $event)"
        >
          <el-radio v-for="d in dbs" :key="d" :value="d" class="db-row" :class="{ selected: modelValue === d }">
            <el-tooltip :content="d" placement="top" :show-after="400">
              <span class="db-name">{{ d }}</span>
            </el-tooltip>
            <span class="db-size" :class="{ 'db-size-empty': dbMeta[d]?.empty }">{{ humanSize(dbMeta[d]) }}</span>
          </el-radio>
        </el-radio-group>
      </el-scrollbar>
    </div>
  </div>
</template>

<style scoped>
.db-card {
  margin-top: 8px;
  max-width: 400px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 8px 10px;
  background: var(--el-fill-color-lighter);
}
.db-card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.db-filter {
  margin-top: 6px;
}
.db-radio-group {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-top: 6px;
}
.db-row {
  width: 100%;
  height: auto;
  margin-right: 0;
  padding: 4px 8px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  transition: border-color 0.15s, background-color 0.15s;
}
.db-row.selected {
  border-color: var(--el-color-primary);
  background: var(--el-color-primary-light-9);
}
.db-row :deep(.el-radio__label) {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  width: 100%;
  min-width: 0;
}
.db-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.db-size {
  flex-shrink: 0;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.db-size-empty {
  color: var(--el-text-color-placeholder);
}
</style>
