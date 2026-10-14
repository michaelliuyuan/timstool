package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		// MC-9 C4 parameter boundaries (0 = default passes; out-of-range 400s).
		{"buckets over max", `{"source":{"uri":"mongodb://s","database":"a"},"buckets":4097}`},
		{"buckets under min", `{"source":{"uri":"mongodb://s","database":"a"},"buckets":-1}`},
		{"parallel_colls over", `{"source":{"uri":"mongodb://s","database":"a"},"parallel_colls":33}`},
		{"parallel_buckets over", `{"source":{"uri":"mongodb://s","database":"a"},"parallel_buckets":0,"parallel_colls":64}`},
		{"batch_size under", `{"source":{"uri":"mongodb://s","database":"a"},"batch_size":50}`},
		{"batch_size over", `{"source":{"uri":"mongodb://s","database":"a"},"batch_size":100001}`},
		{"diff without hash", `{"source":{"uri":"mongodb://s","database":"a"},"levels":["schema","index","count","diff"]}`},
	}
	for _, c := range cases {
		rec := postCompareSpec(t, s, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d want 400, body=%s", c.name, rec.Code, rec.Body.String())
		}
	}
}

// TestValidateCompareParamsBoundaries (MC-9 C4): the exact edges pass and
// everything one step past them rejects; 0 means default everywhere.
func TestValidateCompareParamsBoundaries(t *testing.T) {
	valid := []compareTaskSpec{
		{Buckets: 1}, {Buckets: 4096},
		{ParallelColls: 1}, {ParallelColls: 32},
		{ParallelBuckets: 1}, {ParallelBuckets: 32},
		{BatchSize: 100}, {BatchSize: 100000},
		{Levels: []string{"schema", "hash", "diff"}},
		{Levels: []string{"schema"}}, // diff absent: no hash requirement
	}
	for i := range valid {
		if err := validateCompareParams(&valid[i]); err != nil {
			t.Errorf("valid spec %d rejected: %v (%+v)", i, err, valid[i])
		}
	}
	invalid := []compareTaskSpec{
		{Buckets: 0 - 1, BatchSize: 1000}, // avoid zero-default collisions
		{Buckets: 4097},
		{ParallelColls: 33},
		{ParallelColls: -2},
		{ParallelBuckets: 99},
		{BatchSize: 99},
		{BatchSize: 100001},
		{Levels: []string{"diff"}},
		{Levels: []string{"schema", "diff"}},
	}
	for i := range invalid {
		if err := validateCompareParams(&invalid[i]); err == nil {
			t.Errorf("invalid spec %d accepted: %+v", i, invalid[i])
		}
	}
}

// TestResolveCompareRef (M6): datasource refs fold into the endpoint as a
// creation-time snapshot — field-key mapping (camelCase datasource authDb →
// snake_case auth_db), explicit-fields-override, the mongodb-only type gate,
// and the ref-id strip (the persisted spec carries no live reference).
func TestResolveCompareRef(t *testing.T) {
	s, _ := newTestServer(t)

	// No ref: no-op.
	e := &compareEndpoint{Host: "h"}
	if err := s.resolveCompareRef(e); err != nil || e.Host != "h" {
		t.Fatalf("no-ref call must be a no-op: %+v %v", e, err)
	}

	// Unknown ref id.
	if err := s.resolveCompareRef(&compareEndpoint{DatasourceID: "nope"}); err == nil {
		t.Fatal("unknown ref must error")
	}

	// Non-mongodb datasource is rejected by compare.
	w, req := doReq("POST", "/api/v1/datasources", `{"name":"pg1","type":"postgres","fields":{"host":"h"}}`)
	s.handleCreateDataSource(w, req)
	pgID := dsBody(t, w)["id"].(string)
	err := s.resolveCompareRef(&compareEndpoint{DatasourceID: pgID})
	if err == nil || !strings.Contains(err.Error(), "mongodb") {
		t.Fatalf("postgres ref must be rejected with a mongodb-only message: %v", err)
	}

	// Full fold from a mongodb profile.
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name":"legacy","type":"mongodb",
		"fields":{"host":"10.1.2.3","port":27018,"user":"u","password":"pw","authDb":"admin","tls":"true"}
	}`)
	s.handleCreateDataSource(w, req)
	mgID := dsBody(t, w)["id"].(string)

	e = &compareEndpoint{DatasourceID: mgID}
	if err := s.resolveCompareRef(e); err != nil {
		t.Fatal(err)
	}
	if e.Host != "10.1.2.3" || e.Port != 27018 || e.User != "u" || e.Password != "pw" || e.AuthDB != "admin" || !e.TLS {
		t.Fatalf("fold mismatch: %+v", e)
	}
	if e.DatasourceID != "" {
		t.Fatalf("ref id must be stripped after the fold (snapshot purity): %+v", e)
	}

	// Explicit discrete fields override the stored profile.
	e = &compareEndpoint{DatasourceID: mgID, Host: "override", Port: 27019}
	if err := s.resolveCompareRef(e); err != nil {
		t.Fatal(err)
	}
	if e.Host != "override" || e.Port != 27019 || e.User != "u" {
		t.Fatalf("explicit-wins fold mismatch: %+v", e)
	}

	// tls "false"/absent stays off.
	w, req = doReq("POST", "/api/v1/datasources", `{"name":"notls","type":"mongodb","fields":{"host":"h"}}`)
	s.handleCreateDataSource(w, req)
	noTLS := dsBody(t, w)["id"].(string)
	e = &compareEndpoint{DatasourceID: noTLS}
	if err := s.resolveCompareRef(e); err != nil {
		t.Fatal(err)
	}
	if e.TLS {
		t.Fatalf("tls must stay off for a profile without it: %+v", e)
	}
}

// TestCreateCompareTask_WithDatasourceRef (M6): the create endpoint resolves
// refs end-to-end — the persisted ConfigJSON holds the folded snapshot
// (credentials in, datasource_id out).
func TestCreateCompareTask_WithDatasourceRef(t *testing.T) {
	s, _ := newTestServer(t)
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name":"legacy","type":"mongodb",
		"fields":{"host":"10.1.2.3","port":27018,"user":"u","password":"pw","authDb":"admin"}
	}`)
	s.handleCreateDataSource(w, req)
	mgID := dsBody(t, w)["id"].(string)

	rec := postCompareSpec(t, s, `{
		"source": {"datasource_id": "`+mgID+`", "database": "testdb"},
		"target": {"datasource_id": "`+mgID+`", "database": "testdb"}
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with refs: %d %s", rec.Code, rec.Body.String())
	}
	id := dsBody(t, rec)["id"].(string)
	task, err := s.store.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := parseCompareTask(task.ConfigJSON)
	if !ok {
		t.Fatalf("not a compare config: %s", task.ConfigJSON)
	}
	if spec.Source.Host != "10.1.2.3" || spec.Source.Port != 27018 || spec.Source.Password != "pw" || spec.Source.AuthDB != "admin" {
		t.Fatalf("snapshot not folded into persisted spec: %+v", spec.Source)
	}
	if spec.Source.DatasourceID != "" || spec.Target.DatasourceID != "" {
		t.Fatalf("persisted spec must not carry datasource refs: %+v", spec)
	}
	if _, err := spec.Source.dialConfig(); err != nil {
		t.Fatalf("folded snapshot must dial: %v", err)
	}
}
