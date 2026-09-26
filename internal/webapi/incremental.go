package webapi

// F-04 增量补齐 (timestamp-watermark incremental sync): pull-based,
// table-granular jobs that copy rows whose watermark column advanced past the
// per-table high-water mark. Completely independent of the log-based CDC
// pipeline; manual trigger only (v1); PostgreSQL sources only (v1); plain
// streamed SQL writes (no Lightning). Connection credentials never leave the
// server: jobs reference datasources by id and resolve F-02 snapshots.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"go.uber.org/zap"
)

// incMu serializes all read→modify→write cycles on incremental_jobs.json.
var incMu sync.Mutex

// incTableConfig is the per-table user configuration.
type incTableConfig struct {
	Table            string `json:"table"`
	WatermarkColumn  string `json:"watermark_column"`
	InitialWatermark string `json:"initial_watermark,omitempty"` // "" = MIN(col) → full backfill
}

// incTableState is the per-table runtime progress. Editing a job's config
// never touches state; state resets only via the API's explicit reset flag.
type incTableState struct {
	LastWatermark string     `json:"last_watermark,omitempty"`
	LastSyncAt    *time.Time `json:"last_sync_at,omitempty"`
	TotalRows     int64      `json:"total_rows"`
	Failed        string     `json:"failed,omitempty"` // last per-table error
}

type incJob struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	SourceRef        string                    `json:"source_ref"`
	TargetRef        string                    `json:"target_ref"`
	BatchSize        int                       `json:"batch_size"`
	StrictMode       bool                      `json:"strict_mode"`       // ">" instead of ">=" (may lose same-second late rows)
	ConflictStrategy string                    `json:"conflict_strategy"` // replace | ignore | error
	Tables           []incTableConfig          `json:"tables"`
	States           map[string]*incTableState `json:"states"`
	History          []incRunRecord            `json:"history,omitempty"` // newest first, capped
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

type incTableResult struct {
	Table  string `json:"table"`
	FromWM string `json:"from_watermark"`
	ToWM   string `json:"to_watermark"`
	Rows   int64  `json:"rows"`
	Error  string `json:"error,omitempty"`
}

type incRunRecord struct {
	RunID      string           `json:"run_id"`
	StartedAt  time.Time        `json:"started_at"`
	DurationMs int64            `json:"duration_ms"`
	Tables     []incTableResult `json:"tables"`
}

const incHistoryCap = 20

// incIdentifierRe is the server-side allow-list for table/column names: a
// conservative ASCII identifier (quoted anyway, but never accept anything the
// pattern rejects — belt and braces against SQL injection).
var incIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// incWatermarkTypes lists the information_schema.data_type values eligible as
// watermark columns (comparable, monotonic-ish types).
var incWatermarkTypes = map[string]bool{
	"timestamp with time zone":    true,
	"timestamp without time zone": true,
	"date":                        true,
	"integer":                     true,
	"bigint":                      true,
}

func incIdentifierOK(s string) bool { return incIdentifierRe.MatchString(s) }

// incQuotePG quotes a PostgreSQL identifier.
func incQuotePG(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// incQuoteMySQL quotes a MySQL/TiDB identifier.
func incQuoteMySQL(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// incBuildSelectSQL renders the keyset-paged source scan. The watermark value
// is always the $1 parameter (never inlined); batch size is $2.
func incBuildSelectSQL(schema, table string, cols []string, wmCol string, strict bool) string {
	op := ">="
	if strict {
		op = ">"
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = incQuotePG(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s %s $1 ORDER BY %s LIMIT $2",
		strings.Join(quoted, ", "), incQuotePG(schema), incQuotePG(table), incQuotePG(wmCol), op, incQuotePG(wmCol))
}

// incBuildDrainSQL renders the same-value drain scan used when a full batch
// sits entirely on one watermark value: no LIMIT (streamed), equality only.
// The watermark value stays the $1 parameter.
func incBuildDrainSQL(schema, table string, cols []string, wmCol string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = incQuotePG(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s = $1",
		strings.Join(quoted, ", "), incQuotePG(schema), incQuotePG(table), incQuotePG(wmCol))
}

// incBuildNextWatermarkSQL renders the post-drain jump probe: the smallest
// watermark strictly above the drained value ("" / NULL ⇒ table complete).
func incBuildNextWatermarkSQL(schema, table, wmCol string) string {
	return fmt.Sprintf("SELECT MIN(%s) FROM %s.%s WHERE %s > $1",
		incQuotePG(wmCol), incQuotePG(schema), incQuotePG(table), incQuotePG(wmCol))
}

// incCursorStep decides the keyset cursor after one fetched batch.
// saturated=true means the batch was full AND its max watermark equals the
// watermark we entered the batch with: a plain keyset retry would refetch the
// exact same batch forever (same-value livelock, e.g. same-second bulk
// INSERTs), so the caller must drain that value and jump past it.
func incCursorStep(entryWM, lastWM string, batchLen, batchSize int) (nextWM string, saturated, done bool) {
	if batchLen == 0 {
		return entryWM, false, true
	}
	if batchLen == batchSize && lastWM == entryWM {
		return lastWM, true, false
	}
	if batchLen < batchSize {
		return lastWM, false, true
	}
	return lastWM, false, false
}

// incJumpAfterDrain maps the MIN(watermark) > wm probe result to the next
// cursor: done=true when no value remains above the drained watermark.
func incJumpAfterDrain(nextMin sql.NullString) (string, bool) {
	if !nextMin.Valid || nextMin.String == "" {
		return "", true
	}
	return nextMin.String, false
}

// incBuildInsertSQL renders the batched target write with the conflict
// strategy prefix. nRows rows, len(colNames) placeholders each.
func incBuildInsertSQL(db, table string, colNames []string, nRows int, strategy string) string {
	prefix := "INSERT INTO"
	switch strategy {
	case "replace":
		prefix = "REPLACE INTO"
	case "ignore":
		prefix = "INSERT IGNORE INTO"
	}
	quoted := make([]string, len(colNames))
	for i, c := range colNames {
		quoted[i] = incQuoteMySQL(c)
	}
	oneRow := "(" + strings.Repeat("?, ", len(colNames))
	oneRow = oneRow[:len(oneRow)-2] + ")"
	return fmt.Sprintf("%s %s.%s (%s) VALUES %s",
		prefix, incQuoteMySQL(db), incQuoteMySQL(table), strings.Join(quoted, ", "),
		strings.Repeat(oneRow+", ", nRows-1)+oneRow)
}

// incAdvanceWatermark implements the state rule: rows==0 keeps the current
// watermark (no speculative advance); otherwise the stream's MAX (= last row
// of the ORDER BY scan) becomes the new watermark.
func incAdvanceWatermark(current, streamMax string, rows int64) string {
	if rows == 0 {
		return current
	}
	return streamMax
}

func (s *Server) incrementalJobsFile() string { return s.dataDir + "/incremental_jobs.json" }

// cleanupIncrementalTempFiles removes crash-orphaned temp files at startup
// (they hold connection-watermark state; same hygiene as datasources).
func (s *Server) cleanupIncrementalTempFiles() {
	matches, err := filepath.Glob(s.dataDir + "/incremental-*.tmp")
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			zap.L().Warn("failed to remove orphaned incremental temp file", zap.String("file", m), zap.Error(err))
		}
	}
}

func (s *Server) loadIncrementalJobs() []incJob {
	raw, err := os.ReadFile(s.incrementalJobsFile())
	if err != nil {
		return nil
	}
	var list []incJob
	if err := json.Unmarshal(raw, &list); err != nil {
		zap.L().Warn("incremental_jobs.json corrupted, ignoring saved jobs",
			zap.String("file", s.incrementalJobsFile()), zap.Error(err))
		return nil
	}
	return list
}

func (s *Server) saveIncrementalJobs(list []incJob) error {
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dataDir, "incremental-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.incrementalJobsFile())
}

// --- validation ---

func validateIncJobBody(name string, sourceRef, targetRef string, tables []incTableConfig, strategy string, batchSize int) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required")
	}
	if sourceRef == "" || targetRef == "" {
		return fmt.Errorf("source_ref and target_ref are required")
	}
	switch strategy {
	case "replace", "ignore", "error":
	case "":
		return fmt.Errorf("conflict_strategy is required")
	default:
		return fmt.Errorf("conflict_strategy must be one of replace/ignore/error")
	}
	if len(tables) == 0 {
		return fmt.Errorf("at least one table is required")
	}
	if batchSize < 1 || batchSize > 100000 {
		return fmt.Errorf("batch_size must be in 1-100000")
	}
	seen := map[string]bool{}
	for _, t := range tables {
		if !incIdentifierOK(t.Table) {
			return fmt.Errorf("invalid table name %q", t.Table)
		}
		if !incIdentifierOK(t.WatermarkColumn) {
			return fmt.Errorf("invalid watermark column %q", t.WatermarkColumn)
		}
		// #t1: case-insensitive dedup — PG unquoted identifiers fold to
		// lowercase, so ord_hdr and ORD_HDR are the same table. Quoted
		// case-sensitive twins are out of scope by this early check
		// (ruling tradeoff, see commit message).
		if seen[strings.ToLower(t.Table)] {
			return fmt.Errorf("duplicate table %q", t.Table)
		}
		seen[strings.ToLower(t.Table)] = true
	}
	return nil
}

// --- handlers ---

func (s *Server) handleListIncrementalJobs(w http.ResponseWriter, r *http.Request) {
	incMu.Lock()
	list := s.loadIncrementalJobs()
	incMu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateIncrementalJob(w http.ResponseWriter, r *http.Request) {
	var job incJob
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	job.Name = strings.TrimSpace(job.Name)
	if err := validateIncJobBody(job.Name, job.SourceRef, job.TargetRef, job.Tables, job.ConflictStrategy, job.BatchSize); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// D4: PostgreSQL sources only in v1; target must be tidb (applier SQL is MySQL-flavoured).
	src, err := s.resolveDataSourceRef(job.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if src.Type != "postgres" {
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	tgt, err := s.resolveDataSourceRef(job.TargetRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "target_ref: "+err.Error())
		return
	}
	if tgt.Type != "tidb" {
		s.writeError(w, http.StatusBadRequest, "目标数据源必须是 TiDB")
		return
	}

	now := time.Now()
	job.ID = uuid.New().String()[:8]
	job.States = map[string]*incTableState{}
	job.History = nil
	job.CreatedAt, job.UpdatedAt = now, now

	incMu.Lock()
	list := s.loadIncrementalJobs()
	for _, e := range list {
		if e.Name == job.Name {
			incMu.Unlock()
			s.writeError(w, http.StatusConflict, fmt.Sprintf("任务名称 %q 已存在", job.Name))
			return
		}
	}
	for {
		dup := false
		for _, e := range list {
			if e.ID == job.ID {
				dup = true
			}
		}
		if !dup {
			break
		}
		job.ID = uuid.New().String()[:8]
	}
	list = append(list, job)
	if err := s.saveIncrementalJobs(list); err != nil {
		incMu.Unlock()
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	incMu.Unlock()
	s.writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleUpdateIncrementalJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var job incJob
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	job.Name = strings.TrimSpace(job.Name)
	if err := validateIncJobBody(job.Name, job.SourceRef, job.TargetRef, job.Tables, job.ConflictStrategy, job.BatchSize); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// D4 double-gate on update too (F-02 dual-path gate parity): a PUT must
	// not be able to swap refs past the create-side type checks.
	src, err := s.resolveDataSourceRef(job.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if src.Type != "postgres" {
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	tgt, err := s.resolveDataSourceRef(job.TargetRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "target_ref: "+err.Error())
		return
	}
	if tgt.Type != "tidb" {
		s.writeError(w, http.StatusBadRequest, "目标数据源必须是 TiDB")
		return
	}

	incMu.Lock()
	defer incMu.Unlock()
	list := s.loadIncrementalJobs()
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.writeError(w, http.StatusNotFound, "job not found")
		return
	}
	e := &list[idx]
	for i := range list {
		if i != idx && list[i].Name == job.Name {
			s.writeError(w, http.StatusConflict, fmt.Sprintf("任务名称 %q 已存在", job.Name))
			return
		}
	}
	// Config edits keep per-table state: state survives for tables whose name
	// is unchanged, resets for removed/new ones.
	oldStates := e.States
	if oldStates == nil {
		oldStates = map[string]*incTableState{}
	}
	states := map[string]*incTableState{}
	for _, t := range job.Tables {
		if st, ok := oldStates[t.Table]; ok {
			states[t.Table] = st
		} else {
			states[t.Table] = &incTableState{}
		}
	}
	e.Name, e.SourceRef, e.TargetRef = job.Name, job.SourceRef, job.TargetRef
	e.BatchSize, e.StrictMode, e.ConflictStrategy = job.BatchSize, job.StrictMode, job.ConflictStrategy
	e.Tables, e.States = job.Tables, states
	e.UpdatedAt = time.Now()
	if err := s.saveIncrementalJobs(list); err != nil {
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleDeleteIncrementalJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	incMu.Lock()
	defer incMu.Unlock()
	list := s.loadIncrementalJobs()
	out := list[:0]
	found := false
	for _, e := range list {
		if e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err := s.saveIncrementalJobs(out); err != nil {
		s.writeError(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// incColumnView is the JSON shape of a column's watermark eligibility.
type incColumnView struct {
	Name       string `json:"name"`
	DataType   string `json:"data_type"`
	Comparable bool   `json:"comparable"`
	Indexed    bool   `json:"indexed"`
}

// queryIncColumns runs the watermark-eligibility column query for one table
// (shared by the single-table and batch endpoints).
func queryIncColumns(ctx context.Context, db *sql.DB, schema, table string) ([]incColumnView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.column_name, c.data_type,
		       (SELECT COUNT(*) FROM pg_index i
		         JOIN pg_class tc ON tc.oid = i.indrelid
		         JOIN pg_namespace ns ON ns.oid = tc.relnamespace
		         JOIN pg_attribute a ON a.attrelid = tc.oid AND a.attname = c.column_name
		         JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum, ord) ON true
		        WHERE ns.nspname = c.table_schema AND tc.relname = c.table_name
		          AND k.attnum = a.attnum AND k.ord = 1) AS indexed_first
		FROM information_schema.columns c
		WHERE c.table_schema = $1 AND c.table_name = $2
		ORDER BY c.ordinal_position`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := []incColumnView{}
	for rows.Next() {
		var c incColumnView
		var idxed int
		if err := rows.Scan(&c.Name, &c.DataType, &idxed); err != nil {
			return nil, err
		}
		c.Comparable = incWatermarkTypes[c.DataType]
		c.Indexed = idxed > 0
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// handleIncrementalColumns lists a table's columns with watermark eligibility
// and an index flag (no index on the watermark column ⇒ full scans ⇒ warn).
func (s *Server) handleIncrementalColumns(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("source_ref")
	table := chi.URLParam(r, "table")
	if ref == "" {
		s.writeError(w, http.StatusBadRequest, "source_ref is required")
		return
	}
	if !incIdentifierOK(table) {
		s.writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	e, err := s.resolveDataSourceRef(ref)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if e.Type != "postgres" {
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	sc := dataSourceToSourceConfig(e)
	db, err := openPGTestConn(sc.DSN())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cols, err := queryIncColumns(ctx, db, sc.Schema, table)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "list columns failed: "+err.Error())
		return
	}
	if len(cols) == 0 {
		s.writeError(w, http.StatusNotFound, "table not found in source schema")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"columns": cols})
}

// incColumnsBatchLimit caps the batch endpoint: one information_schema round
// trip per table, so bound the fan-out.
const incColumnsBatchLimit = 200

// handleIncrementalColumnsBatch (#t1) returns the watermark-eligibility
// column list for many tables over ONE source connection — the batch-binding
// UI flow would otherwise open one connection per table.
func (s *Server) handleIncrementalColumnsBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceRef string   `json:"source_ref"`
		Tables    []string `json:"tables"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.SourceRef == "" {
		s.writeError(w, http.StatusBadRequest, "source_ref is required")
		return
	}
	if len(req.Tables) == 0 {
		s.writeError(w, http.StatusBadRequest, "tables is required")
		return
	}
	if len(req.Tables) > incColumnsBatchLimit {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("tables 数超过上限 %d", incColumnsBatchLimit))
		return
	}
	seen := map[string]bool{}
	for _, t := range req.Tables {
		if !incIdentifierOK(t) {
			s.writeError(w, http.StatusBadRequest, "invalid table name: "+t)
			return
		}
		if seen[t] {
			s.writeError(w, http.StatusBadRequest, "表重复: "+t)
			return
		}
		seen[t] = true
	}
	e, err := s.resolveDataSourceRef(req.SourceRef)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source_ref: "+err.Error())
		return
	}
	if e.Type != "postgres" {
		s.writeError(w, http.StatusBadRequest, "增量同步 v1 仅支持 PostgreSQL 源数据源")
		return
	}
	sc := dataSourceToSourceConfig(e)
	db, err := openPGTestConn(sc.DSN())
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	type tableResult struct {
		Table   string          `json:"table"`
		Columns []incColumnView `json:"columns"`
		Error   string          `json:"error,omitempty"`
	}
	out := make([]tableResult, 0, len(req.Tables))
	for _, t := range req.Tables {
		cols, err := queryIncColumns(ctx, db, sc.Schema, t)
		if err != nil {
			out = append(out, tableResult{Table: t, Error: err.Error()})
			continue
		}
		if len(cols) == 0 {
			out = append(out, tableResult{Table: t, Error: "表在源库中不存在"})
			continue
		}
		out = append(out, tableResult{Table: t, Columns: cols})
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"tables": out})
}

// --- run engine ---

func incValueToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// runIncrementalJob executes one manual sync pass over the (sub)set of tables
// and returns the run record. Failures are per-table: one bad table never
// blocks the others.
// v1 assumption: runs are NOT serialized — two concurrent manual runs of the
// same job interleave and the later state persist wins (last writer wins).
func (s *Server) runIncrementalJob(r *http.Request, job *incJob, subset map[string]bool) incRunRecord {
	rec := incRunRecord{RunID: uuid.New().String()[:8], StartedAt: time.Now()}
	src, err := s.resolveDataSourceRef(job.SourceRef)
	if err != nil {
		for _, t := range job.Tables {
			rec.Tables = append(rec.Tables, incTableResult{Table: t.Table, Error: "source_ref: " + err.Error()})
		}
		return rec
	}
	tgt, err := s.resolveDataSourceRef(job.TargetRef)
	if err != nil {
		for _, t := range job.Tables {
			rec.Tables = append(rec.Tables, incTableResult{Table: t.Table, Error: "target_ref: " + err.Error()})
		}
		return rec
	}
	sc := dataSourceToSourceConfig(src)
	tc := dataSourceToTargetConfig(tgt)

	pgDB, err := openPGTestConn(sc.DSN())
	if err != nil {
		for _, t := range job.Tables {
			rec.Tables = append(rec.Tables, incTableResult{Table: t.Table, Error: "连接源端失败: " + err.Error()})
		}
		return rec
	}
	defer pgDB.Close()
	myDB, err := openMySQLTestConn(tc.DSN())
	if err != nil {
		for _, t := range job.Tables {
			rec.Tables = append(rec.Tables, incTableResult{Table: t.Table, Error: "连接目标端失败: " + err.Error()})
		}
		return rec
	}
	defer myDB.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	for _, t := range job.Tables {
		if len(subset) > 0 && !subset[t.Table] {
			continue
		}
		rec.Tables = append(rec.Tables, s.syncOneTable(ctx, pgDB, myDB, sc, tc, job, t))
	}
	rec.DurationMs = time.Since(rec.StartedAt).Milliseconds()
	return rec
}

func (s *Server) syncOneTable(ctx context.Context, pgDB, myDB *sql.DB, sc config.SourceConfig, tc config.TargetConfig, job *incJob, t incTableConfig) incTableResult {
	st := job.States[t.Table]
	if st == nil {
		st = &incTableState{}
		job.States[t.Table] = st
	}
	res := incTableResult{Table: t.Table, FromWM: st.LastWatermark}

	wm := st.LastWatermark
	if wm == "" {
		wm = t.InitialWatermark
	}
	// Discover columns + validate the watermark type (D4 whitelist) at run
	// time against the live source — creation only validates identifiers.
	rows, err := pgDB.QueryContext(ctx, `
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`, sc.Schema, t.Table)
	if err != nil {
		res.Error = "读取源表列信息失败: " + err.Error()
		st.Failed = res.Error
		return res
	}
	var cols []string
	var wmType string
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			rows.Close()
			res.Error = "读取源表列信息失败: " + err.Error()
			st.Failed = res.Error
			return res
		}
		if name == t.WatermarkColumn {
			wmType = dataType
		}
		cols = append(cols, name)
	}
	rows.Close()
	if len(cols) == 0 {
		res.Error = fmt.Sprintf("源 schema %q 中不存在表 %q", sc.Schema, t.Table)
		st.Failed = res.Error
		return res
	}
	if wmType == "" {
		res.Error = fmt.Sprintf("表 %q 不存在水位列 %q", t.Table, t.WatermarkColumn)
		st.Failed = res.Error
		return res
	}
	if !incWatermarkTypes[wmType] {
		res.Error = fmt.Sprintf("水位列 %q 类型 %q 不在白名单（timestamp/timestamptz/date/int/bigint）", t.WatermarkColumn, wmType)
		st.Failed = res.Error
		return res
	}

	// Empty initial watermark ⇒ full backfill from MIN(watermark).
	minDerived := false
	if wm == "" {
		var minWM sql.NullString
		if err := pgDB.QueryRowContext(ctx,
			fmt.Sprintf("SELECT MIN(%s) FROM %s.%s", incQuotePG(t.WatermarkColumn), incQuotePG(sc.Schema), incQuotePG(t.Table)),
		).Scan(&minWM); err != nil {
			res.Error = "计算初始水位失败: " + err.Error()
			st.Failed = res.Error
			return res
		}
		if !minWM.Valid || minWM.String == "" {
			// Empty table: nothing to do, and nothing to advance to
			// (re-probing MIN on every run is harmless — no state to keep).
			now := time.Now()
			st.LastSyncAt = &now
			st.Failed = ""
			res.ToWM = ""
			return res
		}
		wm = minWM.String
		minDerived = true
		res.FromWM = "(MIN) " + wm
	}

	// Strict-mode >= exceptions: (a) a cursor derived from MIN(col) MUST scan
	// with >= first — a strict > would permanently skip every row sitting
	// exactly at MIN (adversarial ①, v2); (b) after a drain jump the
	// jumped-to value's rows are entirely unconsumed — a strict > would skip
	// them all, so the post-jump scan is also >=. Later scans restore the
	// job's strict semantics (the usual boundary-value tradeoff applies).
	geScan := minDerived
	total := int64(0)
	lastWM := wm
	for {
		entryWM := wm
		selSQL := incBuildSelectSQL(sc.Schema, t.Table, cols, t.WatermarkColumn, job.StrictMode && !geScan)
		srows, err := pgDB.QueryContext(ctx, selSQL, wm, job.BatchSize)
		if err != nil {
			res.Error = "查询源端失败: " + err.Error()
			st.Failed = res.Error
			return res
		}
		batch := [][]any{}
		for srows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := srows.Scan(ptrs...); err != nil {
				srows.Close()
				res.Error = "读取源端行失败: " + err.Error()
				st.Failed = res.Error
				return res
			}
			batch = append(batch, vals)
		}
		if err := srows.Err(); err != nil {
			srows.Close()
			res.Error = "遍历源端失败: " + err.Error()
			st.Failed = res.Error
			return res
		}
		srows.Close()
		if len(batch) == 0 {
			break
		}

		args := make([]any, 0, len(batch)*len(cols))
		for _, row := range batch {
			args = append(args, row...)
		}
		insSQL := incBuildInsertSQL(tc.Database, t.Table, cols, len(batch), job.ConflictStrategy)
		if _, err := myDB.ExecContext(ctx, insSQL, args...); err != nil {
			res.Error = "写入目标端失败: " + err.Error()
			st.Failed = res.Error
			return res
		}
		total += int64(len(batch))
		// ORDER BY watermark ⇒ the last row carries the batch MAX.
		lastWM = incValueToString(batch[len(batch)-1][wmIndex(cols, t.WatermarkColumn)])
		next, saturated, done := incCursorStep(entryWM, lastWM, len(batch), job.BatchSize)
		if saturated {
			// Same-value saturation: the keyset cannot advance inside this
			// value's window. Drain every remaining row at this watermark
			// (streamed, chunked writes), then jump to the next value.
			n, jump, tableDone, derr := s.incDrainWatermark(ctx, pgDB, myDB, sc, tc, job, t, cols, lastWM)
			if derr != nil {
				res.Error = "泄流同值批次失败: " + derr.Error()
				st.Failed = res.Error
				return res
			}
			total += n
			if tableDone {
				break
			}
			wm = jump
			geScan = true
			continue
		}
		geScan = false
		wm = next
		if done {
			break
		}
	}

	now := time.Now()
	st.LastWatermark = incAdvanceWatermark(st.LastWatermark, lastWM, total)
	st.LastSyncAt = &now
	st.TotalRows += total
	st.Failed = ""
	res.ToWM = st.LastWatermark
	res.Rows = total
	return res
}

func wmIndex(cols []string, wmCol string) int {
	for i, c := range cols {
		if c == wmCol {
			return i
		}
	}
	return -1
}

// incDrainWatermark consumes ALL rows whose watermark equals wm — the
// same-value saturation fix. The equality scan is unbounded and streamed;
// rows are written in BatchSize chunks with the job's conflict strategy
// (semantics identical to the main path). Afterwards the cursor jumps to
// MIN(watermark) > wm: no such value ⇒ the whole table is synced (done).
// Memory stays bounded regardless of how many rows share the value.
func (s *Server) incDrainWatermark(ctx context.Context, pgDB, myDB *sql.DB, sc config.SourceConfig, tc config.TargetConfig, job *incJob, t incTableConfig, cols []string, wm string) (rows int64, nextWM string, done bool, err error) {
	srows, qErr := pgDB.QueryContext(ctx, incBuildDrainSQL(sc.Schema, t.Table, cols, t.WatermarkColumn), wm)
	if qErr != nil {
		return 0, "", false, qErr
	}
	batch := [][]any{}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		args := make([]any, 0, len(batch)*len(cols))
		for _, row := range batch {
			args = append(args, row...)
		}
		insSQL := incBuildInsertSQL(tc.Database, t.Table, cols, len(batch), job.ConflictStrategy)
		if _, eErr := myDB.ExecContext(ctx, insSQL, args...); eErr != nil {
			return eErr
		}
		rows += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for srows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if sErr := srows.Scan(ptrs...); sErr != nil {
			srows.Close()
			return rows, "", false, sErr
		}
		batch = append(batch, vals)
		if len(batch) >= job.BatchSize {
			if fErr := flush(); fErr != nil {
				srows.Close()
				return rows, "", false, fErr
			}
		}
	}
	if sErr := srows.Err(); sErr != nil {
		srows.Close()
		return rows, "", false, sErr
	}
	srows.Close()
	if fErr := flush(); fErr != nil {
		return rows, "", false, fErr
	}

	var next sql.NullString
	if qErr = pgDB.QueryRowContext(ctx,
		incBuildNextWatermarkSQL(sc.Schema, t.Table, t.WatermarkColumn), wm,
	).Scan(&next); qErr != nil {
		return rows, "", false, qErr
	}
	jump, tableDone := incJumpAfterDrain(next)
	return rows, jump, tableDone, nil
}

func (s *Server) handleRunIncrementalJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Tables []string `json:"tables"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			s.writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	subset := map[string]bool{}
	for _, t := range body.Tables {
		if !incIdentifierOK(t) {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid table name %q", t))
			return
		}
		subset[t] = true
	}

	incMu.Lock()
	list := s.loadIncrementalJobs()
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		incMu.Unlock()
		s.writeError(w, http.StatusNotFound, "job not found")
		return
	}
	job := list[idx]
	incMu.Unlock()

	rec := s.runIncrementalJob(r, &job, subset)

	// Persist advanced states + history.
	incMu.Lock()
	list = s.loadIncrementalJobs()
	for i := range list {
		if list[i].ID == id {
			list[i].States = job.States
			list[i].History = append([]incRunRecord{rec}, list[i].History...)
			if len(list[i].History) > incHistoryCap {
				list[i].History = list[i].History[:incHistoryCap]
			}
			list[i].UpdatedAt = time.Now()
			if err := s.saveIncrementalJobs(list); err != nil {
				zap.L().Warn("failed to persist incremental job state", zap.String("job", id), zap.Error(err))
			}
			s.writeJSON(w, http.StatusOK, rec)
			break
		}
	}
	incMu.Unlock()
}
