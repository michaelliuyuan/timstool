package cdc

// binlog.go — MS-11 pen 1 seams: the source-neutral plug points the MySQL
// binlog collection layer (pen 2) wires into and the runner reuses for
// MySQL chains (pen 3). Nothing here runs yet — newBinlogSource is a
// fail-loud slot, and the PG pipeline (Source/Runner) is untouched.

import (
	"context"
	"errors"

	// Pin the binlog collection dependency (MS-11 pen 1) so go.mod carries it
	// as a direct require and go.sum is complete before pen 2 wires the canal
	// adapter in. Pure Go, CGO=0 preserved.
	_ "github.com/go-mysql-org/go-mysql/canal"
)

// BinlogSourceConfig configures the MySQL binlog replication source
// (MS-11 v1: file:pos positioning; GTID is a v2 candidate).
type BinlogSourceConfig struct {
	// Connection
	Host     string
	Port     int
	User     string
	Password string
	Database string

	// Replication
	ServerID uint32 // binlog dump registration id — must be unique in the replication topology

	// Tables to replicate (database.table format)
	Tables        []string
	ExcludeTables []string
}

// ErrBinlogNotWired is returned by the pen-1 seam until pen 2 lands the
// canal-based implementation. Fail-loud by design: a configured-but-missing
// binlog source must never silently no-op.
var ErrBinlogNotWired = errors.New("mysql binlog source not wired (MS-11 pen 2)")

// binlogStreamer is the contract the pen-2 collection layer implements —
// shaped to mirror the PG *Source surface the Runner already drives:
// stream events with embedded positions, expose the current read position.
type binlogStreamer interface {
	// Start begins the binlog dump from the given position (nil = current
	// master position, i.e. chain-seeded fresh start) and returns the event
	// stream. Events carry Binlog positions (never a raw LSN).
	Start(ctx context.Context, from *BinlogPosition) (<-chan *CDCEvent, error)

	// CurrentPosition returns the most recently observed binlog coordinate
	// (the one-behind rule mirrors the PG source: only positions whose
	// events have been fully delivered may be reported).
	CurrentPosition() *BinlogPosition

	// Err returns the fatal error that stopped the stream, nil while running.
	Err() error

	// Close tears the stream down.
	Close() error
}

// newBinlogSource is the pen-2 fill-in seam (canal adapter vs raw
// replication stream is a ruled pen-2 decision). Package-level var so tests
// can stub it exactly like the orchestrator CIR seams. Implementations
// follow the house pattern: constructor + SetLogger.
var newBinlogSource = func(cfg BinlogSourceConfig) (binlogStreamer, error) {
	return nil, ErrBinlogNotWired
}
