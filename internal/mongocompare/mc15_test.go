package mongocompare

// MC-15 (adversarial MC-14 Level-2 pair): the summary.json artifact write
// must fail loudly — unique os.CreateTemp name (no fixed summary.json.tmp
// collision face), atomic rename, and error propagation so a task whose
// artifact write failed never completes silently with a 404 report or a
// stale summary from a previous run.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMC15WriteSummaryFailLoud(t *testing.T) {
	s := &Summary{StartedAt: time.Now(), FinishedAt: time.Now(),
		Counts: map[string]int{"equal": 0, "diff": 0, "error": 0, "ttl_skip": 0}, Overall: OverallEqual}

	// Happy path: atomic write lands a decodable summary.json with no
	// temp-file leftovers (the pre-MC-15 fixed name stayed behind on a
	// rename failure).
	dir := t.TempDir()
	if err := writeSummary(Config{OutDir: dir}, s); err != nil {
		t.Fatalf("writeSummary: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("summary.json missing: %v", err)
	}
	var back Summary
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("summary.json undecodable: %v", err)
	}
	if back.Overall != OverallEqual {
		t.Fatalf("roundtrip overall = %q, want equal", back.Overall)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp leftover after successful write: %s", e.Name())
		}
	}

	// Create-failure path: OutDir is a regular file, so CreateTemp fails —
	// the error must propagate with the create phase named (pre-MC-15 the
	// write and rename failures were both swallowed).
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSummary(Config{OutDir: blocker}, s); err == nil || !strings.Contains(err.Error(), "create summary temp file") {
		t.Fatalf("create-failure must propagate with phase named, got %v", err)
	}

	// Library mode (no OutDir) stays a no-op, not an error.
	if err := writeSummary(Config{}, s); err != nil {
		t.Fatalf("no-OutDir writeSummary: %v", err)
	}
}
