package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ReadTable streams rows from a PostgreSQL table as CIR Row values. It uses a
// standard SELECT cursor (source-agnostic — the same pattern works for MySQL,
// Oracle, …) rather than the PG-specific COPY→CSV→Lightning path, which stays
// as a PG optimization for the orchestrator to choose (#t64 P1 step 2c).
//
// TypedValue.TiDBType comes from the passed Table's Columns (produced by
// ReadSchema via the same TypeMapper) → consistent with the schema, so the
// target renders correctly.
func (r dataReader) ReadTable(ctx context.Context, t source.Table, chunk source.ChunkSpec) (source.RowIterator, error) {
	db := r.src.DB()
	if db == nil {
		return nil, fmt.Errorf("postgres DataReader: not connected (call Connect first)")
	}

	d := r.src.Dialect()
	q := "SELECT * FROM " + d.QuoteTable(t.Schema, t.Name)
	if chunk.Limit > 0 || chunk.Offset > 0 {
		q += " " + d.LimitOffsetSQL(chunk.Limit, chunk.Offset)
	}

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres DataReader: query %s: %w", q, err)
	}

	return &pgRowIterator{rows: rows, cols: t.Columns}, nil
}

// pgRowIterator wraps *sql.Rows and yields CIR Row values. The column types
// (TypedValue.TiDBType) are taken from the Table definition (from ReadSchema),
// not from the driver's runtime type discovery — so they match the schema
// exactly (same TypeMapper).
type pgRowIterator struct {
	rows    *sql.Rows
	cols    []source.Column
	current source.Row
	err     error
}

func (it *pgRowIterator) Next() bool {
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

func (it *pgRowIterator) Row() source.Row { return it.current }

func (it *pgRowIterator) Err() error { return it.err }

func (it *pgRowIterator) Close() error { return it.rows.Close() }
