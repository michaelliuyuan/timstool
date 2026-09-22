package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/michaelliuyuan/timstool/internal/common"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"github.com/michaelliuyuan/timstool/internal/validator"
	"go.uber.org/zap"
)

// Standalone data-comparison tasks (独立数据比对): run Validator against
// explicit source/target connections without any migration. Tasks are stored
// under dataDir/compare/{id}/ (task.json + report.json), separate from the
// migration task store; passwords are NEVER persisted in plaintext.

const (
	CompareStatusRunning   = "running"
	CompareStatusCompleted = "completed"
	CompareStatusFailed    = "failed"
	CompareStatusCancelled = "cancelled"
)

// CompareTaskRequest is the POST /compare/tasks body.
type CompareTaskRequest struct {
	Name              string              `json:"name"`
	Source            config.SourceConfig `json:"source"`
	Target            config.TargetConfig `json:"target"`
	Mode              string              `json:"mode"` // quick | sample | checksum
	SampleRatio       float64             `json:"sample_ratio"`
	ChecksumChunkSize int64               `json:"checksum_chunk_size"`
	ChecksumParallel  int                 `json:"checksum_parallel"`
	Parallel          int                 `json:"parallel"`
	Tables            []string            `json:"tables"` // empty = all tables
}

// CompareTask is the persisted (password-redacted) compare task state.
type CompareTask struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Status            string              `json:"status"`
	Source            config.SourceConfig `json:"source"` // password redacted on disk
	Target            config.TargetConfig `json:"target"` // password redacted on disk
	Mode              string              `json:"mode"`
	SampleRatio       float64             `json:"sample_ratio"`
	ChecksumChunkSize int64               `json:"checksum_chunk_size"`
	ChecksumParallel  int                 `json:"checksum_parallel"`
	Parallel          int                 `json:"parallel"`
	Tables            []string            `json:"tables"`
	TablesDone        int                 `json:"tables_done"`
	TablesTotal       int                 `json:"tables_total"`
	CurrentTable      string              `json:"current_table,omitempty"`
	Error             string              `json:"error,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	StartedAt         *time.Time          `json:"started_at,omitempty"`
	FinishedAt        *time.Time          `json:"finished_at,omitempty"`
}

// compareState guards the single-running-compare invariant.
type compareState struct {
	mu        sync.Mutex
	cancel    context.CancelFunc // non-nil while a compare task is running
	runningID string
}

func (s *Server) compareDir(id string) string {
	return filepath.Join(s.dataDir, "compare", id)
}

// recoverStaleCompares marks any on-disk compare task still in "running"
// state as failed — the process died mid-run, so it can never finish or be
// cancelled (M3: otherwise the orphan sticks in the history list forever).
// Called once at server startup, before any handler runs.
func (s *Server) recoverStaleCompares() {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "compare"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.loadCompareTask(e.Name())
		if err != nil || t == nil || t.Status != CompareStatusRunning {
			continue
		}
		t.Status = CompareStatusFailed
		t.Error = "interrupted by service restart"
		fin := time.Now()
		t.FinishedAt = &fin
		if err := s.persistCompareTask(t); err != nil {
			zap.L().Warn("failed to recover stale compare task", zap.String("id", t.ID), zap.Error(err))
		}
	}
}

// persistCompareTask writes task.json with passwords redacted (atomic temp+rename).
func (s *Server) persistCompareTask(task *CompareTask) error {
	dir := s.compareDir(task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	redacted := *task
	redacted.Source.Password = ""
	redacted.Target.Password = ""
	data, err := json.MarshalIndent(&redacted, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "task-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, "task.json"))
}

func (s *Server) handleCreateCompare(w http.ResponseWriter, r *http.Request) {
	var req CompareTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Source.Host == "" || req.Target.Host == "" {
		s.writeError(w, http.StatusBadRequest, "source and target host are required")
		return
	}
	switch req.Mode {
	case "":
		req.Mode = "sample"
	case "quick", "sample", "checksum":
	default:
		s.writeError(w, http.StatusBadRequest, "mode must be one of quick/sample/checksum")
		return
	}
	if req.Name == "" {
		req.Name = "Compare " + time.Now().Format("2006-01-02 15:04:05")
	}

	// B2: single-running invariant — check + persist + claim must be one
	// atomic critical section, otherwise two concurrent POSTs both pass the
	// check and run simultaneously.
	s.compare.mu.Lock()
	if s.compare.cancel != nil {
		s.compare.mu.Unlock()
		s.writeError(w, http.StatusConflict, "a comparison task is already running")
		return
	}

	now := time.Now()
	task := &CompareTask{
		ID:                uuid.New().String()[:8],
		Name:              req.Name,
		Status:            CompareStatusRunning,
		Source:            req.Source,
		Target:            req.Target,
		Mode:              req.Mode,
		SampleRatio:       req.SampleRatio,
		ChecksumChunkSize: req.ChecksumChunkSize,
		ChecksumParallel:  req.ChecksumParallel,
		Parallel:          req.Parallel,
		Tables:            req.Tables,
		CreatedAt:         now,
		StartedAt:         &now, // M2: set before the goroutine spawns (no data race on the response)
	}
	if err := s.persistCompareTask(task); err != nil {
		s.compare.mu.Unlock()
		s.writeError(w, http.StatusInternalServerError, "failed to persist compare task: "+err.Error())
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.compare.cancel = cancel
	s.compare.runningID = task.ID
	s.compare.mu.Unlock()

	// Passwords live only in memory for the run; persisted copies and the
	// HTTP response (M1) are redacted. Copy the response BEFORE spawning so
	// no field is shared with the goroutine at all.
	resp := *task
	resp.Source.Password = ""
	resp.Target.Password = ""

	go s.runCompare(ctx, task)

	s.writeJSON(w, http.StatusCreated, &resp)
}

// buildCompareRunParams derives the validator inputs from a compare task
// (L1: sample ratio must reach the sampling path; <=0 falls back to 0.01).
func buildCompareRunParams(task *CompareTask, reportFile string) (float64, common.ValidateOpts, config.CompareConfig) {
	sampleRatio := task.SampleRatio
	if sampleRatio <= 0 {
		sampleRatio = 0.01
	}
	opts := common.ValidateOpts{
		Mode:        task.Mode,
		SampleRatio: sampleRatio,
		Tables:      task.Tables,
		ReportFile:  reportFile,
	}
	compareCfg := config.CompareConfig{
		CompareMode:       task.Mode,
		SampleRatio:       sampleRatio,
		ChecksumChunkSize: task.ChecksumChunkSize,
		ChecksumParallel:  task.ChecksumParallel,
	}
	return sampleRatio, opts, compareCfg
}

func (s *Server) runCompare(ctx context.Context, task *CompareTask) {
	logger := zap.L()
	logger.Info("starting standalone comparison",
		zap.String("compare_id", task.ID), zap.String("mode", task.Mode))

	var taskMu sync.Mutex // serializes progress updates to task/persist/WS

	finish := func(status, errMsg string) {
		taskMu.Lock()
		defer taskMu.Unlock()
		task.Status = status
		task.Error = errMsg
		task.CurrentTable = ""
		fin := time.Now()
		task.FinishedAt = &fin
		if err := s.persistCompareTask(task); err != nil {
			logger.Warn("failed to persist compare task", zap.Error(err))
		}
		// B2: only the task that owns the running slot may clear it.
		s.compare.mu.Lock()
		if s.compare.runningID == task.ID {
			s.compare.cancel = nil
			s.compare.runningID = ""
		}
		s.compare.mu.Unlock()
		s.BroadcastProgress(task.ID, map[string]interface{}{
			"type": "compare", "status": task.Status,
			"tables_done": task.TablesDone, "tables_total": task.TablesTotal,
		})
	}

	v := validator.NewValidator(config.Config{})
	v.OnTableDone(func(done, total int, tr reporter.TableReport) {
		taskMu.Lock()
		defer taskMu.Unlock()
		task.TablesDone = done
		task.TablesTotal = total
		task.CurrentTable = tr.TableName
		if err := s.persistCompareTask(task); err != nil {
			logger.Warn("failed to persist compare progress", zap.Error(err))
		}
		s.BroadcastProgress(task.ID, map[string]interface{}{
			"type": "compare", "status": task.Status,
			"tables_done": done, "tables_total": total, "current_table": tr.TableName,
		})
	})

	_, validateOpts, compareCfg := buildCompareRunParams(task,
		filepath.Join(s.compareDir(task.ID), "report.json"))
	report, err := v.RunWithDSNs(ctx, task.Source.DSN(), task.Target.DSN(),
		task.Source.Schema, compareCfg, task.Parallel, validateOpts)
	// B1: check ctx FIRST regardless of err — Run swallows per-table ctx
	// cancellations into failed table reports and returns (rpt, nil), so a
	// mid-run cancel must not be reported as "completed".
	if ctx.Err() != nil {
		finish(CompareStatusCancelled, "cancelled")
		return
	}
	if err != nil {
		finish(CompareStatusFailed, err.Error())
		return
	}

	// The report was saved by Run via ReportFile; announce completion.
	logger.Info("standalone comparison finished",
		zap.String("compare_id", task.ID),
		zap.String("overall", string(report.Status)))
	finish(CompareStatusCompleted, "")
}

func (s *Server) handleListCompares(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(s.dataDir, "compare")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeJSON(w, http.StatusOK, []*CompareTask{})
			return
		}
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tasks := []*CompareTask{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.loadCompareTask(e.Name())
		if err != nil || t == nil {
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	s.writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) loadCompareTask(id string) (*CompareTask, error) {
	data, err := os.ReadFile(filepath.Join(s.compareDir(id), "task.json"))
	if err != nil {
		return nil, err
	}
	var t CompareTask
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Server) handleGetCompare(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	t, err := s.loadCompareTask(id)
	if err != nil || t == nil {
		s.writeError(w, http.StatusNotFound, "compare task not found")
		return
	}
	s.writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleCompareReport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	data, err := os.ReadFile(filepath.Join(s.compareDir(id), "report.json"))
	if err != nil {
		s.writeError(w, http.StatusNotFound, "compare report not found")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) handleCancelCompare(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	s.compare.mu.Lock()
	cancel := s.compare.cancel
	running := s.compare.runningID
	s.compare.mu.Unlock()
	if cancel == nil || running != id {
		s.writeError(w, http.StatusConflict, "compare task is not running")
		return
	}
	cancel()
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "cancelling", "task_id": id})
}

func (s *Server) handleDeleteCompare(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	s.compare.mu.Lock()
	running := s.compare.runningID == id
	s.compare.mu.Unlock()
	if running {
		s.writeError(w, http.StatusConflict, "cannot delete a running compare task; cancel it first")
		return
	}
	if err := os.RemoveAll(s.compareDir(id)); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
