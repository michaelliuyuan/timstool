// Package source defines the source-agnostic abstractions for the multi-source
// heterogeneous DB → TiDB migration tool (timstool): the Source adapter
// interface, the Common Intermediate Representation (CIR) for schema/data, and
// the adapter registry. Each source DB (PostgreSQL, MySQL, Oracle, …)
// implements Source; the orchestrator + TiDB target depend only on these
// interfaces + CIR, never on a specific source.
//
// This is Phase 1 (#t64) of the multi-source refactor. It is purely additive:
// no existing code is touched, so the PG/CDC behavior is unchanged.
package source

// CIR (Common Intermediate Representation) — DB-neutral schema + data model.
// The orchestrator and the TiDB target only ever see CIR; they never know
// whether the source was PostgreSQL, MySQL, … . Key invariant: a CIR Column's
// type is ALREADY the TiDB type (TypeMapper maps it during ReadSchema), and a
// CIR Row carries per-column TiDB types, so the target renders data directly.

// Schema is the DB-neutral representation of a source database's structure.
type Schema struct {
	Catalog string // source catalog/database name
	Tables  []Table
	Views   []View // optional; sources may leave empty
}

// Table is a DB-neutral table definition.
type Table struct {
	Schema  string // source schema (e.g. PG "public", MySQL db name, MSSQL "dbo")
	Name    string
	Columns []Column
	PK      []string // primary-key column names (empty if none)
	Indexes []Index
}

// Column is a DB-neutral column. SourceType is the original source type string;
// TiDBType is the TypeMapper-mapped target type the applier should use.
type Column struct {
	Name       string
	SourceType string // e.g. PG "varchar(50)", MySQL "int", Oracle "NUMBER(10,2)"
	TiDBType   string // TypeMapper result, e.g. "VARCHAR(50)", "BIGINT", "DECIMAL(10,2)"
	Nullable   bool
	Default    string // default-value expression (may need source→TiDB translation)
	IsAutoIncr bool   // auto-increment/serial/identity
	Comment    string
}

// Index is a DB-neutral index definition.
type Index struct {
	Name    string
	Columns []string
	Unique  bool
}

// View is a DB-neutral view definition (definition is source SQL; may need
// translation for TiDB — sources can leave it for manual review).
type View struct {
	Schema     string
	Name       string
	Definition string
}

// Row is one data row: column name → typed value. The target renders each value
// according to the column's TiDBType (carried in TypedValue).
type Row map[string]TypedValue

// TypedValue pairs a value with the TiDB type the target should use to render it.
type TypedValue struct {
	TiDBType string
	Val      interface{} // nil ⇒ SQL NULL
}

// Filter selects which tables a SchemaReader reads.
type Filter struct {
	Tables        []string // include list (schema.table or table); empty ⇒ all
	ExcludeTables []string // exclude list
}

// ChunkSpec describes a slice of a table for batched/streamed reading.
type ChunkSpec struct {
	Offset int64 // row offset (LIMIT/OFFSET pagination)
	Limit  int64 // max rows in this chunk (0 ⇒ no limit)
	// Future: key-range pagination (MinKey/MaxKey) for large-table sharding.
}

// ChangeKind is the type of an incremental (CDC) change event.
type ChangeKind string

const (
	ChangeInsert ChangeKind = "insert"
	ChangeUpdate ChangeKind = "update"
	ChangeDelete ChangeKind = "delete"
)

// ChangeEvent is one incremental (CDC) change. Columns carries the row image
// (new image for insert/update; key image for delete). Position is the
// source-specific progress marker (PG LSN, MySQL binlog pos, …).
type ChangeEvent struct {
	Kind    ChangeKind
	Schema  string
	Table   string
	Columns Row
}

// Position is an opaque, source-specific CDC progress marker (e.g. PG LSN
// string, MySQL "(file, pos)"). It is checkpointed for at-least-once resume.
type Position string
