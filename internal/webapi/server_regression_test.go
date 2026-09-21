package webapi

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

// --- PD / Status probe tests (target connection test aggregation, N1) ---

// TestProbeTargetExtras covers the four aggregation cases of the optional
// PD/Status probes used by the target connection test:
// 1) nothing provided -> both not provided (fields absent, never block);
// 2) PD provided and reachable -> pd_ok=true + cluster id;
// 3) PD provided but wrong -> pd_ok=false with error (must block);
// 4) Status port provided but wrong -> status_ok=false with error.
func TestProbeTargetExtras(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	// Case 1: nothing provided — non-Lightning users are never probed.
	pd, st := s.probeTargetExtras(ctx, "127.0.0.1", "", 0)
	if pd.provided || st.provided {
		t.Errorf("case1: pd.provided=%v st.provided=%v, want false/false", pd.provided, st.provided)
	}

	// Fake PD serving /pd/api/v1/cluster with a cluster id.
	pdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pd/api/v1/cluster" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"cluster_id":7227548617281234567}`))
	}))
	defer pdSrv.Close()
	pdHost := strings.TrimPrefix(pdSrv.URL, "http://")

	// Case 2: PD reachable (scheme + trailing slash tolerated).
	pd, _ = s.probeTargetExtras(ctx, "127.0.0.1", pdHost+"/", 0)
	if !pd.provided || !pd.ok || pd.clusterID != "7227548617281234567" {
		t.Errorf("case2: provided=%v ok=%v cluster=%q (want true/true/cluster id)", pd.provided, pd.ok, pd.clusterID)
	}
	_, err := probePD(context.Background(), "http://"+pdHost)
	if err != nil {
		t.Errorf("case2 scheme tolerance: probePD(http://host) = %v, want nil", err)
	}

	// Case 3: PD provided but unreachable (SQL-port misconfig scenario).
	pd, _ = s.probeTargetExtras(ctx, "127.0.0.1", "127.0.0.1:1", 0)
	if !pd.provided || pd.ok || pd.errMsg == "" {
		t.Errorf("case3: provided=%v ok=%v (want true/false with error)", pd.provided, pd.ok)
	}

	// Case 4: status port provided but unreachable.
	_, st = s.probeTargetExtras(ctx, "127.0.0.1", "", 1)
	if !st.provided || st.ok || st.errMsg == "" {
		t.Errorf("case4: provided=%v ok=%v (want true/false with error)", st.provided, st.ok)
	}

	// Status port reachable via fake /status.
	stSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"connections":0}`))
	}))
	defer stSrv.Close()
	stHost, stPortStr, _ := net.SplitHostPort(strings.TrimPrefix(stSrv.URL, "http://"))
	stPort, _ := strconv.Atoi(stPortStr)
	_, st = s.probeTargetExtras(ctx, stHost, "", stPort)
	if !st.provided || !st.ok {
		t.Errorf("status reachable: provided=%v ok=%v err=%q (want true/true)", st.provided, st.ok, st.errMsg)
	}
}

// TestTestTiDBConnection_MySQLFail covers the early-return aggregation path:
// when the MySQL target itself is unreachable the result is ok=false and the
// optional PD/Status fields are absent (probes never run without MySQL).
func TestTestTiDBConnection_MySQLFail(t *testing.T) {
	s, _ := newTestServer(t)
	res := s.testTiDBConnection(context.Background(), &TestConnectionRequest{
		Type: "target", Host: "127.0.0.1", Port: 1, User: "u", Password: "p",
	})
	if res["ok"] != false {
		t.Errorf("ok = %v, want false", res["ok"])
	}
	if res["mysql_ok"] != false {
		t.Errorf("mysql_ok = %v, want explicit false", res["mysql_ok"])
	}
	if _, has := res["pd_ok"]; has {
		t.Errorf("pd_ok should be absent when MySQL fails")
	}
	if res["error"] == "" {
		t.Errorf("error message missing")
	}
}
