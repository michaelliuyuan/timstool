package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/michaelliuyuan/timstool/internal/store"
)

// smokeAddrForLive mirrors the mongocompare live harness gating: the env var
// holds host[:port]; unset means skip.
func smokeAddrForLive(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("TIMSTOOL_MONGO_SMOKE")
	if addr == "" {
		t.Skip("TIMSTOOL_MONGO_SMOKE not set; live tests require the standing mongod")
	}
	return addr
}

// uniqueLiveDB follows the M2 harness isolation convention
// (md_*_<pid>x<seq> per-process per-test namespaces).
var liveSeq int

func uniqueLiveDB(t *testing.T, base string) string {
	liveSeq++
	return fmt.Sprintf("%s_%dx%d", base, os.Getpid(), liveSeq)
}

// TestCompareTaskEndToEndLive drives the full M3 wiring against the standing
// smoke mongod: create task → start (through the shared handleStartTask
// routing) → poll to terminal state → verify result JSON and summary.json.
func TestCompareTaskEndToEndLive(t *testing.T) {
	addr := smokeAddrForLive(t)
	dataDir := t.TempDir()
	st, err := store.NewStore(dataDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close() // release the sqlite file so t.TempDir cleanup succeeds
	s := &Server{
		store:        st,
		runningTasks: make(map[string]context.CancelFunc),
		dataDir:      dataDir,
		logCollector: NewLogCollector(),
		logCores:     make(map[string]*TaskLogCore),
	}

	// Seed two fixture databases differing by one document.
	seedURI := "mongodb://" + addr
	srcDB := uniqueLiveDB(t, "md_wtask_src")
	dstDB := uniqueLiveDB(t, "md_wtask_dst")
	seedLiveCompareFixtures(t, seedURI, srcDB, dstDB)
	body := fmt.Sprintf(`{
		"source": {"uri": %q, "database": %q},
		"target": {"uri": %q, "database": %q},
		"buckets": 8
	}`, seedURI, srcDB, seedURI, dstDB)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/compare/tasks", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	s.handleCreateCompareTask(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var task store.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || task.ID == "" {
		t.Fatalf("create response: err=%v body=%s", err, rec.Body.String())
	}

	srec := startTaskChi(t, s, task.ID)
	if srec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", srec.Code, srec.Body.String())
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		got, err := st.GetTask(task.ID)
		if err != nil || got == nil {
			t.Fatalf("get task: %v", err)
		}
		if got.Status != store.TaskStatusRunning && got.Status != store.TaskStatusCreated {
			if got.Status != store.TaskStatusCompleted {
				t.Fatalf("terminal status=%s err=%s", got.Status, got.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not finish, status=%s", got.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}

	got, _ := st.GetTask(task.ID)
	var result map[string]any
	if err := json.Unmarshal([]byte(got.ResultJSON), &result); err != nil {
		t.Fatalf("result json: %v (%s)", err, got.ResultJSON)
	}
	if result["overall"] != "diff" {
		t.Errorf("overall=%v, want diff (fixtures differ by one doc)", result["overall"])
	}
	counts, _ := result["counts"].(map[string]any)
	if counts == nil || int(counts["diff"].(float64)) != 1 {
		t.Errorf("counts=%v, want diff=1", counts)
	}
	summaryPath, _ := result["summary_path"].(string)
	if summaryPath == "" {
		t.Fatalf("summary_path missing: %v", result)
	}
	if _, err := os.Stat(summaryPath); err != nil {
		t.Fatalf("summary.json not written: %v", err)
	}
	if want := filepath.Join(dataDir, "compare", task.ID, "summary.json"); summaryPath != want {
		t.Errorf("summary_path=%s, want %s", summaryPath, want)
	}
}

// startTaskChi invokes handleStartTask with a chi-populated {taskID} param.
func startTaskChi(t *testing.T, s *Server, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskID", taskID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+taskID+"/start", nil).
		WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.handleStartTask(rec, req)
	return rec
}

// seedLiveCompareFixtures creates src/db with 10 docs and dst with the same
// 10 docs minus one modified field, so the compare must report exactly one
// diff collection. Both fixture databases are owned by this test and dropped
// on cleanup (M2 harness convention: seed() declares ownership).
func seedLiveCompareFixtures(t *testing.T, uri, srcDB, dstDB string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(context.Background())
		t.Fatalf("ping %s: %v", uri, err)
	}
	// NOTE: the client stays connected until cleanup — t.Cleanup runs after
	// the helper returns, so a deferred Disconnect here would silently break
	// the cleanup drops (the earlier leak, review finding C).
	// NOTE: t.Cleanup runs LIFO — the client disconnect must be registered
	// FIRST (runs last) so the cleanup drops see a live connection (the
	// earlier leak, review finding C).
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	for _, db := range []string{srcDB, dstDB} {
		db := db // per-iteration capture for the cleanup closure
		_ = client.Database(db).Drop(ctx)
		t.Cleanup(func() { _ = client.Database(db).Drop(context.Background()) })
	}
	var srcDocs, dstDocs []any
	for i := 0; i < 10; i++ {
		srcDocs = append(srcDocs, bson.D{{Key: "_id", Value: i}, {Key: "city", Value: "aa"}, {Key: "n", Value: i}})
		d := bson.D{{Key: "_id", Value: i}, {Key: "city", Value: "aa"}, {Key: "n", Value: i}}
		if i == 7 {
			d[1].Value = "zz" // the one modified doc
		}
		dstDocs = append(dstDocs, d)
	}
	if _, err := client.Database(srcDB).Collection("c1").InsertMany(ctx, srcDocs); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Database(dstDB).Collection("c1").InsertMany(ctx, dstDocs); err != nil {
		t.Fatal(err)
	}
}
