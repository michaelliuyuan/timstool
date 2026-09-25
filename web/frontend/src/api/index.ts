import axios from 'axios'
import type { SourceMeta } from '../composables/sourceTypes'

const api = axios.create({
  baseURL: '/api/v1',
  timeout: 30000,
})

// Long-running endpoints (assess / ddl-export / cdc precheck / replica
// identity ALTERs) scan whole databases server-side; they override the
// global 30s timeout per request so axios never cuts them off mid-run.
// Three-layer alignment (F-10): server chi group Timeout = http.Server
// WriteTimeout = this client timeout, all 120s — a larger value here would
// just wait on a connection the server has already cut.
const LONG_TIMEOUT = 120000

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
  imported_tables: number
  rows_total: number
  rows_done: number
  logs?: { level: string; message: string; timestamp: string }[]
}

// Timestamp-watermark incremental sync (F-04).
export interface IncrementalTableConfig {
  table: string
  watermark_column: string
  initial_watermark?: string
}

export interface IncrementalTableState {
  last_watermark?: string
  last_sync_at?: string
  total_rows: number
  failed?: string
}

export interface IncrementalTableResult {
  table: string
  from_watermark: string
  to_watermark: string
  rows: number
  error?: string
}

export interface IncrementalRunRecord {
  run_id: string
  started_at: string
  duration_ms: number
  tables: IncrementalTableResult[]
}

export interface IncrementalJob {
  id: string
  name: string
  source_ref: string
  target_ref: string
  batch_size: number
  strict_mode: boolean
  conflict_strategy: 'replace' | 'ignore' | 'error'
  tables: IncrementalTableConfig[]
  states: Record<string, IncrementalTableState>
  history?: IncrementalRunRecord[]
  created_at: string
  updated_at: string
}

export interface IncrementalJobRequest {
  name: string
  source_ref: string
  target_ref: string
  batch_size: number
  strict_mode: boolean
  conflict_strategy: 'replace' | 'ignore' | 'error'
  tables: IncrementalTableConfig[]
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
  /** F-02 datasource refs: the server resolves credentials server-side and
   * snapshots them into the task; inline fields are omitted when set. */
  source_ref?: string
  target_ref?: string
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
    cdc_chain: boolean
    target_policy: string
    compare_mode: string
    sample_ratio: number
    checksum_chunk_size: number
    checksum_parallel: number
  }
}

export interface CompareTableReport {
  table_name: string
  status: 'pass' | 'fail' | 'warn' | 'skip'
  duration?: string
  source_rows?: number
  target_rows?: number
  diff_rows?: number
  error?: string
  suggestion?: string
}

export interface CompareReport {
  overall_status: string
  start_time: string
  end_time: string
  duration: string
  summary?: string
  tables: CompareTableReport[]
  stats: {
    total_tables: number
    pass_tables: number
    fail_tables: number
    warn_tables: number
    skip_tables: number
    total_source_rows: number
    total_target_rows: number
    total_diff_rows: number
  }
}

export interface CompareTask {
  id: string
  name: string
  status: 'running' | 'completed' | 'failed' | 'cancelled'
  source: Record<string, any>
  target: Record<string, any>
  mode: string
  sample_ratio: number
  checksum_chunk_size: number
  checksum_parallel: number
  parallel: number
  tables: string[]
  tables_done: number
  tables_total: number
  current_table?: string
  error?: string
  created_at: string
  started_at?: string
  finished_at?: string
}

export interface CreateCompareRequest {
  name: string
  source: Record<string, any>
  target: Record<string, any>
  /** F-02 datasource refs (same semantics as createTask). */
  source_ref?: string
  target_ref?: string
  mode: string
  sample_ratio: number
  checksum_chunk_size: number
  checksum_parallel: number
  parallel: number
  tables: string[]
}

// F-02 unified datasource registry. Passwords are write-only: they are sent
// on create/update, never returned (has_password reports presence instead).
export interface DataSource {
  id: string
  name: string
  type: 'postgres' | 'mysql' | 'tidb'
  fields: Record<string, any>
  has_password: boolean
  created_at: string
  updated_at: string
  last_tested?: string
  last_test_ok: boolean
}

export interface DataSourceTestResult {
  source: string
  success: boolean
  message: string
  version?: string
  datasource?: DataSource
}

// Saved compare-page connection profile. Passwords round-trip as empty:
// the server never stores or returns them.
export interface CompareOptions {
  source_type?: string
  source?: Record<string, any>
  target?: Record<string, any>
  mode?: string
  sample_ratio?: number
  checksum_chunk_size?: number
  checksum_parallel?: number
  parallel?: number
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

  // Same endpoint, datasource-ref flavour (server-side credentials).
  testSourceConnectionRef: (sourceRef: string) =>
    api.post<{ source: string; success: boolean; message: string; version?: string }>(
      '/test-connection',
      { source_ref: sourceRef },
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

  // Standalone comparison (独立数据比对): create-and-run, list/history, report.
  createCompare: (req: CreateCompareRequest) =>
    api.post<CompareTask>('/compare/tasks', req),

  listCompares: () =>
    api.get<CompareTask[]>('/compare/tasks'),

  getCompare: (id: string) =>
    api.get<CompareTask>(`/compare/tasks/${id}`),

  getCompareReport: (id: string) =>
    api.get<CompareReport>(`/compare/tasks/${id}/report`),

  cancelCompare: (id: string) =>
    api.post(`/compare/tasks/${id}/cancel`),

  deleteCompare: (id: string) =>
    api.delete(`/compare/tasks/${id}`),

  // Saved compare-page connection profile (passwords are never persisted
  // server-side; the fields exist so a round-trip response can be typed).
  getCompareOptions: () =>
    api.get<CompareOptions>('/compare/options'),

  saveCompareOptions: (opts: CompareOptions) =>
    api.put<{ success: boolean }>('/compare/options', opts),

  // Unified datasource registry (F-02): server-side named connection
  // profiles. Passwords never round-trip (write-only; empty on update keeps
  // the stored one).
  listDataSources: () =>
    api.get<{ datasources: DataSource[] }>('/datasources'),

  createDataSource: (body: { name: string; type: string; fields: Record<string, any> }) =>
    api.post<DataSource>('/datasources', body),

  updateDataSource: (id: string, body: { name?: string; fields?: Record<string, any> }) =>
    api.put<DataSource>(`/datasources/${id}`, body),

  deleteDataSource: (id: string) =>
    api.delete<{ success: boolean }>(`/datasources/${id}`),

  testDataSource: (id: string) =>
    api.post<DataSourceTestResult>(`/datasources/${id}/test`),

  // Table listing by datasource ref (passwords stay server-side). PG-only
  // endpoint (reltuples estimates); non-PG refs must use getRefTablesMulti.
  getRefTables: (ref: string) =>
    api.post<{ tables: { name: string; row_estimate: number }[]; count: number }>(
      '/config/list-tables',
      { type: 'source', source_ref: ref },
    ),

  // Table listing by datasource ref via the multi-source adapter path
  // (/sources/tables resolves source_ref server-side, supports mysql).
  getRefTablesMulti: (ref: string) =>
    api.post<{ tables: { name: string; row_estimate: number }[]; count: number }>(
      '/sources/tables',
      { source_ref: ref },
    ),

  // CDC: write a postgres datasource (source) + tidb datasource (target)
  // into config.yaml in one click (D4; supervisor run chain untouched).
  importCDCFromDataSource: (sourceRef: string, targetRef: string) =>
    api.post<{ ok: boolean; message: string }>('/cdc/config/import-from-datasource', {
      source_ref: sourceRef,
      target_ref: targetRef,
    }),

  // No-PK table assist (P1): execute ALTER TABLE ... REPLICA IDENTITY FULL on
  // the source for the given "schema.table" refs. Per-table results include
  // the SQL so the UI can fall back to copy-for-manual-execution.
  runReplicaIdentity: (tables: string[]) =>
    api.post<{
      ok: boolean
      results: { table: string; sql: string; ok: boolean; error?: string; can_alter: boolean }[]
      hint: string
    }>('/cdc/replica-identity', { confirm: 'ALTER', tables }, { timeout: LONG_TIMEOUT }),

  // Timestamp-watermark incremental sync (F-04): manual-trigger jobs, PG
  // sources only; passwords stay server-side behind datasource refs.
  listIncrementalJobs: () =>
    api.get<IncrementalJob[]>('/incremental/jobs'),

  createIncrementalJob: (req: IncrementalJobRequest) =>
    api.post<IncrementalJob>('/incremental/jobs', req),

  updateIncrementalJob: (id: string, req: IncrementalJobRequest) =>
    api.put<IncrementalJob>(`/incremental/jobs/${id}`, req),

  deleteIncrementalJob: (id: string) =>
    api.delete<{ deleted: boolean }>(`/incremental/jobs/${id}`),

  runIncrementalJob: (id: string, tables?: string[]) =>
    api.post<IncrementalRunRecord>(`/incremental/jobs/${id}/run`, { tables: tables || [] }),

  // Column listing with watermark eligibility + index flag (F-04): the
  // watermark dropdown marks comparable types and warns on unindexed columns.
  getSourceTableColumns: (ref: string, table: string) =>
    api.get<{ columns: { name: string; data_type: string; comparable: boolean; indexed: boolean }[] }>(
      `/sources/tables/${encodeURIComponent(table)}/columns`,
      { params: { source_ref: ref } },
    ),

  // Compatibility assessment (S1-UI-08): body is { source_ref } or the inline
  // PG form; format 'html' returns the report as text instead of JSON.
  assess: (body: Record<string, any>, format?: string) =>
    api.post<any>(
      '/assess',
      format ? { ...body, format } : body,
      {
        timeout: LONG_TIMEOUT,
        responseType: format === 'html' ? 'text' : 'json',
        transformResponse: format === 'html' ? [(d: any) => d] : undefined,
      },
    ),

  // DDL export (S1-UI-08): schema listing + the ZIP export itself. The export
  // may walk every object in every selected schema — long timeout, blob body.
  ddlExportSchemas: (body: Record<string, any>) =>
    api.post<{ schemas: string[] }>('/ddl-export/schemas', body, { timeout: LONG_TIMEOUT }),

  ddlExport: (body: Record<string, any>) =>
    api.post<Blob>('/ddl-export', body, { timeout: LONG_TIMEOUT, responseType: 'blob' }),

  // CDC monitoring/control (S1-UI-08: migrated off raw fetch). precheck
  // probes PG slot/publication server-side — extended timeout.
  cdcStatus: () => api.get('/cdc/status'),
  cdcStats: () => api.get('/cdc/stats'),
  cdcCheckpoint: () => api.get('/cdc/checkpoint'),
  cdcSlot: () => api.get('/cdc/slot'),
  cdcPrecheck: () => api.get('/cdc/precheck', { timeout: LONG_TIMEOUT }),
  cdcControl: (action: 'start' | 'stop') => api.post(`/cdc/${action}`),
  resetCDCCheckpoint: () => api.post('/cdc/checkpoint/reset', { confirm: 'DELETE' }),
  getCDCConfig: () => api.get('/cdc/config'),
  saveCDCConfig: (source: Record<string, any>, target: Record<string, any>) =>
    api.put('/cdc/config', { source, target }),
  importCDCConfig: () => api.post('/cdc/config/import'),
}

export default apiClient
