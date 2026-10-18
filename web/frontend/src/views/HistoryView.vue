<script setup lang="ts">
import { ref, computed, watch, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import apiClient from '../api'
import type { Task } from '../api'
import PageHeader from '../components/PageHeader.vue'

const router = useRouter()
const tasks = ref<Task[]>([])
const loading = ref(true)

const statusMap: Record<string, { type: string; label: string }> = {
  completed: { type: 'success', label: '已完成' },
  failed: { type: 'danger', label: '失败' },
  cancelled: { type: 'info', label: '已取消' },
}

async function fetchHistory() {
  try {
    const { data } = await apiClient.listTasks()
    tasks.value = (data || []).filter((t: Task) =>
      ['completed', 'failed', 'cancelled'].includes(t.status)
    )
  } catch {
    ElMessage.error('获取历史记录失败')
  } finally {
    loading.value = false
  }
}

// #9 大表 client-side 分页：数据仍全量加载，仅前端切片渲染。
const page = ref(1)
const pageSize = ref(20)
const pagedTasks = computed(() => {
  const start = (page.value - 1) * pageSize.value
  return tasks.value.slice(start, start + pageSize.value)
})
// 删除/刷新后行数收缩时钳制当前页，避免落在空页。
watch(() => tasks.value.length, (len) => {
  const maxPage = Math.max(1, Math.ceil(len / pageSize.value))
  if (page.value > maxPage) page.value = maxPage
})

async function deleteTask(id: string) {
  try {
    await ElMessageBox.confirm('确认删除该任务？删除后不可恢复。', '确认', { type: 'warning' })
  } catch { return }
  try {
    await apiClient.deleteTask(id)
    ElMessage.success('已删除')
    await fetchHistory()
  } catch (e: any) {
    ElMessage.error(e.response?.data?.error || '删除失败')
  }
}

// MS-11i pen3: relative time for the narrow history cells; the full timestamp
// rides the el-tooltip (display-only, no data change).
function fmtRelative(iso?: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return '-'
  const min = Math.floor((Date.now() - d.getTime()) / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr} 小时前`
  const day = Math.floor(hr / 24)
  if (day < 30) return `${day} 天前`
  return d.toLocaleDateString()
}

function fmtFull(iso?: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return isNaN(d.getTime()) ? '-' : d.toLocaleString()
}

onMounted(fetchHistory)
</script>

<template>
  <div class="tims-page">
    <PageHeader title="迁移历史" />
    <el-card v-loading="loading">
      <div v-if="tasks.length > 0" class="tims-table-scroll">
        <el-table :data="pagedTasks" style="width: 100%;">
          <el-table-column prop="name" label="任务名称" min-width="200" />
          <el-table-column label="状态" width="120">
            <template #default="{ row }">
              <el-tag :type="(statusMap[row.status]?.type || 'info') as any">
                {{ statusMap[row.status]?.label || row.status }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="表" width="100">
            <template #default="{ row }">{{ row.tables_done }}/{{ row.tables_total }}</template>
          </el-table-column>
          <el-table-column label="行数" width="140" class-name="hide-sm">
            <template #default="{ row }">{{ row.rows_done?.toLocaleString() || 0 }}</template>
          </el-table-column>
          <el-table-column label="创建时间" width="120" class-name="hide-sm">
            <template #default="{ row }">
              <el-tooltip :content="fmtFull(row.created_at)" placement="top"><span>{{ fmtRelative(row.created_at) }}</span></el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="结束时间" width="120">
            <template #default="{ row }">
              <el-tooltip v-if="row.finished_at" :content="fmtFull(row.finished_at)" placement="top"><span>{{ fmtRelative(row.finished_at) }}</span></el-tooltip>
              <span v-else>-</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="140">
            <template #default="{ row }">
              <!-- MS-11i pen1: link-style row actions, unified with CompareView 比对历史. -->
              <el-button size="small" link type="primary" @click="router.push(`/tasks/${row.id}`)">详情</el-button>
              <el-button size="small" link type="danger" :disabled="row.status === 'running'" @click="deleteTask(row.id)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <!-- #9 client-side 分页（总数展示；pageSize 可选 10/20/50） -->
      <el-pagination
        v-if="tasks.length > 0"
        v-model:current-page="page"
        v-model:page-size="pageSize"
        :page-sizes="[10, 20, 50]"
        :total="tasks.length"
        layout="total, sizes, prev, pager, next"
        style="margin-top: 12px; justify-content: flex-end;"
      />

      <el-empty v-if="!loading && tasks.length === 0" description="暂无历史记录" />
    </el-card>
  </div>
</template>
