package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ReadSchema reads MySQL information_schema → CIR Schema.
// Columns' TiDBType is populated via TypeMapper so the target never needs
// to know the source.
func (r *schemaReader) ReadSchema(ctx context.Context, opts source.Filter) (*source.Schema, error) {
	db := r.src.DB()
	if db == nil {
		return nil, fmt.Errorf("mysql SchemaReader: not connected (call Connect first)")
	}

	database := r.src.Config().Database
	if database == "" {
		return nil, fmt.Errorf("mysql SchemaReader: database not configured")
	}

	tables, err := r.readTables(ctx, db, database, opts)
	if err != nil {
		return nil, fmt.Errorf("mysql SchemaReader: read tables: %w", err)
	}

	cir := &source.Schema{Catalog: database}
	for _, t := range tables {
		columns, pk, err := r.readColumns(ctx, db, database, t)
		if err != nil {
			return nil, fmt.Errorf("mysql SchemaReader: read columns for %s: %w", t, err)
		}

		indexes, err := r.readIndexes(ctx, db, database, t)
		if err != nil {
			return nil, fmt.Errorf("mysql SchemaReader: read indexes for %s: %w", t, err)
		}

		cir.Tables = append(cir.Tables, source.Table{
			Schema:  database, // MySQL: schema = database name
			Name:    t,
			Columns: columns,
			PK:      pk,
			Indexes: indexes,
		})
	}

	return cir, nil
}

func (r *schemaReader) readTables(ctx context.Context, db *sql.DB, database string, opts source.Filter) ([]string, error) {
	query := `
		SELECT TABLE_NAME
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ?
		  AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`

	rows, err := db.QueryContext(ctx, query, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Build include set
	includeSet := make(map[string]bool, len(opts.Tables))
	for _, t := range opts.Tables {
		includeSet[t] = true
	}
	excludeSet := make(map[string]bool, len(opts.ExcludeTables))
	for _, t := range opts.ExcludeTables {
		excludeSet[t] = true
	}

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if excludeSet[name] {
			continue
		}
		if len(includeSet) > 0 && !includeSet[name] {
			continue
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func (r *schemaReader) readColumns(ctx context.Context, db *sql.DB, database, table string) ([]source.Column, []string, error) {
	query := `
		SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT,
		       EXTRA, COLUMN_COMMENT, NUMERIC_PRECISION, NUMERIC_SCALE, COLUMN_KEY
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`

	rows, err := db.QueryContext(ctx, query, database, table)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	tm := r.src.TypeMapper()
	var columns []source.Column
	var pk []string
	for rows.Next() {
		var (
			colName, dataType, colType, isNullable string
			colDefault, extra, comment            sql.NullString
			colKey                                sql.NullString
			numPrec, numScale                     sql.NullInt64
		)
		if err := rows.Scan(&colName, &dataType, &colType, &isNullable,
			&colDefault, &extra, &comment, &numPrec, &numScale, &colKey); err != nil {
			return nil, nil, err
		}

		nullable := isNullable == "YES"
		isAutoIncr := false
		if extra.Valid && (extra.String == "auto_increment" ||
			stringsContains(extra.String, "auto_increment")) {
			isAutoIncr = true
		}

		prec := 0
		if numPrec.Valid {
			prec = int(numPrec.Int64)
		}
		sc := 0
		if numScale.Valid {
			sc = int(numScale.Int64)
		}

		mapped := tm.MapType(colType, prec, sc)
		var defVal string
		if colDefault.Valid {
			defVal = quoteDefault(dataType, colDefault.String) // TiDB-DDL-ready (literals quoted)
		}
		var cmt string
		if comment.Valid {
			cmt = comment.String
		}

		columns = append(columns, source.Column{
			Name:       colName,
			SourceType: colType,
			TiDBType:   mapped.Name,
			Nullable:   nullable,
			Default:    defVal,
			IsAutoIncr: isAutoIncr,
			Comment:    cmt,
		})

		if colKey.Valid && colKey.String == "PRI" {
			pk = append(pk, colName)
		}
	}
	return columns, pk, rows.Err()
}

func (r *schemaReader) readIndexes(ctx context.Context, db *sql.DB, database, table string) ([]source.Index, error) {
	query := `
		SELECT INDEX_NAME, COLUMN_NAME, NON_UNIQUE, SEQ_IN_INDEX
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`

	rows, err := db.QueryContext(ctx, query, database, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type idxKey struct {
		name   string
		unique bool
	}
	idxMap := make(map[idxKey][]string)
	var idxOrder []idxKey

	for rows.Next() {
		var idxName, colName string
		var nonUnique, seq int
		if err := rows.Scan(&idxName, &colName, &nonUnique, &seq); err != nil {
			return nil, err
		}
		// Skip PRIMARY (already in PK column list from readColumns)
		if idxName == "PRIMARY" {
			continue
		}
		key := idxKey{name: idxName, unique: nonUnique == 0}
		if _, exists := idxMap[key]; !exists {
			idxOrder = append(idxOrder, key)
		}
		idxMap[key] = append(idxMap[key], colName)
	}

	var indexes []source.Index
	for _, key := range idxOrder {
		indexes = append(indexes, source.Index{
			Name:    key.name,
			Columns: idxMap[key],
			Unique:  key.unique,
		})
	}
	return indexes, rows.Err()
}

// schemaReader is the MySQL SchemaReader implementation.
type schemaReader struct{ src *Source }

func stringsContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
