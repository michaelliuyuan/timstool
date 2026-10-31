<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import apiClient from '../api'
import type { Task } from '../api'
import { isCompareTask } from '../api'
import PageHeader from '../components/PageHeader.vue'

const router = useRouter()
const tasks = ref<Task[]>([])
const loading = ref(true)

const statusMap: Record<string, { type: string; label: string }> = {
  created: { type: 'info', label: '已创建' },
  running: { type: 'warning', label: '运行中' },
  paused: { type: '', label: '已暂停' },
  completed: { type: 'success', label: '已完成' },
  failed: { type: 'danger', label: '失败' },
  cancelled: { type: 'info', label: '已取消' },
}

// Poll toast throttle (S1-UI-08): routine 5s polls must not re-toast the
// same failure — only the first failure (or a state change back to OK) flips
// the flag so the next failure toasts once again.
let lastFetchFailed = false

async function fetchTasks() {
  try {
    const { data } = await apiClient.listTasks()
    tasks.value = data || []
    lastFetchFailed = false
  } catch {
    if (!lastFetchFailed) ElMessage.error('获取任务列表失败')
    lastFetchFailed = true
  } finally {
    loading.value = false
  }
}

function goToTask(id: string) {
  router.push(`/tasks/${id}`)
}

// #9 大表 client-side 分页：数据仍全量加载（5s 轮询/行点击语义不变），
// 仅前端切片渲染。默认 20 行/页，可选 10/20/50，总数展示。
const page = ref(1)
const pageSize = ref(20)
const pagedTasks = computed(() => {
  const start = (page.value - 1) * pageSize.value
  return tasks.value.slice(start, start + pageSize.value)
})
// 5s 轮询刷新后行数可能收缩（任务转历史）：钳制当前页，避免落在空页。
watch(() => tasks.value.length, (len) => {
  const maxPage = Math.max(1, Math.ceil(len / pageSize.value))
  if (page.value > maxPage) page.value = maxPage
})

let timer: any = null
onMounted(() => {
  fetchTasks()
  timer = setInterval(fetchTasks, 5000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <div class="tims-page">
    <PageHeader title="任务监控">
      <template #actions>
        <el-button type="primary" @click="router.push('/wizard')">
          <el-icon><Plus /></el-icon> 新建迁移
        </el-button>
        <el-button type="success" plain @click="router.push('/mongocompare/new')">
          <el-icon><Switch /></el-icon> MongoDB 对比
        </el-button>
      </template>
    </PageHeader>

    <el-card v-loading="loading">
      <!-- MS-11i pen1: table hidden entirely when empty — the el-empty below is
           the single empty face (previously the built-in "暂无数据" row and the
           el-empty + CTA stacked as two empty messages). -->
      <div v-if="tasks.length > 0" class="tims-table-scroll">
        <el-table stripe :data="pagedTasks" style="width: 100%;" @row-click="(row: Task) => goToTask(row.id)">
          <el-table-column prop="name" label="任务名称" min-width="180">
            <template #default="{ row }">
              <span>{{ row.name }}</span>
              <el-tag v-if="isCompareTask(row)" size="small" type="warning" style="margin-left: 6px;">对比</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="120">
            <template #default="{ row }">
              <el-tag :type="(statusMap[row.status]?.type || 'info') as any">
                {{ statusMap[row.status]?.label || row.status }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="阶段" width="120">
            <template #default="{ row }">{{ row.phase || '-' }}</template>
          </el-table-column>
          <el-table-column label="进度" width="200">
            <template #default="{ row }">
              <el-progress v-if="isCompareTask(row) && row.status === 'running'" :percentage="0" :indeterminate="true" :stroke-width="14" />
              <el-progress v-else :percentage="Math.round(row.progress * 100)" :stroke-width="14"
                :status="row.status === 'completed' ? 'success' : row.status === 'failed' ? 'exception' : undefined" />
            </template>
          </el-table-column>
          <el-table-column label="表进度" width="120" class-name="hide-sm">
            <template #default="{ row }">{{ isCompareTask(row) ? '-' : `${row.tables_done}/${row.tables_total}` }}</template>
          </el-table-column>
          <el-table-column label="行数" width="140" class-name="hide-sm">
            <template #default="{ row }">{{ isCompareTask(row) ? '-' : (row.rows_done?.toLocaleString() || 0) }}</template>
          </el-table-column>
          <el-table-column label="创建时间" width="180" class-name="hide-sm">
            <template #default="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
          </el-table-column>
        </el-table>
      </div>
      <!-- #9 client-side 分页（layout 含 total 总数展示；pageSize 可选 10/20/50） -->
      <el-pagination
        v-if="tasks.length > 0"
        v-model:current-page="page"
        v-model:page-size="pageSize"
        :page-sizes="[10, 20, 50]"
        :total="tasks.length"
        layout="total, sizes, prev, pager, next"
        style="margin-top: 12px; justify-content: flex-end;"
      />

      <el-empty v-if="!loading && tasks.length === 0" description="暂无任务">
        <el-button type="primary" @click="router.push('/wizard')">创建第一个迁移任务</el-button>
      </el-empty>
    </el-card>
  </div>
</template>
