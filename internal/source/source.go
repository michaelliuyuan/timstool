package source

import (
	"context"
	"errors"
)

// ErrNotImplemented is returned by IncrementalCapture (and stub sources) for a
// capability this source does not yet support — e.g. a new source's CDC, or an
// Open() on an unimplemented source (oracle/mssql/db2 stubs).
var ErrNotImplemented = errors.New("source: capability not implemented for this source")

// SourceConfig holds the connection + selection parameters for opening a source.
// It is source-agnostic; source-specific extras go in Options. The Kind must
// match a registered adapter name ("postgres", "mysql", …).
type SourceConfig struct {
	Kind     string
	Host     string
	Port     int
	User     string
	Password string
	Database string
	Schema   string // optional default schema
	Options  map[string]string
}

// TiDBType is the target-side type produced by a TypeMapper.
type TiDBType struct {
	Name string // e.g. "VARCHAR(50)", "BIGINT", "DECIMAL(10,2)", "JSON"
}

// TypeMapper maps a source column type to the TiDB type the target should use.
type TypeMapper interface {
	MapType(srcType string, precision, scale int) TiDBType
}

// SchemaReader reads the source schema into CIR. Columns' TiDBType must be
// populated via TypeMapper so the target never has to know the source.
type SchemaReader interface {
	ReadSchema(ctx context.Context, opts Filter) (*Schema, error)
}

// RowIterator streams CIR rows for one table chunk.
type RowIterator interface {
	Next() bool   // advance; false when exhausted or on error (check Err)
	Row() Row     // the current row (valid after Next returns true)
	Err() error   // any error that stopped iteration
	Close() error // release the underlying cursor/connection
}

// DataReader reads source table data in chunks, yielding CIR rows.
type DataReader interface {
	ReadTable(ctx context.Context, t Table, chunk ChunkSpec) (RowIterator, error)
}

// IncrementalCapture is the CDC/incremental-sync capability. Sources that don't
// yet implement CDC return ErrNotImplemented from the Source.IncrementalCapture
// accessor (the caller then falls back to full-migration mode).
type IncrementalCapture interface {
	// Start streams change events from the given position (empty ⇒ current).
	Start(ctx context.Context, from Position) (<-chan ChangeEvent, error)
	// SnapshotPosition returns the position to resume from after a full snapshot.
	SnapshotPosition(ctx context.Context) (Position, error)
}

// Dialect abstracts source SQL方言: identifier quoting + pagination syntax, so
// the DataReader/SchemaReader can emit source-correct SQL without the rest of
// the pipeline knowing the dialect.
type Dialect interface {
	QuoteIdentifier(name string) string
	QuoteTable(schema, table string) string
	LimitOffsetSQL(limit, offset int64) string
}

// Source is the abstraction over a heterogeneous data source. Each DB
// (PostgreSQL, MySQL, Oracle, …) implements it; the orchestrator + TiDB target
// depend only on this interface + CIR.
type Source interface {
	Name() string // "postgres" | "mysql" | "oracle" | …
	Connect(ctx context.Context) error
	Close() error

	SchemaReader() SchemaReader
	DataReader() DataReader
	TypeMapper() TypeMapper
	// IncrementalCapture returns the CDC capability, or ErrNotImplemented if the
	// source has no CDC yet (the caller falls back to full-migration mode).
	IncrementalCapture() (IncrementalCapture, error)
	Dialect() Dialect
}
