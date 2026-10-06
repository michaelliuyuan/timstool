package assess

import (
	"fmt"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/source/mysql"
)

// Assessor runs all compatibility checks on a ScanResult. The srcKind
// (MS-10b2) calibrates the checker wording/mappings per source dialect:
// "" keeps the legacy PG-calibrated behavior byte-identical (PG parity
// anchor), "mysql" routes the source-aware branches.
type Assessor struct {
	srcKind string
}

// NewAssessor creates a new PG-calibrated Assessor (legacy shape).
func NewAssessor() *Assessor {
	return &Assessor{}
}

// NewAssessorFor creates an Assessor calibrated for the given source kind
// (the routed driver name; unknown/empty kinds keep the PG calibration).
func NewAssessorFor(kind string) *Assessor {
	return &Assessor{srcKind: kind}
}

// isMySQL reports whether the checker runs the MySQL-calibrated branches.
func (a *Assessor) isMySQL() bool { return a.srcKind == "mysql" }

// Assess runs all checkers and returns dimension results.
func (a *Assessor) Assess(result *ScanResult) []DimensionResult {
	var dims []DimensionResult

	dims = append(dims, a.checkDataTypes(result))
	dims = append(dims, a.checkStructure(result))
	dims = append(dims, a.checkIndexes(result))
	dims = append(dims, a.checkViews(result))
	dims = append(dims, a.checkFunctions(result))
	dims = append(dims, a.checkTriggers(result))
	dims = append(dims, a.checkCustomTypes(result))
	dims = append(dims, a.checkExtensions(result))
	dims = append(dims, a.checkSequences(result))

	return dims
}

// scorer helps build dimension results.
type scorer struct {
	findings []Finding
}

func (s *scorer) add(f Finding) {
	s.findings = append(s.findings, f)
}

func (s *scorer) result(dimension string) DimensionResult {
	if len(s.findings) == 0 {
		return DimensionResult{
			Dimension: dimension,
			Total:     0,
			Score:     100,
			Findings:  nil,
		}
	}
	var totalScore float64
	for _, f := range s.findings {
		totalScore += LevelScore[f.Level]
	}
	avg := totalScore / float64(len(s.findings))

	return DimensionResult{
		Dimension: dimension,
		Total:     len(s.findings),
		Score:     avg,
		Findings:  s.findings,
	}
}

// --- Data Types Checker ---

func (a *Assessor) checkDataTypes(result *ScanResult) DimensionResult {
	if a.isMySQL() {
		return a.checkDataTypesMySQL(result)
	}
	s := &scorer{}

	for _, col := range result.Columns {
		dt := strings.ToLower(col.DataType)
		objName := fmt.Sprintf("%s.%s.%s", col.TableSchema, col.TableName, col.ColumnName)

		switch {
		// Fully compatible types
		case dt == "integer" || dt == "int" || dt == "int4":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "integer", TiDBDetail: "INT",
				Suggestion: "", AutoFix: true,
			})
		case dt == "bigint" || dt == "int8":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "bigint", TiDBDetail: "BIGINT",
				Suggestion: "", AutoFix: true,
			})
		case dt == "smallint" || dt == "int2":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "smallint", TiDBDetail: "SMALLINT",
				Suggestion: "", AutoFix: true,
			})
		case dt == "text" || dt == "character varying" || dt == "varchar":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   dt, TiDBDetail: "TEXT/VARCHAR",
				Suggestion: "", AutoFix: true,
			})
		case dt == "character" || dt == "bpchar":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   fmt.Sprintf("CHAR(%d)", col.MaxLength), TiDBDetail: "CHAR",
				Suggestion: "CHAR 类型尾部空格处理可能不同", AutoFix: true,
			})
		case dt == "numeric" || dt == "decimal":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   fmt.Sprintf("NUMERIC(%d,%d)", col.NumericPrec, col.NumericScale),
				TiDBDetail: "DECIMAL",
				Suggestion: "", AutoFix: true,
			})
		case dt == "real" || dt == "float4":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "real", TiDBDetail: "FLOAT",
				Suggestion: "", AutoFix: true,
			})
		case dt == "double precision" || dt == "float8":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "double precision", TiDBDetail: "DOUBLE",
				Suggestion: "", AutoFix: true,
			})
		case dt == "boolean" || dt == "bool":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "boolean", TiDBDetail: "BOOLEAN",
				Suggestion: "", AutoFix: true,
			})
		case dt == "date":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "date", TiDBDetail: "DATE",
				Suggestion: "", AutoFix: true,
			})
		case dt == "timestamp without time zone" || dt == "timestamp":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "timestamp", TiDBDetail: "DATETIME",
				Suggestion: "", AutoFix: true,
			})
		case dt == "timestamp with time zone" || dt == "timestamptz":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "timestamptz", TiDBDetail: "TIMESTAMP",
				Suggestion: "时区信息会丢失，建议应用层处理时区转换", AutoFix: true,
			})
		case dt == "bytea":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "bytea", TiDBDetail: "BLOB",
				Suggestion: "", AutoFix: true,
			})
		case dt == "jsonb":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "jsonb", TiDBDetail: "JSON",
				Suggestion: "TiDB JSON 为文本存储，无二进制优化", AutoFix: true,
			})
		case dt == "json":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "json", TiDBDetail: "JSON",
				Suggestion: "", AutoFix: true,
			})
		case dt == "uuid":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "uuid", TiDBDetail: "CHAR(36) 或 BINARY(16)",
				Suggestion: "建议用 CHAR(36) 存储 UUID 字符串格式", AutoFix: true,
			})
		case dt == "bit" || dt == "bit varying" || dt == "varbit":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   dt, TiDBDetail: "无直接对应",
				Suggestion: "建议用 BINARY/VARBINARY 或 BIGINT 替代", AutoFix: false,
			})
		case dt == "money":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "money", TiDBDetail: "DECIMAL(19,2)",
				Suggestion: "", AutoFix: true,
			})
		case dt == "xml":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   "xml", TiDBDetail: "TEXT 或 JSON",
				Suggestion: "TiDB 无原生 XML 支持，建议用 TEXT 存储", AutoFix: false,
			})
		case dt == "cidr" || dt == "inet":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   dt, TiDBDetail: "VARCHAR(45)",
				Suggestion: "建议用 VARCHAR 存储 IP 地址", AutoFix: false,
			})
		case dt == "macaddr":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   "macaddr", TiDBDetail: "VARCHAR(17)",
				Suggestion: "建议用 VARCHAR 存储 MAC 地址", AutoFix: false,
			})
		case dt == "interval":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelIncompatible,
				PGDetail:   "interval", TiDBDetail: "不支持",
				Suggestion: "TiDB 不支持 INTERVAL 类型，需改为 VARCHAR 或 INT（秒数）", AutoFix: false,
			})
		case dt == "point" || dt == "line" || dt == "lseg" || dt == "box" ||
			dt == "path" || dt == "polygon" || dt == "circle":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelIncompatible,
				PGDetail:   dt, TiDBDetail: "不支持原生几何类型",
				Suggestion: "可考虑用 TiDB 空间函数 + GEOMETRY 替代", AutoFix: false,
			})
		case strings.HasSuffix(dt, "[]"):
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   fmt.Sprintf("ARRAY(%s)", strings.TrimSuffix(dt, "[]")),
				TiDBDetail: "JSON",
				Suggestion: "PG 数组需转为 JSON 格式存储", AutoFix: false,
			})
		case strings.HasPrefix(dt, "user-defined"):
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   dt, TiDBDetail: "需评估",
				Suggestion: "自定义类型需手动映射", AutoFix: false,
			})
		default:
			// Types we don't explicitly handle
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   dt, TiDBDetail: "需评估",
				Suggestion: fmt.Sprintf("数据类型 %s 需手动评估兼容性", dt), AutoFix: false,
			})
		}
	}

	return s.result(DimDataType)
}

// mysqlTypeBase normalizes a lower-cased MySQL DATA_TYPE value to its base
// name: strips the (...) argument clause and the UNSIGNED/SIGNED suffix so
// "decimal(10,2) unsigned" grades as DECIMAL.
func mysqlTypeBase(dt string) string {
	base := dt
	if idx := strings.IndexByte(base, '('); idx >= 0 {
		base = base[:idx]
	}
	base = strings.TrimSpace(base)
	base = strings.TrimSuffix(base, " unsigned")
	base = strings.TrimSuffix(strings.TrimSpace(base), " signed")
	return strings.TrimSpace(base)
}

// mysqlCompatibleBases are the MySQL DATA_TYPE bases that map to TiDB 1:1
// (same type family, at most argument normalization like DECIMAL(10,2)).
var mysqlCompatibleBases = map[string]bool{
	"tinyint": true, "smallint": true, "int": true, "integer": true,
	"bigint": true, "float": true, "double": true,
	"decimal": true, "numeric": true, "dec": true, "fixed": true,
	"char": true, "varchar": true, "binary": true, "varbinary": true,
	"date": true, "time": true, "datetime": true, "timestamp": true,
	"json": true, "bit": true, "text": true, "blob": true,
	"enum": true, "set": true,
}

// checkDataTypesMySQL is the MS-10b2 item-2 calibration: MySQL native
// types grade via the shared mysqlTypeMapper truth instead of falling
// into the PG switch's conservative "需评估" default. Grading contract
// (seq840): mapper-identity = Compatible; MEDIUMINT→INT / YEAR =
// Convertible (conversion noted); geometry / unknown = ManualNeeded.
func (a *Assessor) checkDataTypesMySQL(result *ScanResult) DimensionResult {
	s := &scorer{}
	tm := mysql.NewTypeMapper()

	for _, col := range result.Columns {
		dt := strings.ToLower(strings.TrimSpace(col.DataType))
		base := mysqlTypeBase(dt)
		objName := fmt.Sprintf("%s.%s.%s", col.TableSchema, col.TableName, col.ColumnName)
		mapped := tm.MapType(col.DataType, col.NumericPrec, col.NumericScale).Name

		switch {
		case base == "geometry" || base == "point" || base == "linestring" ||
			base == "polygon" || base == "multipoint" || base == "multilinestring" ||
			base == "multipolygon" || base == "geometrycollection":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   dt, TiDBDetail: "不支持几何类型",
				Suggestion: "TiDB 不支持原生几何类型，需改为 JSON/WKB 或应用层处理（MS-10c2 skip 台账同口径）", AutoFix: false,
			})
		case base == "year":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   dt, TiDBDetail: "YEAR",
				Suggestion: "TiDB 原生支持 YEAR，迁移按 1:1 保留并注记显示宽度语义差异", AutoFix: true,
			})
		case base == "mediumint":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   dt, TiDBDetail: "INT",
				Suggestion: "TiDB 无 MEDIUMINT，自动映射 INT（24 位→32 位，取值范围放大）", AutoFix: true,
			})
		case base == "bool" || base == "boolean":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   dt, TiDBDetail: "TINYINT(1)",
				Suggestion: "布尔别名映射 TINYINT(1)", AutoFix: true,
			})
		case base == "real":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   dt, TiDBDetail: "DOUBLE",
				Suggestion: "REAL 映射 DOUBLE", AutoFix: true,
			})
		case base == "tinytext" || base == "mediumtext" || base == "longtext":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   dt, TiDBDetail: "TEXT",
				Suggestion: "TEXT 家族归一 TEXT（无损放大）", AutoFix: true,
			})
		case base == "tinyblob" || base == "mediumblob" || base == "longblob":
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   dt, TiDBDetail: "BLOB",
				Suggestion: "BLOB 家族归一 BLOB（无损放大）", AutoFix: true,
			})
		case mysqlCompatibleBases[base]:
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   dt, TiDBDetail: mapped,
				Suggestion: "", AutoFix: true,
			})
		default:
			s.add(Finding{
				Dimension: DimDataType, ObjectType: "column",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   dt, TiDBDetail: "需评估",
				Suggestion: fmt.Sprintf("数据类型 %s 需手动评估兼容性", dt), AutoFix: false,
			})
		}
	}

	return s.result(DimDataType)
}

// --- Structure Checker ---

func (a *Assessor) checkStructure(result *ScanResult) DimensionResult {
	s := &scorer{}

	// Check for tables without PK
	for _, t := range result.Tables {
		hasPK := false
		for _, c := range result.Columns {
			if c.TableName == t.Name && c.IsPrimary {
				hasPK = true
				break
			}
		}
		if !hasPK {
			s.add(Finding{
				Dimension: DimStructure, ObjectType: "table",
				ObjectName: fmt.Sprintf("%s.%s", t.Schema, t.Name),
				Level:      LevelManualNeeded,
				PGDetail:   "无主键", TiDBDetail: "建议添加主键",
				Suggestion: "TiDB 强烈建议每张表都有主键，影响性能和数据校验", AutoFix: false,
			})
		}
	}

	// MS-10b2 item 7: AUTO_INCREMENT columns (MySQL EXTRA probe) get an
	// explicit TiDB-compatible structure annotation — the benign MySQL→
	// TiDB case the empty sequences dimension used to hide.
	if a.isMySQL() {
		for _, col := range result.Columns {
			if !col.IsAutoIncr {
				continue
			}
			s.add(Finding{
				Dimension: DimStructure, ObjectType: "column_auto_increment",
				ObjectName: fmt.Sprintf("%s.%s.%s", col.TableSchema, col.TableName, col.ColumnName),
				Level:      LevelCompatible,
				PGDetail:   "AUTO_INCREMENT", TiDBDetail: "AUTO_INCREMENT",
				Suggestion: "TiDB 原生支持 AUTO_INCREMENT，1:1 保留", AutoFix: true,
			})
		}
	}

	// Check column defaults with PG-specific functions
	for _, col := range result.Columns {
		if col.ColumnDefault == "" {
			continue
		}
		def := strings.ToLower(col.ColumnDefault)
		objName := fmt.Sprintf("%s.%s.%s", col.TableSchema, col.TableName, col.ColumnName)

		switch {
		case strings.Contains(def, "nextval(") || strings.Contains(def, "nextval("):
			// Sequence default — handled by sequence checker
		case strings.Contains(def, "now()") || strings.Contains(def, "current_timestamp"):
			s.add(Finding{
				Dimension: DimStructure, ObjectType: "column_default",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   def, TiDBDetail: "CURRENT_TIMESTAMP / NOW()",
				Suggestion: "TiDB 支持 NOW() 和 CURRENT_TIMESTAMP", AutoFix: true,
			})
		case strings.Contains(def, "uuid_generate_v4()") || strings.Contains(def, "gen_random_uuid()"):
			s.add(Finding{
				Dimension: DimStructure, ObjectType: "column_default",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   def, TiDBDetail: "无内置 UUID 生成",
				Suggestion: "TiDB 无内置 UUID 生成函数，建议应用层生成或用 REPLACE(UUID(),'-','')", AutoFix: false,
			})
		}
	}

	if len(s.findings) == 0 {
		return DimensionResult{Dimension: DimStructure, Score: 100}
	}
	return s.result(DimStructure)
}

// --- Index Checker ---

func (a *Assessor) checkIndexes(result *ScanResult) DimensionResult {
	s := &scorer{}

	for _, idx := range result.Indexes {
		idxType := strings.ToLower(idx.IndexType)
		objName := fmt.Sprintf("%s.%s", idx.TableName, idx.Name)

		switch {
		case idxType == "btree":
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "B-tree", TiDBDetail: "B-tree",
				Suggestion: "", AutoFix: true,
				DDL: idx.DDL,
			})
		case idxType == "hash":
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "HASH", TiDBDetail: "不支持",
				Suggestion: "HASH 索引 TiDB 不支持，建议改用 B-tree", AutoFix: false,
				DDL: idx.DDL,
			})
		case idxType == "gin":
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "GIN", TiDBDetail: "不支持",
				Suggestion: "GIN 索引不支持，JSON 查询需改用 TiDB JSON 函数", AutoFix: false,
				DDL: idx.DDL,
			})
		case idxType == "gist":
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "GiST", TiDBDetail: "不支持",
				Suggestion: "GiST 索引不支持，几何/全文索引需替代方案", AutoFix: false,
				DDL: idx.DDL,
			})
		case idxType == "brin":
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "BRIN", TiDBDetail: "不支持",
				Suggestion: "BRIN 索引不支持，建议使用分区表替代", AutoFix: false,
				DDL: idx.DDL,
			})
		case idxType == "spgist":
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   "SP-GiST", TiDBDetail: "不支持",
				Suggestion: "SP-GiST 索引不支持", AutoFix: false,
				DDL: idx.DDL,
			})
		default:
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   idxType, TiDBDetail: "需评估",
				Suggestion: fmt.Sprintf("索引类型 %s 需手动评估", idxType), AutoFix: false,
				DDL: idx.DDL,
			})
		}

		if idx.IsPartial {
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: fmt.Sprintf("%s (部分索引)", objName),
				Level:      LevelConvertible,
				PGDetail:   "部分索引 (WHERE 子句)", TiDBDetail: "不支持",
				Suggestion: "部分索引不支持，考虑使用分区表或覆盖索引替代", AutoFix: false,
				DDL: idx.DDL,
			})
		}

		if idx.IsExpression {
			s.add(Finding{
				Dimension: DimIndex, ObjectType: "index",
				ObjectName: fmt.Sprintf("%s (表达式索引)", objName),
				Level:      LevelConvertible,
				PGDetail:   "表达式索引", TiDBDetail: "不支持",
				Suggestion: "表达式索引不支持，需用生成列+索引替代", AutoFix: false,
				DDL: idx.DDL,
			})
		}
	}

	if len(s.findings) == 0 {
		return DimensionResult{Dimension: DimIndex, Score: 100}
	}
	return s.result(DimIndex)
}

// --- View Checker ---

func (a *Assessor) checkViews(result *ScanResult) DimensionResult {
	s := &scorer{}

	for _, view := range result.Views {
		objName := fmt.Sprintf("%s.%s", view.Schema, view.Name)
		def := strings.ToUpper(view.Definition)

		issues := 0
		// Check for PG-specific syntax. MS-10b2 item 1: the 22-word list
		// below is the PG-calibrated truth (PG parity anchor: unchanged).
		// On the MySQL path NOW()/ROW_NUMBER()/CAST(/CURRENT_DATE etc. are
		// MySQL-native too — flagging them was a full false-positive family
		// for MySQL 8 views — so the MySQL branch checks only the true
		// PG-only subset (seq840 item 1, 7 words).
		pgSyntax := []string{
			"ARRAY_AGG", "STRING_AGG", "BOOL_AND", "BOOL_OR",
			"EXTRACT(EPOCH", "TO_CHAR", "TO_NUMBER", "TO_DATE",
			"REGEXP_REPLACE", "REGEXP_MATCHES",
			"GENERATE_SERIES", "ROW_NUMBER()",
			"LATERAL", "WITH RECURSIVE",
			"ILIKE", "SIMILAR TO",
			"::", "CAST(", "NOW()", "CURRENT_DATE",
		}
		if a.isMySQL() {
			pgSyntax = []string{
				"ILIKE", "SIMILAR TO", "::",
				"ARRAY_AGG", "STRING_AGG",
				"GENERATE_SERIES", "EXTRACT(EPOCH",
			}
		}

		for _, syntax := range pgSyntax {
			if strings.Contains(def, syntax) {
				issues++
			}
		}

		viewDDL := view.DDL
		if viewDDL == "" {
			viewDDL = "CREATE VIEW " + view.Name + " AS " + view.Definition
		}

		switch {
		case issues == 0:
			s.add(Finding{
				Dimension: DimView, ObjectType: "view",
				ObjectName: objName,
				Level:      LevelCompatible,
				PGDetail:   "简单视图", TiDBDetail: "兼容",
				Suggestion: "", AutoFix: true,
				DDL: viewDDL,
			})
		case issues <= 2:
			s.add(Finding{
				Dimension: DimView, ObjectType: "view",
				ObjectName: objName,
				Level:      LevelConvertible,
				PGDetail:   a.viewDialectDetail(issues), TiDBDetail: "需改写",
				Suggestion: "部分函数需要手动改写", AutoFix: false,
			})
		default:
			s.add(Finding{
				Dimension: DimView, ObjectType: "view",
				ObjectName: objName,
				Level:      LevelManualNeeded,
				PGDetail:   a.viewDialectDetail(issues), TiDBDetail: "需大量改写",
				Suggestion: "视图需要大量改写，建议逐步迁移", AutoFix: false,
			})
		}
	}

	if len(s.findings) == 0 {
		return DimensionResult{Dimension: DimView, Score: 100}
	}
	return s.result(DimView)
}

// viewDialectDetail calibrates the finding wording per source kind
// (MS-10b2 item 1): the PG wording stays byte-identical to the pre-batch
// text (PG parity), the MySQL wording names PostgreSQL as the dialect the
// checked constructs belong to.
func (a *Assessor) viewDialectDetail(issues int) string {
	if a.isMySQL() {
		return fmt.Sprintf("含 %d 个 PostgreSQL 方言特有语法", issues)
	}
	return fmt.Sprintf("含 %d 个 PG 特有语法", issues)
}

// --- Function Checker ---

func (a *Assessor) checkFunctions(result *ScanResult) DimensionResult {
	s := &scorer{}

	for _, fn := range result.Functions {
		objName := fmt.Sprintf("%s.%s", fn.Schema, fn.Name)
		s.add(Finding{
			Dimension: DimFunction, ObjectType: "function",
			ObjectName: objName,
			Level:      LevelConvertible,
			PGDetail:   fmt.Sprintf("%s (%s)", fn.Language, fn.ReturnType),
			TiDBDetail: "需改写为 MySQL 兼容语法",
			Suggestion: fmt.Sprintf("函数 %s 需改写为 TiDB 兼容的 MySQL 语法", fn.Name),
			AutoFix:    false,
			DDL:        fn.DDL,
		})
	}

	if len(s.findings) == 0 {
		return DimensionResult{Dimension: DimFunction, Score: 100}
	}
	return s.result(DimFunction)
}

// --- Trigger Checker ---

func (a *Assessor) checkTriggers(result *ScanResult) DimensionResult {
	s := &scorer{}

	for _, trig := range result.Triggers {
		objName := fmt.Sprintf("%s.%s", trig.TableName, trig.Name)
		s.add(Finding{
			Dimension: DimTrigger, ObjectType: "trigger",
			ObjectName: objName,
			Level:      LevelConvertible,
			PGDetail:   fmt.Sprintf("%s %s", trig.Timing, trig.EventType),
			TiDBDetail: "需改写为 MySQL 兼容语法",
			Suggestion: "触发器需改写为 TiDB 兼容的 MySQL 触发器语法",
			AutoFix:    false,
			DDL:        trig.DDL,
		})
	}

	if len(s.findings) == 0 {
		return DimensionResult{Dimension: DimTrigger, Score: 100}
	}
	return s.result(DimTrigger)
}

// --- Custom Type Checker ---

func (a *Assessor) checkCustomTypes(result *ScanResult) DimensionResult {
	s := &scorer{}

	for _, enum := range result.Enums {
		objName := fmt.Sprintf("%s.%s", enum.Schema, enum.Name)
		s.add(Finding{
			Dimension: DimCustomType, ObjectType: "enum",
			ObjectName: objName,
			Level:      LevelConvertible,
			PGDetail:   fmt.Sprintf("ENUM(%s)", strings.Join(enum.Values, ",")),
			TiDBDetail: "ENUM",
			Suggestion: "TiDB 支持 ENUM 类型，可直接映射", AutoFix: true,
			DDL: enum.DDL,
		})
	}

	if len(s.findings) == 0 {
		return DimensionResult{Dimension: DimCustomType, Score: 100}
	}
	return s.result(DimCustomType)
}

// --- Extension Checker ---

func (a *Assessor) checkExtensions(result *ScanResult) DimensionResult {
	s := &scorer{}

	knownExtensions := map[string]struct {
		level      string
		tidbDetail string
		suggestion string
	}{
		"plpgsql":            {LevelIncompatible, "不支持", "存储过程需应用层实现"},
		"uuid-ossp":          {LevelManualNeeded, "无内置", "用 REPLACE(UUID(),'-','') 替代"},
		"pgcrypto":           {LevelManualNeeded, "无内置", "需应用层加密"},
		"pg_stat_statements": {LevelIncompatible, "无直接对应", "TiDB 有内置监控"},
		"postgis":            {LevelIncompatible, "有限支持", "TiDB 有基本空间函数"},
		"hstore":             {LevelManualNeeded, "无内置", "建议用 JSON 替代"},
		"ltree":              {LevelIncompatible, "不支持", "需应用层实现"},
		"pg_trgm":            {LevelIncompatible, "不支持", "用 TiDB 全文索引替代"},
		"btree_gin":          {LevelIncompatible, "不支持", ""},
		"btree_gist":         {LevelIncompatible, "不支持", ""},
	}

	for _, ext := range result.Extensions {
		if info, ok := knownExtensions[ext.Name]; ok {
			s.add(Finding{
				Dimension: DimExtension, ObjectType: "extension",
				ObjectName: ext.Name,
				Level:      info.level,
				PGDetail:   ext.Name, TiDBDetail: info.tidbDetail,
				Suggestion: info.suggestion, AutoFix: false,
				DDL: ext.DDL,
			})
		} else {
			s.add(Finding{
				Dimension: DimExtension, ObjectType: "extension",
				ObjectName: ext.Name,
				Level:      LevelManualNeeded,
				PGDetail:   ext.Name, TiDBDetail: "需评估",
				Suggestion: fmt.Sprintf("扩展 %s 需手动评估替代方案", ext.Name),
				AutoFix:    false,
			})
		}
	}

	if len(s.findings) == 0 {
		return DimensionResult{Dimension: DimExtension, Score: 100}
	}
	return s.result(DimExtension)
}

// --- Sequence Checker ---

func (a *Assessor) checkSequences(result *ScanResult) DimensionResult {
	s := &scorer{}

	for _, seq := range result.Sequences {
		objName := fmt.Sprintf("%s.%s", seq.Schema, seq.Name)
		s.add(Finding{
			Dimension: DimSequence, ObjectType: "sequence",
			ObjectName: objName,
			Level:      LevelConvertible,
			PGDetail:   "SEQUENCE", TiDBDetail: "AUTO_INCREMENT",
			Suggestion: "序列需改为 AUTO_INCREMENT 列", AutoFix: true,
			DDL: seq.DDL,
		})
	}

	if len(s.findings) == 0 {
		return DimensionResult{Dimension: DimSequence, Score: 100}
	}
	return s.result(DimSequence)
}
