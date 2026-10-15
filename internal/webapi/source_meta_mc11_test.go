package webapi

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	// The source registry is populated by driver init()s; production wires
	// them from main.go, test binaries must blank-import what they exercise
	// (same pattern as describe_test.go).
	_ "github.com/michaelliuyuan/timstool/internal/source/mongo"
)

// MC-11 anchors: POST /sources/meta accepts a datasource_id — the stored
// mongodb entry folds in as the connection base (explicit request fields
// still win), the fold is type-gated to mongodb, and the response shape
// stays entries-only (credentials never travel to the browser). Offline
// anchors pin the 400 semantics; the live anchor proves the folded fields
// actually dial and list databases.

// TestSourceMetaDatasourceFold400s pins the three fold gates: unknown id,
// non-mongodb datasource, and an explicit source conflicting with the
// folded kind — all 400, all naming the offender (compare-ref error shape).
func TestSourceMetaDatasourceFold400s(t *testing.T) {
	s, _ := newTestServer(t)

	// Unknown id.
	w, req := doReq("POST", "/api/v1/sources/meta", `{"datasource_id":"nope","database":""}`)
	s.handleSourceMeta(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "nope") || !strings.Contains(w.Body.String(), "not found") {
		t.Fatalf("unknown id: %d %s", w.Code, w.Body.String())
	}

	// Non-mongodb datasource.
	w, req = doReq("POST", "/api/v1/datasources", `{"name":"pg1","type":"postgres","fields":{"host":"h"}}`)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create pg ds: %d %s", w.Code, w.Body.String())
	}
	pgID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/sources/meta", fmt.Sprintf(`{"datasource_id":%q,"database":""}`, pgID))
	s.handleSourceMeta(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "requires a mongodb datasource") {
		t.Fatalf("wrong type: %d %s", w.Code, w.Body.String())
	}

	// Explicit source conflicting with the folded mongodb kind.
	w, req = doReq("POST", "/api/v1/datasources", `{"name":"m1","type":"mongodb","fields":{"host":"h"}}`)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create mongo ds: %d %s", w.Code, w.Body.String())
	}
	mcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/sources/meta", fmt.Sprintf(`{"source":"postgres","datasource_id":%q,"database":""}`, mcID))
	s.handleSourceMeta(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "conflicts with datasource") {
		t.Fatalf("conflict: %d %s", w.Code, w.Body.String())
	}
}

// TestSourceMetaDatasourceFoldLive is the env-gated dial proof (standing
// smoke mongod): a datasource whose fields point at TIMSTOOL_MONGO_SMOKE
// lists databases through the fold, the response carries exactly the four
// browse keys (no field/credential echo), and the collections level routes
// the database alongside the fold.
func TestSourceMetaDatasourceFoldLive(t *testing.T) {
	addr := smokeAddrForLive(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("TIMSTOOL_MONGO_SMOKE must be host:port, got %q: %v", addr, err)
	}
	s, _ := newTestServer(t)

	body := fmt.Sprintf(`{"name":"mc11-live","type":"mongodb","fields":{"host":%q,"port":%s}}`, host, port)
	w, req := doReq("POST", "/api/v1/datasources", body)
	s.handleCreateDataSource(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create ds: %d %s", w.Code, w.Body.String())
	}
	id := dsBody(t, w)["id"].(string)

	// Databases level through the fold.
	w, req = doReq("POST", "/api/v1/sources/meta", fmt.Sprintf(`{"datasource_id":%q,"database":""}`, id))
	s.handleSourceMeta(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("meta databases: %d %s", w.Code, w.Body.String())
	}
	resp := w.Body.String()
	if !strings.Contains(resp, `"level":"databases"`) || !strings.Contains(resp, `"source":"mongodb"`) {
		t.Fatalf("level/source wrong: %s", resp)
	}
	if !strings.Contains(resp, "admin") {
		t.Fatalf("listing lacks the always-present admin db: %s", resp)
	}
	if strings.Contains(resp, "password") || strings.Contains(resp, "fields") {
		t.Fatalf("response echoes fields/credentials: %s", resp)
	}
	for _, key := range []string{`"source"`, `"level"`, `"entries"`, `"count"`} {
		if !strings.Contains(resp, key) {
			t.Fatalf("response missing key %s: %s", key, resp)
		}
	}

	// Collections level routes database through the fold.
	w, req = doReq("POST", "/api/v1/sources/meta", fmt.Sprintf(`{"datasource_id":%q,"database":"admin"}`, id))
	s.handleSourceMeta(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"level":"collections"`) {
		t.Fatalf("meta collections: %d %s", w.Code, w.Body.String())
	}
}
