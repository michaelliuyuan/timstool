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

// #10② 连接测试反馈上限：主机黑洞/网络挂起时 axios 默认 30s 才报错（静默
// loading 半分钟）。测试类端点统一压到 3.5s —— 失败/超时 ≤3.5s 必出错误提示
// （各调用方 catch 分支已有 ElMessage.error）；正常成功路径不受影响。
const TEST_TIMEOUT = 3500

export interface Task {
  id: string
  name: string
  status: 'created' | 'running' | 'paused' | 'completed' | 'failed' | 'cancelled'
  /** Server-derived discriminator ("compare" | "migration"); config_json never leaves the backend. */
  kind?: 'mongocompare' | 'migration'
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
  duration?: number
  warn?: boolean
  error?: string
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
  status?: string // running|completed|failed (absent on legacy records = completed)
  error?: string
}

export interface IncrementalJob {
  id: string
  name: string
  source_ref: string
  target_ref: string
  batch_size: number
  parallelism?: number
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
  parallelism?: number
  strict_mode: boolean
  conflict_strategy: 'replace' | 'ignore' | 'error'
  tables: IncrementalTableConfig[]
}

// FEAT-INC-LOGS: one observable step of a run (see internal/webapi/incremental_logs.go).
export interface IncrementalLogEvent {
  seq: number
  ts: string
  level: 'info' | 'sql' | 'warn' | 'error'
  table?: string
  phase?: 'start' | 'scan' | 'write' | 'drain' | 'jump' | 'done' | 'fail' | 'interrupted'
  msg?: string
  sql?: string
  rows?: number
  wm?: string
  cost_ms?: number
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
    use_dumpling: boolean
    dumpling_path: string
    skip_precheck: boolean
    skip_schema: boolean
    skip_data: boolean
    skip_validate: boolean
    cdc_chain: boolean
    target_policy: string
    compare_mode: string
    sample_ratio: number
    checksum_chunk_size: number
    /** #t4 deprecated — compare concurrency now derives from parallel. */
    checksum_parallel?: number
  }
}

// MongoDB compare task (M3 API face, snake_case per task-API convention).
// One source database vs one target database (v1 semantics); uri OR discrete
// fields per side (discrete wins server-side; passwords never round-trip the
// API — the server stores ConfigJSON with json:"-"). M6: tls rides along as
// an optional switch, and datasource_id references a saved mongodb
// datasource which the server folds into a creation-time snapshot (explicit
// discrete fields override the stored profile).
export interface CompareEndpoint {
  uri?: string
  host?: string
  port?: number
  user?: string
  password?: string
  auth_db?: string
  tls?: boolean
  database: string
  datasource_id?: string
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
  kind: 'mongocompare'
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
    return r && r.kind === 'mongocompare' ? (r as CompareResult) : null
  } catch {
    return null
  }
}

// isCompareTask discriminates task kinds on the wire contract: the server
// derives an explicit `kind` field on every task response (DTO), while
// config_json never leaves the backend (it holds credentials). Tasks without
// a kind (stale cached responses) fall back to migration rendering.
export function isCompareTask(task: Task | null | undefined): boolean {
  return task?.kind === 'mongocompare'
}

// ---------------------------------------------------------------------------
// MySQL/PG standalone compare domain (MS line: /compare/* endpoints). Kept
// side by side with the mongodb compare types above — two independent faces.
// ---------------------------------------------------------------------------

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
  /** #t4 deprecated legacy knobs (kept for display of old tasks). */
  checksum_parallel: number
  parallel: number
  /** #t4 unified total concurrency budget (effective value). */
  concurrency: number
  tables: string[]
  /** #t3 watermark filter group (absent = unfiltered compare). */
  watermark?: { column: string; value: string; op: string; base_mode: string }
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
  /** #t4 deprecated — folded into concurrency server-side. */
  checksum_parallel?: number
  /** #t4 deprecated — folded into concurrency server-side. */
  parallel?: number
  /** #t4 unified total concurrency budget (1-8; 0/absent = derive from legacy knobs). */
  concurrency?: number
  tables: string[]
  /** #t3 optional watermark filter; when set, mode is the filter's base_mode. */
  watermark?: { column: string; value: string; op: string; base_mode: string }
}

export interface CompareOptions {
  source_type?: string
  source?: Record<string, any>
  target?: Record<string, any>
  mode?: string
  sample_ratio?: number
  checksum_chunk_size?: number
  checksum_parallel?: number
  parallel?: number
  concurrency?: number
  /** #t3 present-and-null clears the saved group (partial-merge by presence). */
  watermark?: { column: string; value: string; op: string; base_mode: string } | null
}

// F-02 unified datasource registry. Passwords are write-only: they are sent
// on create/update, never returned (has_password reports presence instead).
export interface DataSource {
  id: string
  name: string
  type: 'postgres' | 'mysql' | 'tidb' | 'mongodb'
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
  /** #t4 unified total concurrency budget. */
  concurrency?: number
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

  // MC-11: browse a mongodb source's database list via /sources/meta
  // (database empty → databases). datasource_id folds the stored profile
  // server-side (credentials never travel back); manual fields ride the
  // plain {source, fields} shape. Entries carry name/size only.
  browseSourceDatabases: (payload: { datasource_id?: string; source?: string; fields?: Record<string, any> }) =>
    api.post<{ source: string; level: string; entries: { name: string; size_on_disk?: number; empty?: boolean }[]; count: number }>(
      '/sources/meta',
      { source: 'mongodb', database: '', ...payload },
    ),

  // Multi-source connection test (doc §6.2): {source, fields} → {success,...}.
  // source defaults to postgres server-side (backward compat).
  testSourceConnection: (source: string, fields: Record<string, any>) =>
    api.post<{ source: string; success: boolean; message: string; version?: string }>(
      '/test-connection',
      { source, fields },
      { timeout: TEST_TIMEOUT },
    ),

  // Same endpoint, datasource-ref flavour (server-side credentials).
  testSourceConnectionRef: (sourceRef: string) =>
    api.post<{ source: string; success: boolean; message: string; version?: string }>(
      '/test-connection',
      { source_ref: sourceRef },
      { timeout: TEST_TIMEOUT },
    ),

  testConnection: (req: ConnectionTestRequest) =>
    api.post<ConnectionTestResult>('/config/test-connection', req, { timeout: TEST_TIMEOUT }),

  listTables: (req: ConnectionTestRequest) =>
    api.post<{ tables: { name: string; row_estimate: number }[]; count: number }>('/config/list-tables', { ...req, type: 'source' }),

  // Lightning path gate for the wizard (doc: 迁移选项页门禁). Empty path probes
  // auto-discovery server-side; non-empty must exist and (on Linux) be
  // executable. MS-11q: PLUS the live -V probe — version carries the
  // real version string back (stat pass + probe pass = green).
  validateLightning: (path: string) =>
    api.post<{ success: boolean; message: string; resolved_path: string; version?: string }>('/validate-lightning', { path }),

  // MS-11p dumpling export gate (MySQL 源端导出快路). Same path shapes as
  // lightning PLUS the live --version probe: version carries the real
  // version string back (no embedded fallback).
  validateDumpling: (path: string) =>
    api.post<{ success: boolean; message: string; resolved_path: string; version?: string }>('/validate-dumpling', { path }),

  // Migration options persistence (server-side memory of temp_dir / lightning
  // settings + the Lightning-only target extras pd_addr / status_port).
  // GET prefill on entering the options/target steps, PUT on advancing or a
  // successful target connection test. pd_addr empty and status_port 0 mean
  // "not remembered" and never overwrite the form defaults.
  getMigrationOptions: () =>
    api.get<{ temp_dir: string; use_lightning: boolean; lightning_path: string; use_dumpling: boolean; dumpling_path: string; pd_addr?: string; status_port?: number }>('/migration-options'),

  saveMigrationOptions: (opts: { temp_dir: string; use_lightning: boolean; lightning_path: string; use_dumpling: boolean; dumpling_path: string; pd_addr?: string; status_port?: number }) =>
    api.put<{ success: boolean }>('/migration-options', opts),

  createTask: (req: CreateTaskRequest) =>
    api.post<Task>('/tasks', req),

  createCompareTask: (req: CompareTaskSpec) =>
    api.post<Task>('/mongocompare/tasks', req),

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

  // MC-14: text for both faces — the json face serves the verbatim engine
  // artifact (summary.json bytes) and must download byte-identical.
  getTaskReport: (id: string, format?: string) =>
    api.get(`/tasks/${id}/report`, { params: { format }, responseType: 'text' }),

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

  // MC-16: format param — 'html' (server default) is the family report face
  // served as text (attachment download); 'json' serves the verbatim
  // report.json artifact. Callers that want the typed report MUST pass
  // 'json' explicitly (no param defaults to html server-side).
  // MC-17 ④: format is required — the server default flipped to the html
  // family face in MC-16, so a parameterless call would fetch HTML into a
  // JSON-typed consumer. Both call sites pass it explicitly.
  getCompareReport: (id: string, format: 'html' | 'json') =>
    format === 'html'
      ? api.get(`/compare/tasks/${id}/report`, { params: { format: 'html' }, responseType: 'text' })
      : api.get<CompareReport>(`/compare/tasks/${id}/report`, { params: { format } }),

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
    api.post<DataSourceTestResult>(`/datasources/${id}/test`, undefined, { timeout: TEST_TIMEOUT }),

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

  // Returns 202 {run_id, status:"running"} immediately — the run executes in
  // the background; the frontend polls GET /incremental/jobs for the terminal
  // history record. No LONG_TIMEOUT needed: the response is instant.
  runIncrementalJob: (id: string, tables?: string[]) =>
    api.post<{ run_id: string; status: string }>(`/incremental/jobs/${id}/run`, { tables: tables || [] }),

  // FEAT-INC-LOGS: incremental event pull for one run (after=seq protocol).
  // Live run serves from memory, finished run from the on-disk archive —
  // one polling loop covers both.
  getIncrementalRunLogs: (id: string, runId: string, after: number) =>
    api.get<{
      events: IncrementalLogEvent[]
      status: string
      dropped: number
      last_seq: number
    }>(`/incremental/jobs/${id}/runs/${runId}/logs`, { params: { after } }),

  // Column listing with watermark eligibility + index flag (F-04): the
  // watermark dropdown marks comparable types and warns on unindexed columns.
  getSourceTableColumns: (ref: string, table: string) =>
    api.get<{ columns: { name: string; data_type: string; comparable: boolean; indexed: boolean }[]; key_info?: { has_pk: boolean; has_unique: boolean } }>(
      `/sources/tables/${encodeURIComponent(table)}/columns`,
      { params: { source_ref: ref } },
    ),

  // #t1 batch column listing: one source connection for the multi-table
  // binding flow; per-table results carry their own error (missing table,
  // query failure) so the UI can flag individual tables red.
  columnsBatch: (ref: string, tables: string[]) =>
    api.post<{
      tables: {
        table: string
        columns: { name: string; data_type: string; comparable: boolean; indexed: boolean }[]
        key_info?: { has_pk: boolean; has_unique: boolean }
        error?: string
      }[]
    }>('/incremental/columns-batch', { source_ref: ref, tables }),

  // FEAT-WM-AUTO: rank unified watermark column candidates by scoring the
  // whole source schema's catalog (coverage/name/type/index/DEFAULT now()).
  suggestWatermark: (ref: string) =>
    api.post<{
      total_tables: number
      candidates: {
        column: string
        score: number
        coverage: number
        indexed_ratio: number
        default_now_ratio: number
        type_histogram: Record<string, number>
        matched_tables: string[]
        unmatched_tables: string[]
        reasons: string[]
        warnings: string[]
      }[]
    }>('/incremental/suggest-watermark', { source_ref: ref }, { timeout: LONG_TIMEOUT }),

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
