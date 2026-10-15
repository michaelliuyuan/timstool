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

// MC-10 ①③ anchors (seq=259 card): the mongocompare token gate (create
// route + compare-kind lifecycle branches) and the start/resume param
// re-validation. Gate semantics mirror the CDC gate exactly: unset
// security.token = 403 fail-closed, missing/wrong X-Auth-Token = 401,
// correct snapshot = pass. Migration tasks on the SAME routes stay
// un-gated (red line). The compare audit trail lands in mc-ops.jsonl —
// never in the CDC trail.

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

func mc10Post(s *Server, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

// R3 create-route decision table: 403 (unset) / 401 (missing, wrong) /
// 201 (correct), audit denied lines in mc-ops.jsonl only.
func TestMC10CompareCreateGate(t *testing.T) {
	s, _, dir := mc10BareServer(t)

	// unset token: fail-closed 403 + denied audit line.
	if rec := mc10Post(s, "/api/v1/mongocompare/tasks", mc10ValidSpec, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("unset token: %d %s, want 403", rec.Code, rec.Body.String())
	}
	assertAuditLine(t, dir, "mc-ops.jsonl", `"action":"mongocompare.create"`, `"result":"denied"`)

	// configured token: missing header → 401; wrong token → 401.
	s.SetSecurityToken(testAuthToken)
	if rec := mc10Post(s, "/api/v1/mongocompare/tasks", mc10ValidSpec, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no header: %d, want 401", rec.Code)
	}
	if rec := mc10Post(s, "/api/v1/mongocompare/tasks", mc10ValidSpec, "wrong-token"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", rec.Code)
	}

	// correct token: the route dispatches to the create handler (201).
	if rec := mc10Post(s, "/api/v1/mongocompare/tasks", mc10ValidSpec, testAuthToken); rec.Code != http.StatusCreated {
		t.Fatalf("correct token: %d %s, want 201", rec.Code, rec.Body.String())
	}
	// success outcome also lands in the compare trail.
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

// R3 lifecycle decision table (inline gate, driven through the shared
// handlers): 403/401 before any state write, 200 with the correct token.
func TestMC10CompareLifecycleGate(t *testing.T) {
	s, st, dir := mc10BareServer(t)
	mc10SeedTask(t, st, "mcgate", mc10CompareCfg, store.TaskStatusCreated)

	start := func(token string) *httptest.ResponseRecorder {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("taskID", "mcgate")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/mcgate/start", nil)
		if token != "" {
			req.Header.Set("X-Auth-Token", token)
		}
		req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		s.handleStartTask(rec, req)
		return rec
	}

	if rec := start(""); rec.Code != http.StatusForbidden {
		t.Fatalf("start unset token: %d %s, want 403", rec.Code, rec.Body.String())
	}
	task, _ := st.GetTask("mcgate")
	if task.Status != store.TaskStatusCreated {
		t.Fatalf("start denial mutated status: %s", task.Status)
	}

	s.SetSecurityToken(testAuthToken)
	if rec := start(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("start no header: %d, want 401", rec.Code)
	}
	if rec := start("wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("start wrong token: %d, want 401", rec.Code)
	}
	task, _ = st.GetTask("mcgate")
	if task.Status != store.TaskStatusCreated {
		t.Fatalf("start denial mutated status: %s", task.Status)
	}
	assertAuditLine(t, dir, "mc-ops.jsonl", `"action":"mongocompare.start"`, `"result":"denied"`)

	if rec := start(testAuthToken); rec.Code != http.StatusOK {
		t.Fatalf("start correct token: %d %s, want 200", rec.Code, rec.Body.String())
	}
	// The compare run goroutine dials an unreachable URI and fails fast —
	// wait out the async terminal write so cleanup never races it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, _ = st.GetTask("mcgate")
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
	s.SetSecurityToken(testAuthToken)
	mc10SeedTask(t, st, "mcleg1", mc10LegacyCfg, store.TaskStatusCreated)
	mc10SeedTask(t, st, "mcleg2", mc10LegacyCfg, store.TaskStatusPaused)

	drive := func(handler string) *httptest.ResponseRecorder {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("taskID", "mcleg1")
		if handler == "resume" {
			rctx.URLParams.Add("taskID", "mcleg2")
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/x/"+handler, nil)
		req.Header.Set("X-Auth-Token", testAuthToken)
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

// R3 lifecycle decision table (seq262/263 定稿): pause/resume/cancel/delete
// compare rows — 403 (unset) / 401 (missing, wrong) / 200 (correct), driven
// through the shared handlers. start's full row set lives above; the resume
// 200 cell spawns a run against an unreachable URI (fails fast, polled out).
func TestMC10LifecycleGateDecisionTable(t *testing.T) {
	s, st, _ := mc10BareServer(t)
	mc10SeedTask(t, st, "gpause", mc10CompareCfg, store.TaskStatusRunning)
	mc10SeedTask(t, st, "gresume", mc10CompareCfg, store.TaskStatusPaused)
	mc10SeedTask(t, st, "gcancel", mc10CompareCfg, store.TaskStatusCreated)
	mc10SeedTask(t, st, "gdelete", mc10CompareCfg, store.TaskStatusCreated)

	ops := []struct {
		name, id, method string
	}{
		{"pause", "gpause", http.MethodPost},
		{"resume", "gresume", http.MethodPost},
		{"cancel", "gcancel", http.MethodPost},
		{"delete", "gdelete", http.MethodDelete},
	}
	byName := map[string]func(token string) *httptest.ResponseRecorder{
		"pause": func(token string) *httptest.ResponseRecorder {
			return driveRouter(s, http.MethodPost, "/api/v1/tasks/gpause/pause", "gpause", token)
		},
		"resume": func(token string) *httptest.ResponseRecorder {
			return driveRouter(s, http.MethodPost, "/api/v1/tasks/gresume/resume", "gresume", token)
		},
		"cancel": func(token string) *httptest.ResponseRecorder {
			return driveRouter(s, http.MethodPost, "/api/v1/tasks/gcancel/cancel", "gcancel", token)
		},
		"delete": func(token string) *httptest.ResponseRecorder {
			return driveRouter(s, http.MethodDelete, "/api/v1/tasks/gdelete", "gdelete", token)
		},
	}

	// unset token: fail-closed 403 everywhere.
	for _, op := range ops {
		if rec := byName[op.name](""); rec.Code != http.StatusForbidden {
			t.Fatalf("%s unset token: %d %s, want 403", op.name, rec.Code, rec.Body.String())
		}
	}
	s.SetSecurityToken(testAuthToken)
	for _, bad := range []string{"", "wrong-token"} {
		for _, op := range ops {
			if rec := byName[op.name](bad); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s token=%q: %d, want 401", op.name, bad, rec.Code)
			}
		}
	}
	for _, op := range ops {
		if rec := byName[op.name](testAuthToken); rec.Code != http.StatusOK {
			t.Fatalf("%s correct token: %d %s, want 200", op.name, rec.Code, rec.Body.String())
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
// chi-populated {taskID} and an optional token header.
func driveRouter(s *Server, method, path, taskID, token string) *httptest.ResponseRecorder {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskID", taskID)
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

// R3 regression anchor: migration tasks on the SAME lifecycle routes stay
// un-gated (no token configured, no header → the ops still succeed), and
// the compare READ endpoints stay open (CDC precedent). start/resume
// migration parity is anchored by the F-08/F-10 suites, which drive those
// handlers without a token header and expect 200.
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
