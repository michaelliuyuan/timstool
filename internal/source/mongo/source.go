// Package mongo is the MongoDB source adapter for timstool (M1 of the
// mongo-compare track). Unlike the pg/mysql adapters it does NOT serve the
// migration pipeline: its Capabilities are Compare-only, and every
// migration-oriented method (SchemaReader/DataReader/TypeMapper/Dialect/
// IncrementalCapture) is an ErrNotImplemented placeholder following the
// stub-source convention (source.go ErrNotImplemented). What it does provide:
//
//   - Connect: version-adaptive dial (M5 internal/mongoconn — v2 native for
//     wire 9+/4.4+, v1 bridge for 3.x) + Ping + ListDatabases (verifies read
//     access), suitable for the Web connection test (it also implements
//     Version).
//   - ListDatabases/ListCollections: the source.MetaBrowser capability — the
//     strongly-typed metadata tree (counts, index summaries incl. TTL) used
//     by the compare module (M2 skips TTL collections via ExpireAfterSeconds)
//     and the /sources/meta endpoint.
//
// A future Mongo→TiDB migration track would fill in the migration methods.
package mongo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
	"github.com/michaelliuyuan/timstool/internal/source"
)

func init() {
	source.Register("mongodb", func(cfg source.SourceConfig) (source.Source, error) {
		return &Source{cfg: cfg}, nil
	})
	source.RegisterMeta("mongodb", mongoMeta)
}

// mongoMeta is the single source of truth for the MongoDB connection form.
// user/password are optional (MongoDB may run without auth); authDb and tls
// live in the source group (doc multi-source-web-form-design §5).
var mongoMeta = source.SourceMeta{
	Name:         "mongodb",
	DisplayName:  "MongoDB",
	Implemented:  true,
	DefaultPort:  27017,
	Capabilities: source.Capabilities{Compare: true}, // Schema/Data/CDC stay false (M1)
	Fields: []source.FieldSpec{
		{Key: "host", Label: "主机地址", Type: "text", Required: true, Default: "localhost", Placeholder: "localhost", Group: "common"},
		{Key: "port", Label: "端口", Type: "number", Required: true, Default: 27017, Group: "common"},
		{Key: "user", Label: "用户名", Type: "text", Placeholder: "留空表示无认证", Group: "common"},
		{Key: "password", Label: "密码", Type: "password", Group: "common"},
		{Key: "authDb", Label: "认证库 (authSource)", Type: "text", Placeholder: "admin", Help: "用于身份认证的库，留空时驱动按默认规则（admin/连接库）选择", Group: "source"},
		{Key: "tls", Label: "TLS/SSL", Type: "switch", Default: false, Help: "启用 TLS 连接", Group: "source"},
	},
}

// Source is the MongoDB source adapter (Connect/Close/Version/ListDatabases/
// ListCollections are real; the migration-oriented methods are
// ErrNotImplemented placeholders).
type Source struct {
	cfg    source.SourceConfig
	client mongoconn.Client
}

func (s *Source) Name() string { return "mongodb" }

// dialConfig maps the source config onto the version-adaptive dial (M5):
// structured fields, TLS read through the loose switch-form convention.
func dialConfig(c source.SourceConfig) mongoconn.DialConfig {
	cfg := mongoconn.DialConfig{
		Host:     c.Host,
		Port:     c.Port,
		User:     c.User,
		Password: c.Password,
		AuthDB:   strings.TrimSpace(c.Options["authDb"]),
	}
	if v, ok := c.Options["tls"]; ok && isTruthy(v) {
		cfg.TLS = true
	}
	return cfg
}

// isTruthy reads a switch-style field value ("true"/"1"/"on" from any of the
// loose JSON types the Web form may submit).
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}

// Connect dials the deployment through the version-adaptive layer (v2 for
// wire 9+, v1 bridge for old servers — per-connection probe, M5), verifies it
// with Ping, and confirms read access with ListDatabases (the
// connection-test contract, M1 acceptance).
func (s *Source) Connect(ctx context.Context) error {
	if s.client != nil {
		return nil // already connected (idempotent, mirrors pg/mysql behavior)
	}
	client, err := mongoconn.Dial(ctx, dialConfig(s.cfg))
	if err != nil {
		return fmt.Errorf("mongodb source: %w", err)
	}
	// Verify the credentials can actually read (catches bad authSource etc.).
	if _, err := client.ListDatabases(ctx); err != nil {
		_ = client.Disconnect(context.Background())
		return fmt.Errorf("mongodb source: listDatabases: %w", err)
	}
	s.client = client
	return nil
}

// Close disconnects the client (nil-safe, mirrors pg/mysql).
func (s *Source) Close() error {
	if s.client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.client.Disconnect(ctx)
	s.client = nil
	return err
}

// Conn exposes the version-adaptive connection (for the M2+ compare engine).
// Requires Connect first; nil before that.
func (s *Source) Conn() mongoconn.Client { return s.client }

// Config exposes the source config (for readers/the compare engine).
func (s *Source) Config() source.SourceConfig { return s.cfg }

// Version returns the server version (buildInfo) for the connection-test UI
// (the webapi `versioner` capability). Requires Connect first.
func (s *Source) Version(ctx context.Context) (string, error) {
	if s.client == nil {
		return "", fmt.Errorf("mongodb source: not connected")
	}
	return s.client.Version(ctx)
}

// --- Migration-oriented placeholders (ErrNotImplemented convention) ---

// SchemaReader: migration schemas are not implemented for MongoDB (M1). The
// stub returns ErrNotImplemented instead of panicking so generic Web paths
// (e.g. /sources/tables) fail with a friendly error.
func (s *Source) SchemaReader() source.SchemaReader { return schemaReader{src: s} }

// DataReader: not implemented for MongoDB (M1).
func (s *Source) DataReader() source.DataReader { return dataReader{src: s} }

// IncrementalCapture: MongoDB CDC (change streams) is out of scope (M1).
func (s *Source) IncrementalCapture() (source.IncrementalCapture, error) {
	return nil, source.ErrNotImplemented
}

// Dialect: MongoDB has no SQL dialect. Returns a lazy not-implemented
// placeholder (never nil, the pg/mysql convention) so generic paths that
// skip the Capabilities gate degrade gracefully instead of nil-dereferencing.
func (s *Source) Dialect() source.Dialect { return notImplementedDialect{} }

// TypeMapper: no Mongo→TiDB type mapping exists yet (future migration track).
// Lazy placeholder, never nil — same rationale as Dialect.
func (s *Source) TypeMapper() source.TypeMapper { return notImplementedTypeMapper{} }

type schemaReader struct{ src *Source }

func (schemaReader) ReadSchema(ctx context.Context, opts source.Filter) (*source.Schema, error) {
	return nil, source.ErrNotImplemented
}

type dataReader struct{ src *Source }

func (dataReader) ReadTable(ctx context.Context, t source.Table, chunk source.ChunkSpec) (source.RowIterator, error) {
	return nil, source.ErrNotImplemented
}

// notImplementedTypeMapper / notImplementedDialect: lazy placeholders that
// satisfy the migration interfaces without panicking — MapType reports
// UNSUPPORTED, the dialect quotes pass through inertly. Consumers gating on
// Capabilities never reach them; unguarded generic paths stay panic-free.
type notImplementedTypeMapper struct{}

func (notImplementedTypeMapper) MapType(srcType string, precision, scale int) source.TiDBType {
	return source.TiDBType{Name: "UNSUPPORTED"}
}

type notImplementedDialect struct{}

func (notImplementedDialect) QuoteIdentifier(name string) string { return name }

func (notImplementedDialect) QuoteTable(schema, table string) string {
	if schema == "" {
		return table
	}
	return schema + "." + table
}

func (notImplementedDialect) LimitOffsetSQL(limit, offset int64) string {
	return fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
}
