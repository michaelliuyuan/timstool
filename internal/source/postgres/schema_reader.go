package postgres

import (
	"context"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/schema"
	"github.com/michaelliuyuan/timstool/internal/source"
)

// ReadSchema reads the PostgreSQL schema via the existing, validated
// internal/schema.Collector and converts it to CIR. Columns' TiDBType is
// populated via TypeMapper (MapTypeWithPrecision) so the target never needs to
// know the source. No existing schema-package code is modified (#t64 P1 step 2b).
func (r schemaReader) ReadSchema(ctx context.Context, opts source.Filter) (*source.Schema, error) {
	db := r.src.DB()
	if db == nil {
		return nil, fmt.Errorf("postgres SchemaReader: not connected (call Connect first)")
	}

	schemaName := r.src.Config().Schema
	if schemaName == "" {
		schemaName = "public"
	}

	collector := schema.NewCollector(db)
	pgTables, err := collector.CollectTables(ctx, schemaName, opts.ExcludeTables)
	if err != nil {
		return nil, fmt.Errorf("postgres SchemaReader: collect tables: %w", err)
	}

	// Include filter: if opts.Tables is non-empty, only keep matching tables
	// (matched by "schema.table" or bare "table").
	includeSet := make(map[string]bool, len(opts.Tables))
	for _, t := range opts.Tables {
		includeSet[t] = true
	}

	tm := r.src.TypeMapper()
	cir := &source.Schema{Catalog: r.src.Config().Database}

	for _, t := range pgTables {
		fullName := t.Schema + "." + t.Name
		if len(includeSet) > 0 && !includeSet[fullName] && !includeSet[t.Name] {
			continue
		}

		columns := make([]source.Column, 0, len(t.Columns))
		var pk []string
		for _, col := range t.Columns {
			mapped := tm.MapType(string(col.PGType), col.NumericPrec, col.NumericScale)
			columns = append(columns, source.Column{
				Name:       col.ColumnName,
				SourceType: col.DataType,
				TiDBType:   mapped.Name,
				Nullable:   col.IsNullable,
				Default:    col.DefaultValue,
				IsAutoIncr: col.IsAutoIncr,
				Comment:    col.Comment,
			})
			if col.IsPrimaryKey {
				pk = append(pk, col.ColumnName)
			}
		}

		indexes := make([]source.Index, 0, len(t.Indexes))
		for _, idx := range t.Indexes {
			indexes = append(indexes, source.Index{
				Name:    idx.IndexName,
				Columns: idx.Columns,
				Unique:  idx.IsUnique,
			})
		}

		cir.Tables = append(cir.Tables, source.Table{
			Schema:  t.Schema,
			Name:    t.Name,
			Columns: columns,
			PK:      pk,
			Indexes: indexes,
		})
	}

	// Views are optional; don't fail the whole read if view collection errors.
	if pgViews, verr := collector.CollectViews(ctx, schemaName); verr == nil {
		for _, v := range pgViews {
			cir.Views = append(cir.Views, source.View{
				Schema:     v.Schema,
				Name:       v.Name,
				Definition: v.Definition,
			})
		}
	}

	return cir, nil
}
