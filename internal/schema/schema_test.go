package schema

import (
	"strings"
	"testing"
)

// TestProgressRegistrationNames anchors the ①b fix: the schema progress
// registration set must align with the DATA migration set. With a whitelist
// (opts.Tables non-empty) only the intersection is registered — a
// whitelisted-out table must not produce a checkpoint entry, or data-phase
// tables_done would never reach tables_total. Empty whitelist ⇒ all
// collected tables (CollectTables already applied ExcludeTables).
func TestProgressRegistrationNames(t *testing.T) {
	tables := []TableInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	if got := progressRegistrationNames(tables, nil); len(got) != 3 {
		t.Errorf("empty include: got %v, want all 3", got)
	}
	got := progressRegistrationNames(tables, []string{"a", "c", "zz"})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("include {a,c,zz}: got %v, want [a c] (intersection only, zz dropped)", got)
	}
	if got := progressRegistrationNames(tables, []string{"zz"}); len(got) != 0 {
		t.Errorf("include {zz}: got %v, want empty", got)
	}
	// Exclude pin (DDL 范围口径, 23e9a24): CollectTables filters excluded
	// tables BEFORE registration (contains() at collector.go), so with
	// exclude={"b"} the collected+registered set is N-1 and "b" produces
	// no checkpoint entry — schema and data sets stay aligned end to end.
	collected := []TableInfo{{Name: "a"}, {Name: "c"}} // b excluded upstream
	if got := progressRegistrationNames(collected, nil); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("exclude scenario: got %v, want [a c]", got)
	}
	if contains([]string{"b"}, "b") != true || contains([]string{"b"}, "a") != false {
		t.Error("contains() exclude semantics broken")
	}
}

func TestMapType(t *testing.T) {
	tests := []struct {
		pgType   PGType
		expected string
		support  SupportLevel
	}{
		{PGInteger, "INT", Supported},
		{PGBigint, "BIGINT", Supported},
		{PGVarchar, "VARCHAR", Supported},
		{PGText, "TEXT", Supported},
		{PGBoolean, "TINYINT(1)", Convert},
		{PGBytea, "BLOB", Convert},
		{PGJSONB, "JSON", Convert},
		{PGUUID, "CHAR(36)", Convert},
		{PGTSVector, "", Unsupported},
		{PGPoint, "", Unsupported},
	}

	for _, tt := range tests {
		m, ok := MapType(tt.pgType)
		if !ok {
			t.Errorf("type %s not found in map", tt.pgType)
			continue
		}
		if m.MySQLType != tt.expected {
			t.Errorf("type %s: expected %s, got %s", tt.pgType, tt.expected, m.MySQLType)
		}
		if m.SupportLevel != tt.support {
			t.Errorf("type %s: expected support %s, got %s", tt.pgType, tt.support, m.SupportLevel)
		}
	}
}

func TestMapTypeWithPrecision(t *testing.T) {
	tests := []struct {
		pgType   PGType
		prec     int
		scale    int
		expected string
	}{
		{PGNumeric, 10, 2, "DECIMAL(10,2)"},
		{PGNumeric, 10, 0, "DECIMAL(10)"},
		{PGNumeric, 0, 0, "DECIMAL"},
		{PGVarchar, 255, 0, "VARCHAR(255)"},
		{PGChar, 10, 0, "CHAR(10)"},
		{PGInteger, 0, 0, "INT"},
	}

	for _, tt := range tests {
		result := MapTypeWithPrecision(tt.pgType, tt.prec, tt.scale)
		if result != tt.expected {
			t.Errorf("MapTypeWithPrecision(%s, %d, %d) = %s, want %s", tt.pgType, tt.prec, tt.scale, result, tt.expected)
		}
	}
}

func TestIsArray(t *testing.T) {
	if !IsArray("_int4") {
		t.Error("_int4 should be array")
	}
	if !IsArray("int[]") {
		t.Error("int[] should be array")
	}
	if IsArray("integer") {
		t.Error("integer should not be array")
	}
}

func TestBaseArrayType(t *testing.T) {
	if BaseArrayType("_int4") != "int4" {
		t.Error("expected int4")
	}
	if BaseArrayType("int[]") != "int" {
		t.Error("expected int")
	}
}

func TestBuildTableDDL(t *testing.T) {
	table := TableInfo{
		Schema: "public",
		Name:   "users",
		Columns: []Column{
			{ColumnName: "id", PGType: PGBigint, IsNullable: false, IsAutoIncr: true},
			{ColumnName: "name", PGType: PGVarchar, MaxLength: 255, IsNullable: false},
			{ColumnName: "email", PGType: PGVarchar, MaxLength: 255, IsNullable: true},
			{ColumnName: "active", PGType: PGBoolean, IsNullable: false, DefaultValue: "true"},
		},
	}

	builder := NewDDLBuilder()
	err := builder.BuildTableDDL(table)
	if err != nil {
		t.Fatal(err)
	}

	sql := builder.JoinSQL()
	if !strings.Contains(sql, "CREATE TABLE") {
		t.Error("should contain CREATE TABLE")
	}
	if !strings.Contains(sql, "`id`") {
		t.Error("should contain id column")
	}
	if !strings.Contains(sql, "BIGINT") {
		t.Error("should contain BIGINT type")
	}
	if !strings.Contains(sql, "AUTO_INCREMENT") {
		t.Error("should contain AUTO_INCREMENT")
	}
	if !strings.Contains(sql, "NOT NULL") {
		t.Error("should contain NOT NULL")
	}
}

func TestBuildIndexDDL(t *testing.T) {
	idx := Index{
		TableName: "users",
		IndexName: "idx_email",
		Columns:   []string{"email"},
		IsUnique:  true,
		IndexType: "btree",
	}

	builder := NewDDLBuilder()
	ddl := builder.BuildIndexDDL(idx)
	if !strings.Contains(ddl, "UNIQUE") {
		t.Error("should be unique index")
	}
	if !strings.Contains(ddl, "idx_email") {
		t.Error("should contain index name")
	}
}

func TestBuildUnsupportedIndexDDL(t *testing.T) {
	idx := Index{
		TableName: "docs",
		IndexName: "idx_content",
		Columns:   []string{"content"},
		IndexType: "gin",
	}

	builder := NewDDLBuilder()
	ddl := builder.BuildIndexDDL(idx)
	if !strings.Contains(ddl, "WARNING") {
		t.Error("should warn about unsupported index type")
	}
}

func TestBuildForeignKeyDDL(t *testing.T) {
	fk := ForeignKey{
		ConstraintName: "fk_orders_user",
		TableName:      "orders",
		Columns:        []string{"user_id"},
		RefTable:       "users",
		RefColumns:     []string{"id"},
		OnDelete:       "CASCADE",
		OnUpdate:       "NO ACTION",
	}

	builder := NewDDLBuilder()
	ddl := builder.BuildForeignKeyDDL(fk)
	if !strings.Contains(ddl, "FOREIGN KEY") {
		t.Error("should contain FOREIGN KEY")
	}
	if !strings.Contains(ddl, "CASCADE") {
		t.Error("should contain CASCADE")
	}
}

func TestBuildViewDDL(t *testing.T) {
	view := View{
		Schema:     "public",
		Name:       "active_users",
		Definition: "SELECT id, name FROM users WHERE active = true",
	}

	builder := NewDDLBuilder()
	ddl := builder.BuildViewDDL(view)
	if !strings.Contains(ddl, "CREATE OR REPLACE VIEW") {
		t.Error("should contain CREATE OR REPLACE VIEW")
	}
}

func TestBuildEnumDDL(t *testing.T) {
	enum := EnumType{
		Schema: "public",
		Name:   "status",
		Values: []string{"active", "inactive", "pending"},
	}

	builder := NewDDLBuilder()
	ddl := builder.BuildEnumDDL(enum)
	if !strings.Contains(ddl, "active") {
		t.Error("should contain enum values")
	}
}

func TestConvertDefaultValue(t *testing.T) {
	tests := []struct {
		input    string
		pgType   PGType
		expected string
	}{
		{"true", PGBoolean, "1"},
		{"false", PGBoolean, "0"},
		{"'hello'", PGText, "'hello'"},
	}

	for _, tt := range tests {
		result := convertDefaultValue(tt.input, tt.pgType)
		if result != tt.expected {
			t.Errorf("convertDefaultValue(%s, %s) = %s, want %s", tt.input, tt.pgType, result, tt.expected)
		}
	}
}

// TestBuildColumnDDLStripsTextFamilyDefault anchors BUG-0930 commit 1: a
// literal DEFAULT on a column mapped to the TEXT/BLOB/JSON family must be
// STRIPPED (recorded for warning/report) instead of emitted — TiDB rejects
// it with 1101 and the whole schema phase fails. varchar(n) defaults stay.
func TestBuildColumnDDLStripsTextFamilyDefault(t *testing.T) {
	buildCol := func(pgType PGType, maxLength int, nullable bool) (string, []StrippedDefault) {
		b := NewDDLBuilder()
		col := Column{
			TableName: "t1", ColumnName: "status", PGType: pgType,
			MaxLength: maxLength, IsNullable: nullable, DefaultValue: "'applied'",
		}
		ddl, err := b.buildColumnDDL(col)
		if err != nil {
			t.Fatalf("buildColumnDDL: %v", err)
		}
		return ddl, b.strippedDefaults
	}

	// text + default → stripped, NOT NULL flag recorded.
	ddl, sd := buildCol(PGText, 0, false)
	if strings.Contains(strings.ToUpper(ddl), "DEFAULT") {
		t.Errorf("text+default must strip DEFAULT, got: %s", ddl)
	}
	if len(sd) != 1 || sd[0].Table != "t1" || sd[0].Column != "status" || sd[0].Default != "'applied'" || !sd[0].NotNull {
		t.Errorf("stripped record = %+v, want one NOT NULL entry for t1.status", sd)
	}

	// varchar(n) + default → KEPT (legal on VARCHAR).
	ddl, sd = buildCol(PGVarchar, 64, true)
	if !strings.Contains(ddl, "DEFAULT 'applied'") {
		t.Errorf("varchar(n)+default must keep DEFAULT, got: %s", ddl)
	}
	if len(sd) != 0 {
		t.Errorf("varchar must not record a strip, got %+v", sd)
	}

	// bytea → BLOB, json → JSON, xml → LONGTEXT: same family strip.
	for _, pt := range []PGType{PGBytea, PGJSON, PGXML} {
		ddl, sd = buildCol(pt, 0, true)
		if strings.Contains(strings.ToUpper(ddl), "DEFAULT") {
			t.Errorf("%s+default must strip DEFAULT, got: %s", pt, ddl)
		}
		if len(sd) != 1 {
			t.Errorf("%s must record one strip, got %+v", pt, sd)
		}
	}

	// unknown PG type falls back to TEXT → same strip.
	ddl, sd = buildCol(PGType("weird_type"), 0, true)
	if !strings.Contains(strings.ToUpper(ddl), "TEXT") || strings.Contains(strings.ToUpper(ddl), "DEFAULT") {
		t.Errorf("unknown-type fallback TEXT must strip DEFAULT, got: %s", ddl)
	}
	if len(sd) != 1 {
		t.Errorf("unknown-type must record one strip, got %+v", sd)
	}

	// adversarial angle 2: PG text DEFAULT CURRENT_TIMESTAMP maps to a TEXT
	// column — CURRENT_TIMESTAMP is illegal there too (1101), must strip.
	b := NewDDLBuilder()
	col := Column{
		TableName: "t1", ColumnName: "ts_col", PGType: PGText,
		IsNullable: true, DefaultValue: "CURRENT_TIMESTAMP",
	}
	ddl, err := b.buildColumnDDL(col)
	if err != nil {
		t.Fatalf("buildColumnDDL: %v", err)
	}
	if strings.Contains(strings.ToUpper(ddl), "DEFAULT") {
		t.Errorf("text+CURRENT_TIMESTAMP must strip DEFAULT, got: %s", ddl)
	}
	if len(b.strippedDefaults) != 1 || b.strippedDefaults[0].Default != "CURRENT_TIMESTAMP" {
		t.Errorf("text+CURRENT_TIMESTAMP must record strip, got %+v", b.strippedDefaults)
	}
}

func TestQuoteIdentifier(t *testing.T) {
	if QuoteIdentifier("table") != "`table`" {
		t.Error("should backtick-quote identifier")
	}
	if QuoteIdentifier("ta`ble") != "`ta``ble`" {
		t.Error("should escape backticks")
	}
}

func TestTableInfoPrimaryKey(t *testing.T) {
	table := TableInfo{
		Indexes: []Index{
			{IndexName: "idx_name", Columns: []string{"name"}, IsPrimary: false},
			{IndexName: "pk_id", Columns: []string{"id"}, IsPrimary: true},
		},
	}
	pk := table.PrimaryKey()
	if pk == nil || pk.IndexName != "pk_id" {
		t.Error("should find primary key")
	}

	table2 := TableInfo{Indexes: []Index{}}
	if table2.PrimaryKey() != nil {
		t.Error("should return nil when no PK")
	}
}

func TestEscapeSQLString(t *testing.T) {
	if escapeSQLString("it's") != "it''s" {
		t.Error("should escape single quotes")
	}
}
