package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Migration options persistence (迁移选项记忆): PUT must validate and persist
// {temp_dir, use_lightning, lightning_path} under dataDir; GET must return the
// saved values and empty defaults when nothing was saved yet.

func doReq(method, path, body string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return httptest.NewRecorder(), req
}

func TestMigrationOptions_RoundTrip(t *testing.T) {
	s, _ := newTestServer(t)

	// GET before any save: empty defaults.
	w, req := doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initial GET status = %d", w.Code)
	}
	var initial migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode initial GET: %v", err)
	}
	if initial.TempDir != "" || initial.UseLightning || initial.LightningPath != "" {
		t.Fatalf("expected empty defaults, got %+v", initial)
	}

	// PUT valid options (empty lightning path = auto-discovery, allowed).
	w, req = doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"","use_lightning":true,"lightning_path":""}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", w.Code, w.Body.String())
	}

	// File must exist under dataDir.
	if _, err := os.Stat(filepath.Join(s.dataDir, "migration-options.json")); err != nil {
		t.Fatalf("persisted file missing: %v", err)
	}

	// GET returns saved values.
	w, req = doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	var got migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if !got.UseLightning {
		t.Fatalf("expected use_lightning=true, got %+v", got)
	}
}

func TestMigrationOptions_PutValidation(t *testing.T) {
	s, _ := newTestServer(t)

	// Nonexistent temp dir path that cannot be created (a file where a
	// directory is needed).
	block := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(block, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, req := doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"`+filepath.Join(block, "sub")+`","use_lightning":false}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("temp_dir under file: status = %d body=%s", w.Code, w.Body.String())
	}

	// lightning_path pointing at a nonexistent file: rejected.
	w, req = doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"","use_lightning":true,"lightning_path":"/no/such/lightning"}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad lightning path: status = %d body=%s", w.Code, w.Body.String())
	}

	// Directory as lightning path: rejected.
	dir := t.TempDir()
	w, req = doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"","use_lightning":true,"lightning_path":"`+dir+`"}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("dir lightning path: status = %d body=%s", w.Code, w.Body.String())
	}

	// A real executable file passes (skip x-bit concern on Windows).
	exe := filepath.Join(dir, "tidb-lightning")
	script := "#!/bin/sh\n"
	if runtime.GOOS == "windows" {
		script = "@echo off\r\n"
	}
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(migrationOptionsBody{UseLightning: true, LightningPath: exe})
	if err != nil {
		t.Fatal(err)
	}
	w, req = doReq("PUT", "/api/v1/migration-options", string(body))
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid lightning path: status = %d body=%s", w.Code, w.Body.String())
	}

	// Whitespace-only temp_dir is normalized to empty (default semantics) and
	// stored trimmed — never persisted raw (M2).
	w, req = doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"   ","use_lightning":false,"lightning_path":"  "}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("whitespace-only temp_dir: status = %d body=%s", w.Code, w.Body.String())
	}
	w, req = doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	var trimmed migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &trimmed); err != nil {
		t.Fatal(err)
	}
	if trimmed.TempDir != "" || trimmed.LightningPath != "" {
		t.Fatalf("expected trimmed empty values persisted, got %+v", trimmed)
	}

	// UNC temp_dir is rejected (M3 hardening).
	w, req = doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"\\\\server\\share\\tmp","use_lightning":false}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("UNC temp_dir: status = %d body=%s", w.Code, w.Body.String())
	}

	// Atomic write leaves no stray temp files in dataDir.
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("stray temp file left behind: %s", e.Name())
		}
	}
}

// The Lightning-only target extras (pd_addr / status_port) persist alongside
// the other options; a legacy options file without the new fields reads back
// as zero values (frontend leaves its defaults untouched).
func TestMigrationOptions_TargetExtras(t *testing.T) {
	s, _ := newTestServer(t)

	// Round trip with the extras present.
	w, req := doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"","use_lightning":false,"pd_addr":" 127.0.0.1:2389 ","status_port":10081}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT extras: status = %d body=%s", w.Code, w.Body.String())
	}
	w, req = doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	var got migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PDAddr != "127.0.0.1:2389" || got.StatusPort != 10081 {
		t.Fatalf("extras round trip: got pd=%q status_port=%d, want trimmed pd and 10081", got.PDAddr, got.StatusPort)
	}

	// Clearing the memory: empty pd_addr (and status_port 0) persist as such.
	w, req = doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"","use_lightning":false,"pd_addr":"","status_port":0}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT clear extras: status = %d body=%s", w.Code, w.Body.String())
	}
	w, req = doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	var cleared migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.PDAddr != "" || cleared.StatusPort != 0 {
		t.Fatalf("cleared extras: got %+v", cleared)
	}
}

func TestMigrationOptions_LegacyFileCompat(t *testing.T) {
	s, _ := newTestServer(t)

	// A file written before the extras existed: no pd_addr / status_port keys.
	legacy := `{"temp_dir":"/tmp/x","use_lightning":true,"lightning_path":""}`
	if err := os.WriteFile(s.migrationOptionsFile(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	w, req := doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	var got migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PDAddr != "" || got.StatusPort != 0 || got.TempDir != "/tmp/x" || !got.UseLightning {
		t.Fatalf("legacy file: got %+v", got)
	}
}

func TestMigrationOptions_InvalidStatusPort(t *testing.T) {
	s, _ := newTestServer(t)

	for _, port := range []string{"-1", "65536"} {
		w, req := doReq("PUT", "/api/v1/migration-options",
			`{"temp_dir":"","use_lightning":false,"status_port":`+port+`}`)
		s.handlePutMigrationOptions(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status_port %s: status = %d body=%s", port, w.Code, w.Body.String())
		}
	}

	// Nothing was persisted by the rejected PUTs.
	w, req := doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	var got migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.StatusPort != 0 {
		t.Fatalf("rejected PUT must not persist: got %+v", got)
	}
}

// R1 merge semantics: a PUT carrying only a subset of fields must not wipe
// the values of the fields it omits.
func TestMigrationOptions_PartialPutMerges(t *testing.T) {
	s, _ := newTestServer(t)

	// Save a full set of options first.
	w, req := doReq("PUT", "/api/v1/migration-options",
		`{"temp_dir":"/tmp/keepme","use_lightning":true,"lightning_path":"","pd_addr":"10.0.0.1:2389","status_port":10081}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("full PUT: status = %d body=%s", w.Code, w.Body.String())
	}

	// Partial PUT: only pd_addr (explicit empty = clear the memory).
	w, req = doReq("PUT", "/api/v1/migration-options", `{"pd_addr":""}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial PUT: status = %d body=%s", w.Code, w.Body.String())
	}
	w, req = doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	var got migrationOptionsBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TempDir != "/tmp/keepme" || !got.UseLightning || got.PDAddr != "" || got.StatusPort != 10081 {
		t.Fatalf("partial PUT must merge, got %+v", got)
	}

	// Partial PUT of status_port only: pd_addr keeps the (cleared) value and
	// the other fields stay intact.
	w, req = doReq("PUT", "/api/v1/migration-options", `{"status_port":10080}`)
	s.handlePutMigrationOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial PUT status_port: status = %d body=%s", w.Code, w.Body.String())
	}
	w, req = doReq("GET", "/api/v1/migration-options", "")
	s.handleGetMigrationOptions(w, req)
	got = migrationOptionsBody{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.StatusPort != 10080 || got.TempDir != "/tmp/keepme" || got.PDAddr != "" {
		t.Fatalf("partial status_port PUT must merge, got %+v", got)
	}
}

// A1: pd_addr is stored normalized (scheme stripped, trailing slash removed)
// so the remembered value matches what probePD and the lightning toml use.
func TestMigrationOptions_PDAddrNormalization(t *testing.T) {
	s, _ := newTestServer(t)

	for in, want := range map[string]string{
		"http://127.0.0.1:2389/": "127.0.0.1:2389",
		"pd://host:2389":         "host:2389",
		" 10.0.0.1:2389/ ":       "10.0.0.1:2389",
		"host:2389":              "host:2389",
	} {
		body, _ := json.Marshal(map[string]string{"pd_addr": in})
		w, req := doReq("PUT", "/api/v1/migration-options", string(body))
		s.handlePutMigrationOptions(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT pd_addr=%q: status = %d body=%s", in, w.Code, w.Body.String())
		}
		w, req = doReq("GET", "/api/v1/migration-options", "")
		s.handleGetMigrationOptions(w, req)
		var got migrationOptionsBody
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.PDAddr != want {
			t.Fatalf("pd_addr %q stored as %q, want %q", in, got.PDAddr, want)
		}
	}
}
