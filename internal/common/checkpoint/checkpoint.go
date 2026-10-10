package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateSkipped   State = "skipped"
)

type TableCheckpoint struct {
	TableName  string                   `json:"table_name"`
	State      State                    `json:"state"`
	RowsDone   int64                    `json:"rows_done"`
	RowsTotal  int64                    `json:"rows_total"`
	BytesDone  int64                    `json:"bytes_done"`
	StartedAt  time.Time                `json:"started_at"`
	UpdatedAt  time.Time                `json:"updated_at"`
	FinishedAt time.Time                `json:"finished_at,omitempty"`
	Error      string                   `json:"error,omitempty"`
	Chunks     map[int]*ChunkCheckpoint `json:"chunks,omitempty"`

	// SchemaState tracks per-table SCHEMA migration progress, fully
	// independent of State: State is the data-resume cursor (resume skips
	// tables whose State==completed, checkpoint.go IsTableCompleted /
	// GetPendingTables), so the schema phase must never write it — a table
	// whose DDL was applied but whose rows were not exported would
	// otherwise be silently skipped on resume. Schema registration also
	// never touches RowsTotal so the data-phase row denominators stay
	// clean (GetOrCreateTable keeps an existing entry's RowsTotal).
	SchemaState State `json:"schema_state,omitempty"`
}

type ChunkCheckpoint struct {
	Index      int       `json:"index"`
	State      State     `json:"state"`
	RowCount   int64     `json:"row_count"`
	ByteCount  int64     `json:"byte_count"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func (tc *TableCheckpoint) Progress() float64 {
	if tc.RowsTotal <= 0 {
		return 0
	}
	p := float64(tc.RowsDone) / float64(tc.RowsTotal)
	if p > 1.0 {
		p = 1.0
	}
	return p
}

type Checkpoint struct {
	Version        string                      `json:"version"`
	CreatedAt      time.Time                   `json:"created_at"`
	UpdatedAt      time.Time                   `json:"updated_at"`
	Phase          string                      `json:"phase"`
	Tables         map[string]*TableCheckpoint `json:"tables"`
	ImportedTables int                         `json:"imported_tables"`
	ImportMode     string                      `json:"import_mode"`
	// Phases is the per-phase lifecycle state machine (P1). Historical
	// checkpoints written before the field exist simply lack it (nil map)
	// — consumers must fall back to ordinal inference then.
	Phases map[string]*PhaseRecord `json:"phases,omitempty"`
}

// PhaseRecord tracks one pipeline phase's lifecycle (P1): pending →
// running → completed/failed, or skipped outright.
type PhaseRecord struct {
	Name       string    `json:"name"`
	Status     State     `json:"status"`
	SubPhase   string    `json:"sub_phase,omitempty"`
	StartedAt  time.Time `json:",omitempty"`
	FinishedAt time.Time `json:",omitempty"`
	Error      string    `json:"error,omitempty"`
	Warn       bool      `json:"warn,omitempty"`
}

// Import modes recorded in the checkpoint so progress consumers (webapi)
// can pick the correct import-fraction formula. Exported to avoid raw
// string constants scattered across packages.
const (
	ImportModeLightning = "lightning"
	ImportModeStream    = "stream"
)

type Manager struct {
	mu       sync.Mutex
	dir      string
	filePath string
	data     *Checkpoint
}

func NewManager(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create checkpoint dir: %w", err)
	}
	fp := filepath.Join(dir, "checkpoint.json")
	m := &Manager{
		dir:      dir,
		filePath: fp,
		data: &Checkpoint{
			Version:   "1.0",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Tables:    make(map[string]*TableCheckpoint),
		},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func NewReadOnlyManager(dir string) (*Manager, error) {
	fp := filepath.Join(dir, "checkpoint.json")
	m := &Manager{
		dir:      dir,
		filePath: fp,
		data: &Checkpoint{
			Version:   "1.0",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Tables:    make(map[string]*TableCheckpoint),
		},
	}
	if err := m.load(); err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	return m, nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read checkpoint: %w", err)
	}
	return json.Unmarshal(data, m.data)
}

func (m *Manager) save() {
	m.data.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(m.data, "", "  ")
	if err != nil {
		return
	}
	tmp := m.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, m.filePath)
}

func (m *Manager) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.save()
}

func (m *Manager) GetOrCreateTable(tableName string, totalRows int64) *TableCheckpoint {
	m.mu.Lock()
	defer m.mu.Unlock()

	if tc, ok := m.data.Tables[tableName]; ok {
		return tc
	}
	tc := &TableCheckpoint{
		TableName: tableName,
		State:     StatePending,
		RowsTotal: totalRows,
	}
	m.data.Tables[tableName] = tc
	m.save()
	return tc
}

func (m *Manager) UpdateTable(tableName string, fn func(tc *TableCheckpoint)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tc, ok := m.data.Tables[tableName]
	if !ok {
		return fmt.Errorf("table %s not found in checkpoint", tableName)
	}
	fn(tc)
	m.save()
	return nil
}

func (m *Manager) MarkTableRunning(tableName string) error {
	return m.UpdateTable(tableName, func(tc *TableCheckpoint) {
		tc.State = StateRunning
		tc.StartedAt = time.Now()
	})
}

func (m *Manager) MarkTableCompleted(tableName string, rowsDone int64) error {
	return m.UpdateTable(tableName, func(tc *TableCheckpoint) {
		// Sticky completion (mirrors MarkSchemaTableCompleted): once a
		// table is completed, a later re-mark only converges the row
		// counters — State stays completed and the FIRST completion's
		// FinishedAt is never rewritten, so per-table durations stay real.
		if tc.State != StateCompleted {
			tc.State = StateCompleted
			tc.FinishedAt = time.Now()
		}
		// Raise the denominator if more rows were actually exported than the
		// registered estimate, so aggregated progress never exceeds 100%.
		if rowsDone > tc.RowsTotal {
			tc.RowsTotal = rowsDone
		}
		tc.RowsDone = rowsDone
	})
}

func (m *Manager) MarkTableFailed(tableName string, errStr string) error {
	return m.UpdateTable(tableName, func(tc *TableCheckpoint) {
		tc.State = StateFailed
		tc.Error = errStr
		tc.FinishedAt = time.Now()
	})
}

// RegisterSchemaTables marks all given tables as registered for the SCHEMA
// phase (tables_total becomes correct immediately). It never touches State /
// RowsDone / RowsTotal — only SchemaState — so data-phase resume and row
// denominators are unaffected (see TableCheckpoint.SchemaState).
func (m *Manager) RegisterSchemaTables(names []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range names {
		tc, ok := m.data.Tables[name]
		if !ok {
			tc = &TableCheckpoint{TableName: name, State: StatePending}
			m.data.Tables[name] = tc
		}
		tc.SchemaState = StatePending
	}
	m.save()
	return nil
}

// MarkSchemaTableCompleted marks one table's schema DDL as applied.
// Idempotent and sticky-on-failure: completed is not rewritten, and a
// FAILED table is never flipped back to completed by a later successful
// statement on the same table (e.g. CREATE INDEX failed, deferred
// ALTER/INDEX then succeeded) — the failure must stay visible. Resetting
// to pending only happens via RegisterSchemaTables on resume.
func (m *Manager) MarkSchemaTableCompleted(tableName string) error {
	return m.UpdateTable(tableName, func(tc *TableCheckpoint) {
		if tc.SchemaState == StateCompleted || tc.SchemaState == StateFailed {
			return
		}
		tc.SchemaState = StateCompleted
	})
}

// MarkSchemaTableFailed marks one table's schema DDL as failed.
// No completed guard on purpose: a table whose CREATE TABLE succeeded
// can still end failed when a later INDEX/ALTER on it failed — failed is
// the honest terminal state ("the table's full DDL set did not land").
func (m *Manager) MarkSchemaTableFailed(tableName string, errStr string) error {
	return m.UpdateTable(tableName, func(tc *TableCheckpoint) {
		tc.SchemaState = StateFailed
	})
}

func (m *Manager) UpdateTableProgress(tableName string, rowsDone int64, bytesDone int64) error {
	return m.UpdateTable(tableName, func(tc *TableCheckpoint) {
		// Keep the denominator >= rowsDone so progress stays <= 100%.
		if rowsDone > tc.RowsTotal {
			tc.RowsTotal = rowsDone
		}
		tc.RowsDone = rowsDone
		tc.BytesDone = bytesDone
	})
}

// SetImportedTables records the number of tables already imported by
// tidb-lightning (deduplicated count) and persists it.
func (m *Manager) SetImportedTables(n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.ImportedTables = n
	m.save()
	return nil
}

// GetImportedTables returns the recorded number of imported tables.
func (m *Manager) GetImportedTables() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data.ImportedTables
}

// SetImportMode records the active data-import mode (lightning / stream)
// and persists it, so the progress poller picks the matching formula even
// across process restarts and the lightning→stream fallback chain.
func (m *Manager) SetImportMode(mode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.ImportMode = mode
	m.save()
	return nil
}

// GetImportMode returns the recorded import mode ("" for historical
// checkpoints written before the field existed).
func (m *Manager) GetImportMode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data.ImportMode
}

func (m *Manager) GetPhase() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data.Phase
}

func (m *Manager) SetPhase(phase string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.Phase = phase
	m.save()
	return nil
}

func (m *Manager) SetPhaseWithReload(phase string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	m.data.Phase = phase
	m.save()
	return nil
}

// InitPhases seeds the phase state machine: every given phase starts
// pending, except those flagged skip which are immediately recorded as
// skipped (they will never run in this pipeline).
func (m *Manager) InitPhases(skip map[string]bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.Phases == nil {
		m.data.Phases = make(map[string]*PhaseRecord)
	}
	for name, isSkip := range skip {
		st := StatePending
		if isSkip {
			st = StateSkipped
		}
		m.data.Phases[name] = &PhaseRecord{Name: name, Status: st}
	}
	m.save()
	return nil
}

// StartPhase marks a phase running.
func (m *Manager) StartPhase(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.Phases == nil {
		m.data.Phases = make(map[string]*PhaseRecord)
	}
	rec, ok := m.data.Phases[name]
	if !ok {
		rec = &PhaseRecord{Name: name}
		m.data.Phases[name] = rec
	}
	rec.Status = StateRunning
	rec.StartedAt = time.Now()
	rec.FinishedAt = time.Time{}
	rec.Error = ""
	rec.Warn = false
	m.save()
	return nil
}

// FinishPhase records the phase outcome: err == nil → completed (warn
// flags "completed with warnings"), err != nil → failed with the error.
func (m *Manager) FinishPhase(name string, err error, warn bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finishPhaseLocked(name, err, warn)
	m.save()
	return nil
}

// FinishPhaseWithReload is the dual-instance-safe variant of FinishPhase:
// it reloads the checkpoint from disk before recording the outcome, so a
// manager holding a stale in-memory copy (e.g. the orchestrator's manager
// while the data migrator writes through its own instance) does not
// overwrite the data-plane progress already persisted by the other writer.
// Mirrors SetPhaseWithReload.
func (m *Manager) FinishPhaseWithReload(name string, err error, warn bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.load()
	m.finishPhaseLocked(name, err, warn)
	m.save()
	return nil
}

func (m *Manager) finishPhaseLocked(name string, err error, warn bool) {
	if m.data.Phases == nil {
		m.data.Phases = make(map[string]*PhaseRecord)
	}
	rec, ok := m.data.Phases[name]
	if !ok {
		rec = &PhaseRecord{Name: name, StartedAt: time.Now()}
		m.data.Phases[name] = rec
	}
	rec.FinishedAt = time.Now()
	rec.Warn = warn
	if err != nil {
		rec.Status = StateFailed
		rec.Error = err.Error()
	} else {
		rec.Status = StateCompleted
		rec.Error = ""
	}
}

// SetSubPhase records the current sub-step of a phase (e.g. schema-build /
// schema-execute / data-export / data-import) for precise UI labels.
func (m *Manager) SetSubPhase(phase, sub string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.Phases == nil {
		m.data.Phases = make(map[string]*PhaseRecord)
	}
	rec, ok := m.data.Phases[phase]
	if !ok {
		rec = &PhaseRecord{Name: phase, Status: StateRunning, StartedAt: time.Now()}
		m.data.Phases[phase] = rec
	}
	rec.SubPhase = sub
	m.save()
	return nil
}

// GetPhases returns a copy of the phase state machine (nil map for
// historical checkpoints written before the field existed).
func (m *Manager) GetPhases() map[string]*PhaseRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]*PhaseRecord, len(m.data.Phases))
	for k, v := range m.data.Phases {
		cp := *v
		result[k] = &cp
	}
	return result
}

func (m *Manager) IsTableCompleted(tableName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	return ok && tc.State == StateCompleted
}

func (m *Manager) GetPendingTables() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var tables []string
	for name, tc := range m.data.Tables {
		if tc.State != StateCompleted {
			tables = append(tables, name)
		}
	}
	return tables
}

func (m *Manager) GetTable(tableName string) (*TableCheckpoint, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	return tc, ok
}

func (m *Manager) GetAllTables() map[string]*TableCheckpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]*TableCheckpoint, len(m.data.Tables))
	for k, v := range m.data.Tables {
		result[k] = v
	}
	return result
}

func (m *Manager) ResetAllTables() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tc := range m.data.Tables {
		tc.State = StatePending
		tc.RowsDone = 0
		tc.BytesDone = 0
		tc.SchemaState = ""
	}
	m.save()
}

func (m *Manager) Summary() (completed, failed, pending, running int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tc := range m.data.Tables {
		switch tc.State {
		case StateCompleted:
			completed++
		case StateFailed:
			failed++
		case StateRunning:
			running++
		default:
			pending++
		}
	}
	return
}

func (m *Manager) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.Tables = make(map[string]*TableCheckpoint)
	m.data.Phase = ""
	m.data.Phases = nil
	m.save()
	return nil
}

func (m *Manager) IsChunkCompleted(tableName string, chunkIndex int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	if !ok || tc.Chunks == nil {
		return false
	}
	chunk, ok := tc.Chunks[chunkIndex]
	return ok && chunk.State == StateCompleted
}

func (m *Manager) GetChunkProgress(tableName string, chunkIndex int) (rows, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	if !ok || tc.Chunks == nil {
		return 0, 0
	}
	chunk, ok := tc.Chunks[chunkIndex]
	if !ok {
		return 0, 0
	}
	return chunk.RowCount, chunk.ByteCount
}

func (m *Manager) MarkChunkCompleted(tableName string, chunkIndex int, rows, bytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	if !ok {
		return fmt.Errorf("table %s not found in checkpoint", tableName)
	}
	if tc.Chunks == nil {
		tc.Chunks = make(map[int]*ChunkCheckpoint)
	}
	tc.Chunks[chunkIndex] = &ChunkCheckpoint{
		Index:      chunkIndex,
		State:      StateCompleted,
		RowCount:   rows,
		ByteCount:  bytes,
		FinishedAt: time.Now(),
	}
	return nil
}

func (m *Manager) MarkChunkFailed(tableName string, chunkIndex int, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	if !ok {
		return fmt.Errorf("table %s not found in checkpoint", tableName)
	}
	if tc.Chunks == nil {
		tc.Chunks = make(map[int]*ChunkCheckpoint)
	}
	tc.Chunks[chunkIndex] = &ChunkCheckpoint{
		Index:      chunkIndex,
		State:      StateFailed,
		Error:      errMsg,
		FinishedAt: time.Now(),
	}
	return nil
}

func (m *Manager) GetPendingChunks(tableName string) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	if !ok || tc.Chunks == nil {
		return nil
	}
	var pending []int
	for idx, chunk := range tc.Chunks {
		if chunk.State != StateCompleted {
			pending = append(pending, idx)
		}
	}
	return pending
}

func (m *Manager) InitChunk(tableName string, chunkIndex int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	if !ok {
		return
	}
	if tc.Chunks == nil {
		tc.Chunks = make(map[int]*ChunkCheckpoint)
	}
	if _, exists := tc.Chunks[chunkIndex]; !exists {
		tc.Chunks[chunkIndex] = &ChunkCheckpoint{
			Index: chunkIndex,
			State: StatePending,
		}
	}
}

func (m *Manager) ResetChunks(tableName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.data.Tables[tableName]
	if !ok {
		return
	}
	tc.Chunks = nil
}
