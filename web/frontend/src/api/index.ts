import axios from 'axios'
import type { SourceMeta } from '../composables/sourceTypes'

const api = axios.create({
  baseURL: '/api/v1',
  timeout: 30000,
})

export interface Task {
  id: string
  name: string
  status: 'created' | 'running' | 'paused' | 'completed' | 'failed' | 'cancelled'
  config_json: string
  phase: string
  progress: number
  tables_total: number
  tables_done: number
  rows_total: number
  rows_done: number
  imported_tables?: number
  error: string
  result_json: string
  created_at: string
  started_at: string | null
  finished_at: string | null
  updated_at: string
}

export interface PhaseTableInfo {
  name: string
  state: string
  rows_done: number
  rows_total: number
}

export interface PhaseInfo {
  name: string
  label: string
  status: string
  sub_label?: string
  tables: PhaseTableInfo[]
  table_count: number
  tables_done: number
  rows_total: number
  rows_done: number
  logs?: { level: string; message: string; timestamp: string }[]
}

export interface TaskPhasesResponse {
  task_id: string
  phase: string
  phases: PhaseInfo[]
}

export interface TaskLogEntry {
  timestamp: string
  level: string
  message: string
  caller?: string
}

export interface TaskLogsResponse {
  task_id: string
  logs: TaskLogEntry[]
  count: number
}

export interface ConnectionTestRequest {
  type: 'source' | 'target'
  host: string
  port: number
  user: string
  password: string
  database: string
  schema?: string
  sslmode?: string
  /** Target-only extras; empty/0 = probe skipped (non-Lightning flow). */
  pd_addr?: string
  status_port?: number
}

export interface ConnectionTestResult {
  ok: boolean
  type: string
  host: string
  port: number
  database: string
  version?: string
  error?: string
  elapsed: string
  /** PD/Status probe outcomes (present only when the fields were provided). */
  mysql_ok?: boolean
  pd_ok?: boolean
  pd_error?: string
  pd_cluster_id?: string
  status_ok?: boolean
  status_error?: string
}

export interface CreateTaskRequest {
  name: string
  source: Record<string, any>
  target: {
    host: string
    port: number
    user: string
    password: string
    database: string
    pd_addr: string
    status_port: number
  }
  opts: {
    parallel: number
    batch_size: number
    temp_dir: string
    tables: string[]
    exclude_tables: string[]
    use_lightning: boolean
    lightning_path: string
    skip_precheck: boolean
    skip_schema: boolean
    skip_data: boolean
    skip_validate: boolean
    target_policy: string
    compare_mode: string
    sample_ratio: number
    checksum_chunk_size: number
    checksum_parallel: number
  }
}

export const apiClient = {
  health: () => api.get('/health'),

  getSources: () => api.get<{ sources: SourceMeta[] }>('/sources'),

  // Multi-source table listing via the adapter's SchemaReader (#t79). PG keeps
  // its dedicated /config/list-tables (reltuples estimates); non-PG uses this.
  getSourceTables: (source: string, fields: Record<string, any>) =>
    api.post<{ tables: { name: string; row_estimate: number }[]; count: number }>(
      '/sources/tables',
      { source, fields },
    ),

  // Multi-source connection test (doc §6.2): {source, fields} → {success,...}.
  // source defaults to postgres server-side (backward compat).
  testSourceConnection: (source: string, fields: Record<string, any>) =>
    api.post<{ source: string; success: boolean; message: string; version?: string }>(
      '/test-connection',
      { source, fields },
    ),

  testConnection: (req: ConnectionTestRequest) =>
    api.post<ConnectionTestResult>('/config/test-connection', req),

  listTables: (req: ConnectionTestRequest) =>
    api.post<{ tables: { name: string; row_estimate: number }[]; count: number }>('/config/list-tables', { ...req, type: 'source' }),

  // Lightning path gate for the wizard (doc: 迁移选项页门禁). Empty path probes
  // auto-discovery server-side; non-empty must exist and (on Linux) be executable.
  validateLightning: (path: string) =>
    api.post<{ success: boolean; message: string; resolved_path: string }>('/validate-lightning', { path }),

  // Migration options persistence (server-side memory of temp_dir / lightning
  // settings + the Lightning-only target extras pd_addr / status_port).
  // GET prefill on entering the options/target steps, PUT on advancing or a
  // successful target connection test. pd_addr empty and status_port 0 mean
  // "not remembered" and never overwrite the form defaults.
  getMigrationOptions: () =>
    api.get<{ temp_dir: string; use_lightning: boolean; lightning_path: string; pd_addr?: string; status_port?: number }>('/migration-options'),

  saveMigrationOptions: (opts: { temp_dir: string; use_lightning: boolean; lightning_path: string; pd_addr?: string; status_port?: number }) =>
    api.put<{ success: boolean }>('/migration-options', opts),

  createTask: (req: CreateTaskRequest) =>
    api.post<Task>('/tasks', req),

  listTasks: () =>
    api.get<Task[]>('/tasks'),

  getTask: (id: string) =>
    api.get<Task>(`/tasks/${id}`),

  startTask: (id: string) =>
    api.post(`/tasks/${id}/start`),

  pauseTask: (id: string) =>
    api.post(`/tasks/${id}/pause`),

  resumeTask: (id: string) =>
    api.post(`/tasks/${id}/resume`),

  cancelTask: (id: string) =>
    api.post(`/tasks/${id}/cancel`),

  deleteTask: (id: string) =>
    api.delete(`/tasks/${id}`),

  getTaskProgress: (id: string) =>
    api.get<Task>(`/tasks/${id}/progress`),

  getTaskReport: (id: string, format?: string) =>
    api.get(`/tasks/${id}/report`, { params: { format }, responseType: format === 'json' ? 'json' : 'text' }),

  getTaskLogs: (id: string) =>
    api.get<TaskLogsResponse>(`/tasks/${id}/logs`),

  getTaskPhases: (id: string) =>
    api.get<TaskPhasesResponse>(`/tasks/${id}/phases`),
}

export default apiClient
