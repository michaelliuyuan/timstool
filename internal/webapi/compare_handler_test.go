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

func TestCompareEndpointDialConfig(t *testing.T) {
	// Structured mapping (M1 pattern): credentials stay literal fields,
	// never string-concatenated into a URI — passwords with @ : / % would
	// break URI parsing (review finding A). M5: the endpoint maps onto the
	// version-adaptive DialConfig.
	e := compareEndpoint{Host: "h", Port: 27018, User: "u", Password: "p@:/x", AuthDB: "adb"}
	cfg, err := e.dialConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "h" || cfg.Port != 27018 || cfg.User != "u" || cfg.Password != "p@:/x" || cfg.AuthDB != "adb" {
		t.Errorf("cfg = %+v, want structured credential passthrough", cfg)
	}
	if cfg.URI != "" {
		t.Errorf("URI = %q, want empty when discrete fields given", cfg.URI)
	}
	// No credentials: zero-value fields (the dial applies driver defaults).
	e2 := compareEndpoint{Host: "h2"}
	cfg2, _ := e2.dialConfig()
	if cfg2.User != "" || cfg2.AuthDB != "" {
		t.Errorf("cfg2 = %+v, want empty credential fields", cfg2)
	}
	// Bare URI passes through; discrete fields win over URI; neither = error.
	cfg3, err3 := (&compareEndpoint{URI: "mongodb://x"}).dialConfig()
	if err3 != nil || cfg3.URI != "mongodb://x" {
		t.Errorf("uri-only endpoint: cfg=%+v err=%v, want URI passthrough", cfg3, err3)
	}
	cfg4, _ := (&compareEndpoint{URI: "mongodb://z", Host: "h"}).dialConfig()
	if cfg4.Host != "h" || cfg4.URI != "" {
		t.Errorf("cfg4 = %+v, want discrete fields to win over uri", cfg4)
	}
	if _, err := (&compareEndpoint{}).dialConfig(); err == nil {
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
