package assess

import (
	"context"
	"database/sql"
	"strings"
)

// SchemaScanner is the source-agnostic scanning surface shared by the PG
// catalog scanner (NewScanner) and the MySQL information_schema scanner
// (NewScannerFor "mysql"). MS-10b: the assess pipeline stays single-shape
// downstream (checker/report consume ScanResult); only the catalog dialect
// differs per source kind.
type SchemaScanner interface {
	ScanAll(ctx context.Context) (*ScanResult, error)
}

// NewScannerFor returns a scanner dispatched on the normalized source kind.
// Unknown/empty kinds keep the legacy PG scanner (zero-regression: callers
// that predate MS-10b pass ""). The kind string is the driver name routed by
// the webapi connection layer (sourceConnSpec), not a re-implementation of
// the capability gate - guards stay at the handlers.
func NewScannerFor(kind string, db *sql.DB, schema string) SchemaScanner {
	if kind == "mysql" {
		return newMySQLScanner(db, schema)
	}
	return NewScanner(db, schema)
}

// mysqlScanner reads schema information from a MySQL server via
// information_schema. MS-10b per-scan dialect map (equivalent vs degraded):
//
//	tables     information_schema.TABLES            - equivalent
//	columns    information_schema.COLUMNS + PK probe - equivalent
//	indexes    information_schema.STATISTICS aggregate
//	          (per-index row) - equivalent modulo IsPartial, which MySQL
//	          catalogs do not expose (MySQL has no partial indexes at all);
//	          IsExpression IS detected via the functional-key-part shape
//	          (EXPRESSION non-NULL / COLUMN_NAME NULL, MySQL 8.0.13+)
//	views      information_schema.VIEWS             - equivalent for the
//	          definition; DDL is degraded (no pg_get_viewdef analog, a
//	          per-view SHOW CREATE VIEW round-trip is not paid here)
//	functions  information_schema.ROUTINES          - degraded: Language is
//	          reported as "sql" (MySQL routines are SQL-bodied)
//	triggers   information_schema.TRIGGERS          - equivalent (DDL degraded)
//	enums      n/a - MySQL has no schema-level enum types (column ENUM
//	          types surface via the data_type dimension instead)
//	extensions n/a - no extension concept (empty)
//	sequences  n/a - no sequence objects (AUTO_INCREMENT is column-level;
//	          NOT collected today - the sequences dimension stays empty,
//	          which is benign MySQL->TiDB (TiDB supports AUTO_INCREMENT)
//	          but a real gap if this scanner is ever reused in reverse)
//
// The connection is expected to arrive with the MS-08d UTC session pin
// (time_zone='+00:00' on the DSN); the catalog queries below read no
// temporal VALUES, only names/ints/flags, so the pin is a no-op here but is
// kept uniform with every other MySQL source path (F-13 discipline).
type mysqlScanner struct {
	db     *sql.DB
	schema string
}

func newMySQLScanner(db *sql.DB, schema string) *mysqlScanner {
	if schema == "" {
		schema = "public"
	}
	return &mysqlScanner{db: db, schema: schema}
}

// mysqlIdxVisibleFrag mirrors the MS-10a probe discipline: STATISTICS rows
// for invisible indexes (MySQL 8.0+) must not count as usable index shapes.
const mysqlIdxVisibleFrag = ` AND s.IS_VISIBLE = 'YES'`

const mysqlTablesSQL = `
	SELECT TABLE_SCHEMA, TABLE_NAME
	FROM information_schema.TABLES
	WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
	ORDER BY TABLE_NAME
`

const mysqlColumnsSQL = `
	SELECT
		c.TABLE_SCHEMA,
		c.TABLE_NAME,
		c.COLUMN_NAME,
		c.DATA_TYPE,
		COALESCE(c.CHARACTER_MAXIMUM_LENGTH, 0),
		COALESCE(c.NUMERIC_PRECISION, 0),
		COALESCE(c.NUMERIC_SCALE, 0),
		c.IS_NULLABLE = 'YES',
		COALESCE(c.COLUMN_DEFAULT, ''),
		EXISTS (
			SELECT 1 FROM information_schema.STATISTICS s
			WHERE s.TABLE_SCHEMA = c.TABLE_SCHEMA
				AND s.TABLE_NAME = c.TABLE_NAME
				AND s.COLUMN_NAME = c.COLUMN_NAME
				AND s.INDEX_NAME = 'PRIMARY'
		),
		c.ORDINAL_POSITION
	FROM information_schema.COLUMNS c
	WHERE c.TABLE_SCHEMA = ?
	ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION
`

const mysqlIndexesSQL = `
	SELECT
		s.TABLE_NAME,
		s.INDEX_NAME,
		CASE WHEN s.INDEX_TYPE = 'BTREE' THEN 'btree' ELSE LOWER(s.INDEX_TYPE) END,
		MAX(s.NON_UNIQUE) = 0,
		s.INDEX_NAME = 'PRIMARY',
		CONCAT(s.INDEX_NAME, ' (', GROUP_CONCAT(COALESCE(s.COLUMN_NAME, s.EXPRESSION) ORDER BY s.SEQ_IN_INDEX SEPARATOR ','), ')'),
		0,
		MAX(s.COLUMN_NAME IS NULL)
	FROM information_schema.STATISTICS s
	WHERE s.TABLE_SCHEMA = ?` + mysqlIdxVisibleFrag + `
	GROUP BY s.TABLE_SCHEMA, s.TABLE_NAME, s.INDEX_NAME, s.INDEX_TYPE
	ORDER BY s.TABLE_NAME, s.INDEX_NAME
`

const mysqlViewsSQL = `
	SELECT TABLE_SCHEMA, TABLE_NAME, VIEW_DEFINITION, ''
	FROM information_schema.VIEWS
	WHERE TABLE_SCHEMA = ?
	ORDER BY TABLE_NAME
`

const mysqlRoutinesSQL = `
	SELECT ROUTINE_SCHEMA, ROUTINE_NAME, COALESCE(DTD_IDENTIFIER, 'void'),
		'sql', COALESCE(ROUTINE_DEFINITION, ''), ROUTINE_TYPE = 'PROCEDURE', ''
	FROM information_schema.ROUTINES
	WHERE ROUTINE_SCHEMA = ?
	ORDER BY ROUTINE_NAME
`

const mysqlTriggersSQL = `
	SELECT EVENT_OBJECT_TABLE, TRIGGER_NAME,
		EVENT_MANIPULATION, ACTION_TIMING, ACTION_STATEMENT, ''
	FROM information_schema.TRIGGERS
	WHERE TRIGGER_SCHEMA = ?
	ORDER BY EVENT_OBJECT_TABLE, TRIGGER_NAME
`

// mysqlGroupConcatPin lifts the server-side GROUP_CONCAT cap for this
// session only: the default group_concat_max_len=1024 silently truncates
// the Definition text of wide composite indexes (adversarial P3, hardened
// per the leader c-fix ticket). 1 MiB covers any realistic index shape.
const mysqlGroupConcatPin = `SET SESSION group_concat_max_len = 1048576`

func (s *mysqlScanner) ScanAll(ctx context.Context) (*ScanResult, error) {
	if rows, err := s.db.QueryContext(ctx, mysqlGroupConcatPin); err == nil {
		rows.Close()
	} else {
		return nil, wrapScanErr("pin group_concat_max_len", err)
	}
	result := &ScanResult{}

	tables, err := s.queryTables(ctx)
	if err != nil {
		return nil, wrapScanErr("scan tables", err)
	}
	result.Tables = tables

	columns, err := s.queryColumns(ctx)
	if err != nil {
		return nil, wrapScanErr("scan columns", err)
	}
	result.Columns = columns

	indexes, err := s.queryIndexes(ctx)
	if err != nil {
		return nil, wrapScanErr("scan indexes", err)
	}
	result.Indexes = indexes

	views, err := s.queryViews(ctx)
	if err != nil {
		return nil, wrapScanErr("scan views", err)
	}
	result.Views = views

	functions, err := s.queryFunctions(ctx)
	if err != nil {
		return nil, wrapScanErr("scan functions", err)
	}
	result.Functions = functions

	triggers, err := s.queryTriggers(ctx)
	if err != nil {
		return nil, wrapScanErr("scan triggers", err)
	}
	result.Triggers = triggers

	// enums/extensions/sequences: degraded empty sets per the dialect map
	// above - MySQL has no schema-level analogs and the checkers score an
	// empty dimension as fully compatible by construction.
	result.Enums = nil
	result.Extensions = nil
	result.Sequences = nil

	return result, nil
}

func wrapScanErr(stage string, err error) error {
	return &scanError{stage: stage, err: err}
}

type scanError struct {
	stage string
	err   error
}

func (e *scanError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *scanError) Unwrap() error { return e.err }

func (s *mysqlScanner) queryTables(ctx context.Context) ([]TableInfo, error) {
	rows, err := s.db.QueryContext(ctx, mysqlTablesSQL, s.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []TableInfo
	for rows.Next() {
		var t TableInfo
		if err := rows.Scan(&t.Schema, &t.Name); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

func (s *mysqlScanner) queryColumns(ctx context.Context) ([]ColumnInfo, error) {
	rows, err := s.db.QueryContext(ctx, mysqlColumnsSQL, s.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []ColumnInfo
	for rows.Next() {
		var c ColumnInfo
		if err := rows.Scan(&c.TableSchema, &c.TableName, &c.ColumnName,
			&c.DataType, &c.MaxLength, &c.NumericPrec, &c.NumericScale,
			&c.IsNullable, &c.ColumnDefault, &c.IsPrimary, &c.OrdinalPosition); err != nil {
			return nil, err
		}
		columns = append(columns, c)
	}
	return columns, rows.Err()
}

func (s *mysqlScanner) queryIndexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := s.db.QueryContext(ctx, mysqlIndexesSQL, s.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var indexes []IndexInfo
	for rows.Next() {
		var idx IndexInfo
		var partial, expression int
		if err := rows.Scan(&idx.TableName, &idx.Name, &idx.IndexType,
			&idx.IsUnique, &idx.IsPrimary, &idx.Definition,
			&partial, &expression); err != nil {
			return nil, err
		}
		idx.IsPartial = partial != 0
		idx.IsExpression = expression != 0
		idx.DDL = idx.Definition
		indexes = append(indexes, idx)
	}
	return indexes, rows.Err()
}

func (s *mysqlScanner) queryViews(ctx context.Context) ([]ViewInfo, error) {
	rows, err := s.db.QueryContext(ctx, mysqlViewsSQL, s.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var views []ViewInfo
	for rows.Next() {
		var v ViewInfo
		if err := rows.Scan(&v.Schema, &v.Name, &v.Definition, &v.DDL); err != nil {
			return nil, err
		}
		v.Definition = strings.TrimSpace(v.Definition)
		views = append(views, v)
	}
	return views, rows.Err()
}

func (s *mysqlScanner) queryFunctions(ctx context.Context) ([]FunctionInfo, error) {
	rows, err := s.db.QueryContext(ctx, mysqlRoutinesSQL, s.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var functions []FunctionInfo
	for rows.Next() {
		var f FunctionInfo
		if err := rows.Scan(&f.Schema, &f.Name, &f.ReturnType,
			&f.Language, &f.Source, &f.IsProcedure, &f.DDL); err != nil {
			return nil, err
		}
		functions = append(functions, f)
	}
	return functions, rows.Err()
}

func (s *mysqlScanner) queryTriggers(ctx context.Context) ([]TriggerInfo, error) {
	rows, err := s.db.QueryContext(ctx, mysqlTriggersSQL, s.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []TriggerInfo
	for rows.Next() {
		var t TriggerInfo
		if err := rows.Scan(&t.TableName, &t.Name, &t.EventType,
			&t.Timing, &t.Statement, &t.DDL); err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}
