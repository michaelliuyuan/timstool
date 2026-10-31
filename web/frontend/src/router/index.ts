import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/',
      name: 'Dashboard',
      component: () => import('../views/DashboardView.vue'),
      meta: { title: '工作台' },
    },
    {
      path: '/pipeline',
      name: 'Pipeline',
      component: () => import('../views/PipelineView.vue'),
      meta: { title: '管线拓扑' },
    },
    {
      path: '/wizard',
      name: 'Wizard',
      component: () => import('../views/WizardView.vue'),
      meta: { title: '新建迁移' },
    },
    {
      path: '/mongocompare/new',
      name: 'CompareCreate',
      component: () => import('../views/CompareCreateView.vue'),
      meta: { title: 'MongoDB 对比' },
    },
    {
      path: '/tasks',
      name: 'TaskList',
      component: () => import('../views/TaskListView.vue'),
      meta: { title: '任务监控' },
    },
    {
      path: '/tasks/:id',
      name: 'TaskDetail',
      component: () => import('../views/TaskDetailView.vue'),
      meta: { title: '任务详情' },
    },
    {
      path: '/history',
      name: 'History',
      component: () => import('../views/HistoryView.vue'),
      meta: { title: '迁移历史' },
    },
    {
      path: '/datasources',
      name: 'DataSources',
      component: () => import('../views/DataSourcesView.vue'),
      meta: { title: '数据源' },
    },
    {
      path: '/compare',
      name: 'Compare',
      component: () => import('../views/CompareView.vue'),
      meta: { title: '数据比对' },
    },
    {
      path: '/assess',
      name: 'Assess',
      component: () => import('../views/AssessView.vue'),
      meta: { title: '兼容评估' },
    },
    {
      path: '/ddl-export',
      name: 'DDLExport',
      component: () => import('../views/DDLExportView.vue'),
      meta: { title: 'DDL 导出' },
    },
    {
      path: '/ddl-compare',
      name: 'DDLCompare',
      component: () => import('../views/DdlCompareView.vue'),
      meta: { title: 'DDL 对比' },
    },
    {
      path: '/incremental',
      name: 'Incremental',
      component: () => import('../views/IncrementalView.vue'),
      meta: { title: '时间戳水位补齐' },
    },
    {
      path: '/cdc',
      name: 'CDC',
      component: () => import('../views/CDCView.vue'),
      meta: { title: 'CDC 实时同步' },
    },
    {
      path: '/:pathMatch(.*)*',
      redirect: '/',
    },
  ],
})

// #12 route-scoped document title: "<page> - TiMS"; untitled routes fall back to "TiMS".
router.afterEach((to) => {
  const t = to.meta.title as string | undefined
  document.title = t ? `${t} - TiMS` : 'TiMS'
})

export default router
