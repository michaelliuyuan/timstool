package cdc

// binlog_runner.go — MS-11 pen 3: the MySQL CDC runner. Mirrors the PG
// Runner's loop (source stream → applier → checkpoint ticker → status file)
// but drives a binlogStreamer and advances the checkpoint via
// UpdateBinlog. The PG Runner is untouched.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

// BinlogRunnerConfig combines the MySQL CDC sub-configs.
type BinlogRunnerConfig struct {
	Source         BinlogSourceConfig
	Batch          BatchConfig
	Transformer    TransformerConfig
	Filter         *TableFilter
	TargetDSN      string
	CheckpointFile string
	StatusFile     string
}

// BinlogRunner orchestrates the MySQL CDC pipeline: binlog stream →
// transform → apply. Reuses Applier/Transformer/CheckpointManager verbatim.
type BinlogRunner struct {
	cfg        BinlogRunnerConfig
	targetDSN  string
	statusFile string
	startTime  time.Time

	streamer   binlogStreamer
	applier    *Applier
	checkpoint *CheckpointManager

	eventsReceived atomic.Int64

	log *zap.Logger
}

// NewBinlogRunner creates the MySQL CDC runner.
func NewBinlogRunner(cfg BinlogRunnerConfig) (*BinlogRunner, error) {
	if cfg.Source.ServerID == 0 {
		return nil, fmt.Errorf("binlog runner: ServerID must be non-zero")
	}
	if cfg.CheckpointFile == "" {
		cfg.CheckpointFile = ".cdc_checkpoint.json"
	}
	if cfg.Filter == nil {
		cfg.Filter = NewTableFilter()
	}
	return &BinlogRunner{
		cfg:        cfg,
		targetDSN:  cfg.TargetDSN,
		statusFile: cfg.StatusFile,
		checkpoint: NewCheckpointManager(cfg.CheckpointFile),
		log:        zap.NewNop(),
	}, nil
}

// SetLogger sets the logger.
func (r *BinlogRunner) SetLogger(log *zap.Logger) {
	r.log = log
	r.checkpoint.SetLogger(log)
}

// Run executes the full MySQL CDC pipeline. On halt the checkpoint is saved
// at the last-good (one-behind) position, mirroring the PG Runner's
// at-least-once contract.
func (r *BinlogRunner) Run(ctx context.Context) error {
	r.log.Info("binlog runner: starting")
	r.startTime = time.Now()

	cp, err := r.checkpoint.Load()
	if err != nil {
		return fmt.Errorf("binlog runner: load checkpoint: %w", err)
	}
	var from *BinlogPosition
	if cp != nil && cp.Binlog != nil {
		from = cp.Binlog
		r.log.Info("binlog runner: resuming from checkpoint", zap.String("position", from.String()))
	}

	r.streamer, err = newBinlogSource(r.cfg.Source)
	if err != nil {
		return fmt.Errorf("binlog runner: create source: %w", err)
	}
	if cs, ok := r.streamer.(interface{ SetLogger(*zap.Logger) }); ok {
		cs.SetLogger(r.log)
	}

	targetDB, err := sql.Open("mysql", r.targetDSN)
	if err != nil {
		return fmt.Errorf("binlog runner: connect to target: %w", err)
	}
	defer targetDB.Close()
	if err := targetDB.PingContext(ctx); err != nil {
		return fmt.Errorf("binlog runner: ping target: %w", err)
	}
	r.log.Info("binlog runner: connected to TiDB target")

	transformer := NewTransformer(r.cfg.Transformer)
	transformer.SetLogger(r.log)
	r.applier = NewApplier(targetDB, r.cfg.Batch, transformer)
	r.applier.SetLogger(r.log)

	events, err := r.streamer.Start(ctx, from)
	if err != nil {
		return fmt.Errorf("binlog runner: start source: %w", err)
	}

	// Source-event counter: wrap the stream so Stats/writeStatus see the
	// receive count (the PG source counts internally; the streamer contract
	// keeps it runner-side).
	// MS-11 pen 7 (blackbox finding): the goroutine must capture the ORIGINAL
	// channel VALUE, not the `events` variable — `events = counted` below
	// reassigns it, and a not-yet-scheduled goroutine ranging the variable
	// would instead range `counted` itself (self-deadlock: the stream channel
	// fills, nothing drains it, zero events delivered).
	counted := make(chan *CDCEvent, 1024)
	srcCh := events
	go func() {
		defer close(counted)
		for ev := range srcCh {
			r.eventsReceived.Add(1)
			counted <- ev
		}
	}()
	events = counted

	cpTicker := time.NewTicker(10 * time.Second)
	defer cpTicker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() { errCh <- r.applier.Start(ctx, events) }()

	r.updateCheckpoint()
	for {
		select {
		case err := <-errCh:
			if err != nil {
				r.log.Error("binlog runner: applier error", zap.Error(err))
			}
			srcErr := r.streamer.Err()
			r.updateCheckpoint()
			if saveErr := r.checkpoint.Save(); saveErr != nil {
				r.log.Error("binlog runner: final checkpoint save failed", zap.Error(saveErr))
			}
			r.writeStatus()
			_ = r.streamer.Close()
			if srcErr != nil {
				return fmt.Errorf("binlog runner: source halted on fatal: %w", srcErr)
			}
			return err

		case sig := <-sigCh:
			r.log.Info("binlog runner: received signal, shutting down", zap.String("signal", sig.String()))
			_ = r.streamer.Close()
			select {
			case <-errCh:
			case <-time.After(30 * time.Second):
				r.log.Warn("binlog runner: applier shutdown timeout")
			}
			r.updateCheckpoint()
			if saveErr := r.checkpoint.Save(); saveErr != nil {
				r.log.Error("binlog runner: final checkpoint save failed", zap.Error(saveErr))
			}
			r.writeStatus()
			return nil

		case <-cpTicker.C:
			r.updateCheckpoint()
			if r.checkpoint.IsDirty() {
				if saveErr := r.checkpoint.Save(); saveErr != nil {
					r.log.Error("binlog runner: checkpoint save failed", zap.Error(saveErr))
				}
			}
			r.writeStatus()

		case <-ctx.Done():
			_ = r.streamer.Close()
			return ctx.Err()
		}
	}
}

// updateCheckpoint advances the binlog checkpoint to the streamer's
// one-behind delivered position (never past unapplied events' boundary more
// than the applier's flush window — same at-least-once contract as PG).
func (r *BinlogRunner) updateCheckpoint() {
	if r.streamer == nil {
		return
	}
	if pos := r.streamer.CurrentPosition(); pos != nil {
		r.checkpoint.UpdateBinlog(*pos)
	}
}

// writeStatus mirrors the PG Runner's #t48 B contract; positions render as
// binlog file:pos strings through the same LSN display fields.
func (r *BinlogRunner) writeStatus() {
	if r.statusFile == "" {
		return
	}
	var stats CDCStatusStats
	if r.applier != nil {
		as := r.applier.Stats()
		stats = CDCStatusStats{
			Applied:   as.EventsApplied,
			Failed:    as.EventsFailed,
			Skipped:   as.EventsSkipped,
			Batches:   as.BatchesFlushed,
			LastError: as.LastError,
		}
	}
	stats.SourceEvents = r.eventsReceived.Load()
	if !r.startTime.IsZero() {
		stats.UptimeSeconds = time.Since(r.startTime).Seconds()
	}

	state := CDCSelfRunning
	fatal := ""
	if r.streamer != nil {
		if srcErr := r.streamer.Err(); srcErr != nil {
			state = CDCSelfHalted
			fatal = srcErr.Error()
		}
	}

	cur := ""
	if r.streamer != nil {
		if pos := r.streamer.CurrentPosition(); pos != nil {
			cur = pos.String()
		}
	}
	cp := r.checkpoint.GetCheckpoint()
	st := CDCStatusFile{
		Schema:     2, // binlog runner family (PG runner writes 1)
		Timestamp:  time.Now(),
		PID:        os.Getpid(),
		LSN:        cur,
		State:      state,
		FatalError: fatal,
		Stats:      stats,
		Checkpoint: CDCStatusCheckpoint{
			LSN:       cp.Position(),
			UpdatedAt: cp.Timestamp,
		},
	}
	if err := WriteStatusFile(r.statusFile, st); err != nil {
		r.log.Warn("binlog runner: write status file failed", zap.Error(err))
	}
}

// Stats returns a summary of the current MySQL CDC state.
func (r *BinlogRunner) Stats() map[string]interface{} {
	stats := map[string]interface{}{
		"source_events":  r.eventsReceived.Load(),
		"source_running": r.streamer != nil && r.streamer.Err() == nil,
	}
	if r.streamer != nil {
		if pos := r.streamer.CurrentPosition(); pos != nil {
			stats["source_position"] = pos.String()
		}
	}
	stats["checkpoint_position"] = r.checkpoint.Position()
	if r.applier != nil {
		appStats := r.applier.Stats()
		stats["applier_events_received"] = appStats.EventsReceived
		stats["applier_events_applied"] = appStats.EventsApplied
		stats["applier_last_lsn"] = appStats.LastLSN
	}
	return stats
}
