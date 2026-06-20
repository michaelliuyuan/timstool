package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ReadTable streams rows from a MySQL table as CIR Row values using standard
// SELECT * with LIMIT/OFFSET pagination.
func (r *dataReader) ReadTable(ctx context.Context, t source.Table, chunk source.ChunkSpec) (source.RowIterator, error) {
	db := r.src.DB()
	if db == nil {
		return nil, fmt.Errorf("mysql DataReader: not connected (call Connect first)")
	}

	d := r.src.Dialect()
	q := "SELECT * FROM " + d.QuoteTable(t.Schema, t.Name)
	if chunk.Limit > 0 || chunk.Offset > 0 {
		q += " " + d.LimitOffsetSQL(chunk.Limit, chunk.Offset)
	}

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("mysql DataReader: query %s: %w", q, err)
	}

	return &mysqlRowIterator{rows: rows, cols: t.Columns}, nil
}

// mysqlRowIterator wraps *sql.Rows and yields CIR Row values. The column types
// (TypedValue.TiDBType) are taken from the Table definition (from ReadSchema),
// not from the driver's runtime type discovery.
type mysqlRowIterator struct {
	rows    *sql.Rows
	cols    []source.Column
	current source.Row
	err     error
}

func (it *mysqlRowIterator) Next() bool {
	if !it.rows.Next() {
		it.err = it.rows.Err()
		return false
	}
	values := make([]interface{}, len(it.cols))
	ptrs := make([]interface{}, len(it.cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := it.rows.Scan(ptrs...); err != nil {
		it.err = fmt.Errorf("scan row: %w", err)
		return false
	}
	row := make(source.Row, len(it.cols))
	for i, col := range it.cols {
		row[col.Name] = source.TypedValue{TiDBType: col.TiDBType, Val: values[i]}
	}
	it.current = row
	return true
}

func (it *mysqlRowIterator) Row() source.Row { return it.current }

func (it *mysqlRowIterator) Err() error { return it.err }

func (it *mysqlRowIterator) Close() error { return it.rows.Close() }

// dataReader is the MySQL DataReader implementation.
type dataReader struct{ src *Source }
