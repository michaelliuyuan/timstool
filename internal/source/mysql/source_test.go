package mysql

import (
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// TestMySQLRegistered verifies "mysql" is registered + skeleton exposes
// Dialect/TypeMapper/ConfigSchema correctly.
func TestMySQLRegistered(t *testing.T) {
	src, err := source.Open("mysql", source.SourceConfig{Host: "h", Port: 3306})
	if err != nil {
		t.Fatalf("Open(mysql) err = %v", err)
	}
	if src.Name() != "mysql" {
		t.Errorf("Name() = %q, want mysql", src.Name())
	}
	if src.Dialect() == nil || src.TypeMapper() == nil {
		t.Error("Dialect()/TypeMapper() must not be nil")
	}

	// ConfigSchema must return the connection form fields.
	schema := src.ConfigSchema()
	if len(schema) == 0 {
		t.Fatal("ConfigSchema() must return fields")
	}
	names := make(map[string]bool)
	for _, f := range schema {
		names[f.Name] = true
	}
	required := []string{"host", "port", "user", "password", "database"}
	for _, n := range required {
		if !names[n] {
			t.Errorf("ConfigSchema missing field %q", n)
		}
	}

	// IncrementalCapture: MySQL CDC not yet implemented.
	ic, err := src.IncrementalCapture()
	if ic != nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("IncrementalCapture() = (%v, %v), want ErrNotImplemented", ic, err)
	}
}

// TestMySQLTypeMapper covers the key MySQL→TiDB type mappings.
func TestMySQLTypeMapper(t *testing.T) {
	tests := []struct {
		srcType  string
		prec     int
		scale    int
		wantTiDB string
	}{
		// Integer
		{"int", 0, 0, "INT"},
		{"int unsigned", 0, 0, "INT UNSIGNED"},
		{"bigint", 0, 0, "BIGINT"},
		{"tinyint", 0, 0, "TINYINT"},
		{"tinyint unsigned", 0, 0, "TINYINT UNSIGNED"},
		{"smallint", 0, 0, "SMALLINT"},
		{"mediumint", 0, 0, "INT"},
		{"mediumint unsigned", 0, 0, "INT UNSIGNED"},

		// Decimal
		{"decimal(10,2)", 10, 2, "DECIMAL(10,2)"},

		// String
		{"varchar(255)", 0, 0, "VARCHAR(255)"},
		{"char(10)", 0, 0, "CHAR(10)"},
		{"text", 0, 0, "TEXT"},
		{"tinytext", 0, 0, "TEXT"},
		{"mediumtext", 0, 0, "TEXT"},
		{"longtext", 0, 0, "TEXT"},

		// ENUM/SET
		{"enum('a','b','c')", 0, 0, "VARCHAR(255)"},
		{"set('x','y')", 0, 0, "VARCHAR(255)"},

		// Date/time
		{"datetime", 0, 0, "DATETIME"},
		{"datetime(6)", 6, 0, "DATETIME(6)"},
		{"timestamp", 0, 0, "TIMESTAMP"},
		{"date", 0, 0, "DATE"},
		{"year", 0, 0, "SMALLINT"},

		// JSON
		{"json", 0, 0, "JSON"},

		// BIT
		{"bit", 0, 0, "BIT"},
		{"bit(8)", 8, 0, "BIT(8)"},

		// Boolean
		{"bool", 0, 0, "TINYINT(1)"},
		{"boolean", 0, 0, "TINYINT(1)"},
	}

	tm := mysqlTypeMapper{}
	for _, tc := range tests {
		t.Run(tc.srcType, func(t *testing.T) {
			got := tm.MapType(tc.srcType, tc.prec, tc.scale)
			if got.Name != tc.wantTiDB {
				t.Errorf("MapType(%q, %d, %d) = %q, want %q",
					tc.srcType, tc.prec, tc.scale, got.Name, tc.wantTiDB)
			}
		})
	}
}

// TestMySQLDialect covers identifier quoting and LIMIT/OFFSET syntax.
func TestMySQLDialect(t *testing.T) {
	d := mysqlDialect{}

	if got := d.QuoteIdentifier("col"); got != "`col`" {
		t.Errorf("QuoteIdentifier = %q, want `col`", got)
	}
	if got := d.QuoteIdentifier("a`b"); got != "`a``b`" {
		t.Errorf("QuoteIdentifier(escaped) = %q", got)
	}
	if got := d.QuoteTable("mydb", "t"); got != "`mydb`.`t`" {
		t.Errorf("QuoteTable = %q, want `mydb`.`t`", got)
	}

	// LIMIT/OFFSET
	if got := d.LimitOffsetSQL(10, 5); got != "LIMIT 10 OFFSET 5" {
		t.Errorf("LimitOffsetSQL(10,5) = %q", got)
	}
	if got := d.LimitOffsetSQL(0, 100); !strings.Contains(got, "OFFSET 100") {
		t.Errorf("LimitOffsetSQL(0,100) = %q, want OFFSET 100", got)
	}
}

// TestMySQLConfigSchema verifies the required fields for Web UI.
func TestMySQLConfigSchema(t *testing.T) {
	s := &Source{cfg: source.SourceConfig{}}
	schema := s.ConfigSchema()

	// Verify fields are in order
	expectedNames := []string{"host", "port", "user", "password", "database", "charset"}
	if len(schema) != len(expectedNames) {
		t.Fatalf("ConfigSchema length = %d, want %d", len(schema), len(expectedNames))
	}
	for i, f := range schema {
		if f.Name != expectedNames[i] {
			t.Errorf("ConfigSchema[%d].Name = %q, want %q", i, f.Name, expectedNames[i])
		}
	}
	// charset should be a select with options
	charsetField := schema[5]
	if charsetField.Type != "select" {
		t.Errorf("charset field Type = %q, want select", charsetField.Type)
	}
	if len(charsetField.Options) == 0 {
		t.Error("charset field must have options")
	}
}
