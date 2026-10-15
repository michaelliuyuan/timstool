<script setup lang="ts">
import { ref, reactive, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { ArrowLeft } from '@element-plus/icons-vue'
import apiClient from '../api'
import type { CompareTaskSpec } from '../api'
import PageHeader from '../components/PageHeader.vue'
import ConnectionForm from '../components/ConnectionForm.vue'
import DataSourcePicker from '../components/DataSourcePicker.vue'
import DbBrowseSelect from '../components/DbBrowseSelect.vue'
import { useSourceSchema } from '../composables/useSourceSchema'
import { promptForToken } from '../composables/useAuthTokenGate'

// MongoDB compare task creation (M4): schema-driven forms for both sides
// (the /sources metadata is the single source of truth — no hard-coded
// fields), database names, and an advanced collapse for engine knobs.
// Submit: create → start (clean run) → task detail.
// M6: each side may reference a saved mongodb datasource instead of a
// manual form (server-side credential snapshot; explicit manual fields
// override the stored profile server-side).
const router = useRouter()
const { load, getSource } = useSourceSchema()

const mongoMeta = ref<ReturnType<typeof getSource>>()
const creating = ref(false)
const advancedOpen = ref(false)

const srcModel = reactive<Record<string, any>>({ host: 'localhost', port: 27017 })
const tgtModel = reactive<Record<string, any>>({ host: 'localhost', port: 27017 })
const srcRef = ref('')
const tgtRef = ref('')
const srcDb = ref('')
const tgtDb = ref('')
const tgtSameAsSrc = ref(true)

const adv = reactive({ buckets: 64, parallel_colls: 4, parallel_buckets: 4, batch_size: 1000 })
const levels = ref<string[]>(['schema', 'index', 'count', 'hash', 'diff'])
const levelOptions = [
  { label: '集合存在性 (schema)', value: 'schema' },
  { label: '索引 (index)', value: 'index' },
  { label: '文档数 (count)', value: 'count' },
  { label: '分桶哈希 (hash)', value: 'hash' },
  { label: '差异下钻 (diff)', value: 'diff' },
]

const testing = ref({ src: false, tgt: false })
const testState = ref<{ src?: boolean; tgt?: boolean }>({})

onMounted(async () => {
  await load()
  mongoMeta.value = getSource('mongodb')
  if (!mongoMeta.value?.implemented) {
    ElMessage.error('MongoDB 源不可用（后端未注册）')
  }
})

async function testSide(side: 'src' | 'tgt') {
  testing.value[side] = true
  testState.value[side] = undefined
  try {
    // Referenced datasource: credentials live server-side — test through
    // the datasource test endpoint, not the inline field form.
    const refId = side === 'src' ? srcRef.value : tgtRef.value
    if (refId) {
      const { data } = await apiClient.testDataSource(refId)
      testState.value[side] = !!data.success
      if (!data.success) ElMessage.warning(`连接失败: ${data.message || '未知错误'}`)
    } else {
      const model = side === 'src' ? srcModel : tgtModel
      const { data } = await apiClient.testSourceConnection('mongodb', { ...model })
      testState.value[side] = !!data.success
      if (!data.success) ElMessage.warning(`连接失败: ${data.message || '未知错误'}`)
    }
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '连接测试失败')
  } finally {
    testing.value[side] = false
  }
}

function endpointOf(side: 'src' | 'tgt'): CompareTaskSpec['source'] {
  // Datasource reference: only the database name is needed here — the
  // server folds host/port/user/password/authDb/tls from the stored
  // profile into a creation-time snapshot.
  const refId = side === 'src' ? srcRef.value : tgtRef.value
  if (refId) return { datasource_id: refId, database: '' }
  const model = side === 'src' ? srcModel : tgtModel
  const ep: CompareTaskSpec['source'] = { database: '' }
  if (model.host) ep.host = model.host
  if (model.port) ep.port = Number(model.port)
  if (model.user) ep.user = model.user
  if (model.password) ep.password = model.password
  // the /sources field key is authDb; the compare DTO key is auth_db
  if (model.authDb) ep.auth_db = model.authDb
  // the meta form renders tls as a switch (boolean)
  if (model.tls) ep.tls = true
  return ep
}

// MC-10 C5: create + start sit behind the mongocompare token gate; a 401
// opens the shared token prompt exactly once, then retries the flow.
async function submit(retriedAuth = false) {
  if (!srcDb.value) {
    ElMessage.warning('请填写源端数据库名')
    return
  }
  const targetDb = tgtSameAsSrc.value ? srcDb.value : tgtDb.value
  if (!targetDb) {
    ElMessage.warning('请填写目标端数据库名')
    return
  }
  const spec: CompareTaskSpec = {
    source: { ...endpointOf('src'), database: srcDb.value },
    target: { ...endpointOf('tgt'), database: targetDb },
    buckets: adv.buckets,
    parallel_colls: adv.parallel_colls,
    parallel_buckets: adv.parallel_buckets,
    batch_size: adv.batch_size,
    levels: [...levels.value],
  }
  creating.value = true
  try {
    const { data } = await apiClient.createCompareTask(spec)
    await apiClient.startTask(data.id)
    ElMessage.success('对比任务已创建并启动')
    router.push(`/tasks/${data.id}`)
  } catch (e: any) {
    if (e.response?.status === 401 && !retriedAuth && await promptForToken()) {
      return submit(true)
    }
    ElMessage.error(e.response?.data?.error || '创建对比任务失败')
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <div class="tims-page">
    <PageHeader
      title="新建 MongoDB 对比任务"
      subtitle="配置两端 MongoDB 连接与对比级别，生成对比任务"
    >
      <template #actions>
        <el-button @click="router.push('/tasks')">
          <el-icon style="margin-right: 4px;"><ArrowLeft /></el-icon>返回任务列表
        </el-button>
      </template>
    </PageHeader>

    <el-alert
      type="info"
      :closable="false"
      style="margin-bottom: 20px;"
      title="对比源端与目标端的同一数据库（v1：单库对单库，两端库名可不同）"
      description="任务启动后按集合分桶哈希对比，失配桶自动下钻到字段级差异；结果包含 summary.json 与逐集合差异明细。"
    />

    <el-card v-if="mongoMeta?.implemented" style="margin-bottom: 20px;">
      <template #header>源端（被对比方）</template>
      <el-form label-width="160px">
        <el-form-item label="连接方式">
          <DataSourcePicker v-model="srcRef" :types="['mongodb']" />
        </el-form-item>
        <template v-if="!srcRef">
          <ConnectionForm :meta="mongoMeta" :model="srcModel" />
        </template>
        <el-form-item label="数据库名" required>
          <!-- MC-11: browse+single-select fill, dual-track with manual entry.
               Referenced datasource → datasource_id fold server-side; manual
               fields ride the plain {source, fields} shape. -->
          <DbBrowseSelect
            v-model="srcDb"
            :payload="() => (srcRef ? { datasource_id: srcRef } : { fields: { ...srcModel } })"
          />
        </el-form-item>
        <el-form-item>
          <el-button :loading="testing.src" @click="testSide('src')">
            测试源端连接
          </el-button>
          <el-tag v-if="testState.src === true" type="success" style="margin-left: 8px;">连接成功</el-tag>
          <el-tag v-else-if="testState.src === false" type="danger" style="margin-left: 8px;">连接失败</el-tag>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card v-if="mongoMeta?.implemented" style="margin-bottom: 20px;">
      <template #header>目标端（对比方）</template>
      <el-form label-width="160px">
        <el-form-item label="连接方式">
          <DataSourcePicker v-model="tgtRef" :types="['mongodb']" />
        </el-form-item>
        <template v-if="!tgtRef">
          <ConnectionForm :meta="mongoMeta" :model="tgtModel" />
        </template>
        <el-form-item>
          <el-checkbox v-model="tgtSameAsSrc">目标数据库名与源端相同</el-checkbox>
        </el-form-item>
        <el-form-item v-if="!tgtSameAsSrc" label="目标数据库名" required>
          <!-- MC-11 symmetric browse on the target side (only when the name
               differs from the source). -->
          <DbBrowseSelect
            v-model="tgtDb"
            :payload="() => (tgtRef ? { datasource_id: tgtRef } : { fields: { ...tgtModel } })"
          />
        </el-form-item>
        <el-form-item>
          <el-button :loading="testing.tgt" @click="testSide('tgt')">
            测试目标端连接
          </el-button>
          <el-tag v-if="testState.tgt === true" type="success" style="margin-left: 8px;">连接成功</el-tag>
          <el-tag v-else-if="testState.tgt === false" type="danger" style="margin-left: 8px;">连接失败</el-tag>
        </el-form-item>
      </el-form>
    </el-card>

    <el-card v-if="mongoMeta?.implemented" style="margin-bottom: 20px;">
      <template #header>
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <span>高级选项</span>
          <el-switch v-model="advancedOpen" active-text="展开" />
        </div>
      </template>
      <el-form v-if="advancedOpen" label-width="160px">
        <el-form-item label="分桶数 (buckets)">
          <el-input-number v-model="adv.buckets" :min="1" :max="4096" />
          <span class="adv-hint">_id 范围切分的桶数，默认 64</span>
        </el-form-item>
        <el-form-item label="集合并行 (parallel_colls)">
          <el-input-number v-model="adv.parallel_colls" :min="1" :max="32" />
        </el-form-item>
        <el-form-item label="桶并行 (parallel_buckets)">
          <el-input-number v-model="adv.parallel_buckets" :min="1" :max="32" />
        </el-form-item>
        <el-form-item label="批大小 (batch_size)">
          <el-input-number v-model="adv.batch_size" :min="100" :max="100000" :step="100" />
        </el-form-item>
        <el-form-item label="对比级别 (levels)">
          <el-select v-model="levels" multiple style="width: 100%;">
            <el-option v-for="o in levelOptions" :key="o.value" :label="o.label" :value="o.value" />
          </el-select>
          <span class="adv-hint">留空 = 全部级别；hash 依赖前置级别，建议保持默认</span>
        </el-form-item>
      </el-form>
      <div v-else style="color: #909399; font-size: 13px;">
        使用引擎默认参数（64 桶 / 4+4 并行 / 批 1000 / 全部级别）
      </div>
    </el-card>

    <div style="display: flex; justify-content: flex-end; gap: 12px;">
      <el-button @click="router.push('/tasks')">取消</el-button>
      <el-button type="primary" :loading="creating" @click="submit">创建并启动对比</el-button>
    </div>
  </div>
</template>

<style scoped>
.adv-hint {
  margin-left: 12px;
  color: #909399;
  font-size: 12px;
}
</style>
