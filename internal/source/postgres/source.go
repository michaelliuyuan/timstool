// Package postgres is the PostgreSQL source adapter for timstool. It implements
// source.Source by wrapping the existing, already-validated PG code
// (internal/schema type mapping; SchemaReader/DataReader/CDC wired in P1 step
// 2b/2c/2d). Phase 1 step 2a is the skeleton: Connect/Close/Dialect/TypeMapper
// are real; SchemaReader/DataReader/IncrementalCapture are filled in next. No
// existing PG code is modified, so PG/CDC behavior is unchanged (zero regression).
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/schema"
	"github.com/michaelliuyuan/timstool/internal/source"

	// PG driver for the *sql.DB used by Connect.
	_ "github.com/jackc/pgx/v5/stdlib"
)

func init() {
	source.Register("postgres", func(cfg source.SourceConfig) (source.Source, error) {
		return &Source{cfg: cfg}, nil
	})
}

// Source is the PostgreSQL source adapter.
type Source struct {
	cfg source.SourceConfig
	db  *sql.DB
}

func (s *Source) Name() string { return "postgres" }

func (s *Source) Connect(ctx context.Context) error {
	db, err := sql.Open("pgx", dsn(s.cfg))
	if err != nil {
		return fmt.Errorf("postgres source: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return fmt.Errorf("postgres source: ping: %w", err)
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

// DB exposes the underlying connection for the (2b/2c) readers that wrap the
// existing schema/data packages.
func (s *Source) DB() *sql.DB { return s.db }

// Config exposes the source config for the readers.
func (s *Source) Config() source.SourceConfig { return s.cfg }

func (s *Source) Dialect() source.Dialect { return pgDialect{} }

func (s *Source) TypeMapper() source.TypeMapper { return pgTypeMapper{} }

// ConfigSchema declares the PG connection-form fields for the Web UI (#t67 WSC).
func (s *Source) ConfigSchema() []source.ConfigField {
	return []source.ConfigField{
		{Name: "host", Label: "Host", Type: "text", Required: true, Default: "localhost", Placeholder: "localhost", Group: "connection"},
		{Name: "port", Label: "Port", Type: "number", Required: true, Default: "5432", Group: "connection"},
		{Name: "user", Label: "User", Type: "text", Required: true, Default: "postgres", Group: "connection"},
		{Name: "password", Label: "Password", Type: "password", Group: "connection"},
		{Name: "database", Label: "Database", Type: "text", Required: true, Group: "connection"},
		{Name: "schema", Label: "Schema", Type: "text", Default: "public", Group: "connection"},
		{Name: "sslmode", Label: "SSL Mode", Type: "select", Default: "disable", Options: []string{"disable", "require", "verify-ca", "verify-full"}, Group: "advanced"},
	}
}

// SchemaReader is wired in 2b (internal/schema Collector → CIR). 2a stub.
func (s *Source) SchemaReader() source.SchemaReader { return &schemaReader{src: s} }

// DataReader is wired in 2c (internal/data COPY → CIR Row). 2a stub.
func (s *Source) DataReader() source.DataReader { return &dataReader{src: s} }

// IncrementalCapture signals that PG has CDC capability (unlike MySQL which
// returns ErrNotImplemented). The full CDC pipeline (logical replication →
// transform → apply + DDL replication + Web monitoring) stays intact in
// internal/cdc.Runner and is managed by the orchestrator (2e) which has the
// target config. Constraint §10.3: CDC kept intact, not split.
func (s *Source) IncrementalCapture() (source.IncrementalCapture, error) {
	return &pgIncrementalCapture{src: s}, nil
}

// dsn builds a libpq URL from the source config.
func dsn(c source.SourceConfig) string {
	sslmode := c.Options["sslmode"]
	if sslmode == "" {
		sslmode = "disable"
	}
	return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(c.User), url.QueryEscape(c.Password), c.Host, c.Port, c.Database, sslmode)
}

// --- Dialect ---

type pgDialect struct{}

func (pgDialect) QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (d pgDialect) QuoteTable(schemaName, table string) string {
	if schemaName == "" {
		return d.QuoteIdentifier(table)
	}
	return d.QuoteIdentifier(schemaName) + "." + d.QuoteIdentifier(table)
}

func (pgDialect) LimitOffsetSQL(limit, offset int64) string {
	if limit <= 0 {
		return fmt.Sprintf("OFFSET %d", offset)
	}
	if offset <= 0 {
		return fmt.Sprintf("LIMIT %d", limit)
	}
	return fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
}

// --- TypeMapper (delegates to the existing validated PG→TiDB mapping) ---

type pgTypeMapper struct{}

func (pgTypeMapper) MapType(srcType string, precision, scale int) source.TiDBType {
	return source.TiDBType{Name: schema.MapTypeWithPrecision(schema.PGType(srcType), precision, scale)}
}

// --- SchemaReader / DataReader (2b wired, 2c stub) ---

type schemaReader struct{ src *Source }

type dataReader struct{ src *Source }
