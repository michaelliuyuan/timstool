package mongo

import (
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// TestMongoRegistered verifies "mongodb" registers with the Open factory and
// describes itself for the schema-driven Web form (M1 acceptance: zero
// frontend change, the form appears from RegisterMeta alone).
func TestMongoRegistered(t *testing.T) {
	src, err := source.Open("mongodb", source.SourceConfig{Host: "h", Port: 27017})
	if err != nil {
		t.Fatalf("Open(mongodb) err = %v", err)
	}
	if src.Name() != "mongodb" {
		t.Errorf("Name() = %q, want mongodb", src.Name())
	}

	meta, err := source.Describe("mongodb")
	if err != nil {
		t.Fatalf("Describe(mongodb) err = %v", err)
	}
	if !meta.Implemented {
		t.Error("mongodb Implemented = false, want true")
	}
	if meta.DefaultPort != 27017 {
		t.Errorf("DefaultPort = %d, want 27017", meta.DefaultPort)
	}
	// Compare-only capabilities: the migration booleans must stay off in M1.
	if !meta.Capabilities.Compare {
		t.Error("Capabilities.Compare = false, want true")
	}
	if meta.Capabilities.Schema || meta.Capabilities.Data || meta.Capabilities.CDC {
		t.Errorf("Capabilities = %+v, want Schema/Data/CDC all false (M1)", meta.Capabilities)
	}

	// Form fields: common group first (host/port/user/password), then the
	// source group (authDb/tls) — the exact set from the M1 task brief.
	wantKeys := []string{"host", "port", "user", "password", "authDb", "tls"}
	if len(meta.Fields) != len(wantKeys) {
		t.Fatalf("Fields length = %d, want %d (%v)", len(meta.Fields), len(wantKeys), meta.Fields)
	}
	for i, f := range meta.Fields {
		if f.Key != wantKeys[i] {
			t.Errorf("Fields[%d].Key = %q, want %q", i, f.Key, wantKeys[i])
		}
	}
	tls := meta.Fields[5]
	if tls.Type != "switch" {
		t.Errorf("tls.Type = %q, want switch", tls.Type)
	}
	if tls.Group != "source" || meta.Fields[4].Group != "source" {
		t.Error("authDb/tls must be in the source group")
	}
}

// TestMongoMigrationPlaceholders: the migration-oriented methods follow the
// ErrNotImplemented convention (M1: compare-only adapter).
func TestMongoMigrationPlaceholders(t *testing.T) {
	src, err := source.Open("mongodb", source.SourceConfig{Host: "h"})
	if err != nil {
		t.Fatalf("Open(mongodb) err = %v", err)
	}

	if _, err := src.SchemaReader().ReadSchema(nil, source.Filter{}); !errors.Is(err, source.ErrNotImplemented) {
		t.Errorf("ReadSchema() err = %v, want ErrNotImplemented", err)
	}
	if _, err := src.DataReader().ReadTable(nil, source.Table{}, source.ChunkSpec{}); !errors.Is(err, source.ErrNotImplemented) {
		t.Errorf("ReadTable() err = %v, want ErrNotImplemented", err)
	}
	if ic, err := src.IncrementalCapture(); ic != nil || !errors.Is(err, source.ErrNotImplemented) {
		t.Errorf("IncrementalCapture() = (%v, %v), want (nil, ErrNotImplemented)", ic, err)
	}
	// Lazy not-implemented placeholders: never nil, and calling them must not
	// panic (convention symmetry with the pg/mysql adapters).
	if tm := src.TypeMapper(); tm == nil {
		t.Error("TypeMapper() = nil, want lazy not-implemented implementation")
	} else if got := tm.MapType("int", 0, 0); got.Name != "UNSUPPORTED" {
		t.Errorf("MapType() = %+v, want UNSUPPORTED placeholder", got)
	}
	if d := src.Dialect(); d == nil {
		t.Error("Dialect() = nil, want lazy not-implemented implementation")
	} else if d.QuoteIdentifier("x") == "" || d.QuoteTable("", "t") == "" {
		t.Error("Dialect() quoting returned empty (must not panic)")
	}
}

// TestIsBrowsableCollection: the empty type (older drivers) must be treated as
// a collection so those collections are never silently dropped from listings.
func TestIsBrowsableCollection(t *testing.T) {
	for _, typ := range []string{"collection", ""} {
		if !isBrowsableCollection(typ) {
			t.Errorf("isBrowsableCollection(%q) = false, want true", typ)
		}
	}
	if isBrowsableCollection("view") {
		t.Error(`isBrowsableCollection("view") = true, want false`)
	}
}

// TestMongoInterfaces: the adapter satisfies the small connect/browse
// capabilities (source.Connector / source.MetaBrowser) for the compare track.
// Browse calls before Connect fail with a friendly error, not a panic.
func TestMongoInterfaces(t *testing.T) {
	src, err := source.Open("mongodb", source.SourceConfig{Host: "h"})
	if err != nil {
		t.Fatalf("Open(mongodb) err = %v", err)
	}
	if _, ok := any(src).(source.Connector); !ok {
		t.Error("mongodb Source does not satisfy source.Connector")
	}
	browser, ok := src.(source.MetaBrowser)
	if !ok {
		t.Fatal("mongodb Source does not satisfy source.MetaBrowser")
	}
	if _, err := browser.ListDatabases(nil); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Errorf("ListDatabases(before connect) err = %v, want not-connected error", err)
	}
	if _, err := browser.ListCollections(nil, "db"); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Errorf("ListCollections(before connect) err = %v, want not-connected error", err)
	}
	// Close on a never-connected source is nil-safe.
	if err := src.Close(); err != nil {
		t.Errorf("Close() on unconnected = %v, want nil", err)
	}
}

// TestMongoClientOptions covers the structured (clientOptions-based) client
// construction: host/port defaults, credential + authSource, TLS switch — the
// URI-string-escaping pitfalls this replaces (arbitration merge item 1).
func TestMongoClientOptions(t *testing.T) {
	// Defaults: empty host/port fall back to localhost:27017.
	opts := buildClientOptions(source.SourceConfig{})
	if len(opts.Hosts) != 1 || opts.Hosts[0] != "localhost:27017" {
		t.Errorf("default Hosts = %v, want [localhost:27017]", opts.Hosts)
	}

	// Explicit host/port.
	opts = buildClientOptions(source.SourceConfig{Host: "mdb1", Port: 28018})
	if len(opts.Hosts) != 1 || opts.Hosts[0] != "mdb1:28018" {
		t.Errorf("Hosts = %v, want [mdb1:28018]", opts.Hosts)
	}

	// Credentials without authDb: no AuthSource override (driver default).
	opts = buildClientOptions(source.SourceConfig{Host: "h", Port: 27017, User: "u", Password: "p@/+:&="})
	if opts.Auth == nil || opts.Auth.Username != "u" || opts.Auth.Password != "p@/+:&=" {
		t.Errorf("Auth = %+v, want username u + literal password (no escaping needed)", opts.Auth)
	}
	if opts.Auth.AuthSource != "" {
		t.Errorf("AuthSource = %q, want empty (driver default)", opts.Auth.AuthSource)
	}

	// Credentials + authDb.
	opts = buildClientOptions(source.SourceConfig{Host: "h", Port: 27017, User: "u", Password: "p",
		Options: map[string]string{"authDb": "myauth"}})
	if opts.Auth == nil || opts.Auth.AuthSource != "myauth" {
		t.Errorf("Auth = %+v, want AuthSource=myauth", opts.Auth)
	}

	// authDb without credentials is ignored (no auth block at all).
	opts = buildClientOptions(source.SourceConfig{Host: "h", Port: 27017,
		Options: map[string]string{"authDb": "admin"}})
	if opts.Auth != nil {
		t.Errorf("Auth = %+v, want nil without credentials", opts.Auth)
	}

	// TLS off by default, on via the switch (loose JSON values included).
	opts = buildClientOptions(source.SourceConfig{Host: "h", Port: 27017})
	if opts.TLSConfig != nil {
		t.Error("TLSConfig set without tls option, want nil")
	}
	for _, v := range []string{"true", "1", "on", "True"} {
		opts = buildClientOptions(source.SourceConfig{Host: "h", Port: 27017,
			Options: map[string]string{"tls": v}})
		if opts.TLSConfig == nil {
			t.Errorf("TLSConfig nil for tls=%q, want set", v)
		}
	}
	opts = buildClientOptions(source.SourceConfig{Host: "h", Port: 27017,
		Options: map[string]string{"tls": "false"}})
	if opts.TLSConfig != nil {
		t.Error("TLSConfig set for tls=false, want nil")
	}
}

// TestIndexSummaries covers the driver-spec → source.IndexInfo conversion:
// order-preserved keys, unique flag, TTL passthrough (M2 hard requirement:
// the engine skips TTL collections via ExpireAfterSeconds).
func TestIndexSummaries(t *testing.T) {
	ttl := int32(3600)
	unique := true
	compound := mustBSON(t, bson.D{{Key: "a", Value: 1}, {Key: "b", Value: -1}})
	single := mustBSON(t, bson.D{{Key: "createdAt", Value: 1}})
	textish := mustBSON(t, bson.D{{Key: "title", Value: "text"}})

	specs := []mongo.IndexSpecification{
		{Name: "compound_idx", KeysDocument: compound},
		{Name: "ttl_idx", KeysDocument: single, Unique: &unique, ExpireAfterSeconds: &ttl},
		{Name: "text_idx", KeysDocument: textish},
	}

	out := indexSummaries(specs)
	if len(out) != 3 {
		t.Fatalf("indexSummaries len = %d, want 3", len(out))
	}

	// Order-preserved compound keys.
	if k := out[0].Keys; len(k) != 2 || k[0].Field != "a" || k[1].Field != "b" {
		t.Errorf("compound keys = %+v, want [a b] in order", k)
	}
	if v, ok := out[0].Keys[0].Order.(int32); !ok || v != 1 {
		t.Errorf("keys[0].Order = %#v, want int32(1)", out[0].Keys[0].Order)
	}

	// Unique + TTL passthrough.
	if !out[1].Unique || out[1].ExpireAfterSeconds == nil || *out[1].ExpireAfterSeconds != 3600 {
		t.Errorf("ttl_idx = %+v, want Unique + ExpireAfterSeconds=3600", out[1])
	}
	if out[0].ExpireAfterSeconds != nil {
		t.Errorf("non-TTL index ExpireAfterSeconds = %v, want nil", out[0].ExpireAfterSeconds)
	}

	// Non-numeric direction (text) survives as-is.
	if v, ok := out[2].Keys[0].Order.(string); !ok || v != "text" {
		t.Errorf("text direction = %#v, want \"text\"", out[2].Keys[0].Order)
	}
}

func mustBSON(t *testing.T, doc bson.D) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(doc)
	if err != nil {
		t.Fatalf("bson.Marshal: %v", err)
	}
	return bson.Raw(b)
}
