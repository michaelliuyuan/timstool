package dumpling

import (
	"os"
	"path/filepath"
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

// TestCountExportedRows locks in the row-counting the dumpling fast-path needs
// to report real rows to the progress layer. Guards the regression where the
// orchestrator's dumpling branch hard-coded 0 and the UI showed "0 rows
// migrated" despite Lightning loading all data. See #t83.
func TestCountExportedRows(t *testing.T) {
	dir := t.TempDir()
	db := "mydb"

	// t_users: single shard, 3 rows (trailing newline).
	mustWrite(t, filepath.Join(dir, "mydb.t_users.000000000000.csv"),
		"1\talice\n2\tbob\n3\tcarol\n")
	// t_orders: sharded across two files (4 + 6 = 10 rows).
	mustWrite(t, filepath.Join(dir, "mydb.t_orders.000000000000.csv"),
		"1\t100\n2\t100\n3\t100\n4\t100\n")
	mustWrite(t, filepath.Join(dir, "mydb.t_orders.000000000001.csv"),
		"5\t100\n6\t100\n7\t100\n8\t100\n9\t100\n10\t100\n")
	// t_empty: one shard, zero rows (empty file).
	mustWrite(t, filepath.Join(dir, "mydb.t_empty.000000000000.csv"), "")
	// t_notrail: final record with NO trailing newline (1 row, must still count).
	mustWrite(t, filepath.Join(dir, "mydb.t_notrail.000000000000.csv"),
		"1\tsolo")
	// Distractor files that must be ignored: schema SQL + a different table
	// whose name starts with the same prefix as t_orders (t_orders_archive).
	mustWrite(t, filepath.Join(dir, "mydb.t_users-schema.sql"), "CREATE TABLE...\n")
	mustWrite(t, filepath.Join(dir, "mydb.t_orders_archive.000000000000.csv"),
		"99\t999\n")

	counts := CountExportedRows(dir, db, []string{"t_users", "t_orders", "t_empty", "t_notrail", "missing"})

	cases := map[string]int64{
		"t_users":   3,
		"t_orders":  10, // summed across shards
		"t_empty":   0,
		"t_notrail": 1, // no trailing newline, still counted
		"missing":   0, // no files -> 0, zero-value entry present
	}
	for table, want := range cases {
		if got := counts[table]; got != want {
			t.Errorf("CountExportedRows[%q] = %d, want %d", table, got, want)
		}
	}
	// t_orders_archive must NOT leak into t_orders (prefix "." boundary).
	if _, leaked := counts["t_orders_archive"]; leaked {
		t.Errorf("t_orders_archive leaked into counts; tables arg should scope counting")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
