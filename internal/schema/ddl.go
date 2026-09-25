package schema

import (
	"fmt"
	"strings"
)

type DDLBuilder struct {
	statements []string
}

func NewDDLBuilder() *DDLBuilder {
	return &DDLBuilder{}
}

func (b *DDLBuilder) BuildTableDDL(table TableInfo) error {
	var cols []string
	pk := table.PrimaryKey()
	pkColSet := make(map[string]bool)
	if pk != nil {
		for _, c := range pk.Columns {
			pkColSet[c] = true
		}
	}

	for _, col := range table.Columns {
		colDDL, err := b.buildColumnDDL(col)
		if err != nil {
			return fmt.Errorf("column %s.%s: %w", table.Name, col.ColumnName, err)
		}
		cols = append(cols, colDDL)
	}

	if pk != nil {
		pkCols := make([]string, len(pk.Columns))
		for i, c := range pk.Columns {
			pkCols[i] = QuoteIdentifier(c)
		}
		cols = append(cols, fmt.Sprintf("PRIMARY KEY (%s)", strings.Join(pkCols, ", ")))
	}

	var tableSuffix string
	if table.Comment != "" {
		tableSuffix = fmt.Sprintf(" COMMENT '%s'", escapeSQLString(table.Comment))
	}

	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)%s",
		QuoteIdentifier(table.Name),
		strings.Join(cols, ",\n  "),
		tableSuffix)

	b.statements = append(b.statements, ddl)

	return nil
}

func (b *DDLBuilder) buildColumnDDL(col Column) (string, error) {
	// For character types, MaxLength holds character_maximum_length;
	// for numeric types, NumericPrec/NumericScale hold the precision.
	// Pass the correct precision based on column type.
	precision := col.NumericPrec
	if col.PGType == PGVarchar || col.PGType == PGChar {
		precision = col.MaxLength
	}
	mysqlType := MapTypeWithPrecision(col.PGType, precision, col.NumericScale)
	if mysqlType == "" {
		mysqlType = "TEXT"
	}

	if col.ColumnName == "" {
		return "", fmt.Errorf("empty column name")
	}

	parts := []string{
		QuoteIdentifier(col.ColumnName),
		mysqlType,
	}

	if !col.IsNullable {
		parts = append(parts, "NOT NULL")
	} else {
		parts = append(parts, "NULL")
	}

	if col.IsAutoIncr {
		parts = append(parts, "AUTO_INCREMENT")
	} else if col.DefaultValue != "" {
		def := convertDefaultValue(col.DefaultValue, col.PGType)
		if def != "" {
			parts = append(parts, "DEFAULT "+def)
		}
	}

	if col.Comment != "" {
		parts = append(parts, fmt.Sprintf("COMMENT '%s'", escapeSQLString(col.Comment)))
	}

	return strings.Join(parts, " "), nil
}

func (b *DDLBuilder) BuildPrimaryKeyDDL(table TableInfo) {
	pk := table.PrimaryKey()
	if pk == nil {
		return
	}
	cols := make([]string, len(pk.Columns))
	for i, c := range pk.Columns {
		cols[i] = QuoteIdentifier(c)
	}
	b.statements = append(b.statements, fmt.Sprintf(
		"ALTER TABLE %s ADD PRIMARY KEY (%s)",
		QuoteIdentifier(table.Name),
		strings.Join(cols, ", "),
	))
}

func (b *DDLBuilder) BuildIndexDDL(idx Index) string {
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = QuoteIdentifier(c)
	}

	switch idx.IndexType {
	case "btree", "":
		// Supported natively
	case "gin", "gist", "hash", "spgist", "brin":
		return fmt.Sprintf("-- WARNING: index %s uses %s which is not supported in TiDB, converted to regular index",
			idx.IndexName, idx.IndexType)
	default:
		return fmt.Sprintf("-- WARNING: unknown index type %s for %s", idx.IndexType, idx.IndexName)
	}

	if idx.IsPrimary {
		return ""
	}

	var unique string
	if idx.IsUnique {
		unique = "UNIQUE "
	}

	return fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s)",
		unique,
		QuoteIdentifier(idx.IndexName),
		QuoteIdentifier(idx.TableName),
		strings.Join(cols, ", "))
}

func (b *DDLBuilder) BuildForeignKeyDDL(fk ForeignKey) string {
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = QuoteIdentifier(c)
	}
	refCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		refCols[i] = QuoteIdentifier(c)
	}
	return fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s ON UPDATE %s",
		QuoteIdentifier(fk.TableName),
		QuoteIdentifier(fk.ConstraintName),
		strings.Join(cols, ", "),
		QuoteIdentifier(fk.RefTable),
		strings.Join(refCols, ", "),
		fk.OnDelete,
		fk.OnUpdate)
}

func (b *DDLBuilder) BuildViewDDL(view View) string {
	def := view.Definition
	def = strings.TrimSpace(def)
	if !strings.HasPrefix(strings.ToUpper(def), "SELECT") {
		return fmt.Sprintf("-- WARNING: view %s has complex definition, manual review needed\n-- %s", view.Name, def)
	}
	// pg_get_viewdef always emits PG casts (::text, ::character varying(10),
	// ::timestamp without time zone, ...). TiDB rejects them with ER 1064,
	// so strip casts outside string literals before applying (F-07).
	def = stripPGCasts(def)
	return fmt.Sprintf("CREATE OR REPLACE VIEW %s AS %s", QuoteIdentifier(view.Name), def)
}

// stripPGCasts removes `::<type>` casts from a view definition. Text inside
// single-quoted literals (with ” escapes) is left untouched; multi-word PG
// types (character varying, double precision, timestamp without time zone)
// and sized types (numeric(10,2)) are consumed as a whole.
func stripPGCasts(def string) string {
	var b strings.Builder
	i := 0
	n := len(def)
	for i < n {
		c := def[i]
		if c == '\'' {
			// copy the entire literal verbatim, honoring '' escapes
			j := i + 1
			for j < n {
				if def[j] == '\'' {
					if j+1 < n && def[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			b.WriteString(def[i:j])
			i = j
			continue
		}
		if c == ':' && i+1 < n && def[i+1] == ':' {
			end := skipPGCastType(def, i+2)
			if end > i+2 {
				i = end
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// skipPGCastType returns the index just past a PG type name starting at def[i],
// or i if no type name is present.
func skipPGCastType(def string, i int) int {
	n := len(def)
	start := i
	readWord := func(k int) (string, int) {
		j := k
		for j < n && (isIdentChar(def[j])) {
			j++
		}
		return def[k:j], j
	}
	word, j := readWord(i)
	if word == "" {
		return start
	}
	lower := strings.ToLower(word)
	switch lower {
	case "character", "bit":
		// character varying / bit varying
		k := j
		for k < n && (def[k] == ' ' || def[k] == '\t' || def[k] == '\n' || def[k] == '\r') {
			k++
		}
		if w2, j2 := readWord(k); strings.ToLower(w2) == "varying" {
			j = j2
		}
	case "double":
		k := j
		for k < n && (def[k] == ' ' || def[k] == '\t' || def[k] == '\n' || def[k] == '\r') {
			k++
		}
		if w2, j2 := readWord(k); strings.ToLower(w2) == "precision" {
			j = j2
		}
	case "timestamp", "time":
		// optional precision modifier before the with/without clause:
		// ::timestamp(3) with time zone (timestamptz(3) — real
		// pg_get_viewdef output, F-07 amendment)
		if j < n && def[j] == '(' {
			k := j + 1
			for k < n && (def[k] >= '0' && def[k] <= '9' || def[k] == ',' || def[k] == ' ') {
				k++
			}
			if k < n && def[k] == ')' {
				j = k + 1
			}
		}
		k := j
		for k < n && (def[k] == ' ' || def[k] == '\t' || def[k] == '\n' || def[k] == '\r') {
			k++
		}
		if w2, j2 := readWord(k); strings.ToLower(w2) == "with" || strings.ToLower(w2) == "without" {
			k = j2
			for k < n && (def[k] == ' ' || def[k] == '\t' || def[k] == '\n' || def[k] == '\r') {
				k++
			}
			if w3, j3 := readWord(k); strings.ToLower(w3) == "time" {
				k = j3
				for k < n && (def[k] == ' ' || def[k] == '\t' || def[k] == '\n' || def[k] == '\r') {
					k++
				}
				if w4, j4 := readWord(k); strings.ToLower(w4) == "zone" {
					j = j4
				} else {
					j = k
				}
			}
		}
	}
	// optional type modifier: (N) or (N,M)
	if j < n && def[j] == '(' {
		k := j + 1
		for k < n && (def[k] >= '0' && def[k] <= '9' || def[k] == ',' || def[k] == ' ') {
			k++
		}
		if k < n && def[k] == ')' {
			j = k + 1
		}
	}
	return j
}

func isIdentChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func (b *DDLBuilder) BuildEnumDDL(enum EnumType) string {
	values := make([]string, len(enum.Values))
	for i, v := range enum.Values {
		values[i] = fmt.Sprintf("'%s'", escapeSQLString(v))
	}
	return fmt.Sprintf("-- ENUM %s: TiDB does not support CREATE TYPE AS ENUM, using VARCHAR or explicit ENUM\n-- Values: %s",
		enum.Name, strings.Join(values, ", "))
}

func (b *DDLBuilder) Statements() []string {
	return b.statements
}

func (b *DDLBuilder) JoinSQL() string {
	return strings.Join(b.statements, ";\n\n") + ";"
}

func convertDefaultValue(pgDefault string, pgType PGType) string {
	d := strings.TrimSpace(pgDefault)

	switch {
	case d == "", strings.ToUpper(d) == "NULL":
		return ""
	case strings.Contains(strings.ToUpper(d), "NEXTVAL"):
		return ""
	case strings.Contains(strings.ToUpper(d), "CURRENT_TIMESTAMP"):
		if pgType == PGTimestamp || pgType == PGTimestampTZ {
			return "CURRENT_TIMESTAMP(6)"
		}
		return "CURRENT_TIMESTAMP"
	case strings.ToUpper(d) == "TRUE":
		return "1"
	case strings.ToUpper(d) == "FALSE":
		return "0"
	}

	if idx := strings.Index(d, "::"); idx > 0 {
		raw := strings.TrimSpace(d[:idx])
		if strings.ToUpper(raw) == "NULL" {
			return ""
		}
		if strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'") {
			return raw
		}
		if isSimpleLiteral(raw) {
			return raw
		}
		return ""
	}

	if strings.HasPrefix(d, "'") && strings.HasSuffix(d, "'") {
		return d
	}

	if isSimpleLiteral(d) {
		return d
	}

	return ""
}

func isSimpleLiteral(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == '+':
		default:
			return false
		}
	}
	return true
}

func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
