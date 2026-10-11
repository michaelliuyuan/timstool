package lightning

// MS-11q: the -V live probe (探真), mirroring dumpling.Version
// per-package (symmetric, no shared package — scope discipline). Lightning
// validation upgrades from "stat pass = green" to "stat pass + probe pass =
// green": the embedded form is a placeholder stub that never yields a fake
// path, so a probe always runs against a real executable — a REAL version
// string is the only honest proof the binary runs on THIS machine.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// versionTimeout bounds the `<path> -V` probe. Package-level so tests
// can shrink it (the timeout shape needs to be anchored without waiting the
// real 5s).
var versionTimeout = 5 * time.Second

// SetVersionTimeout overrides the -V probe timeout and returns a
// restore func (test seam; production default stays 5s).
func SetVersionTimeout(d time.Duration) func() {
	prev := versionTimeout
	versionTimeout = d
	return func() { versionTimeout = prev }
}

// Version runs `<binary> -V` and returns the trimmed first output line
// — live evidence that the path is truly executable. A hung or wedged binary
// fails at the timeout instead of hanging the wizard request.
func Version(ctx context.Context, binary string) (string, error) {
	c, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, binary, "-V")
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "TERM=dumb")
	out, err := cmd.CombinedOutput()
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if c.Err() != nil {
		return line, fmt.Errorf("-V timed out after %s: %w", versionTimeout, c.Err())
	}
	if err != nil {
		return line, fmt.Errorf("-V failed: %w%s", err, tailHint(out))
	}
	if line == "" {
		return "", fmt.Errorf("-V produced no output")
	}
	return line, nil
}

// tailHint renders a short output tail for error messages (bounded like
// Dump's).
func tailHint(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	if s != "" {
		return "\n--- output tail ---\n" + s
	}
	return ""
}
