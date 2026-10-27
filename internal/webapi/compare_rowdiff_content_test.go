package webapi

// compare_rowdiff_content_test.go anchors the content-probe read route
// (K7): filename whitelist (traversal/extension), the four 404 faces
// (missing task / bad name / missing artifact), the 500 read-failure face
// with a body free of server paths, and the inline application/json happy
// path (no Content-Disposition attachment — the CSV twin downloads, this
// one is fetched).

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contentGet(file string) func(t *testing.T, s *Server) *httptest.ResponseRecorder {
	return func(t *testing.T, s *Server) *httptest.ResponseRecorder {
		t.Helper()
		req := withChiParams(httptest.NewRequest(http.MethodGet, "/api/v1/compare/tasks/abcd0123/rowdiff-content/"+url.PathEscape(file), nil), "compareID", "abcd0123", "file", file)
		rec := httptest.NewRecorder()
		s.handleCompareRowDiffContent(rec, req)
		return rec
	}
}

func TestCompareRowDiffContentRoute(t *testing.T) {
	s, _ := newTestServer(t)

	// missing task → 404 task-not-found (before any fs touch on diff/)
	dir := s.compareDir("abcd0123")
	rec := contentGet("ok-12345678.content.json")(t, s)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "compare task not found") {
		t.Fatalf("missing task: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// task exists
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(`{"id":"abcd0123","status":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// whitelist: traversal, wrong extension, CSV twin name, space — 404
	for _, bad := range []string{
		"../../task.json",      // traversal
		"secrets.txt",          // wrong extension
		"ok-12345678.csv",      // the CSV twin is NOT a content artifact
		"a b.content.json",     // space outside the alphabet
		"..content.json",       // leading dots
		"sub/dir.content.json", // embedded slash cannot match, belt-and-braces
	} {
		rec := contentGet(bad)(t, s)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("whitelist violation %q must 404, got %d", bad, rec.Code)
		}
	}

	// well-formed name, no artifact → the artifact 404 face
	rec = contentGet("nohash.content.json")(t, s)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "row diff content file not found") {
		t.Fatalf("absent artifact face: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 500 face: read failure on an "existing" path (a directory) —
	// generic body, no server path leaked
	if err := os.MkdirAll(filepath.Join(dir, "diff", "dir.content.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec = contentGet("dir.content.json")(t, s)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("read failure must 500, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "\\") || strings.Contains(rec.Body.String(), "compare/") {
		t.Fatalf("500 body must not leak server paths: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "logged server-side") {
		t.Fatalf("500 body face: %s", rec.Body.String())
	}

	// happy path: inline JSON — application/json, NO Content-Disposition
	payload := `[{"key":"3","category":"content_diff","src_row":{"columns":["id"],"values":["3"]}}]`
	if err := os.WriteFile(filepath.Join(dir, "diff", "ok-12345678.content.json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = contentGet("ok-12345678.content.json")(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("happy path code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "" {
		t.Fatalf("content route serves inline (no disposition header), got %q", cd)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
