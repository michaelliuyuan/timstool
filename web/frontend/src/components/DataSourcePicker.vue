<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useDataSources } from '../composables/useDataSources'

// F-02 DataSourcePicker: dropdown of saved datasources (server-side
// credentials) + a "手动填写" collapse entry (D5). v-model is the datasource
// id; '' means manual entry (the parent renders its usual connection form).
const props = defineProps<{
  modelValue: string
  /** Restrict the list to these datasource types (e.g. ['tidb']). */
  types?: string[]
}>()

const emit = defineEmits<{ (e: 'update:modelValue', v: string): void }>()

const { load, byType, get } = useDataSources()
onMounted(() => { load() })

const options = computed(() => byType(props.types))
const isManual = computed(() => props.modelValue === '')
const selectedName = computed(() => get(props.modelValue)?.name || '')

const typeLabels: Record<string, string> = { postgres: 'PostgreSQL', mysql: 'MySQL', tidb: 'TiDB' }

function onSelect(val: string | undefined) {
  // '' option = "手动填写"
  emit('update:modelValue', val || '')
}
</script>

<template>
  <div class="ds-picker">
    <el-select
      :model-value="modelValue"
      :placeholder="options.length === 0 ? '暂无数据源，手动填写或前往「数据源管理」新建' : '选择已保存的数据源'"
      style="width: 100%"
      @change="onSelect"
    >
      <el-option value="" label="手动填写" />
      <el-option v-for="d in options" :key="d.id" :value="d.id" :label="`${d.name}（${typeLabels[d.type] || d.type}）`">
        <span>{{ d.name }}</span>
        <span class="ds-picker-meta">
          {{ typeLabels[d.type] || d.type }}
          <template v-if="d.fields?.host"> · {{ d.fields.host }}<template v-if="d.fields.port">:{{ d.fields.port }}</template><template v-if="d.fields.database">/{{ d.fields.database }}</template></template>
        </span>
      </el-option>
    </el-select>
    <div class="ds-picker-hint">
      <template v-if="!isManual">已选用数据源「{{ selectedName }}」，凭据由服务端保管，本页无需输入密码</template>
      <template v-else>选择上方数据源可免填连接信息；密码只保存在服务端（数据源管理页维护）</template>
    </div>
  </div>
</template>

<style scoped>
.ds-picker {
  width: 100%;
}
.ds-picker-meta {
  float: right;
  color: var(--tims-text-2, #909399);
  font-size: 12px;
}
.ds-picker-hint {
  color: var(--tims-text-2, #909399);
  font-size: 12px;
  line-height: 1.4;
  margin-top: 4px;
}
</style>
