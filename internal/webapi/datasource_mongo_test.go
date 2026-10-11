package webapi

import (
	"strings"
	"testing"
)

// M6 contract suite: mongodb joins the datasource registry. Three gates from
// the frozen card (dataSourceTypes map / validate error string /
// dsDefaultPort) plus the anti-special-casing red line (seq=162/163): a
// datasource created WITHOUT a port must persist and return NO port key —
// the default is the adapter layer's business (buildClientOptions /
// mongoconn.DialConfig), never a stored default (mysql/pg semantics, zero
// special-casing for mongodb).

// TestDatasources_MongoDBGateEnum pins the three hardcoded-enum gates.
func TestDatasources_MongoDBGateEnum(t *testing.T) {
	if p, ok := dataSourceTypes["mongodb"]; !ok || p != 27017 {
		t.Fatalf("dataSourceTypes[mongodb] = %d,%v want 27017,true (gate 1: map row)", p, ok)
	}
	if got := dsDefaultPort("mongodb"); got != 27017 {
		t.Fatalf("dsDefaultPort(mongodb) = %d, want 27017 (gate 3: map-backed default)", got)
	}
	// Gate 2: the validation error string must name mongodb (a stale string
	// lies about what the registry accepts).
	s := &Server{}
	w, req := doReq("POST", "/api/v1/datasources", `{"name":"x","type":"oracle","fields":{"host":"h"}}`)
	s.handleCreateDataSource(w, req)
	if !strings.Contains(w.Body.String(), "mongodb") {
		t.Fatalf("validate error string must enumerate mongodb: %s", w.Body.String())
	}
}

// TestDatasources_MongoDBNoPortRedLine is the seq=163 red line: creating a
// mongodb datasource without a port stores (and returns) NO port key; an
// explicit port survives verbatim. Redaction is the mysql/pg behavior with
// zero special-casing (password absent + has_password present).
func TestDatasources_MongoDBNoPortRedLine(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "legacy-32", "type": "mongodb",
		"fields": {"host": "120.92.109.95", "user": "u", "password": "p@:s", "authDb": "admin", "tls": true}
	}`)
	s.handleCreateDataSource(w, req)
	if w.Code != 201 {
		t.Fatalf("create mongodb: %d %s", w.Code, w.Body.String())
	}
	created := dsBody(t, w)
	fields := created["fields"].(map[string]any)
	if _, ok := fields["port"]; ok {
		t.Fatalf("RED LINE: created mongodb datasource carries a port key %v — defaults belong to the adapter layer, not storage", fields)
	}
	if fields["authDb"] != "admin" {
		t.Fatalf("authDb (camelCase, mongoMeta key) not round-tripped: %v", fields)
	}
	if created["has_password"] != true {
		t.Fatalf("has_password missing: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "p@:s") {
		t.Fatalf("password leaked: %s", w.Body.String())
	}
	id := created["id"].(string)

	// Persisted entry: no port key either (the stored snapshot is what the
	// compare-ref resolution and the connection test consume).
	e, err := s.resolveDataSourceRef(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Fields["port"]; ok {
		t.Fatalf("RED LINE: stored mongodb entry has a port key: %v", e.Fields)
	}
	if stringField(e.Fields, "password") != "p@:s" {
		t.Fatalf("stored password lost: %v", e.Fields)
	}

	// Explicit port survives (no coercion either way).
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "mongo-alt-port", "type": "mongodb",
		"fields": {"host": "h2", "port": 27018}
	}`)
	s.handleCreateDataSource(w, req)
	if w.Code != 201 {
		t.Fatalf("create with explicit port: %d %s", w.Code, w.Body.String())
	}
	if f := dsBody(t, w)["fields"].(map[string]any); f["port"] != "27018" {
		t.Fatalf("explicit port not preserved verbatim: %v", f)
	}
}
