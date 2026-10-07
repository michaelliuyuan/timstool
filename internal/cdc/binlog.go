package cdc

// binlog.go — MS-11 pen 1 seams: the source-neutral plug points the MySQL
// binlog collection layer (pen 2, binlog_canal.go) wires into and the runner
// reuses for MySQL chains (pen 3). The PG pipeline (Source/Runner) is
// untouched.

import (
	"context"

	// Pin the binlog collection dependency (MS-11 pen 1) so go.mod carries it
	// as a direct require and go.sum is complete. Pure Go, CGO=0 preserved.
	// binlog_canal.go (pen 2) imports it for real.
	_ "github.com/go-mysql-org/go-mysql/canal"
)

type errorString string

func (e errorString) Error() string { return string(e) }

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

// newBinlogSource is the constructor seam (canal adapter since pen 2).
// Package-level var so tests can stub it exactly like the orchestrator CIR
// seams; the default wires the canal implementation (init in
// binlog_canal.go).
var newBinlogSource = func(cfg BinlogSourceConfig) (binlogStreamer, error) {
	return nil, errBinlogSourceNil
}

// errBinlogSourceNil is replaced by binlog_canal.go's init wiring; seeing it
// means the wiring was removed — fail loud rather than nil-stream.
var errBinlogSourceNil = errorString("binlog source not wired (canal adapter missing)")

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
