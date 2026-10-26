// mongo_collections_test.go anchors the coll-select ① faces offline: the
// picker endpoint's validation branches (dial path is te103's), the
// mcPickEntries filter (system.* exclusion, view flag, stable ordering),
// and the create-task collections normalization (trim/dedupe, system.*
// rejection, all-empty collapse back to legacy semantics).
package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

func postListCollections(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mongocompare/list-collections", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleListMongoCollections(rec, req)
	return rec
}

// TestHandleListMongoCollectionsValidation pins the fast-reject branches
// (create-handler parity: dialConfig check before the database check).
func TestHandleListMongoCollectionsValidation(t *testing.T) {
	s, _ := newTestServer(t)

	rec := postListCollections(t, s, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: code = %d; want 400", rec.Code)
	}
	rec = postListCollections(t, s, `{}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "host or uri") {
		t.Errorf("empty source: code = %d body = %s; want 400 endpoint error", rec.Code, rec.Body.String())
	}
	rec = postListCollections(t, s, `{"source":{"host":"h"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "source.database is required") {
		t.Errorf("no database: code = %d body = %s; want 400 database required", rec.Code, rec.Body.String())
	}
	rec = postListCollections(t, s, `{"source":{"datasource_id":"no-such-ref","database":"db"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown datasource ref: code = %d; want 400", rec.Code)
	}
}

// TestMCPickEntries: system.* never reaches the picker, views carry the
// is_view flag (FE disables their checkbox), and the listing is sorted by
// name so the picker and its "select all" default behave deterministically.
func TestMCPickEntries(t *testing.T) {
	list := []mongoconn.CollType{
		{Name: "zz", Type: "collection"},
		{Name: "system.views", Type: "collection"},
		{Name: "aa", Type: "view"},
		{Name: "mm", Type: "collection"},
		{Name: "system.profile", Type: "collection"},
		// Empty type = plain collection (the M1 guard inside IsCollection).
		{Name: "plain", Type: ""},
	}
	entries := mcPickEntries(list)
	var names []string
	views := map[string]bool{}
	for _, e := range entries {
		names = append(names, e.Name)
		views[e.Name] = e.IsView
	}
	if strings.Join(names, ",") != "aa,mm,plain,zz" {
		t.Fatalf("entries = %v, want sorted [aa mm plain zz] with system.* excluded", names)
	}
	if !views["aa"] || views["mm"] || views["plain"] {
		t.Fatalf("is_view flags = %v, want aa=true only", views)
	}
	if !isSystemNamespace("system.js") || !isSystemNamespace("system.views") || isSystemNamespace("systemy") {
		t.Fatal("isSystemNamespace prefix rule broken")
	}
}

// TestCreateCompareTaskCollectionsNormalize pins the create-path gates:
// system.* selections are refused loudly; the surviving entries are
// trimmed/deduped into the persisted spec; an explicitly-provided-but-empty
// list 400s instead of silently widening to the whole database (对抗席 F5);
// a watermark override outside the whitelist 400s (对抗席 F3).
func TestCreateCompareTaskCollectionsNormalize(t *testing.T) {
	s, _ := newTestServer(t)

	// system.* refusal.
	rec := postCompareSpec(t, s, `{"source":{"uri":"mongodb://s","database":"a"},"target":{"uri":"mongodb://t"},"collections":["c1","system.js"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "system namespace") {
		t.Fatalf("system.* selection: code=%d body=%s, want 400 system-namespace refusal", rec.Code, rec.Body.String())
	}

	// Explicit-but-empty must 400, never collapse to full-database.
	rec = postCompareSpec(t, s, `{"source":{"uri":"mongodb://s","database":"a"},"target":{"uri":"mongodb://t"},"levels":["schema"],"collections":["","  ",""]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "empty after normalization") {
		t.Fatalf("explicit-empty list: code=%d body=%s, want 400 (no silent scope expansion)", rec.Code, rec.Body.String())
	}

	// Watermark override outside the whitelist must 400 (never a silently
	// lost exception).
	rec = postCompareSpec(t, s, `{"source":{"uri":"mongodb://s","database":"a"},"target":{"uri":"mongodb://t"},"levels":["schema"],"collections":["c1"],"table_watermarks":[{"table":"c9","column":"utime","op":"<=","value":"1"}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not in the selected collections") {
		t.Fatalf("override outside whitelist: code=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	create := func(t *testing.T, colls string) string {
		body := `{"source":{"uri":"mongodb://s","database":"a"},"target":{"uri":"mongodb://t"},"levels":["schema"]` + colls + `}`
		rec := postCompareSpec(t, s, body)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.ID == "" {
			t.Fatalf("create response = %s (err %v), want {id}", rec.Body.String(), err)
		}
		return resp.ID
	}

	// Duplicates + blanks normalize to the deduped pair, order preserved.
	id := create(t, `,"collections":["c1"," c2 ","c1","","c2"]`)
	task, err := s.store.GetTask(id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !strings.Contains(task.ConfigJSON, `"collections":["c1","c2"]`) {
		t.Fatalf("ConfigJSON collections not normalized: %s", task.ConfigJSON)
	}

	// Field OMITTED = legacy full-database contract (no collections key).
	id2 := create(t, ``)
	task2, err := s.store.GetTask(id2)
	if err != nil {
		t.Fatalf("get task2: %v", err)
	}
	if strings.Contains(task2.ConfigJSON, "collections") {
		t.Fatalf("omitted field must stay legacy (no collections key): %s", task2.ConfigJSON)
	}
}
