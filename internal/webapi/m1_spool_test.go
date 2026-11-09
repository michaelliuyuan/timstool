package webapi

import (
	"os"
	"path/filepath"
	"testing"
)

// M1: startup recovery purges the whole task-level spool tree — a crash
// can leave row-hash spool temp files behind and no compare runs during
// recovery, so everything under <dataDir>/spool is stale by definition.
func TestCompareRecoveryPurgesStaleSpool(t *testing.T) {
	s, _ := newTestServer(t)
	spool := filepath.Join(s.dataDir, "spool", "deadtask01")
	if err := os.MkdirAll(spool, 0o755); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(spool, "run-000001.spool")
	if err := os.WriteFile(junk, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.recoverStaleCompares()

	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatalf("stale spool file survived recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "spool")); !os.IsNotExist(err) {
		t.Fatalf("spool root itself should be gone after recovery purge: %v", err)
	}
}
