// compare_rowdiff.go serves the row-level diff detail artifacts of finished
// standalone compare tasks: GET /api/v1/compare/tasks/{compareID}/rowdiff
// /{file} streams compare/{id}/diff/{file} as a CSV attachment. The filename
// is pinned to the engine's whitelist alphabet (^[A-Za-z0-9._-]+\.csv$) —
// non-conforming names 404 before any filesystem touch (no path traversal:
// '/' and '..' cannot appear in a matching name, and Base(file)==file is
// belt-and-braces). Read-route policy follows the report family (MC-10/
// MC-14/MC-16): reads stay open, no token gate, no store/audit writes.
package webapi

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// rowDiffFileRe is the engine-side artifact name contract
// (rowDiffFileName: sanitized table + md8 + ".csv"). Anything else —
// traversal attempts included — fails the match and 404s.
var rowDiffFileRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.csv$`)

// handleCompareRowDiff serves one row-diff detail artifact.
//
// Error faces mirror handleCompareReport (MC-14/16 policy): missing task.json
// → 404 "compare task not found"; a non-whitelisted or missing file → 404
// naming the causes (task not drilled / artifact cleared / bad name); a read
// failure on an existing file → 500 with a generic body (original error to
// the server log only — never a server-side path).
func (s *Server) handleCompareRowDiff(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "compareID")
	file := chi.URLParam(r, "file")

	if task, err := s.loadCompareTask(id); err != nil || task == nil {
		s.writeError(w, http.StatusNotFound, "compare task not found")
		return
	}
	if !rowDiffFileRe.MatchString(file) || filepath.Base(file) != file {
		s.writeError(w, http.StatusNotFound, "row diff file not found — the name is outside the artifact whitelist (engine artifacts are {table}-{hash8}.csv)")
		return
	}
	path := filepath.Join(s.compareDir(id), "diff", file)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "row diff file not found — the task produced no such artifact (no drill ran, zero diffs, or the artifact was cleared)")
			return
		}
		zap.L().Warn("read row diff file failed", zap.String("task", id), zap.String("file", file), zap.Error(err))
		s.writeError(w, http.StatusInternalServerError, "read row diff file failed; cause logged server-side")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+file)
	w.Write(b)
}
