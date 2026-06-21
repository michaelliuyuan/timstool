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

	// Describe (schema-driven form) returns the mysql fields without opening a
	// connection (the single source of truth for the Web form).
	meta, err := source.Describe("mysql")
	if err != nil {
		t.Fatalf("Describe(mysql) err = %v", err)
	}
	if meta.DefaultPort != 3306 {
		t.Errorf("Describe(mysql).DefaultPort = %d, want 3306", meta.DefaultPort)
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

		// ENUM/SET: 1:1 fidelity, member values preserved (case-sensitive)
		{"enum('a','b','c')", 0, 0, "ENUM('a','b','c')"},
		{"set('x','y')", 0, 0, "SET('x','y')"},

		// Date/time
		{"datetime", 0, 0, "DATETIME"},
		{"datetime(6)", 6, 0, "DATETIME(6)"},
		{"timestamp", 0, 0, "TIMESTAMP"},
		{"date", 0, 0, "DATE"},
		{"year", 0, 0, "YEAR"},

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

// TestMySQLDescribe verifies the schema-driven form fields (single source of
// truth, doc §5): order, charset as a source-group select.
func TestMySQLDescribe(t *testing.T) {
	meta, err := source.Describe("mysql")
	if err != nil {
		t.Fatalf("Describe(mysql) err = %v", err)
	}
	expectedKeys := []string{"host", "port", "user", "password", "database", "charset"}
	if len(meta.Fields) != len(expectedKeys) {
		t.Fatalf("Fields length = %d, want %d", len(meta.Fields), len(expectedKeys))
	}
	for i, f := range meta.Fields {
		if f.Key != expectedKeys[i] {
			t.Errorf("Fields[%d].Key = %q, want %q", i, f.Key, expectedKeys[i])
		}
	}
	// charset is a select with options, in the source group (no tls/advanced this phase)
	charset := meta.Fields[5]
	if charset.Type != "select" || len(charset.Options) == 0 {
		t.Errorf("charset = %+v, want select with options", charset)
	}
	if charset.Group != "source" {
		t.Errorf("charset.Group = %q, want source", charset.Group)
	}
	if !meta.Capabilities.Data || meta.Capabilities.CDC {
		t.Errorf("mysql capabilities = %+v, want Data=true CDC=false", meta.Capabilities)
	}
}
