// compare_rowdiff_content.go serves the B-line content-probe artifact of
// finished standalone compare tasks: GET /api/v1/compare/tasks/{compareID}
// /rowdiff-content/{file} streams compare/{id}/diff/{file} as INLINE
// application/json — the FE expandable row lazy-loads it once per table,
// so attachment semantics (the CSV twin's) would fight the fetch. The
// filename is pinned to the engine's derived name contract
// (^[A-Za-z0-9._-]+\.content\.json$ — the CSV artifact name with .csv
// swapped for .content.json); non-conforming names 404 before any
// filesystem touch (no path traversal: '/' and '..' cannot appear in a
// matching name, and Base(file)==file is belt-and-braces). Error faces
// mirror handleCompareRowDiff (MC-14/16 read family): reads stay open, no
// token gate, no store/audit writes.
package webapi

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// rowDiffContentFileRe is the engine-side content artifact name contract
// (rowdiff: {table}-{hash8}.content.json). Anything else — traversal
// attempts included — fails the match and 404s.
var rowDiffContentFileRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.content\.json$`)

// handleCompareRowDiffContent serves one row-diff content artifact.
//
// Error faces mirror handleCompareRowDiff: missing task.json → 404
// "compare task not found"; a non-whitelisted or missing file → 404 naming
// the causes; a read failure on an existing file → 500 with a generic body
// (original error to the server log only — never a server-side path).
func (s *Server) handleCompareRowDiffContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	file := chi.URLParam(r, "file")

	if task, err := s.loadCompareTask(id); err != nil || task == nil {
		s.writeError(w, http.StatusNotFound, "compare task not found")
		return
	}
	if !rowDiffContentFileRe.MatchString(file) || filepath.Base(file) != file {
		s.writeError(w, http.StatusNotFound, "row diff content file not found — the name is outside the artifact whitelist (engine content artifacts are {table}-{hash8}.content.json)")
		return
	}
	path := filepath.Join(s.compareDir(id), "diff", file)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "row diff content file not found — the task produced no such artifact (no drill ran, zero diffs, no probe budget, or the artifact was cleared)")
			return
		}
		zap.L().Warn("read row diff content file failed", zap.String("task", id), zap.String("file", file), zap.Error(err))
		s.writeError(w, http.StatusInternalServerError, "read row diff content file failed; cause logged server-side")
		return
	}
	// Inline JSON (no Content-Disposition): the FE fetches it via XHR for
	// the lazy side-by-side compare table, it is not a download.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(b)
}
