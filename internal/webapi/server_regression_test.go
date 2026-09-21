package webapi

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/store"
)

// emptyFS satisfies the embed.FS parameter of NewServer for tests.
var emptyFS embed.FS

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(st, "127.0.0.1", 0, dir, emptyFS, nil, "", 0)
	return s, st
}

// postJSON helper for handler tests.
func postJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/validate-lightning", strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// TestHandleValidateLightning covers the wizard's Lightning path gate
// (Lightning 配置门禁): explicit path must exist, be a regular file, and on
// unix carry the x-bit; an empty path falls back to auto-discovery (PATH →
// embedded), failing only when nothing can be resolved.
func TestHandleValidateLightning(t *testing.T) {
	s, _ := newTestServer(t)

	dir := t.TempDir()

	// Case 1: exists + executable (0755). On Windows the x-bit is not
	// representable, so this also covers the existence-only branch.
	exe := filepath.Join(dir, "tidb-lightning")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := postJSON(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, exe))
	var resp validateLightningResponse
	if w.Code != 200 || json.NewDecoder(w.Body).Decode(&resp) != nil {
		t.Fatalf("case1: code=%d", w.Code)
	}
	if !resp.Success || resp.ResolvedPath != exe {
		t.Errorf("case1: success=%v resolved=%q msg=%q", resp.Success, resp.ResolvedPath, resp.Message)
	}

	// Case 2: path does not exist.
	w = postJSON(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, filepath.Join(dir, "missing")))
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp.Success {
		t.Errorf("case2: err=%v success=%v (want fail)", err, resp.Success)
	}

	// Case 3: directory → fail.
	w = postJSON(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, dir))
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp.Success {
		t.Errorf("case3: err=%v success=%v (want fail)", err, resp.Success)
	}

	// Case 4: exists but no x-bit (unix only — Windows cannot represent it).
	if runtime.GOOS != "windows" {
		noexec := filepath.Join(dir, "noexec")
		if err := os.WriteFile(noexec, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		w = postJSON(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, noexec))
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp.Success {
			t.Errorf("case4: err=%v success=%v (want fail: no x-bit)", err, resp.Success)
		}
	}

	// Case 5: empty path → auto-discovery. Put a real executable on PATH so
	// FindBinary resolves it regardless of the build's embedded placeholder.
	bindir := t.TempDir()
	stub := filepath.Join(bindir, "tidb-lightning")
	if runtime.GOOS == "windows" {
		stub += ".exe" // exec.LookPath on Windows requires a PATHEXT extension
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+string(os.PathListSeparator)+os.Getenv("PATH"))
	w = postJSON(t, s.handleValidateLightning, `{"path":""}`)
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("case5 decode: %v", err)
	}
	if !resp.Success || resp.ResolvedPath == "" {
		t.Errorf("case5: success=%v resolved=%q msg=%q (want auto-discovery hit)", resp.Success, resp.ResolvedPath, resp.Message)
	}
}
