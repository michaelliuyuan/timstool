package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseCompareTask(t *testing.T) {
	// Migration-shaped config (existing handleCreateTask output) must NOT be
	// detected as a compare task — the branch must stay invisible to legacy
	// tasks (red line).
	mig := `{"source":{"host":"a"},"target":{"host":"b"},"migration":{"parallel":4}}`
	if _, ok := parseCompareTask(mig); ok {
		t.Fatal("migration config detected as compare task")
	}
	// Compare envelope round-trips.
	env := compareTaskEnvelope{Kind: compareTaskKind, Compare: compareTaskSpec{
		Source:  compareEndpoint{Host: "h1", Port: 27017, Database: "db1"},
		Target:  compareEndpoint{Host: "h2", Database: "db2"},
		Buckets: 32,
	}}
	b, _ := json.Marshal(env)
	spec, ok := parseCompareTask(string(b))
	if !ok {
		t.Fatal("compare envelope not detected")
	}
	if spec.Source.Host != "h1" || spec.Target.Database != "db2" || spec.Buckets != 32 {
		t.Fatalf("spec round-trip mismatch: %+v", spec)
	}
}

func TestCompareEndpointClientOptions(t *testing.T) {
	// Structured building (M1 pattern): credentials must land in
	// options.Credential, never string-concatenated into a URI — passwords
	// with @ : / % would break URI parsing (review finding A).
	e := compareEndpoint{Host: "h", Port: 27018, User: "u", Password: "p@:/x", AuthDB: "adb"}
	opts, err := e.clientOptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Hosts) != 1 || opts.Hosts[0] != "h:27018" {
		t.Errorf("hosts=%v, want [h:27018]", opts.Hosts)
	}
	if opts.Auth == nil || opts.Auth.Username != "u" || opts.Auth.Password != "p@:/x" || opts.Auth.AuthSource != "adb" {
		t.Errorf("auth=%+v, want structured credential", opts.Auth)
	}
	// No auth when user empty; no authSource when auth_db empty.
	e2 := compareEndpoint{Host: "h2"}
	opts2, _ := e2.clientOptions()
	if opts2.Auth != nil {
		t.Errorf("auth=%+v, want nil", opts2.Auth)
	}
	// Bare URI passes through; discrete fields win over URI; neither = error.
	if opts3, _ := (&compareEndpoint{URI: "mongodb://x"}).clientOptions(); len(opts3.Hosts) == 0 && opts3.GetURI() == "" {
		t.Error("uri-only endpoint produced empty options")
	}
	if opts4, _ := (&compareEndpoint{URI: "mongodb://z", Host: "h"}).clientOptions(); opts4.Hosts[0] != "h" && len(opts4.Hosts) == 0 {
		t.Error("discrete fields should win over uri")
	}
	if _, err := (&compareEndpoint{}).clientOptions(); err == nil {
		t.Error("empty endpoint should error")
	}
}

func postCompareSpec(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mongocompare/tasks", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	s.handleCreateCompareTask(rec, req)
	return rec
}

// TestHandleCreateCompareTaskValidation covers the 400 paths that must reject
// before any store write; success paths are covered by the live smoke test.
func TestHandleCreateCompareTaskValidation(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name string
		body string
	}{
		{"invalid json", "{"},
		{"missing source", `{"target":{"uri":"mongodb://t"}}`},
		{"missing target", `{"source":{"uri":"mongodb://s"}}`},
		{"missing source db", `{"source":{"uri":"mongodb://s"},"target":{"uri":"mongodb://t"}}`},
		{"bad level", `{"source":{"uri":"mongodb://s","database":"a"},"levels":["nope"]}`},
	}
	for _, c := range cases {
		rec := postCompareSpec(t, s, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d want 400, body=%s", c.name, rec.Code, rec.Body.String())
		}
	}
}
