package mongo

import (
	"errors"
	"strings"
	"testing"

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
		t.Fatalf("Fields length = %d, want %v (%v)", len(meta.Fields), wantKeys, meta.Fields)
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

// TestDialConfigMapping covers the source-config → DialConfig mapping (the
// structured-credential convention this layer enforces before the
// version-adaptive dial): literal credential passthrough (no URI string
// escaping to get wrong), authDb trim, TLS loose-value reading.
func TestDialConfigMapping(t *testing.T) {
	cfg := dialConfig(source.SourceConfig{Host: "mdb1", Port: 28018, User: "u", Password: "p@/+:&="})
	if cfg.Host != "mdb1" || cfg.Port != 28018 || cfg.User != "u" || cfg.Password != "p@/+:&=" {
		t.Errorf("cfg = %+v, want literal field passthrough", cfg)
	}
	if cfg.TLS {
		t.Error("TLS set without tls option, want false")
	}

	cfg = dialConfig(source.SourceConfig{Host: "h", User: "u", Password: "p",
		Options: map[string]string{"authDb": "  myauth  "}})
	if cfg.AuthDB != "myauth" {
		t.Errorf("AuthDB = %q, want trimmed myauth", cfg.AuthDB)
	}

	for _, v := range []string{"true", "1", "on", "True"} {
		cfg = dialConfig(source.SourceConfig{Host: "h", Options: map[string]string{"tls": v}})
		if !cfg.TLS {
			t.Errorf("TLS false for tls=%q, want true", v)
		}
	}
	cfg = dialConfig(source.SourceConfig{Host: "h", Options: map[string]string{"tls": "false"}})
	if cfg.TLS {
		t.Error("TLS true for tls=false, want false")
	}
}
