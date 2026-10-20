package webapi

import (
	"context"
	"embed"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/michaelliuyuan/timstool/internal/store"
)

// MC-10 ①③ anchors (seq=259 card, post gate removal): the mongocompare
// create route + compare-kind lifecycle branches run ungated but audited;
// the start/resume param re-validation stays fail-loud. The compare audit
// trail lands in mc-ops.jsonl — never in the CDC trail.

const mc10ValidSpec = `{"source":{"uri":"mongodb://127.0.0.1:1","database":"a"},"target":{"uri":"mongodb://127.0.0.1:1","database":"b"}}`

func mc10BareServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(st, "127.0.0.1", 0, dir, embed.FS{}, nil, "", time.Minute)
	return s, st, dir
}

func mc10Post(s *Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

// Create route: no token anywhere — the route dispatches to the create
// handler (201) and the success outcome lands in the compare trail.
func TestMC10CompareCreateUngated(t *testing.T) {
	s, _, dir := mc10BareServer(t)

	if rec := mc10Post(s, "/api/v1/mongocompare/tasks", mc10ValidSpec); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s, want 201", rec.Code, rec.Body.String())
	}
	assertAuditLine(t, dir, "mc-ops.jsonl", `"action":"mongocompare.create"`, `"result":"success"`)

	// trail separation: compare ops never touch the CDC audit file.
	if _, err := os.Stat(filepath.Join(dir, "audit", "cdc-ops.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("cdc-ops.jsonl written by compare ops: err=%v", err)
	}
}

func mc10SeedTask(t *testing.T, st *store.Store, id, cfgJSON string, status store.TaskStatus) {
	t.Helper()
	if err := st.CreateTask(&store.Task{ID: id, Name: "mc10", Status: status, ConfigJSON: cfgJSON}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

const mc10CompareCfg = `{"kind":"mongocompare","compare":{"source":{"uri":"mongodb://127.0.0.1:1","database":"a"},"target":{"uri":"mongodb://127.0.0.1:1","database":"b"}}}`
const mc10LegacyCfg = `{"kind":"mongocompare","compare":{"source":{"uri":"mongodb://127.0.0.1:1","database":"a"},"target":{"uri":"mongodb://127.0.0.1:1","database":"b"},"buckets":4097}}`

// Lifecycle: start runs ungated through the shared handler (200), and the
// audited outcome lands in the compare trail.
func TestMC10CompareLifecycleUngated(t *testing.T) {
	s, st, dir := mc10BareServer(t)
	mc10SeedTask(t, st, "mcgate", mc10CompareCfg, store.TaskStatusCreated)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskID", "mcgate")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/mcgate/start", nil)
	req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.handleStartTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s, want 200", rec.Code, rec.Body.String())
	}
	assertAuditLine(t, dir, "mc-ops.jsonl", `"action":"mongocompare.start"`)
	// The compare run goroutine dials an unreachable URI and fails fast —
	// wait out the async terminal write so cleanup never races it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := st.GetTask("mcgate")
		if task.Status != store.TaskStatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// R5: legacy out-of-range stored params are rejected at start/resume with
// zero status mutation (fail-loud; no silent clamping).
func TestMC10LegacyParamRevalidation(t *testing.T) {
	s, st, _ := mc10BareServer(t)
	mc10SeedTask(t, st, "mcleg1", mc10LegacyCfg, store.TaskStatusCreated)
	mc10SeedTask(t, st, "mcleg2", mc10LegacyCfg, store.TaskStatusPaused)

	drive := func(handler string) *httptest.ResponseRecorder {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("taskID", "mcleg1")
		if handler == "resume" {
			rctx.URLParams.Add("taskID", "mcleg2")
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/x/"+handler, nil)
		req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		switch handler {
		case "start":
			s.handleStartTask(rec, req)
		case "resume":
			s.handleResumeTask(rec, req)
		}
		return rec
	}

	for _, h := range []string{"start", "resume"} {
		rec := drive(h)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s legacy params: %d %s, want 400", h, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "buckets must be 1..4096") {
			t.Fatalf("%s legacy params: body %s, want boundary wording", h, rec.Body.String())
		}
	}
	t1, _ := st.GetTask("mcleg1")
	t2, _ := st.GetTask("mcleg2")
	if t1.Status != store.TaskStatusCreated || t2.Status != store.TaskStatusPaused {
		t.Fatalf("rejection mutated status: %s / %s, want created / paused", t1.Status, t2.Status)
	}
}

// Lifecycle decision table (seq262/263 定稿, post gate removal):
// pause/resume/cancel/delete compare rows — 200 driven through the shared
// handlers, no token anywhere. The resume 200 cell spawns a run against an
// unreachable URI (fails fast, polled out).
func TestMC10LifecycleUngatedDecisionTable(t *testing.T) {
	s, st, _ := mc10BareServer(t)
	mc10SeedTask(t, st, "gpause", mc10CompareCfg, store.TaskStatusRunning)
	mc10SeedTask(t, st, "gresume", mc10CompareCfg, store.TaskStatusPaused)
	mc10SeedTask(t, st, "gcancel", mc10CompareCfg, store.TaskStatusCreated)
	mc10SeedTask(t, st, "gdelete", mc10CompareCfg, store.TaskStatusCreated)

	ops := []struct {
		name, id, method, path string
	}{
		{"pause", "gpause", http.MethodPost, "/api/v1/tasks/gpause/pause"},
		{"resume", "gresume", http.MethodPost, "/api/v1/tasks/gresume/resume"},
		{"cancel", "gcancel", http.MethodPost, "/api/v1/tasks/gcancel/cancel"},
		{"delete", "gdelete", http.MethodDelete, "/api/v1/tasks/gdelete"},
	}
	for _, op := range ops {
		if rec := driveRouter(s, op.method, op.path, op.id); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s, want 200", op.name, rec.Code, rec.Body.String())
		}
	}
	// the resume 200 spawned a compare run on an unreachable URI — wait out
	// the async terminal write so cleanup never races it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := st.GetTask("gresume")
		if task == nil || task.Status != store.TaskStatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// driveRouter drives one lifecycle op through the real router with a
// chi-populated {taskID}.
func driveRouter(s *Server, method, path, taskID string) *httptest.ResponseRecorder {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskID", taskID)
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

// R3 regression anchor: migration tasks on the SAME lifecycle routes stay
// un-gated (no header → the ops still succeed), and the compare READ
// endpoints stay open (CDC precedent). start/resume migration parity is
// anchored by the F-08/F-10 suites, which drive those handlers and
// expect 200.
func TestMC10MigrationLifecycleUngated(t *testing.T) {
	s, st, _ := mc10BareServer(t)
	mc10SeedTask(t, st, "migmig", `{"source":{"host":"a"},"migration":{"parallel":4}}`, store.TaskStatusRunning)
	mc10SeedTask(t, st, "migcan", `{"source":{"host":"a"},"migration":{"parallel":4}}`, store.TaskStatusCreated)
	mc10SeedTask(t, st, "mcread", mc10CompareCfg, store.TaskStatusCompleted)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskID", "migmig")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/migmig/pause", nil).
		WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.handlePauseTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("migration pause: %d %s, want 200 (ungated)", rec.Code, rec.Body.String())
	}

	rctxC := chi.NewRouteContext()
	rctxC.URLParams.Add("taskID", "migcan")
	reqC := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/migcan/cancel", nil).
		WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctxC))
	recC := httptest.NewRecorder()
	s.handleCancelTask(recC, reqC)
	if recC.Code != http.StatusOK {
		t.Fatalf("migration cancel: %d %s, want 200 (ungated)", recC.Code, recC.Body.String())
	}

	rctx2 := chi.NewRouteContext()
	rctx2.URLParams.Add("taskID", "migmig")
	req2 := httptest.NewRequest(http.MethodDelete, "/api/v1/tasks/migmig", nil).
		WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx2))
	rec2 := httptest.NewRecorder()
	s.handleDeleteTask(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("migration delete: %d %s, want 200 (ungated)", rec2.Code, rec2.Body.String())
	}
}

func assertAuditLine(t *testing.T, dir, file string, want ...string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "audit", file))
	if err != nil {
		t.Fatalf("read audit %s: %v", file, err)
	}
	for _, w := range want {
		if !strings.Contains(string(b), w) {
			t.Fatalf("audit %s missing %q:\n%s", file, w, string(b))
		}
	}
}
