// Package tidb is the minimal TiDB source adapter (MS-12): TiDB speaks
// the MySQL client protocol, so the DDL export feature reuses the
// MySQL-wire SHOW CREATE walk (ddlexport/exporter_mysql.go) instead of a
// fourth exporter. This adapter provides exactly the connection
// lifecycle: Connect/Close via BuildMySQLDSN (charset=utf8mb4,
// time_zone='+00:00' session pin, MS-10c note 3 discipline) and the
// tidb_version echo for the connection-test UI. Every migration-oriented
// capability (SchemaReader / DataReader / IncrementalCapture) returns a
// friendly not-implemented error — the six non-DDL-export flows are
// capability-gated 400 before they can ever reach these, the errors are
// the depth guard (nil-safe stubs, no nil interface returns).
package tidb

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/version"
	"github.com/michaelliuyuan/timstool/internal/source"

	// MySQL driver for *sql.DB (TiDB speaks the MySQL wire protocol).
	_ "github.com/go-sql-driver/mysql"
)

func init() {
	source.Register("tidb", func(cfg source.SourceConfig) (source.Source, error) {
		return &Source{cfg: cfg}, nil
	})
}

// Source is the TiDB source adapter (connection lifecycle only, MS-12).
type Source struct {
	cfg source.SourceConfig
	db  *sql.DB
}

func (s *Source) Name() string { return "tidb" }

// dsn builds the go-sql-driver DSN. Same session pinning as the mysql
// source path (DSNByType / MS-10c note 3): charset=utf8mb4 and
// time_zone='+00:00' so SHOW CREATE timestamp literals render UTC and
// the export walk is deterministic. Port 0 falls back to the registry
// default 4000 (the adapter applies its own default at dial time).
func dsn(c source.SourceConfig) string {
	port := c.Port
	if port == 0 {
		port = 4000
	}
	return config.BuildMySQLDSN(c.Host, port, c.User, c.Password, c.Database,
		map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}, nil)
}

func (s *Source) Connect(ctx context.Context) error {
	db, err := sql.Open("mysql", dsn(s.cfg))
	if err != nil {
		return fmt.Errorf("tidb source: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return fmt.Errorf("tidb source: ping: %w", err)
	}
	s.db = db
	return nil
}

func (s *Source) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Version returns the compact TiDB server version for the connection-test
// UI — ShortTiDB reduces the tidb_version() release block to
// "TiDB vX.Y.Z" (unparseable input falls back to the first line), same
// S1-UI-07 discipline as the postgres adapter; the caller echoes it as-is.
func (s *Source) Version(ctx context.Context) (string, error) {
	if s.db == nil {
		return "", fmt.Errorf("tidb source: not connected")
	}
	var v string
	if err := s.db.QueryRowContext(ctx, "SELECT tidb_version()").Scan(&v); err != nil {
		return "", err
	}
	return version.ShortTiDB(v), nil
}

// --- not-implemented capabilities (friendly errors, nil-safe stubs) ---

type notImplSchemaReader struct{}

func (notImplSchemaReader) ReadSchema(ctx context.Context, opts source.Filter) (*source.Schema, error) {
	return nil, fmt.Errorf("tidb source: schema reading is not implemented (TiDB as a source is DDL-export only, MS-12): %w", source.ErrNotImplemented)
}

type notImplDataReader struct{}

func (notImplDataReader) ReadTable(ctx context.Context, t source.Table, chunk source.ChunkSpec) (source.RowIterator, error) {
	return nil, fmt.Errorf("tidb source: data reading is not implemented (TiDB as a source is DDL-export only, MS-12): %w", source.ErrNotImplemented)
}

// passthroughTypeMapper echoes the source type: TiDB column types ARE the
// target types (same protocol family), so the identity map is the correct
// mapping. Kept non-nil so a caller that ignores the capability gate gets
// a sane answer instead of a nil-interface panic.
type passthroughTypeMapper struct{}

func (passthroughTypeMapper) MapType(srcType string, precision, scale int) source.TiDBType {
	return source.TiDBType{Name: srcType}
}

// tidbDialect is the MySQL-wire dialect shape (backtick quoting, LIMIT
// syntax) — same-source different-package twin of the mysql adapter's
// dialect, deliberately NOT unified (the adapters own their dialect).
type tidbDialect struct{}

func (tidbDialect) QuoteIdentifier(name string) string {
	return "`" + name + "`"
}

func (d tidbDialect) QuoteTable(schemaName, table string) string {
	if schemaName == "" {
		return d.QuoteIdentifier(table)
	}
	return d.QuoteIdentifier(schemaName) + "." + d.QuoteIdentifier(table)
}

func (tidbDialect) LimitOffsetSQL(limit, offset int64) string {
	if limit <= 0 {
		return fmt.Sprintf("LIMIT %d OFFSET %d", int64(1<<62-1), offset)
	}
	if offset <= 0 {
		return fmt.Sprintf("LIMIT %d", limit)
	}
	return fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
}

func (s *Source) SchemaReader() source.SchemaReader { return notImplSchemaReader{} }
func (s *Source) DataReader() source.DataReader     { return notImplDataReader{} }
func (s *Source) TypeMapper() source.TypeMapper     { return passthroughTypeMapper{} }
func (s *Source) IncrementalCapture() (source.IncrementalCapture, error) {
	return nil, source.ErrNotImplemented
}
func (s *Source) Dialect() source.Dialect { return tidbDialect{} }
