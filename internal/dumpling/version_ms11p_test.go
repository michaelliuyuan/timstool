package dumpling

// MS-11p anchors: the --version live probe (探真) and per-table export
// evidence. dumpling has no embedded fallback, so a REAL version string from
// a REAL execution is the only honest proof the binary runs on this machine;
// ExportedTables is the evidence face for honest per-table checkpoint stamps.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeDumplingScript writes a platform-runnable fake tidb-dumpling that
// prints a known version line (or hangs, for the timeout shape) and returns
// its path.
func fakeDumplingScript(t *testing.T, name string, slow bool) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, name+".cmd")
		content := "@echo off\r\necho tidb-dumpling v9.9.9-fake\r\n"
		if slow {
			// Busy-loop INSIDE cmd.exe (no grandchild): a spawned child
			// would inherit the probe pipes and hold them for its full
			// lifetime even after the parent kill, stalling CombinedOutput.
			content = "@echo off\r\nfor /l %%i in (1,1,50000000) do rem\r\n"
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(dir, name)
	content := "#!/bin/sh\necho 'tidb-dumpling v9.9.9-fake'\n"
	if slow {
		content = "#!/bin/sh\nsleep 30\n"
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestVersionProbeReturnsRealString: a runnable binary's --version output
// comes back trimmed-first-line — live evidence, no embedded fallback.
func TestVersionProbeReturnsRealString(t *testing.T) {
	p := fakeDumplingScript(t, "tidb-dumpling", false)
	ver, err := Version(context.Background(), p)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if ver != "tidb-dumpling v9.9.9-fake" {
		t.Fatalf("version = %q, want the fake script's real output line", ver)
	}
}

// TestVersionProbeTimeoutFails: a wedged binary fails at the (shrinkable)
// timeout instead of hanging the caller — the timeout shape must surface as
// an error, never as success-with-empty-string.
func TestVersionProbeTimeoutFails(t *testing.T) {
	restore := SetVersionTimeout(300 * time.Millisecond)
	defer restore()
	p := fakeDumplingScript(t, "tidb-dumpling-hung", true)
	ver, err := Version(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout error (ver=%q)", err, ver)
	}
}

// TestVersionProbeFailingBinary: a nonzero exit is an error, so a corrupt
// download can never validate green.
func TestVersionProbeFailingBinary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "broken")
	if runtime.GOOS == "windows" {
		p += ".cmd"
		if err := os.WriteFile(p, []byte("@echo off\r\nexit /b 3\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Version(context.Background(), p); err == nil {
		t.Fatal("nonzero exit must fail the probe")
	}
}

// TestExportedTablesEvidence: only tables with a dumped CSV count as
// exported — file-presence truth for the honest per-table stamps.
func TestExportedTablesEvidence(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"db.ta.000000000000.csv", "db.tc.000000000001.csv", "db.tc.000000000002.csv", "notes.txt", "other.x.tsv"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := ExportedTables(dir, "db", []string{"ta", "tb", "tc"})
	if !got["ta"] || got["tb"] || !got["tc"] {
		t.Fatalf("exported = %v, want ta=true tb=false tc=true", got)
	}
	// Unreadable/missing dir = no evidence at all (never a false green).
	empty := ExportedTables(filepath.Join(dir, "missing"), "db", []string{"ta"})
	if len(empty) != 0 {
		t.Fatalf("missing dir = %v, want empty", empty)
	}
}
