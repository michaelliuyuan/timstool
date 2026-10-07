package cdc

// MS-11 pen 1 anchors: dual-source position shape. PG behavior is pinned
// byte-identical (legacy checkpoint JSON keeps parsing; a PG checkpoint
// marshals without any binlog key); MySQL binlog positions round-trip and
// render canonically; the pen-2 seam fails loud.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
)

func TestBinlogPositionString(t *testing.T) {
	bp := &BinlogPosition{File: "mysql-bin.000003", Pos: 157}
	if got := bp.String(); got != "mysql-bin.000003:157" {
		t.Fatalf("String() = %q, want mysql-bin.000003:157", got)
	}
	var nilPos *BinlogPosition
	if got := nilPos.String(); got != "" {
		t.Fatalf("nil String() = %q, want empty", got)
	}
}

func TestCDCEventPositionDualSource(t *testing.T) {
	pg := &CDCEvent{LSN: pglogrepl.LSN(0x1234)}
	if got := pg.Position(); got != pglogrepl.LSN(0x1234).String() {
		t.Fatalf("PG Position() = %q, want LSN text", got)
	}
	if pg.Binlog != nil {
		t.Fatalf("PG event must not carry a binlog position")
	}
	my := &CDCEvent{Binlog: &BinlogPosition{File: "mysql-bin.000009", Pos: 4211}}
	if got := my.Position(); got != "mysql-bin.000009:4211" {
		t.Fatalf("MySQL Position() = %q, want file:pos", got)
	}
	if my.LSN != 0 {
		t.Fatalf("MySQL event must not carry a PG LSN")
	}
}

func TestCheckpointLegacyJSONCompat(t *testing.T) {
	// A pre-MS-11 checkpoint file: LSN numeric, no binlog key. It must keep
	// parsing and render through Position() exactly as before.
	legacy := `{"lsn": 12345, "timestamp": "2026-10-07T12:00:00Z", "slot_name": "pg2tidb_cdc"}`
	var cp Checkpoint
	if err := json.Unmarshal([]byte(legacy), &cp); err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	if cp.LSN != pglogrepl.LSN(12345) || cp.Binlog != nil {
		t.Fatalf("legacy checkpoint = %+v, want LSN=12345 / no binlog", cp)
	}
	if got := cp.Position(); got != pglogrepl.LSN(12345).String() {
		t.Fatalf("legacy Position() = %q", got)
	}

	// A PG checkpoint must marshal WITHOUT any binlog key (zero PG byte-shape).
	out, err := json.Marshal(&Checkpoint{LSN: pglogrepl.LSN(7), Timestamp: time.Now(), SlotName: "s"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), `"binlog"`) {
		t.Fatalf("PG checkpoint JSON carries a binlog key: %s", out)
	}

	// A MySQL checkpoint round-trips its position and drops the LSN default.
	out2, err := json.Marshal(&Checkpoint{Timestamp: time.Now(), SlotName: "s", Binlog: &BinlogPosition{File: "mysql-bin.000001", Pos: 4}})
	if err != nil {
		t.Fatalf("marshal mysql: %v", err)
	}
	var cp2 Checkpoint
	if err := json.Unmarshal(out2, &cp2); err != nil {
		t.Fatalf("parse mysql: %v", err)
	}
	if cp2.Binlog == nil || cp2.Binlog.File != "mysql-bin.000001" || cp2.Binlog.Pos != 4 {
		t.Fatalf("mysql round-trip = %+v", cp2.Binlog)
	}
	if got := cp2.Position(); got != "mysql-bin.000001:4" {
		t.Fatalf("mysql Position() = %q", got)
	}
}

func TestCheckpointManagerBinlogRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	cm := NewCheckpointManager(path)
	cm.SetSlotName("pg2tidb_cdc")

	if got := cm.GetBinlog(); got != nil {
		t.Fatalf("fresh checkpoint binlog = %v, want nil", got)
	}
	cm.UpdateBinlog(BinlogPosition{File: "mysql-bin.000002", Pos: 900})
	if !cm.IsDirty() {
		t.Fatalf("UpdateBinlog must mark dirty")
	}
	if got := cm.GetBinlog().String(); got != "mysql-bin.000002:900" {
		t.Fatalf("GetBinlog = %q", got)
	}
	if got := cm.Position(); got != "mysql-bin.000002:900" {
		t.Fatalf("Position = %q", got)
	}
	if err := cm.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := cm.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Binlog == nil || loaded.Binlog.String() != "mysql-bin.000002:900" {
		t.Fatalf("loaded binlog = %+v", loaded.Binlog)
	}
}

func TestNewBinlogSourceFailLoud(t *testing.T) {
	if _, err := newBinlogSource(BinlogSourceConfig{ServerID: 1}); !errors.Is(err, ErrBinlogNotWired) {
		t.Fatalf("pen-1 seam must fail loud with ErrBinlogNotWired, got %v", err)
	}
	_ = context.Background()
}
