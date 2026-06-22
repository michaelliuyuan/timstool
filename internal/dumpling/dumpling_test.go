package dumpling

import (
	"strings"
	"testing"
)

// TestBuildArgs_CSVSeparatorIsRealTab locks in the separator/delimiter contract
// with Lightning. This guards against a subtle regression: writing "\\t" (a
// 2-char backslash-t literal) instead of "\t" (a single TAB byte) silently
// corrupts every exported value — dumpling splits on 2 bytes (0x5c 0x74) while
// Lightning splits on a real TAB (0x09), so rows misalign. Row counts still
// pass, masking the corruption. See #t83 (MySQL export) follow-up.
func TestBuildArgs_CSVSeparatorIsRealTab(t *testing.T) {
	cfg := DumpConfig{
		Host: "127.0.0.1", Port: 3306, User: "u", Password: "p",
		OutputDir: "/tmp/out", Database: "db",
	}
	args := buildArgs(cfg)

	sep, ok := flagValue(args, "--csv-separator")
	if !ok {
		t.Fatalf("--csv-separator missing from args: %v", args)
	}
	if len(sep) != 1 || sep[0] != 0x09 {
		t.Fatalf("--csv-separator = %q (bytes=% x), want single real TAB byte 0x09", sep, []byte(sep))
	}
	// Guard against the exact regression: the 2-char literal backslash-t.
	if sep == "\\t" {
		t.Fatalf("--csv-separator is the 2-char literal backslash-t — dumpling will treat it as a 2-byte separator and corrupt data")
	}

	del, ok := flagValue(args, "--csv-delimiter")
	if !ok {
		t.Fatalf("--csv-delimiter missing from args: %v", args)
	}
	if del != "" {
		t.Fatalf("--csv-delimiter = %q, want empty (match Lightning delimiter=\"\")", del)
	}
}

// TestBuildArgs_OtherFlags asserts the MySQL-compatible defaults that prior
// #t83 fixes established (consistency=lock, no-header) survive refactoring.
func TestBuildArgs_OtherFlags(t *testing.T) {
	args := buildArgs(DumpConfig{OutputDir: "/tmp/out"})

	consistency, _ := flagValue(args, "--consistency")
	// "--consistency=" may be a single combined arg or a value after the flag.
	if !containsArg(args, "--consistency=lock") && consistency != "lock" {
		t.Fatalf("consistency default = %q, want lock (snapshot is TiDB-only)", consistency)
	}
	if !containsArg(args, "--no-header") {
		t.Fatalf("--no-header missing from args: %v", args)
	}
}

// flagValue returns the value following a "--name" flag token, or the empty
// combined form "--name=value" (returns "" for empty value). ok=false if absent.
func flagValue(args []string, name string) (string, bool) {
	prefix := name + "="
	for i, a := range args {
		if a == name {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix), true
		}
	}
	return "", false
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
