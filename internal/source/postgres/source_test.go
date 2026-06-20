package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// TestPostgresRegistered: "postgres" is registered + the 2a skeleton exposes the
// real simple parts (Dialect/TypeMapper) and the 2b/2c/2d stubs.
func TestPostgresRegistered(t *testing.T) {
	src, err := source.Open("postgres", source.SourceConfig{Host: "h", Port: 5432})
	if err != nil {
		t.Fatalf("Open(postgres) err = %v", err)
	}
	if src.Name() != "postgres" {
		t.Errorf("Name() = %q, want postgres", src.Name())
	}
	if src.Dialect() == nil || src.TypeMapper() == nil {
		t.Error("Dialect()/TypeMapper() must not be nil")
	}
	// 2b: ReadSchema is wired (delegates to Collector → CIR); without Connect it
	// returns "not connected". DataReader is still a 2c stub.
	if _, err := src.SchemaReader().ReadSchema(context.Background(), source.Filter{}); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("SchemaReader.ReadSchema (no Connect) err = %v, want not-connected", err)
	}
	if _, err := src.DataReader().ReadTable(context.Background(), source.Table{}, source.ChunkSpec{}); err == nil || !strings.Contains(err.Error(), "2c") {
		t.Errorf("DataReader.ReadTable (2a stub) err = %v, want a 2c-not-wired error", err)
	}
	if _, err := src.IncrementalCapture(); err == nil {
		t.Error("IncrementalCapture (2a) must return ErrNotImplemented")
	}
}

func TestPgDialect(t *testing.T) {
	d := pgDialect{}
	if got := d.QuoteIdentifier("col"); got != `"col"` {
		t.Errorf("QuoteIdentifier = %q, want \"col\"", got)
	}
	if got := d.QuoteIdentifier("a\"b"); got != `"a""b"` {
		t.Errorf("QuoteIdentifier(escaped) = %q", got)
	}
	if got := d.QuoteTable("public", "t"); got != `"public"."t"` {
		t.Errorf("QuoteTable = %q", got)
	}
	if got := d.LimitOffsetSQL(10, 5); got != "LIMIT 10 OFFSET 5" {
		t.Errorf("LimitOffsetSQL = %q", got)
	}
}
