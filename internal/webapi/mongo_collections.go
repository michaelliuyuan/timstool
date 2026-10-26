// mongo_collections.go serves the coll-select ① picker face:
// POST /api/v1/mongocompare/list-collections lists a database's collection
// names (plus the view marker) so the create form can offer per-collection
// selection with everything checked by default.
//
// Read-route policy (same family as suggest-watermark): this is a read-only
// catalog query — no token, no store write, no task side effects. The
// engine's run-time enumeration stays the authority: a collection renamed
// or dropped after picking is handled by the run's selected/matched
// disclosure, never silently.
//
// The filter mirrors the engine's own namespace policy (MC-9 A2) so the
// picker never offers what a run would skip: system.* is excluded outright,
// views are listed but flagged is_view (the FE disables their checkbox).
package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

// mcListBudget bounds the whole request (dial + one nameOnly listing). The
// command itself is a single cheap round-trip; the budget mostly covers
// server selection against an unresponsive host.
const mcListBudget = 30 * time.Second

type mcCollectionEntry struct {
	Name   string `json:"name"`
	IsView bool   `json:"is_view"`
}

// handleListMongoCollections: POST /mongocompare/list-collections
// {source} — source is the task-create endpoint shape (datasource_id or
// discrete fields); database REQUIRED and is the listing target. Response:
// {total, collections:[{name, is_view}]} sorted by name for a stable
// picker; total counts entries including views (the FE subtracts the
// disabled rows for its "selected N" caption).
func (s *Server) handleListMongoCollections(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source compareEndpoint `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.resolveCompareRef(&req.Source); err != nil {
		s.writeError(w, http.StatusBadRequest, "source: "+err.Error())
		return
	}
	dc, err := req.Source.dialConfig()
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "source: "+err.Error())
		return
	}
	if req.Source.Database == "" {
		s.writeError(w, http.StatusBadRequest, "source.database is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mcListBudget)
	defer cancel()
	client, err := mongoconn.Dial(ctx, dc)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}()

	list, err := client.ListCollectionNameTypes(ctx, req.Source.Database)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "collection listing failed: "+err.Error())
		return
	}
	entries := mcPickEntries(list)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":       len(entries),
		"collections": entries,
	})
}

// mcPickEntries applies the picker filter to a raw name+type listing:
// system.* excluded (the engine's own namespace rule), views flagged for
// the FE's disabled checkbox, result sorted by name for a stable picker.
func mcPickEntries(list []mongoconn.CollType) []mcCollectionEntry {
	entries := make([]mcCollectionEntry, 0, len(list))
	for _, ct := range list {
		// Same exclusion the engine applies (compareDB): system.* never
		// carries comparable user data.
		if isSystemNamespace(ct.Name) {
			continue
		}
		entries = append(entries, mcCollectionEntry{Name: ct.Name, IsView: !ct.IsCollection()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// isSystemNamespace is the single system.* rule shared by the picker and
// (as the raw prefix check) the engine — kept here so the HTTP face states
// its own filter without importing engine internals.
func isSystemNamespace(name string) bool {
	return len(name) >= 7 && name[:7] == "system."
}
