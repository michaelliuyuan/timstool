package postgres

import (
	"context"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// pgIncrementalCapture is the PG adapter's CDC capability marker + WAL position
// provider. The full CDC pipeline (logical replication → transform → apply +
// DDL replication + Web monitoring) is kept intact in internal/cdc.Runner and
// managed by the orchestrator (which has the target config); this Source-level
// abstraction captures the source WAL position and signals CDC availability.
// Constraint §10.3: CDC kept intact, not split (#t64 P1 step 2d).
type pgIncrementalCapture struct {
	src *Source
}

// SnapshotPosition returns the current PostgreSQL WAL LSN — the position to
// start CDC from after a full-migration snapshot.
func (c *pgIncrementalCapture) SnapshotPosition(ctx context.Context) (source.Position, error) {
	db := c.src.DB()
	if db == nil {
		return "", fmt.Errorf("postgres IncrementalCapture: not connected (call Connect first)")
	}
	var lsn string
	if err := db.QueryRowContext(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn); err != nil {
		return "", fmt.Errorf("postgres IncrementalCapture: snapshot position: %w", err)
	}
	return source.Position(lsn), nil
}

// Start is not implemented at the Source level — the full CDC pipeline
// (replication → transform → apply → DDL replication → Web monitoring) needs
// the TARGET config and is managed by the orchestrator via the existing
// internal/cdc.Runner (P1 step 2e). Use SnapshotPosition for the checkpoint,
// then the orchestrator starts the Runner.
func (c *pgIncrementalCapture) Start(ctx context.Context, from source.Position) (<-chan source.ChangeEvent, error) {
	return nil, fmt.Errorf("postgres IncrementalCapture.Start: the full CDC pipeline (cdc.Runner) is managed by the orchestrator (2e) which has the target config; use SnapshotPosition for the WAL checkpoint")
}
