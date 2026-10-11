// compare_handler.go wires the MongoDB compare engine (M2, internal/
// mongocompare) into the task layer as a standalone compare task type (M3).
//
// Design (thread seq=65, review-approved direction):
//   - compare tasks do NOT go through orchestrator — this handler drives
//     mongocompare.CompareDatabases directly, mirroring the M1
//     source_handler.go purely-additive pattern (mysql/pg paths untouched).
//   - task kind is encoded inside ConfigJSON ({"kind":"compare", ...}); the
//     store schema is not extended (red line: no store/orchestrator edits).
//   - v1 semantics: one source database vs one target database (src/dst db
//     names may differ, via source.database/target.database). Whole-instance
//     compare with database mapping is left for a later milestone.
//   - resume semantics (engine doc, engine.go): checkpoint reuse assumes the
//     data has not changed and bucket boundaries match; changes within the
//     same boundaries are surfaced as diffs (usually what is wanted).
//     AlgoVersion mismatch invalidates checkpoints engine-side; the task
//     layer only passes Resume through.
//   - OutDir is mandatory at this layer (engine skips all persistence when
//     it is empty, which would silently break resume): <dataDir>/compare/
//     <taskID>. It intentionally does NOT live under .checkpoint/<taskID>,
//     which handleStartTask wipes on every start.
//
// Ops note (seq=61-①): crashed live-test processes can leave orphan fixture
// databases named md_*_<pid>x<seq> (~15MB each) on shared smoke containers;
// the harness sweep deliberately never touches suffixed names, so orphans
// must be dropped manually.
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/michaelliuyuan/timstool/internal/mongocompare"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// compareEndpoint is one side of the comparison. Either a full URI or the
// discrete fields may be supplied; discrete fields win when both are given.
type compareEndpoint struct {
	URI      string `json:"uri"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	AuthDB   string `json:"auth_db"`
	Database string `json:"database"`
}

// clientOptions assembles driver options for this endpoint. Discrete fields
// build structured options (M1 pattern, internal/source/mongo/source.go
// buildClientOptions: "no URI string escaping to get wrong" — passwords
// containing @ : / % must not be string-concatenated into a URI). A bare URI
// is passed through verbatim; its escaping is the caller's responsibility.
func (e compareEndpoint) clientOptions() (*options.ClientOptions, error) {
	if e.Host != "" {
		host := e.Host
		if e.Port != 0 {
			host = fmt.Sprintf("%s:%d", host, e.Port)
		}
		opts := options.Client().SetHosts([]string{host})
		if e.User != "" {
			cred := options.Credential{Username: e.User}
			if e.Password != "" {
				cred.Password = e.Password
			}
			// authSource only when explicitly set, preserving the driver
			// default (admin / connection database) otherwise (M1 parity).
			if authDB := strings.TrimSpace(e.AuthDB); authDB != "" {
				cred.AuthSource = authDB
			}
			opts.SetAuth(cred)
		}
		return opts, nil
	}
	if e.URI != "" {
		return options.Client().ApplyURI(e.URI), nil
	}
	return nil, fmt.Errorf("endpoint needs either host or uri")
}

// compareTaskSpec is the persisted compare-task configuration (stored under
// ConfigJSON key "compare"; all JSON tags snake_case per task-API convention).
// Resume is intentionally NOT persisted here: a fresh start wipes the task's
// OutDir (a retry after cancel/failure wants a clean compare); resume is an
// explicit start-time parameter for the checkpoint-continuation case only
// (review ruling seq=68, semantic pitfall 1).
type compareTaskSpec struct {
	Source          compareEndpoint `json:"source"`
	Target          compareEndpoint `json:"target"`
	Buckets         int             `json:"buckets"`
	ParallelColls   int             `json:"parallel_colls"`
	ParallelBuckets int             `json:"parallel_buckets"`
	BatchSize       int             `json:"batch_size"`
	Levels          []string        `json:"levels"`
}

// compareTaskEnvelope is the ConfigJSON shape for compare tasks. The kind
// discriminator lets handleStartTask/handleResumeTask route without touching
// the store schema.
type compareTaskEnvelope struct {
	Kind    string          `json:"kind"`
	Compare compareTaskSpec `json:"compare"`
}

const compareTaskKind = "compare"

// parseCompareTask returns the spec when cfgJSON is a compare task.
func parseCompareTask(cfgJSON string) (*compareTaskSpec, bool) {
	var env compareTaskEnvelope
	if err := json.Unmarshal([]byte(cfgJSON), &env); err != nil {
		return nil, false
	}
	if env.Kind != compareTaskKind {
		return nil, false
	}
	return &env.Compare, true
}

// handleCreateCompareTask creates a standalone mongodb compare task.
// Route: POST /api/v1/compare/tasks (registered next to POST /tasks).
func (s *Server) handleCreateCompareTask(w http.ResponseWriter, r *http.Request) {
	var req compareTaskSpec
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := req.Source.clientOptions(); err != nil {
		s.writeError(w, http.StatusBadRequest, "source: "+err.Error())
		return
	}
	if _, err := req.Target.clientOptions(); err != nil {
		s.writeError(w, http.StatusBadRequest, "target: "+err.Error())
		return
	}
	if req.Source.Database == "" {
		s.writeError(w, http.StatusBadRequest, "source.database is required")
		return
	}
	if req.Target.Database == "" {
		req.Target.Database = req.Source.Database
	}
	for _, l := range req.Levels {
		switch mongocompare.Level(l) {
		case mongocompare.LevelSchema, mongocompare.LevelIndex, mongocompare.LevelCount, mongocompare.LevelHash, mongocompare.LevelDiff:
		default:
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown level %q", l))
			return
		}
	}

	task := &store.Task{
		ID:   newTaskID(),
		Name: fmt.Sprintf("MongoDB Compare %s", time.Now().Format("2006-01-02 15:04:05")),
	}
	env := compareTaskEnvelope{Kind: compareTaskKind, Compare: req}
	cfgBytes, err := json.Marshal(env)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	task.ConfigJSON = string(cfgBytes)

	if err := s.store.CreateTask(task); err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, task)
}

// startCompareTask is the compare branch of task start/resume. A fresh start
// wipes the task's OutDir (retry wants a clean compare); resume=true keeps
// engine checkpoints for continuation (review ruling seq=68).
func (s *Server) startCompareTask(ctx context.Context, taskID string, spec *compareTaskSpec, resume bool) {
	if !resume {
		_ = os.RemoveAll(filepath.Join(s.dataDir, "compare", taskID))
	}
	go s.runCompare(ctx, taskID, spec, resume)
}

func (s *Server) runCompare(ctx context.Context, taskID string, spec *compareTaskSpec, resume bool) {
	s.logCollector.GetBuffer(taskID)
	s.logCollector.Append(taskID, "INFO", "MongoDB compare task started", "")

	outDir := filepath.Join(s.dataDir, "compare", taskID)

	srcClient, err := connectMongo(ctx, spec.Source)
	if err != nil {
		s.failCompare(taskID, "connect source: "+err.Error())
		return
	}
	defer srcClient.Disconnect(context.Background())
	dstClient, err := connectMongo(ctx, spec.Target)
	if err != nil {
		s.failCompare(taskID, "connect target: "+err.Error())
		return
	}
	defer dstClient.Disconnect(context.Background())

	cfg := mongocompare.Config{
		Buckets:         spec.Buckets,
		ParallelColls:   spec.ParallelColls,
		ParallelBuckets: spec.ParallelBuckets,
		BatchSize:       spec.BatchSize,
		Resume:          resume,
		OutDir:          outDir,
	}
	for _, l := range spec.Levels {
		cfg.Levels = append(cfg.Levels, mongocompare.Level(l))
	}

	// Lightweight progress: poll completed checkpoint files until the run
	// finishes (the engine has no streaming callback; totals are only known
	// in the final summary, so mid-run progress reports counts, not ratio).
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	go s.pollCompareProgress(pollCtx, taskID, outDir)

	summary, err := mongocompare.CompareDatabases(ctx, srcClient, dstClient, spec.Source.Database, spec.Target.Database, cfg)
	pollCancel()

	if ctx.Err() == context.Canceled {
		s.logCollector.Append(taskID, "WARN", "MongoDB compare task cancelled", "")
		s.store.UpdateTaskStatus(taskID, store.TaskStatusCancelled)
		return
	}
	if err != nil {
		s.failCompare(taskID, "compare failed: "+err.Error())
		return
	}

	result := map[string]any{
		"kind":         compareTaskKind,
		"overall":      summary.Overall,
		"counts":       summary.Counts,
		"duration_sec": summary.DurationSec,
		"summary_path": filepath.Join(outDir, "summary.json"),
		"diff_dir":     filepath.Join(outDir, "diff"),
	}
	s.store.SetTaskResult(taskID, result)

	total := len(summary.Collections)
	done := summary.Counts["equal"] + summary.Counts["diff"] + summary.Counts["error"] + summary.Counts["ttl_skip"]
	_ = s.store.UpdateTaskProgress(taskID, "compare", 1.0, done, total, int64(done), int64(total))

	status := store.TaskStatusCompleted
	if summary.Overall == mongocompare.OverallError {
		status = store.TaskStatusFailed
	}
	s.logCollector.Append(taskID, "INFO", fmt.Sprintf("MongoDB compare finished: overall=%s counts=%v", summary.Overall, summary.Counts), "")
	s.store.UpdateTaskStatus(taskID, status)
}

func (s *Server) failCompare(taskID, msg string) {
	s.logCollector.Append(taskID, "ERROR", msg, "")
	s.store.SetTaskError(taskID, msg)
}

// pollCompareProgress reports completed bucket-checkpoint files as a coarse
// heartbeat every 2s (mirrors pollProgress cadence).
func (s *Server) pollCompareProgress(ctx context.Context, taskID, outDir string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			entries, err := os.ReadDir(filepath.Join(outDir, ".checkpoints"))
			if err != nil {
				continue
			}
			done := len(entries)
			s.BroadcastProgress(taskID, map[string]any{
				"phase":        "compare",
				"progress":     0, // ratio unknown until summary; clients treat 0 as indeterminate-running
				"buckets_done": done,
			})
		}
	}
}

func connectMongo(ctx context.Context, e compareEndpoint) (*mongo.Client, error) {
	opts, err := e.clientOptions()
	if err != nil {
		return nil, err
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		client.Disconnect(context.Background())
		return nil, err
	}
	return client, nil
}

// newTaskID mirrors the id scheme used by handleCreateTask.
func newTaskID() string {
	return uuid.New().String()[:8]
}
