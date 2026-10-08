package cdc

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"go.uber.org/zap"
)

// CheckpointManager persists and recovers LSN checkpoints for CDC resume.
type CheckpointManager struct {
	filePath string
	log      *zap.Logger

	mu         sync.Mutex
	checkpoint Checkpoint
	dirty      bool
}

// NewCheckpointManager creates a new checkpoint manager.
func NewCheckpointManager(filePath string) *CheckpointManager {
	return &CheckpointManager{
		filePath: filePath,
		log:      zap.NewNop(),
		checkpoint: Checkpoint{
			SlotName: "pg2tidb_cdc",
		},
	}
}

// SetLogger sets the logger.
func (c *CheckpointManager) SetLogger(log *zap.Logger) {
	c.log = log
}

// Load reads the checkpoint from disk. Returns nil, nil if no checkpoint exists.
func (c *CheckpointManager) Load() (*Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(c.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.log.Info("cdc checkpoint: no existing checkpoint file, starting fresh")
			return nil, nil
		}
		return nil, fmt.Errorf("cdc checkpoint: read file: %w", err)
	}

	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("cdc checkpoint: parse: %w", err)
	}

	c.checkpoint = cp
	c.dirty = false

	c.log.Info("cdc checkpoint loaded",
		zap.String("position", cp.Position()),
		zap.Time("timestamp", cp.Timestamp),
	)
	return &cp, nil
}

// pgLSNShape pins the PG checkpoint position form (MS-11d): upper-case hex
// hi/lo separated by a slash, e.g. 0/1A2B3C4.
var pgLSNShape = regexp.MustCompile(`^[0-9A-F]+/[0-9A-F]+$`)

// Source kinds accepted by LoadForSource.
const (
	SourceKindPostgres = "postgres"
	SourceKindMySQL    = "mysql"
)

// checkpointMatchesSource reports whether the loaded checkpoint belongs to
// the configured source kind: PG = bare LSN in hi/lo hex form; MySQL = a
// binlog file:pos pair (File non-empty with a rotation suffix, Pos > 0).
func checkpointMatchesSource(cp *Checkpoint, kind string) bool {
	if cp == nil {
		return false
	}
	switch kind {
	case SourceKindMySQL:
		return cp.Binlog != nil && binlogPositionShapeOK(cp.Binlog)
	case SourceKindPostgres:
		return cp.Binlog == nil && cp.LSN > 0 && pgLSNShape.MatchString(cp.LSN.String())
	}
	return false
}

// binlogPositionShapeOK: the file name carries a numeric rotation suffix
// ("binlog.000005") and the position is positive.
func binlogPositionShapeOK(bp *BinlogPosition) bool {
	if bp == nil || bp.File == "" || bp.Pos <= 0 {
		return false
	}
	dot := strings.LastIndex(bp.File, ".")
	if dot <= 0 || dot == len(bp.File)-1 {
		return false
	}
	suffix := bp.File[dot+1:]
	_, err := strconv.Atoi(suffix)
	return err == nil
}

// LoadForSource loads the checkpoint but REFUSES one written by the other
// source kind (MS-11d root fix): a MySQL chain must never resume from a PG
// LSN and vice versa — the positions are not comparable. On mismatch the
// stale file is discarded (removed) and a nil checkpoint returned so the
// runner starts from the current position; the discard is logged, never
// fatal (a cross-source switch legitimately has no resumable position).
func (c *CheckpointManager) LoadForSource(kind string) (*Checkpoint, error) {
	cp, err := c.Load()
	if err != nil || cp == nil {
		return cp, err
	}
	if checkpointMatchesSource(cp, kind) {
		return cp, nil
	}
	c.log.Info("cdc checkpoint: 旧源 checkpoint 已弃用（source type changed），从当前位点起步",
		zap.String("position", cp.Position()),
		zap.String("source_kind", kind),
	)
	if rmErr := os.Remove(c.filePath); rmErr != nil && !os.IsNotExist(rmErr) {
		return nil, fmt.Errorf("cdc checkpoint: discard stale file: %w", rmErr)
	}
	c.Reset()
	return nil, nil
}

// Save writes the current checkpoint to disk.
func (c *CheckpointManager) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cp := c.checkpoint
	cp.Timestamp = time.Now()

	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("cdc checkpoint: marshal: %w", err)
	}

	if err := os.WriteFile(c.filePath, data, 0644); err != nil {
		return fmt.Errorf("cdc checkpoint: write: %w", err)
	}

	c.dirty = false
	c.log.Debug("cdc checkpoint saved",
		zap.String("position", cp.Position()),
	)
	return nil
}

// Update records a new LSN position. Call this after successfully applying a batch.
// The checkpoint is marked dirty; call Save() to persist.
func (c *CheckpointManager) Update(lsn pglogrepl.LSN) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.checkpoint.LSN = lsn
	c.checkpoint.Timestamp = time.Now()
	c.dirty = true
}

// GetLSN returns the current checkpoint LSN.
func (c *CheckpointManager) GetLSN() pglogrepl.LSN {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkpoint.LSN
}

// UpdateBinlog records a new MySQL binlog position (MS-11 pen 1, dual-source
// shape; the PG Update(lsn) path is untouched). Call after a successfully
// applied batch; Save() persists.
func (c *CheckpointManager) UpdateBinlog(bp BinlogPosition) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.checkpoint.Binlog = &bp
	c.checkpoint.Timestamp = time.Now()
	c.dirty = true
}

// GetBinlog returns a copy of the current MySQL binlog position, nil when the
// checkpoint is a PG one (or fresh).
func (c *CheckpointManager) GetBinlog() *BinlogPosition {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checkpoint.Binlog == nil {
		return nil
	}
	bp := *c.checkpoint.Binlog
	return &bp
}

// GetCheckpoint returns a copy of the current checkpoint.
func (c *CheckpointManager) GetCheckpoint() Checkpoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkpoint
}

// Position renders the source-neutral checkpoint marker (PG LSN text /
// MySQL binlog file:pos) — MS-11 pen 1.
func (c *CheckpointManager) Position() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkpoint.Position()
}

// IsDirty returns true if there are unpersisted LSN updates.
func (c *CheckpointManager) IsDirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirty
}

// Reset clears the checkpoint (for fresh start).
func (c *CheckpointManager) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkpoint = Checkpoint{
		SlotName: c.checkpoint.SlotName,
	}
	c.dirty = true
}

// SetSlotName sets the replication slot name in the checkpoint.
func (c *CheckpointManager) SetSlotName(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkpoint.SlotName = name
}

// GetLastDDLID returns the last applied DDL log id (DDL replication resume, #t59).
func (c *CheckpointManager) GetLastDDLID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkpoint.LastDDLID
}

// SetLastDDLID records the last applied DDL log id and marks the checkpoint dirty.
func (c *CheckpointManager) SetLastDDLID(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkpoint.LastDDLID = id
	c.dirty = true
}
