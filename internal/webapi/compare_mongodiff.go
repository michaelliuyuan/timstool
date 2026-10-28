// compare_mongodiff.go serves the field-level diff detail artifacts of
// finished mongocompare tasks: GET /api/v1/tasks/{taskID}/mongodiff/{file}
// streams compare/{id}/diff/{file} as an ndjson attachment. The filename
// is pinned to the engine's whitelist alphabet (^[A-Za-z0-9_-]+\.ndjson$
// — writeDeltas names are {db}__{coll}.ndjson with inner dots replaced),
// so non-conforming names — traversal attempts included — 404 before any
// filesystem touch ('/' and '..' cannot appear in a matching name, and
// Base(file)==file is belt-and-braces). Read-route policy follows the
// report family (MC-10/MC-14/rowdiff twin): reads stay open, no token
// gate, no store/audit writes.
package webapi

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// mongoDiffFileRe is the engine-side artifact name contract (writeDeltas:
// strings.ReplaceAll(db+"."+coll, ".", "__") + ".ndjson"). Anything else —
// traversal attempts included — fails the match and 404s.
var mongoDiffFileRe = regexp.MustCompile(`^[A-Za-z0-9_-]+\.ndjson$`)

// handleMongoDiff serves one mongocompare field-diff ndjson artifact.
//
// Gate order (unit-12 A): unknown task → 404; a ConfigJSON that does not
// parse as a mongocompare envelope → 404 (parseCompareTask gate — the
// tasks namespace is shared with migration tasks); a non-whitelisted name
// → 404; a missing file → 404 naming the causes (writeCompareSummaryError
// style — never a server-side path); a read failure on an existing file →
// 500 with a generic body (original error to the server log only).
func (s *Server) handleMongoDiff(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "taskID")
	file := chi.URLParam(r, "file")

	task, err := s.store.GetTask(id)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		s.writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if _, ok := parseCompareTask(task.ConfigJSON); !ok {
		s.writeError(w, http.StatusNotFound, "mongo diff file not found — the task is not a mongocompare task")
		return
	}
	if !mongoDiffFileRe.MatchString(file) || filepath.Base(file) != file {
		s.writeError(w, http.StatusNotFound, "mongo diff file not found — the name is outside the artifact whitelist (engine artifacts are {db}__{coll}.ndjson)")
		return
	}
	path := filepath.Join(s.dataDir, "compare", id, "diff", file)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "mongo diff file not found — the task produced no such artifact (diff level off, zero field diffs, or the artifact was cleared)")
			return
		}
		zap.L().Warn("read mongo diff file failed", zap.String("task", id), zap.String("file", file), zap.Error(err))
		s.writeError(w, http.StatusInternalServerError, "read mongo diff file failed; cause logged server-side")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", "attachment; filename="+file)
	w.Write(b)
}
