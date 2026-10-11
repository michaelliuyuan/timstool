// Package cdc implements PostgreSQL → TiDB change data capture (CDC)
// incremental sync using PG logical replication (pgoutput plugin).
package cdc

import (
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"
)

// EventKind categorizes a CDC change event.
type EventKind string

const (
	EventInsert   EventKind = "insert"
	EventUpdate   EventKind = "update"
	EventDelete   EventKind = "delete"
	EventDDL      EventKind = "ddl"
	EventTruncate EventKind = "truncate"
)

// CDCEvent is a unified change event produced by the Transformer and consumed
// by downstream appliers.
type CDCEvent struct {
	LSN        pglogrepl.LSN `json:"lsn"`
	Timestamp  time.Time     `json:"timestamp"`
	Kind       EventKind     `json:"kind"`
	Schema     string        `json:"schema"`
	Table      string        `json:"table"`
	Columns    []ColumnValue `json:"columns,omitempty"`     // INSERT / UPDATE / DELETE
	OldColumns []ColumnValue `json:"old_columns,omitempty"` // UPDATE old values
	DDL        string        `json:"ddl,omitempty"`         // DDL statement text
	RawData    []byte        `json:"-"`                     // raw pgoutput message (for replay)

	// Binlog is the MySQL source position (MS-11 pen 1, dual-source shape).
	// Nil on PG events; LSN is the PG marker. Exactly one of the two is set
	// for a real event from a live source.
	Binlog *BinlogPosition `json:"binlog,omitempty"`
}

// Position renders the source-neutral progress marker of this event:
// PG → WAL LSN text, MySQL → binlog file:pos (MS-11 pen 1).
func (e *CDCEvent) Position() string {
	if e.Binlog != nil {
		return e.Binlog.String()
	}
	return e.LSN.String()
}

// BinlogPosition is a MySQL binlog coordinate (MS-11 v1: file:pos; GTID is a
// v2 candidate — deliberately NOT carried here to keep one positioning
// semantic per struct).
type BinlogPosition struct {
	File string `json:"file"` // binlog file name, e.g. "mysql-bin.000003"
	Pos  uint32 `json:"pos"`  // byte offset within the file
}

// String renders the canonical "file:pos" display form.
func (p *BinlogPosition) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", p.File, p.Pos)
}

// ColumnValue is a single column name/value pair in a CDC event.
type ColumnValue struct {
	Name      string      `json:"name"`
	Value     interface{} `json:"value"`
	Type      string      `json:"type"`                // PG data type OID string
	IsKey     bool        `json:"is_key,omitempty"`    // part of the relation PK / replica identity (from RelationMessage KeyColumn flag)
	Unchanged bool        `json:"unchanged,omitempty"` // pgoutput 'u': unchanged TOASTed value, not sent — must never be rendered as a literal
}

// Checkpoint records the last successfully processed position (dual-source
// shape, MS-11 pen 1): LSN is the PG WAL marker (legacy field — existing
// checkpoint files keep parsing unchanged), Binlog the MySQL marker.
type Checkpoint struct {
	LSN       pglogrepl.LSN   `json:"lsn"`
	Timestamp time.Time       `json:"timestamp"`
	SlotName  string          `json:"slot_name"`
	Binlog    *BinlogPosition `json:"binlog,omitempty"`
	// LastDDLID is the last applied pg2tidb_ddl_log.id (DDL replication resume,
	// #t59). At-least-once: on restart, DDL poll resumes from here so already-
	// applied DDL isn't replayed.
	LastDDLID int64 `json:"last_ddl_id,omitempty"`
}

// Position renders the source-neutral checkpoint marker: PG → WAL LSN text,
// MySQL → binlog file:pos (MS-11 pen 1).
func (cp *Checkpoint) Position() string {
	if cp == nil {
		return ""
	}
	if cp.Binlog != nil {
		return cp.Binlog.String()
	}
	return cp.LSN.String()
}

// SourceConfig configures the PG logical replication source.
type SourceConfig struct {
	// Connection
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	SSLMode  string `json:"sslmode"`

	// Replication
	SlotName     string `json:"slot_name"`     // replication slot name
	Publication  string `json:"publication"`   // publication name
	OutputPlugin string `json:"output_plugin"` // default "pgoutput"

	// Tables to replicate (schema.table format)
	Tables        []string `json:"tables"`
	ExcludeTables []string `json:"exclude_tables"`

	// Checkpoint
	CheckpointFile string `json:"checkpoint_file"` // LSN checkpoint file path
}

// DefaultSourceConfig returns a SourceConfig with sensible defaults.
func DefaultSourceConfig() SourceConfig {
	return SourceConfig{
		SSLMode:        "disable",
		SlotName:       "pg2tidb_cdc",
		Publication:    "pg2tidb_pub",
		OutputPlugin:   "pgoutput",
		CheckpointFile: ".cdc_checkpoint.json",
	}
}

// TransformerConfig configures event transformation behavior.
type TransformerConfig struct {
	// IncludeOldValues controls whether UPDATE events carry old column values.
	IncludeOldValues bool `json:"include_old_values"`

	// MaxColumnValueLength truncates large column values (0 = no limit).
	MaxColumnValueLength int `json:"max_column_value_length"`
}

// DefaultTransformerConfig returns sensible defaults.
func DefaultTransformerConfig() TransformerConfig {
	return TransformerConfig{
		IncludeOldValues:     true,
		MaxColumnValueLength: 0,
	}
}
