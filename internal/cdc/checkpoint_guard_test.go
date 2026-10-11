package cdc

// MS-11d anchors: cross-source checkpoint residue must never resume — a PG
// LSN is not a binlog position and vice versa. LoadForSource discards the
// stale file (logged, never fatal) and returns a fresh start.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"go.uber.org/zap"
)

func writeCheckpointFile(t *testing.T, dir string, cp Checkpoint) string {
	t.Helper()
	path := filepath.Join(dir, "cp.json")
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadForSourceCrossSourceDiscard(t *testing.T) {
	dir := t.TempDir()

	// Case 1: MySQL checkpoint + PG config -> discarded, file removed.
	path := writeCheckpointFile(t, dir, Checkpoint{
		Binlog:    &BinlogPosition{File: "binlog.000005", Pos: 6636},
		Timestamp: time.Now(),
	})
	cm := NewCheckpointManager(path)
	cm.SetLogger(zap.NewNop())
	cp, err := cm.LoadForSource(SourceKindPostgres)
	if err != nil {
		t.Fatal(err)
	}
	if cp != nil {
		t.Fatalf("PG runner must not resume from a binlog checkpoint: %+v", cp)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale cross-source checkpoint file must be removed")
	}

	// Case 2: PG checkpoint + MySQL config -> discarded, file removed.
	path = writeCheckpointFile(t, dir, Checkpoint{
		LSN:       pglogrepl.LSN(0x1A2B3C4),
		Timestamp: time.Now(),
	})
	cm = NewCheckpointManager(path)
	cm.SetLogger(zap.NewNop())
	cp, err = cm.LoadForSource(SourceKindMySQL)
	if err != nil {
		t.Fatal(err)
	}
	if cp != nil {
		t.Fatalf("MySQL runner must not resume from a PG LSN checkpoint: %+v", cp)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale cross-source checkpoint file must be removed")
	}
}

func TestLoadForSourceSameSourceResumes(t *testing.T) {
	dir := t.TempDir()

	// Same-kind MySQL: binlog checkpoint resumes verbatim.
	path := writeCheckpointFile(t, dir, Checkpoint{
		Binlog:    &BinlogPosition{File: "mysql-bin.000003", Pos: 4567},
		Timestamp: time.Now(),
	})
	cm := NewCheckpointManager(path)
	cm.SetLogger(zap.NewNop())
	cp, err := cm.LoadForSource(SourceKindMySQL)
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil || cp.Binlog == nil || cp.Binlog.File != "mysql-bin.000003" || cp.Binlog.Pos != 4567 {
		t.Fatalf("same-source binlog checkpoint must resume, got %+v", cp)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("matching checkpoint file must survive")
	}

	// Same-kind PG: hex LSN resumes verbatim.
	path = writeCheckpointFile(t, dir, Checkpoint{
		LSN:       pglogrepl.LSN(0x2B00000000),
		Timestamp: time.Now(),
	})
	cm = NewCheckpointManager(path)
	cm.SetLogger(zap.NewNop())
	cp, err = cm.LoadForSource(SourceKindPostgres)
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil || cp.LSN != pglogrepl.LSN(0x2B00000000) {
		t.Fatalf("same-source PG checkpoint must resume, got %+v", cp)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("matching checkpoint file must survive")
	}
}

func TestLoadForSourceMalformedShapeDiscarded(t *testing.T) {
	dir := t.TempDir()
	// A PG checkpoint whose LSN renders outside the hex/hex shape (zero LSN
	// writes "0/0" but carries no resumable position) never resumes.
	path := writeCheckpointFile(t, dir, Checkpoint{Timestamp: time.Now()})
	cm := NewCheckpointManager(path)
	cm.SetLogger(zap.NewNop())
	if cp, err := cm.LoadForSource(SourceKindPostgres); err != nil || cp != nil {
		t.Fatalf("zero-LSN checkpoint must not resume: %+v %v", cp, err)
	}
	// A binlog pair without the numeric rotation suffix is not a shape match.
	path = writeCheckpointFile(t, dir, Checkpoint{
		Binlog:    &BinlogPosition{File: "binlog", Pos: 100},
		Timestamp: time.Now(),
	})
	cm = NewCheckpointManager(path)
	cm.SetLogger(zap.NewNop())
	if cp, err := cm.LoadForSource(SourceKindMySQL); err != nil || cp != nil {
		t.Fatalf("suffix-less binlog file must not resume: %+v %v", cp, err)
	}
}
