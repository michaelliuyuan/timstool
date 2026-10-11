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
  /** Server-derived discriminator ("compare" | "migration"); config_json never leaves the backend. */
  kind?: 'compare' | 'migration'
  phase: string
  progress: number
  tables_total: number
  tables_done: number
  rows_total: number
  rows_done: number
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
    temp_dir?: string
    tables: string[]
    exclude_tables: string[]
    use_lightning: boolean
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

// MongoDB compare task (M3 API face, snake_case per task-API convention).
// One source database vs one target database (v1 semantics); uri OR discrete
// fields per side (discrete wins server-side; passwords never round-trip the
// API — the server stores ConfigJSON with json:"-").
export interface CompareEndpoint {
  uri?: string
  host?: string
  port?: number
  user?: string
  password?: string
  auth_db?: string
  database: string
}

export interface CompareTaskSpec {
  source: CompareEndpoint
  target: CompareEndpoint
  buckets?: number
  parallel_colls?: number
  parallel_buckets?: number
  batch_size?: number
  levels?: string[]
}

// Compare result (the kind discriminator inside result_json of finished
// compare tasks).
export interface CompareResult {
  kind: 'compare'
  overall: 'equal' | 'diff' | 'error' | 'ttl_skip'
  counts: { equal: number; diff: number; error: number; ttl_skip: number }
  duration_sec: number
  summary_path: string
  diff_dir: string
}

// parseCompareResult returns the compare payload when the task is a compare
// task with a result (single JSON.parse of result_json; null otherwise).
export function parseCompareResult(task: Task | null): CompareResult | null {
  if (!task?.result_json) return null
  try {
    const r = JSON.parse(task.result_json)
    return r && r.kind === 'compare' ? (r as CompareResult) : null
  } catch {
    return null
  }
}

// isCompareTask discriminates task kinds on the wire contract: the server
// derives an explicit `kind` field on every task response (DTO), while
// config_json never leaves the backend (it holds credentials). Tasks without
// a kind (stale cached responses) fall back to migration rendering.
export function isCompareTask(task: Task | null | undefined): boolean {
  return task?.kind === 'compare'
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

  createTask: (req: CreateTaskRequest) =>
    api.post<Task>('/tasks', req),

  createCompareTask: (req: CompareTaskSpec) =>
    api.post<Task>('/compare/tasks', req),

  listTasks: () =>
    api.get<Task[]>('/tasks'),

  getTask: (id: string) =>
    api.get<Task>(`/tasks/${id}`),

  // opts.resume=true continues a compare task from engine checkpoints
  // (fresh start wipes the compare OutDir server-side). Migration tasks
  // ignore the body — safe to pass for both kinds.
  startTask: (id: string, opts?: { resume?: boolean }) =>
    api.post(`/tasks/${id}/start`, { resume: opts?.resume ?? false }),

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
