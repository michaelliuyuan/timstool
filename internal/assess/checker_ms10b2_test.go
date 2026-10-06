package assess

import (
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source/mysql"
)

// MS-10b2 anchors: the source-aware checker calibration. The nine
// grading contracts below are the delivery standard (seq840 item 2 /
// seq843 补钉②): every one of the seven probe types must have at least
// one explicit level assertion, MySQL wording must name PostgreSQL where
// the PG path keeps the legacy text, and the PG assessor must stay
// byte-identical (parity anchor).

// mysqlColumnsFor builds a ScanResult with one column per probe type.
func mysqlColumnsFor(types ...string) *ScanResult {
	res := &ScanResult{}
	for _, dt := range types {
		res.Columns = append(res.Columns, ColumnInfo{
			TableSchema: "db1", TableName: "t1", ColumnName: "c_" + dt,
			DataType: dt, NumericPrec: 10,
		})
	}
	return res
}

// findingLevels maps column name suffix -> level for one data_type scan.
func dataTypeLevels(t *testing.T, a *Assessor, res *ScanResult) map[string]string {
	t.Helper()
	dims := a.Assess(res)
	var dt DimensionResult
	for _, d := range dims {
		if d.Dimension == DimDataType {
			dt = d
		}
	}
	out := map[string]string{}
	for _, f := range dt.Findings {
		out[f.ObjectName] = f.Level
	}
	return out
}

// TestMySQLDataTypesCalibration pins the item-2 grading contract: identity
// types (datetime/float/enum/set/time/tinyint…) = compatible; documented
// conversions (year/mediumint/text-family) = convertible; geometry and
// unknown types = manual_needed (never the PG switch's blanket default).
func TestMySQLDataTypesCalibration(t *testing.T) {
	a := NewAssessorFor("mysql")
	levels := dataTypeLevels(t, a, mysqlColumnsFor(
		"datetime", "float", "enum", "set", "time", "year",
		"tinyint", "mediumint", "mediumtext", "tinyblob",
		"geometry", "widget_type",
	))

	want := map[string]string{
		"db1.t1.c_datetime":    LevelCompatible,  // identity 1:1
		"db1.t1.c_float":       LevelCompatible,  // identity 1:1
		"db1.t1.c_enum":        LevelCompatible,  // TiDB native ENUM
		"db1.t1.c_set":         LevelCompatible,  // TiDB native SET
		"db1.t1.c_time":        LevelCompatible,  // identity 1:1
		"db1.t1.c_tinyint":     LevelCompatible,  // identity 1:1 (tinyint(1) bool semantics included)
		"db1.t1.c_year":        LevelConvertible, // seq840 contract: conversion noted
		"db1.t1.c_mediumint":   LevelConvertible, // TiDB has no MEDIUMINT -> INT
		"db1.t1.c_mediumtext":  LevelConvertible, // text family normalized to TEXT
		"db1.t1.c_tinyblob":    LevelConvertible, // blob family normalized to BLOB
		"db1.t1.c_geometry":    LevelManualNeeded,
		"db1.t1.c_widget_type": LevelManualNeeded, // unknown never silently compatible
	}
	for name, level := range want {
		if levels[name] != level {
			t.Errorf("%s level = %q, want %q", name, levels[name], level)
		}
	}
}

// TestMySQLDataTypesUnsignedArgs pins that UNSIGNED suffixes and argument
// clauses grade on the base type (decimal(10,2) unsigned = compatible via
// the shared mapper truth, not the PG switch default).
func TestMySQLDataTypesUnsignedArgs(t *testing.T) {
	a := NewAssessorFor("mysql")
	levels := dataTypeLevels(t, a, mysqlColumnsFor("decimal(10,2) unsigned", "varchar(255)"))
	if levels["db1.t1.c_decimal(10,2) unsigned"] != LevelCompatible {
		t.Errorf("decimal unsigned = %q, want compatible", levels["db1.t1.c_decimal(10,2) unsigned"])
	}
	if levels["db1.t1.c_varchar(255)"] != LevelCompatible {
		t.Errorf("varchar(255) = %q, want compatible", levels["db1.t1.c_varchar(255)"])
	}
}

// TestMySQLViewDialectSubset pins item 1: MySQL-native constructs
// (NOW()/ROW_NUMBER()/CAST() are MySQL 8 syntax too) must NOT be flagged
// on the MySQL path, while the true PG-only subset (ILIKE, ::, …) still
// is — with wording that names PostgreSQL.
func TestMySQLViewDialectSubset(t *testing.T) {
	base := func(def string) *ScanResult {
		return &ScanResult{Views: []ViewInfo{{Schema: "db1", Name: "v1", Definition: def}}}
	}

	// MySQL path: NOW()/ROW_NUMBER()/CAST( are native — compatible.
	dims := NewAssessorFor("mysql").Assess(base("SELECT NOW(), ROW_NUMBER() OVER (), CAST(x AS CHAR) FROM t"))
	for _, d := range dims {
		if d.Dimension != DimView {
			continue
		}
		if d.Total != 1 || d.Findings[0].Level != LevelCompatible {
			t.Errorf("mysql native view findings = %+v, want single compatible", d.Findings)
		}
	}

	// MySQL path: ILIKE + :: are PG-only — convertible, wording names
	// PostgreSQL (not the legacy "PG 特有语法" text).
	dims = NewAssessorFor("mysql").Assess(base("SELECT a FROM t WHERE a ILIKE 'x' AND b::text = 'y'"))
	for _, d := range dims {
		if d.Dimension != DimView {
			continue
		}
		if d.Total != 1 || d.Findings[0].Level != LevelConvertible {
			t.Errorf("mysql pg-only view findings = %+v, want single convertible", d.Findings)
		}
		if !strings.Contains(d.Findings[0].PGDetail, "PostgreSQL 方言") {
			t.Errorf("mysql wording = %q, want PostgreSQL dialect text", d.Findings[0].PGDetail)
		}
	}

	// PG path: the same native-shaped view still counts NOW()/ROW_NUMBER()
	// (22-word list unchanged — PG parity).
	dims = NewAssessor().Assess(base("SELECT NOW(), ROW_NUMBER() OVER (), CAST(x AS CHAR) FROM t"))
	for _, d := range dims {
		if d.Dimension != DimView {
			continue
		}
		if d.Total != 1 || d.Findings[0].Level != LevelManualNeeded {
			t.Errorf("pg view findings = %+v, want manual_needed (3 hits, >2)", d.Findings)
		}
	}
}

// TestMySQLAutoIncrementAnnotation pins item 7: AUTO_INCREMENT columns
// (EXTRA probe) get an explicit compatible structure finding on the MySQL
// path and none on the PG path (PG never sets IsAutoIncr).
func TestMySQLAutoIncrementAnnotation(t *testing.T) {
	res := &ScanResult{
		Columns: []ColumnInfo{{
			TableSchema: "db1", TableName: "t1", ColumnName: "id",
			DataType: "bigint", IsPrimary: true, IsAutoIncr: true,
		}},
		Tables: []TableInfo{{Schema: "db1", Name: "t1"}},
	}
	dims := NewAssessorFor("mysql").Assess(res)
	found := false
	for _, d := range dims {
		if d.Dimension != DimStructure {
			continue
		}
		for _, f := range d.Findings {
			if f.ObjectType == "column_auto_increment" {
				found = true
				if f.Level != LevelCompatible {
					t.Errorf("auto_increment level = %q, want compatible", f.Level)
				}
			}
		}
	}
	if !found {
		t.Error("mysql structure scan must annotate AUTO_INCREMENT columns")
	}

	// PG parity: the same scan via the legacy assessor has no such finding.
	for _, d := range NewAssessor().Assess(res) {
		if d.Dimension != DimStructure {
			continue
		}
		for _, f := range d.Findings {
			if f.ObjectType == "column_auto_increment" {
				t.Error("PG assessor must never emit column_auto_increment findings")
			}
		}
	}
}

// TestAssessorPGParity pins the zero-regression contract: for a
// PG-shaped scan, NewAssessor() and the pre-batch calibration produce
// identical dimension results (the srcKind seam is inert on "" / "pgx").
func TestAssessorPGParity(t *testing.T) {
	res := &ScanResult{
		Tables: []TableInfo{{Schema: "public", Name: "t1"}},
		Columns: []ColumnInfo{
			{TableSchema: "public", TableName: "t1", ColumnName: "id", DataType: "integer", IsPrimary: true},
			{TableSchema: "public", TableName: "t1", ColumnName: "val", DataType: "jsonb"},
		},
		Views: []ViewInfo{{Schema: "public", Name: "v1", Definition: "SELECT now() FROM t1"}},
	}
	legacy := NewAssessor().Assess(res)
	routed := NewAssessorFor("pgx").Assess(res)
	if len(legacy) != len(routed) {
		t.Fatalf("dimension count %d vs %d", len(legacy), len(routed))
	}
	for i := range legacy {
		if legacy[i].Dimension != routed[i].Dimension ||
			legacy[i].Total != routed[i].Total ||
			legacy[i].Score != routed[i].Score {
			t.Errorf("dim %d mismatch: %+v vs %+v", i, legacy[i], routed[i])
		}
		for j := range legacy[i].Findings {
			if legacy[i].Findings[j] != routed[i].Findings[j] {
				t.Errorf("dim %d finding %d mismatch", i, j)
			}
		}
	}
	// Spot pins on the legacy grading itself (PG switch untouched).
	if legacy[0].Findings[0].Level != LevelCompatible || legacy[0].Findings[1].Level != LevelConvertible {
		t.Errorf("PG data_type grading changed: %+v", legacy[0].Findings)
	}
}

// TestMySQLSystemSchemaGuard pins item 8's assess-side guard surface: the
// guard set is the shared exported source/mysql.SystemDatabases (single
// truth consumed by both ddlexport and the webapi assess handler).
func TestMySQLSystemSchemaGuard(t *testing.T) {
	for _, sys := range []string{"mysql", "INFORMATION_SCHEMA", "Sys"} {
		if !mysql.SystemDatabases[strings.ToLower(sys)] {
			t.Errorf("system schema %q must be guarded", sys)
		}
	}
	if mysql.SystemDatabases[strings.ToLower("migration_test")] {
		t.Error("user schema must not be guarded")
	}
}
