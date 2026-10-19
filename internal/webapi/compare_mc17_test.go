package webapi

// MC-17 polish-batch anchors:
//   ⑥a  the {compareID} allow-list front gate — non-conforming ids
//       (traversal, backslash, overlong, uppercase, empty) 404 through the
//       REAL router before any filesystem touch, on every gated route;
//   ⑥b  the list face strips usernames (source.user/target.user) while the
//       detail face keeps them (scope pin — widening is deliberate);
//   ①   statusClass escaping (the MC-16 renderer's twin anchor lives in
//       compare_data_report_test.go; the reporter twin in reporter_test.go);
//   ②   dcModeLabel >1 clamp (twin anchor in compare_data_report_test.go).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// mc17GateProbe drives one request through the real router and returns the
// recorder (the middleware must 404 before any disk access).
func mc17GateProbe(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

// TestCompareIDGate_404s: every malformed id shape 404s on every gated
// route (GET detail / GET report / POST cancel / DELETE) — never 500, never
// a disk touch. ..%2F and %5C are the traversal carriers; overlong,
// uppercase, and empty round out the boundary set.
func TestCompareIDGate_404s(t *testing.T) {
	s, _ := newTestServer(t)
	bad := []string{
		"..%2F..%2Fetc", // encoded traversal (../)
		"..%5C..%5Cetc", // encoded backslash traversal (..\)
		"aabbccddd",     // overlong (9 chars)
		"AABBCCDD",      // uppercase hex
		"a1b2c3d",       // short (7 chars)
		"gggggggg",      // non-hex
		"a1b2c3d4%20",   // trailing escape
		"a1b2c3d4%00",   // NUL escape
	}
	routes := []struct{ method, suffix string }{
		{http.MethodGet, ""},
		{http.MethodGet, "/report"},
		{http.MethodPost, "/cancel"},
		{http.MethodDelete, ""},
	}
	for _, id := range bad {
		for _, rt := range routes {
			rec := mc17GateProbe(t, s, rt.method, "/api/v1/compare/tasks/"+id+rt.suffix)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s: want 404, got %d body=%s", rt.method, id+rt.suffix, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "compare task not found") {
				t.Fatalf("%s %s: gate body mismatch: %s", rt.method, id+rt.suffix, rec.Body.String())
			}
		}
	}
	// The empty segment (…/tasks//) must also be a 404 — whichever layer
	// rejects it, the response never reaches a handler.
	if rec := mc17GateProbe(t, s, http.MethodGet, "/api/v1/compare/tasks//"); rec.Code != http.StatusNotFound {
		t.Fatalf("empty id: want 404, got %d", rec.Code)
	}

	// Disk-touch negative: a traversal id must not create or delete
	// anything under dataDir even if a matching directory name existed.
	marker := filepath.Join(s.dataDir, "mc17-gate-marker.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := mc17GateProbe(t, s, http.MethodDelete, "/api/v1/compare/tasks/..%2Fmc17-gate-marker.txt%00")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal delete: want 404, got %d", rec.Code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("traversal delete touched disk: %v", err)
	}
}

// TestCompareList_StripsUsernames: GET /compare/tasks never carries
// source.user/target.user (FE renders host:port/db only); GET
// /compare/tasks/{id} (detail) is unchanged — pinned so any future widening
// of the strip is a deliberate decision, not drift.
func TestCompareList_StripsUsernames(t *testing.T) {
	s, _ := newTestServer(t)
	task := &CompareTask{
		ID:     "9f8e7d6c",
		Name:   "mc17 list strip",
		Status: CompareStatusCompleted,
		Source: config.SourceConfig{Host: "s.example", Port: 5432, User: "srcuser", Database: "d1"},
		Target: config.TargetConfig{Host: "t.example", Port: 4000, User: "tgtuser", Database: "d2"},
		Mode:   "quick",
	}
	if err := s.persistCompareTask(task); err != nil {
		t.Fatal(err)
	}

	rec := mc17GateProbe(t, s, http.MethodGet, "/api/v1/compare/tasks")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list []*CompareTask
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "9f8e7d6c" {
		t.Fatalf("unexpected list: %+v", list)
	}
	if list[0].Source.User != "" || list[0].Target.User != "" {
		t.Fatalf("list leaked usernames: %+v", list[0])
	}
	if list[0].Source.Host != "s.example" || list[0].Source.Database != "d1" {
		t.Fatalf("list over-stripped connection fields: %+v", list[0].Source)
	}

	rec = mc17GateProbe(t, s, http.MethodGet, "/api/v1/compare/tasks/9f8e7d6c")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d", rec.Code)
	}
	var detail CompareTask
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Source.User != "srcuser" || detail.Target.User != "tgtuser" {
		t.Fatalf("detail face changed (scope pin): %+v", detail)
	}
}
