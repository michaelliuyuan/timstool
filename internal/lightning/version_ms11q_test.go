package lightning

// MS-11q anchors: the --version live probe (探真) for tidb-lightning —
// validation upgrades from "stat pass = green" to "stat pass + probe pass =
// green". The embedded form is a placeholder stub that never yields a fake
// path, so a probe always runs against a real executable: a REAL version
// string from a REAL execution is the only honest proof the binary runs on
// this machine.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeLightningScript writes a platform-runnable fake tidb-lightning that
// prints a known version line (or hangs, for the timeout shape) and returns
// its path.
func fakeLightningScript(t *testing.T, name string, slow bool) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, name+".cmd")
		content := "@echo off\r\necho tidb-lightning v9.9.9-fake\r\n"
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
	content := "#!/bin/sh\necho 'tidb-lightning v9.9.9-fake'\n"
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
	p := fakeLightningScript(t, "tidb-lightning", false)
	ver, err := Version(context.Background(), p)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if ver != "tidb-lightning v9.9.9-fake" {
		t.Fatalf("version = %q, want the fake script's real output line", ver)
	}
}

// TestVersionProbeTimeoutFails: a wedged binary fails at the (shrinkable)
// timeout instead of hanging the caller — the timeout shape must surface as
// an error, never as success-with-empty-string.
func TestVersionProbeTimeoutFails(t *testing.T) {
	restore := SetVersionTimeout(300 * time.Millisecond)
	defer restore()
	p := fakeLightningScript(t, "tidb-lightning-hung", true)
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

// TestVersionProbeEmptyOutputFails: a zero-exit binary that prints nothing is
// an error — silence is not evidence.
func TestVersionProbeEmptyOutputFails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "silent")
	if runtime.GOOS == "windows" {
		p += ".cmd"
		if err := os.WriteFile(p, []byte("@echo off\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Version(context.Background(), p); err == nil || !strings.Contains(err.Error(), "produced no output") {
		t.Fatalf("err = %v, want produced-no-output error", err)
	}
}
