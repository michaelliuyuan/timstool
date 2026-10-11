package webapi

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// TestTaskEnvelopeWireContract welds the seq=89 gap shut through the real
// serialization path: every task-bearing response must carry a server-derived
// kind discriminator and must never expose config_json (credentials live
// there). httptest response bodies are asserted directly — fixture-shaped
// mocks may not self-certify this contract.
func TestTaskEnvelopeWireContract(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	s := &Server{store: st}

	// Both task kinds, each with a credential marker that must never surface.
	mig := &store.Task{ID: "tvmig1", Name: "legacy migration", ConfigJSON: `{"source":{"host":"a","password":"sekrit-mig"},"migration":{"parallel":4}}`}
	if err := st.CreateTask(mig); err != nil {
		t.Fatalf("seed migration: %v", err)
	}
	cmp := &store.Task{ID: "tvcmp1", Name: "md compare", ConfigJSON: `{"kind":"mongocompare","compare":{"source":{"uri":"mongodb://x","database":"d","user":"u","password":"sekrit-cmp"}}}`}
	if err := st.CreateTask(cmp); err != nil {
		t.Fatalf("seed compare: %v", err)
	}

	assertWire := func(t *testing.T, context string, body []byte, wantID, wantKind string) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("%s: bad json: %v body=%s", context, err, body)
		}
		if m["id"] != wantID {
			t.Errorf("%s: id=%v want %s", context, m["id"], wantID)
		}
		if m["kind"] != wantKind {
			t.Errorf("%s: kind=%v want %q", context, m["kind"], wantKind)
		}
		if _, leaked := m["config_json"]; leaked {
			t.Errorf("%s: config_json leaked into response", context)
		}
		for _, secret := range []string{"sekrit-mig", "sekrit-cmp"} {
			if strings.Contains(string(body), secret) {
				t.Errorf("%s: credential marker %q leaked", context, secret)
			}
		}
	}

	// GET /tasks: both kinds derived, config hidden.
	rec := httptest.NewRecorder()
	s.handleListTasks(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 2 {
		t.Fatalf("list decode: err=%v len=%d body=%s", err, len(list), rec.Body.String())
	}
	for _, item := range list {
		wantKind := taskKindMigration
		if item["id"] == cmp.ID {
			wantKind = compareTaskKind
		}
		if item["kind"] != wantKind {
			t.Errorf("list: id=%v kind=%v want %q", item["id"], item["kind"], wantKind)
		}
		if _, leaked := item["config_json"]; leaked {
			t.Error("list: config_json leaked into response")
		}
	}
	if strings.Contains(rec.Body.String(), "sekrit") {
		t.Error("list: credential marker leaked")
	}

	// GET /tasks/{id}: single task view (chi-populated {taskID} param, same
	// pattern as startTaskChi in the live e2e).
	for _, tc := range []struct{ id, kind string }{{cmp.ID, compareTaskKind}, {mig.ID, taskKindMigration}} {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("taskID", tc.id)
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/tasks/%s", tc.id), nil).
			WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		s.handleGetTask(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get %s: %d %s", tc.id, rec.Code, rec.Body.String())
		}
		assertWire(t, "get", rec.Body.Bytes(), tc.id, tc.kind)
	}

	// POST /mongocompare/tasks: creation response already carries the kind.
	rec = postCompareSpec(t, s, `{"source":{"host":"localhost","database":"d1"},"target":{"host":"localhost","database":"d2"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create compare: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create compare decode: %v body=%s", err, rec.Body.String())
	}
	if created["kind"] != compareTaskKind {
		t.Errorf("create compare: kind=%v want %q", created["kind"], compareTaskKind)
	}
	if _, leaked := created["config_json"]; leaked {
		t.Error("create compare: config_json leaked into response")
	}
}

// TestMongoCompareRoutesDispatch exercises the REAL router (NewServer-built,
// middleware chain included) — the seq=89 "real serialization path" lesson
// extended to the routing layer: handler-direct tests cannot see route
// collisions (two registrations of method+path panic chi at startup). The
// mongocompare prefix is the collision firewall against the remote MySQL
// compare domain (/compare/tasks) during integration.
func TestMongoCompareRoutesDispatch(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	s := NewServer(st, "127.0.0.1", 0, t.TempDir(), embed.FS{}, nil, "", time.Minute)

	body := `{"source":{"host":"localhost","database":"d1"},"target":{"host":"localhost","database":"d2"}}`

	// New prefixed route dispatches to the compare handler end to end.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mongocompare/tasks", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("new route: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("new route decode: %v body=%s", err, rec.Body.String())
	}
	if created["kind"] != compareTaskKind {
		t.Errorf("new route: kind=%v want %q", created["kind"], compareTaskKind)
	}

	// The old unprefixed path must no longer dispatch to the mongodb handler
	// (404 on this tree; post-merge it belongs to the MySQL compare domain).
	req = httptest.NewRequest(http.MethodPost, "/api/v1/compare/tasks", strings.NewReader(body))
	rec = httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		t.Fatalf("old route still dispatches to mongocompare handler: %d %s", rec.Code, rec.Body.String())
	}
}
