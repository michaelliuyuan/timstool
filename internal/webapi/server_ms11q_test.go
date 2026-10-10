package webapi

// MS-11q anchors: the validate-lightning handler upgraded with the -V
// live probe (mirror of the MS-11p dumpling face) — both branches (explicit
// path + auto-discovery) must yield a REAL version string; probe failure /
// timeout / silent binaries fail validation; the gate-reject shapes
// (missing / directory / no x-bit) keep their exact legacy behavior.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/lightning"
)

func fakeLightningScript(t *testing.T, name string, slow bool) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, name+".cmd")
		content := "@echo off\r\necho tidb-lightning v9.9.9-fake\r\n"
		if slow {
			// Busy-loop INSIDE cmd.exe (no grandchild): a spawned child
			// would inherit the probe pipes and hold them past the kill.
			content = "@echo off\r\nfor /l %%i in (1,1,50000000) do rem\r\n"
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(dir, name)
	content := "#!/bin/sh\necho 'tidb-lightning v9.9.9-fake'\n"
	if slow {
		content = "#!/bin/sh\nsleep 30\n"
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func postValidateLightning(t *testing.T, h http.HandlerFunc, body string) validateLightningResponse {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/validate-lightning", strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	var resp validateLightningResponse
	if w.Code != 200 || json.NewDecoder(w.Body).Decode(&resp) != nil {
		t.Fatalf("validate-lightning: code=%d body=%s", w.Code, w.Body.String())
	}
	return resp
}

// TestHandleValidateLightningFourShapesPlusLiveProbe: the legacy gate shapes
// (missing / directory / no x-bit unchanged) plus the MS-11q enhancement — a
// passing path must yield a REAL -V string, and an auto-discovery hit
// (empty path) is probed too.
func TestHandleValidateLightningFourShapesPlusLiveProbe(t *testing.T) {
	s, _ := newTestServer(t)

	exe := fakeLightningScript(t, "tidb-lightning", false)

	// Shape 1: explicit runnable path → success + live version string.
	resp := postValidateLightning(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, exe))
	if !resp.Success || resp.ResolvedPath != exe {
		t.Fatalf("shape1: success=%v resolved=%q msg=%q", resp.Success, resp.ResolvedPath, resp.Message)
	}
	if resp.Version != "tidb-lightning v9.9.9-fake" {
		t.Fatalf("shape1: version = %q, want the live probe output", resp.Version)
	}

	// Shape 2: missing path (legacy reject unchanged).
	resp = postValidateLightning(t, s.handleValidateLightning, `{"path":"/no/such/lightning"}`)
	if resp.Success {
		t.Fatalf("shape2: success=true, want fail (missing)")
	}

	// Shape 3: directory (legacy reject unchanged).
	resp = postValidateLightning(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, t.TempDir()))
	if resp.Success {
		t.Fatalf("shape3: success=true, want fail (directory)")
	}

	// Shape 4: no x-bit (unix only; legacy reject unchanged).
	if runtime.GOOS != "windows" {
		noexec := filepath.Join(t.TempDir(), "noexec")
		if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		resp = postValidateLightning(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, noexec))
		if resp.Success {
			t.Fatalf("shape4: success=true, want fail (no x-bit)")
		}
	}

	// Empty path + discovery miss → fail with the auto-discovery message.
	prevFind := webapiFindLightning
	webapiFindLightning = func(dataDir string) string { return "" }
	defer func() { webapiFindLightning = prevFind }()
	resp = postValidateLightning(t, s.handleValidateLightning, `{"path":""}`)
	if resp.Success || !strings.Contains(resp.Message, "自动发现失败") {
		t.Fatalf("discovery miss: success=%v msg=%q", resp.Success, resp.Message)
	}

	// Empty path + discovery hit → success carries the discovered path AND
	// the live version string (discovery does not skip 探真).
	webapiFindLightning = func(dataDir string) string { return exe }
	resp = postValidateLightning(t, s.handleValidateLightning, `{"path":"  "}`)
	if !resp.Success || resp.ResolvedPath != exe || resp.Version != "tidb-lightning v9.9.9-fake" {
		t.Fatalf("discovery hit: success=%v resolved=%q version=%q msg=%q", resp.Success, resp.ResolvedPath, resp.Version, resp.Message)
	}
	if !strings.Contains(resp.Message, "自动发现") {
		t.Fatalf("discovery hit message = %q, want 自动发现 note", resp.Message)
	}
}

// TestHandleValidateLightningProbeFailure: a path that passes the stat checks
// but cannot execute (corrupt download / wrong arch) must FAIL validation —
// the probe is the evidence, not the stat.
func TestHandleValidateLightningProbeFailure(t *testing.T) {
	s, _ := newTestServer(t)
	garbage := filepath.Join(t.TempDir(), "garbage-lightning")
	if err := os.WriteFile(garbage, []byte("this is not a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	resp := postValidateLightning(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, garbage))
	if resp.Success {
		t.Fatalf("probe failure: success=true msg=%q, want fail (cannot execute)", resp.Message)
	}
	if !strings.Contains(resp.Message, "探真失败") {
		t.Fatalf("probe failure message = %q, want 探真失败 wording", resp.Message)
	}
}

// TestHandleValidateLightningProbeTimeout: a wedged binary fails at the
// (shrunken) probe timeout — the wizard request can never hang.
func TestHandleValidateLightningProbeTimeout(t *testing.T) {
	s, _ := newTestServer(t)
	restore := lightning.SetVersionTimeout(300 * time.Millisecond)
	defer restore()
	hung := fakeLightningScript(t, "tidb-lightning-hung", true)
	resp := postValidateLightning(t, s.handleValidateLightning, fmt.Sprintf(`{"path":%q}`, hung))
	if resp.Success {
		t.Fatalf("probe timeout: success=true, want fail")
	}
	if !strings.Contains(resp.Message, "探真失败") || !strings.Contains(resp.Message, "timed out") {
		t.Fatalf("probe timeout message = %q, want 探真失败+timed out", resp.Message)
	}
}
