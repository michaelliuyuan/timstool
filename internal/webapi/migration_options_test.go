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
