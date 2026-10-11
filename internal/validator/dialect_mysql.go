package validator

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// mysqlDialect implements CompareDialect for a MySQL SOURCE (MS-08). It
// mirrors the MySQL-wire shapes already proven by tidbDialect (backtick
// quoting, `?` placeholder, SHOW TABLE STATUS estimate) with source-side
// semantics: the schema is a DATABASE name (two-part qualification is kept,
// matching the PG source shape), key metadata comes from
// information_schema.statistics, and the watermark column whitelist speaks
// MySQL DATA_TYPE names. No concurrency, no fallback policy, no guard logic
// lives here - those stay in the main flow (MS-03 ruling seq 202-2).
type mysqlDialect struct{}

// QuoteIdent mirrors tidbDialect.QuoteIdent (backtick doubling).
func (mysqlDialect) QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// Qualify keeps the source-side TWO-part reference shape (the MySQL schema
// IS the database name): `schema`.`table`.
func (d mysqlDialect) Qualify(schema, table string) string {
	return d.QuoteIdent(schema) + "." + d.QuoteIdent(table)
}

// EstimateRows mirrors the tidbDialect SHOW TABLE STATUS estimate, scoped to
// the source schema via SHOW TABLE STATUS FROM (the connection database may
// differ from the compare schema). An error means "estimate unavailable" -
// the fallback decision belongs to the main flow (ruling seq 202-2). Parse
// failures yield 0+nil, preserving the legacy semantics.
func (d mysqlDialect) EstimateRows(ctx context.Context, q dialectQueryer, schema, table string) (int64, error) {
	var name, engine, rowFormat, rows, avgRowLen, dataLen, maxDataLen, indexLen, autoInc, createTime, updateTime, checkTime, collation, checksum, createOpts, comment sql.NullString
	err := q.QueryRowContext(ctx,
		fmt.Sprintf("SHOW TABLE STATUS FROM %s LIKE '%s'", d.QuoteIdent(schema), escapeSQLLike(table))).Scan(
		&name, &engine, &rowFormat, &rows, &avgRowLen, &dataLen, &maxDataLen, &indexLen,
		&autoInc, &createTime, &updateTime, &checkTime, &collation, &checksum, &createOpts, &comment)
	if err != nil {
		return 0, err
	}
	if rows.Valid {
		var val int64
		if _, err := fmt.Sscanf(rows.String, "%d", &val); err == nil {
			return val, nil
		}
	}
	return 0, nil
}

// CountExact mirrors tidbDialect.CountExact with two-part source
// qualification.
func (d mysqlDialect) CountExact(ctx context.Context, q dialectQueryer, schema, table string) (int64, error) {
	var count sql.NullInt64
	err := q.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", d.Qualify(schema, table))).Scan(&count)
	return count.Int64, err
}

// WmPredicateFragment mirrors tidbDialect (the `?` placeholder follows the
// go-sql-driver); wmOp semantics stay in the main flow.
func (d mysqlDialect) WmPredicateFragment(wm *config.WatermarkFilter) string {
	return fmt.Sprintf("%s %s ?", d.QuoteIdent(wm.Column), wmOp(wm))
}

// wmAllowedColumnTypesMySQL is the MySQL DATA_TYPE whitelist (the
// information_schema probe returns lowercase names like "datetime"); it maps
// the PG whitelist semantics (wmfilter.go) onto MySQL types. wmOp semantics
// stay in the main flow.
var wmAllowedColumnTypesMySQL = map[string]bool{
	"datetime":  true,
	"timestamp": true,
	"date":      true,
	"int":       true,
	"bigint":    true,
}

// ValidateWatermarkColumn mirrors postgresDialect.ValidateWatermarkColumn
// with the MySQL `?`-placeholder information_schema probe and the MySQL
// DATA_TYPE whitelist (action-a verified: DATA_TYPE returns lowercase
// "datetime" on TiDB/MySQL wire).
func (mysqlDialect) ValidateWatermarkColumn(ctx context.Context, q dialectQueryer, schema, table string, wm *config.WatermarkFilter) error {
	var dataType string
	err := q.QueryRowContext(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ? AND column_name = ?`,
		schema, table, wm.Column).Scan(&dataType)
	if err == sql.ErrNoRows {
		return fmt.Errorf("水位列 %s 不存在于表 %s（水位过滤仅支持列存在的表参与比对）", wm.Column, table)
	}
	if err != nil {
		return fmt.Errorf("校验水位列: %w", err)
	}
	if !wmAllowedColumnTypesMySQL[dataType] {
		return fmt.Errorf("水位列 %s 类型 %s 不在可比白名单（datetime/timestamp/date/int/bigint）", wm.Column, dataType)
	}
	return nil
}

// DetectTableKey reads PK / unique-index metadata from
// information_schema.statistics (the MySQL equivalent of the PG
// table_constraints/pg_indexes probe). NON_UNIQUE=0 marks unique indexes;
// PRIMARY KEY rows carry INDEX_NAME='PRIMARY'. Columns are ordered by
// SEQ_IN_INDEX.
func (mysqlDialect) DetectTableKey(ctx context.Context, q dialectQueryer, schema, table string) (*TableKeyInfo, error) {
	info := &TableKeyInfo{}

	idxRows, err := q.QueryContext(ctx, `
		SELECT index_name, column_name, non_unique
		FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ?
		ORDER BY index_name, seq_in_index`,
		schema, table)
	if err != nil {
		return nil, fmt.Errorf("query index metadata: %w", err)
	}
	defer idxRows.Close()

	type idxCols struct {
		unique bool
		cols   []string
	}
	indexes := make(map[string]*idxCols)
	var order []string
	for idxRows.Next() {
		var idxName, colName string
		var nonUnique int
		if err := idxRows.Scan(&idxName, &colName, &nonUnique); err != nil {
			return nil, fmt.Errorf("scan index column: %w", err)
		}
		entry, ok := indexes[idxName]
		if !ok {
			entry = &idxCols{unique: nonUnique == 0}
			indexes[idxName] = entry
			order = append(order, idxName)
		}
		entry.cols = append(entry.cols, colName)
	}
	if err := idxRows.Err(); err != nil {
		return nil, fmt.Errorf("list index metadata: %w", err)
	}

	if pk, ok := indexes["PRIMARY"]; ok && len(pk.cols) > 0 {
		info.HasPK = true
		info.PKColumns = pk.cols
		return info, nil
	}
	for _, idxName := range order {
		entry := indexes[idxName]
		if entry.unique && len(entry.cols) > 0 {
			info.HasUniqueIndex = true
			info.UniqueColumns = entry.cols
			break
		}
	}
	return info, nil
}

// AdjustDSN returns the DSN unchanged: BuildMySQLDSN already pins Loc=UTC
// (driver default) for the source pool; the TIMESTAMP session-timezone
// symmetry is guaranteed by the pool-level location, so no per-DSN suffix is
// needed (the PG implementation appends the UTC TimeZone option - the MySQL
// equivalent is baked into the DSN builder).
func (mysqlDialect) AdjustDSN(dsn string) string {
	return dsn
}

// ListTables mirrors postgresDialect.ListTables with the go-sql-driver `?`
// placeholder (action-a verified against TiDB's MySQL wire).
func (mysqlDialect) ListTables(ctx context.Context, q dialectQueryer, schema string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = ? AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return tables, nil
}
