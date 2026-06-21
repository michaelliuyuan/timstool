// Package target is the source-agnostic TiDB execution engine (#t81): it
// consumes the CIR (Common Intermediate Representation) produced by the source
// adapters and applies it to TiDB — CREATE TABLE from a CIR Schema (ApplyDDL),
// later LoadData from CIR rows. The target never knows whether the source was
// PostgreSQL, MySQL, … ; it only sees CIR (doc multi-source-execution-engine-
// design §2/§4).
package target

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ApplyDDL creates the CIR schema's tables on the TiDB target. Uses CREATE TABLE
// IF NOT EXISTS (idempotent / resume-safe). Columns render by their pre-mapped
// TiDBType (the adapter's TypeMapper populated it during ReadSchema), so the
// target is source-agnostic.
func ApplyDDL(ctx context.Context, db *sql.DB, schema *source.Schema) error {
	if db == nil {
		return fmt.Errorf("target.ApplyDDL: nil db")
	}
	for _, t := range schema.Tables {
		ddl := RenderCreateTable(t)
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("target.ApplyDDL: create table %q: %w (ddl=%s)", t.Name, err, ddl)
		}
	}
	return nil
}

// RenderCreateTable renders a TiDB/MySQL CREATE TABLE IF NOT EXISTS from a CIR
// Table. Pure function (unit-testable). Column order, PK, and indexes follow the
// CIR definition.
func RenderCreateTable(t source.Table) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE IF NOT EXISTS ")
	b.WriteString(QuoteIdent(t.Name))
	b.WriteString(" (")
	for i, c := range t.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		renderColumn(&b, c)
	}
	if len(t.PK) > 0 {
		b.WriteString(", PRIMARY KEY (")
		writeIdentList(&b, t.PK)
		b.WriteString(")")
	}
	for _, idx := range t.Indexes {
		b.WriteString(", ")
		if idx.Unique {
			b.WriteString("UNIQUE ")
		}
		b.WriteString("INDEX ")
		b.WriteString(QuoteIdent(idx.Name))
		b.WriteString(" (")
		writeIdentList(&b, idx.Columns)
		b.WriteString(")")
	}
	b.WriteString(")")
	return b.String()
}

func renderColumn(b *strings.Builder, c source.Column) {
	b.WriteString(QuoteIdent(c.Name))
	b.WriteByte(' ')
	b.WriteString(c.TiDBType)
	if !c.Nullable {
		b.WriteString(" NOT NULL")
	}
	if c.IsAutoIncr {
		b.WriteString(" AUTO_INCREMENT")
	}
	if c.Default != "" {
		// Default is a source expression; source→TiDB translation is the
		// TypeMapper's concern (design §4). Rendered raw — CURRENT_TIMESTAMP /
		// numeric defaults are directly valid for MySQL-family sources.
		b.WriteString(" DEFAULT ")
		b.WriteString(c.Default)
	}
	if c.Comment != "" {
		b.WriteString(" COMMENT '")
		b.WriteString(strings.ReplaceAll(c.Comment, "'", "''"))
		b.WriteByte('\'')
	}
}

func writeIdentList(b *strings.Builder, names []string) {
	for i, n := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(QuoteIdent(n))
	}
}

// QuoteIdent quotes a TiDB/MySQL identifier with backticks (doubled internally).
func QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
