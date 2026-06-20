<script setup lang="ts">
import { computed } from 'vue'
import type { FieldSpec, SourceMeta } from '../composables/sourceTypes'

// Schema-driven connection form: renders fields from meta.fields by group.
// Nothing is hard-coded to PG — switching the source swaps `meta` and the form
// re-renders for the new fields (doc §7.1). Uses the explicit
// :model-value + @update:model-value pattern (robust with el-select; avoids the
// earlier v-model/watch pitfall).
const props = defineProps<{
  meta: SourceMeta
  model: Record<string, any>
  disabled?: boolean
}>()

const isStub = computed(() => !props.meta.implemented)
const disabledAll = (extra?: boolean) => !!extra || isStub.value

function fieldsOf(group: string): FieldSpec[] {
  return props.meta.fields.filter(f => f.group === group)
}

const groups = ['common', 'source']
</script>

<template>
  <el-alert
    v-if="isStub"
    type="info"
    :closable="false"
    :title="meta.notImplMsg || '该源暂未实现，敬请期待'"
    style="margin-bottom: 12px;"
  />
  <template v-for="g in groups" :key="g">
    <el-form-item
      v-for="f in fieldsOf(g)"
      :key="f.key"
      :label="f.label"
      :required="f.required"
    >
      <el-input
        v-if="f.type === 'text'"
        :model-value="model[f.key]"
        :placeholder="f.placeholder"
        :disabled="disabledAll(disabled)"
        @update:model-value="model[f.key] = $event"
      />
      <el-input-number
        v-else-if="f.type === 'number'"
        :model-value="model[f.key]"
        :min="1"
        :max="65535"
        :disabled="disabledAll(disabled)"
        @update:model-value="model[f.key] = $event"
      />
      <el-input
        v-else-if="f.type === 'password'"
        type="password"
        show-password
        :model-value="model[f.key]"
        :disabled="disabledAll(disabled)"
        @update:model-value="model[f.key] = $event"
      />
      <el-select
        v-else-if="f.type === 'select'"
        :model-value="model[f.key]"
        :disabled="disabledAll(disabled)"
        @update:model-value="model[f.key] = $event"
      >
        <el-option
          v-for="o in f.options || []"
          :key="o.value"
          :label="o.label"
          :value="o.value"
        />
      </el-select>
      <el-switch
        v-else-if="f.type === 'switch'"
        :model-value="!!model[f.key]"
        :disabled="disabledAll(disabled)"
        @update:model-value="model[f.key] = $event"
      />
      <el-input
        v-else
        :model-value="model[f.key]"
        :disabled="disabledAll(disabled)"
        @update:model-value="model[f.key] = $event"
      />
    </el-form-item>
  </template>
</template>
