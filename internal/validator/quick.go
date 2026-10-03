package validator

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"go.uber.org/zap"
)

// validateQuick performs fast row count estimation using the source and
// target dialect estimate queries (pg_stat_user_tables for PG, SHOW TABLE
// STATUS for TiDB), avoiding full table scans. The estimate->exact-COUNT
// fallback DECISION lives here in the main flow (MS-03 dialect ruling): a
// dialect only supplies the query shapes; the trigger (estimate query error)
// and call order are byte-identical to the pre-relocation code.
func (v *Validator) validateQuick(ctx context.Context, pgDB *sql.DB, tidbConn *sql.Conn, table string) reporter.TableReport {
	tr := reporter.TableReport{TableName: table, Status: reporter.StatusPass}
	logger := zap.L()

	schema := v.cfg.Source.Schema
	if schema == "" {
		schema = "public"
	}

	// PG: use pg_stat_user_tables for fast row count estimation
	pgCount, err := v.srcDialect.EstimateRows(ctx, pgDB, schema, table)
	if err != nil {
		logger.Warn("quick mode: pg_stat estimate failed, falling back to COUNT(*)",
			zap.String("table", table), zap.Error(err))
		// Fallback to exact COUNT(*)
		pgCount, err = v.srcDialect.CountExact(ctx, pgDB, schema, table)
		if err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("quick: PG count: %v", err)
			return tr
		}
	}

	// TiDB: use SHOW TABLE STATUS for fast row count estimation
	tidbCount, err := v.tgtDialect.EstimateRows(ctx, tidbConn, schema, table)
	if err != nil {
		logger.Warn("quick mode: SHOW TABLE STATUS failed, falling back to COUNT(*)",
			zap.String("table", table), zap.Error(err))
		// Fallback to exact COUNT(*)
		tidbCount, err = v.tgtDialect.CountExact(ctx, tidbConn, schema, table)
		if err != nil {
			tr.Status = reporter.StatusFail
			tr.Error = fmt.Sprintf("quick: TiDB count: %v", err)
			return tr
		}
	}

	sourceCount := pgCount
	targetCount := tidbCount
	tr.SourceRows = sourceCount
	tr.TargetRows = targetCount
	tr.DiffRows = sourceCount - targetCount

	if tr.DiffRows != 0 {
		tr.Status = reporter.StatusFail
		tr.Error = fmt.Sprintf("row count mismatch (estimated): source=%d target=%d diff=%d", sourceCount, targetCount, tr.DiffRows)
	}

	tr.Suggestion = "quick mode: row count estimation (no full scan)"
	return tr
}
