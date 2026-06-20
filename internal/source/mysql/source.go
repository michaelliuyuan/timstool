// Package mysql is the MySQL source adapter for timstool. It implements
// source.Source by reading MySQL's information_schema for schema discovery
// and SELECT * with LIMIT/OFFSET pagination for data export. CDC is not yet
// implemented (returns ErrNotImplemented — binlog-based CDC is a future
// follow-up).
//
// This is Phase 2 (#t65) of the multi-source refactor.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/source"

	// MySQL driver for *sql.DB.
	_ "github.com/go-sql-driver/mysql"
)

func init() {
	source.Register("mysql", func(cfg source.SourceConfig) (source.Source, error) {
		return &Source{cfg: cfg}, nil
	})
}

// Source is the MySQL source adapter.
type Source struct {
	cfg source.SourceConfig
	db  *sql.DB
}

func (s *Source) Name() string { return "mysql" }

func (s *Source) Connect(ctx context.Context) error {
	db, err := sql.Open("mysql", dsn(s.cfg))
	if err != nil {
		return fmt.Errorf("mysql source: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return fmt.Errorf("mysql source: ping: %w", err)
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

// DB exposes the underlying connection for the readers.
func (s *Source) DB() *sql.DB { return s.db }

// Config exposes the source config for the readers.
func (s *Source) Config() source.SourceConfig { return s.cfg }

func (s *Source) Dialect() source.Dialect { return mysqlDialect{} }

func (s *Source) TypeMapper() source.TypeMapper { return mysqlTypeMapper{} }

// ConfigSchema declares the MySQL connection-form fields for the Web UI (#t65).
func (s *Source) ConfigSchema() []source.ConfigField {
	return []source.ConfigField{
		{Name: "host", Label: "Host", Type: "text", Required: true, Default: "localhost", Placeholder: "localhost", Group: "connection"},
		{Name: "port", Label: "Port", Type: "number", Required: true, Default: "3306", Group: "connection"},
		{Name: "user", Label: "User", Type: "text", Required: true, Default: "root", Group: "connection"},
		{Name: "password", Label: "Password", Type: "password", Group: "connection"},
		{Name: "database", Label: "Database", Type: "text", Required: true, Group: "connection"},
		{Name: "charset", Label: "Charset", Type: "select", Default: "utf8mb4", Options: []string{"utf8mb4", "utf8", "latin1", "gbk"}, Group: "advanced"},
	}
}

// SchemaReader reads MySQL information_schema → CIR (3b).
func (s *Source) SchemaReader() source.SchemaReader { return &schemaReader{src: s} }

// DataReader reads MySQL tables as CIR rows (3c).
func (s *Source) DataReader() source.DataReader { return &dataReader{src: s} }

// IncrementalCapture: MySQL CDC (binlog-based) is not yet implemented.
func (s *Source) IncrementalCapture() (source.IncrementalCapture, error) {
	return nil, source.ErrNotImplemented
}

// dsn builds a MySQL DSN from the source config.
func dsn(c source.SourceConfig) string {
	charset := c.Options["charset"]
	if charset == "" {
		charset = "utf8mb4"
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=true&loc=Local",
		url.QueryEscape(c.User), url.QueryEscape(c.Password),
		c.Host, c.Port, c.Database, charset)
	return dsn
}

// --- Dialect ---

type mysqlDialect struct{}

func (mysqlDialect) QuoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func (d mysqlDialect) QuoteTable(schemaName, table string) string {
	if schemaName == "" {
		return d.QuoteIdentifier(table)
	}
	return d.QuoteIdentifier(schemaName) + "." + d.QuoteIdentifier(table)
}

func (mysqlDialect) LimitOffsetSQL(limit, offset int64) string {
	if limit <= 0 {
		// MySQL requires LIMIT when OFFSET is used; use a large value.
		return fmt.Sprintf("LIMIT %d OFFSET %d", int64(1<<62-1), offset)
	}
	if offset <= 0 {
		return fmt.Sprintf("LIMIT %d", limit)
	}
	return fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
}

// --- TypeMapper (3d: stub passthrough for now; full mapping in step 3d) ---

type mysqlTypeMapper struct{}

func (mysqlTypeMapper) MapType(srcType string, precision, scale int) source.TiDBType {
	// Full type mapping is implemented in step 3d.
	return source.TiDBType{Name: mapMySQLType(srcType, precision, scale)}
}

// mapMySQLType maps a MySQL column type to the TiDB-compatible type.
// MySQL → TiDB is largely passthrough (same protocol family), with a few
// conversions: YEAR→SMALLINT, MEDIUMINT→INT, ENUM/SET→VARCHAR, etc.
func mapMySQLType(srcType string, precision, scale int) string {
	t := strings.ToUpper(strings.TrimSpace(srcType))

	// Strip length/parentheses for matching
	base := t
	if idx := strings.IndexByte(t, '('); idx >= 0 {
		base = t[:idx]
	}
	// Strip UNSIGNED suffix for matching
	unsigned := strings.Contains(t, "UNSIGNED")
	base = strings.TrimSuffix(strings.TrimSpace(base), " UNSIGNED")
	base = strings.TrimSuffix(strings.TrimSpace(base), " SIGNED")

	switch base {
	// Integer types: MySQL → TiDB passthrough (same)
	case "TINYINT":
		if unsigned {
			return "TINYINT UNSIGNED"
		}
		return "TINYINT"
	case "SMALLINT":
		if unsigned {
			return "SMALLINT UNSIGNED"
		}
		return "SMALLINT"
	case "MEDIUMINT":
		if unsigned {
			return "INT UNSIGNED" // TiDB has no MEDIUMINT
		}
		return "INT"
	case "INT", "INTEGER":
		if unsigned {
			return "INT UNSIGNED"
		}
		return "INT"
	case "BIGINT":
		if unsigned {
			return "BIGINT UNSIGNED"
		}
		return "BIGINT"

	// Floating point
	case "FLOAT":
		return "FLOAT"
	case "DOUBLE", "REAL":
		return "DOUBLE"
	case "DECIMAL", "NUMERIC", "DEC", "FIXED":
		if precision > 0 && scale > 0 {
			return fmt.Sprintf("DECIMAL(%d,%d)", precision, scale)
		}
		if precision > 0 {
			return fmt.Sprintf("DECIMAL(%d)", precision)
		}
		return "DECIMAL"

	// String types
	case "CHAR":
		return t // e.g. CHAR(10)
	case "VARCHAR":
		return t // e.g. VARCHAR(255)
	case "TINYTEXT":
		return "TEXT"
	case "TEXT":
		return "TEXT"
	case "MEDIUMTEXT":
		return "TEXT"
	case "LONGTEXT":
		return "TEXT"
	case "TINYBLOB":
		return "BLOB"
	case "BLOB":
		return "BLOB"
	case "MEDIUMBLOB":
		return "BLOB"
	case "LONGBLOB":
		return "BLOB"
	case "BINARY":
		return t
	case "VARBINARY":
		return t

	// ENUM/SET: TiDB supports ENUM/SET but for migration purposes map to VARCHAR
	case "ENUM":
		return "VARCHAR(255)"
	case "SET":
		return "VARCHAR(255)"

	// Date/time types
	case "DATE":
		return "DATE"
	case "TIME":
		return "TIME"
	case "DATETIME":
		if precision > 0 {
			return fmt.Sprintf("DATETIME(%d)", precision)
		}
		return "DATETIME"
	case "TIMESTAMP":
		if precision > 0 {
			return fmt.Sprintf("TIMESTAMP(%d)", precision)
		}
		return "TIMESTAMP"
	case "YEAR":
		return "SMALLINT" // TiDB has no YEAR type

	// JSON
	case "JSON":
		return "JSON"

	// BIT
	case "BIT":
		if precision > 0 {
			return fmt.Sprintf("BIT(%d)", precision)
		}
		return "BIT"

	// Geometry
	case "GEOMETRY", "POINT", "LINESTRING", "POLYGON", "MULTIPOINT",
		"MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION":
		return t

	// Boolean
	case "BOOL", "BOOLEAN":
		return "TINYINT(1)"

	default:
		// Passthrough: keep original type string (handles custom types, etc.)
		return srcType
	}
}
