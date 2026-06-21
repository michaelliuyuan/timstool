package mysql

import "strings"

// quoteDefault renders a MySQL column default TiDB-DDL-ready for the CIR
// (architect decision, seq=151: the adapter owns literal-vs-expression since
// only it has DATA_TYPE; the target renderer emits the default verbatim). MySQL
// information_schema returns string/date literal defaults unquoted, so a CHAR
// DEFAULT CNY must become 'CNY'; expression defaults (CURRENT_TIMESTAMP, NOW(),
// NULL, anything with parens) and numeric defaults stay raw.
func quoteDefault(dataType, def string) string {
	d := strings.TrimSpace(def)
	if d == "" {
		return ""
	}
	upper := strings.ToUpper(d)
	if upper == "NULL" || isExprDefault(upper) || isNumericDataType(dataType) {
		return d
	}
	// string / date / enum / set / json / ... literal → single-quote (escape ')
	return "'" + strings.ReplaceAll(d, "'", "''") + "'"
}

// isExprDefault reports whether def is a function/expression default that must
// render raw (unquoted).
func isExprDefault(upper string) bool {
	for _, f := range []string{"CURRENT_TIMESTAMP", "CURRENT_DATE", "CURRENT_TIME", "LOCALTIMESTAMP", "LOCALTIME", "NOW", "TRUE", "FALSE", "UUID"} {
		if upper == f || strings.HasPrefix(upper, f+"(") {
			return true
		}
	}
	return strings.ContainsAny(upper, "()") // any expression with parentheses
}

func isNumericDataType(dt string) bool {
	switch strings.ToLower(strings.TrimSpace(dt)) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint",
		"decimal", "numeric", "float", "double", "real", "bit", "year",
		"bool", "boolean":
		return true
	}
	return false
}
