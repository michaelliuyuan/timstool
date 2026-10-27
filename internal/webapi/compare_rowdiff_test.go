package webapi

// compare_rowdiff_test.go anchors the row-diff detail download route:
// filename whitelist (traversal/extension), 404 faces (missing task /
// missing artifact / bad name), happy-path CSV serving, and the
// report.json/html plumbing of the row_diffs summary field.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rowDiffFixture assembles a compare task dir with a diff artifact.
func rowDiffFixture(t *testing.T, s *Server, withTask, withFile bool, file, content string) {
	t.Helper()
	dir := s.compareDir("abcd0123")
	if withTask {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(`{"id":"abcd0123","status":"completed"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if withFile {
		if err := os.MkdirAll(filepath.Join(dir, "diff"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "diff", file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompareRowDiffRouteWhitelistAnd404s(t *testing.T) {
	s, _ := newTestServer(t)

	rowDiffGet := func(file string) *httptest.ResponseRecorder {
		req := withChiParams(httptest.NewRequest(http.MethodGet, "/api/v1/compare/tasks/abcd0123/rowdiff/"+url.PathEscape(file), nil), "compareID", "abcd0123", "file", file)
		rec := httptest.NewRecorder()
		s.handleCompareRowDiff(rec, req)
		return rec
	}

	// missing task → 404 task-not-found (before any fs touch on diff/)
	rowDiffFixture(t, s, false, false, "", "")
	rec := rowDiffGet("ok-12345678.csv")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "compare task not found") {
		t.Fatalf("missing task: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// task exists, artifact missing → 404 artifact causes
	rowDiffFixture(t, s, true, false, "", "")
	rec = rowDiffGet("ok-12345678.csv")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "row diff file not found") {
		t.Fatalf("missing artifact: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// whitelist: traversal, wrong extension, space — all 404, none touch the fs
	rowDiffFixture(t, s, true, true, "ok-12345678.csv", "x")
	for _, bad := range []string{
		"../../task.json", // traversal
		"secrets.txt",     // wrong extension
		"a b.csv",         // space outside the alphabet
	} {
		rec = rowDiffGet(bad)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("whitelist violation %q must 404, got %d", bad, rec.Code)
		}
	}
	// well-formed name that simply has no artifact → the artifact 404 face
	if rec = rowDiffGet("nohash.csv"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "row diff file not found") {
		t.Fatalf("absent artifact face: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// happy path: served verbatim with the CSV face
	rec = rowDiffGet("ok-12345678.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("happy path code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "ok-12345678.csv") {
		t.Fatalf("disposition = %q", cd)
	}
	if rec.Body.String() != "x" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// report.json roundtrip carries row_diffs verbatim (format=json face), and
// the html family report renders the drill disclosure column.
func TestCompareRowDiffReportPlumbing(t *testing.T) {
	s, _ := newTestServer(t)
	rowDiffFixture(t, s, true, false, "", "")
	reportJSON := `{
  "tool": "timstool-migrator", "phase": "data-validation",
  "start_time": "2026-10-26T10:00:00Z", "end_time": "2026-10-26T10:01:00Z",
  "duration": "1m", "overall_status": "fail",
  "tables": [
    {"table_name": "orders", "status": "fail", "source_rows": 5, "target_rows": 4,
     "diff_rows": 3, "error": "checksum mismatch in 1/1 chunks",
     "row_diffs": {"key_columns": ["id"], "content_diff": 1, "src_only": 1, "dst_only": 1,
                    "sample_keys": ["3"], "file": "orders-12345678.csv"}},
    {"table_name": "users", "status": "pass", "source_rows": 2, "target_rows": 2}
  ],
  "stats": {"total_tables": 2, "fail_tables": 1, "pass_tables": 1,
            "total_source_rows": 7, "total_target_rows": 6, "total_diff_rows": 3}
}`
	if err := os.WriteFile(filepath.Join(s.compareDir("abcd0123"), "report.json"), []byte(reportJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// format=json: verbatim bytes — row_diffs survives untouched
	rec := dcReportGet(t, s, "abcd0123", "json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"row_diffs"`) {
		t.Fatalf("json face must carry row_diffs verbatim: code=%d", rec.Code)
	}

	// html face: the 行级差异 column renders the summary + file name
	rec = dcReportGet(t, s, "abcd0123", "html")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("html face code=%d", rec.Code)
	}
	for _, want := range []string{"行级差异", "内容不一致 1 · 仅源端 1 · 仅目标端 1", "orders-12345678.csv"} {
		if !strings.Contains(body, want) {
			t.Fatalf("html report missing %q", want)
		}
	}
	// a PASS table without row_diffs renders the neutral dash, no fake summary
	if strings.Count(body, "仅源端") != 1 {
		t.Fatalf("only the drilled table may carry drill counts")
	}
}
