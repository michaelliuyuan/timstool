package target

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/source"
	lightningpkg "github.com/michaelliuyuan/timstool/internal/lightning"
	"go.uber.org/zap"
)

// LoadData streams each CIR table's rows (via the source adapter's DataReader)
// into TAB-separated CSV files — one per table, named {db}.{table}.csv for
// Lightning's file router — then invokes tidb-lightning to import them into the
// TiDB target. Source-agnostic: only CIR + Lightning. (#t81 Step 2.)
//
// It mirrors the existing PG path's Lightning invocation (same [mydumper.csv]
// TSV contract, local backend, target config) but is self-contained so the PG
// path is untouched (zero-regression).
func LoadData(ctx context.Context, src source.Source, schema *source.Schema, target config.TargetConfig, tempDir string, onTable func(string, int64)) error {
	log := zap.L()
	targetDB := target.Database
	if targetDB == "" {
		targetDB = "test"
	}

	// 1. Export each table to {db}.{table}.csv (TSV).
	for _, t := range schema.Tables {
		rows, err := exportTableCSV(ctx, src, t, targetDB, tempDir)
		if err != nil {
			return fmt.Errorf("LoadData: export table %q: %w", t.Name, err)
		}
		log.Info("LoadData: exported table", zap.String("table", t.Name), zap.Int64("rows", rows))
		if onTable != nil {
			onTable(t.Name, rows)
		}
	}

	// 2. Lightning import.
	bin := lightningpkg.FindBinary(tempDir)
	if bin == "" {
		return fmt.Errorf("LoadData: tidb-lightning binary not found (install it or build with embedded binary)")
	}
	return runLightning(ctx, bin, tempDir, target)
}

// exportTableCSV streams one table's rows to {targetDB}.{table}.csv (TSV via
// RenderCSVRow). Full-table read (no chunking) — fine for the data volumes in
// scope; chunking is a future perf optimization.
func exportTableCSV(ctx context.Context, src source.Source, t source.Table, targetDB, tempDir string) (int64, error) {
	fileName := fmt.Sprintf("%s.%s.csv", targetDB, t.Name)
	f, err := os.Create(filepath.Join(tempDir, fileName))
	if err != nil {
		return 0, fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()

	iter, err := src.DataReader().ReadTable(ctx, t, source.ChunkSpec{})
	if err != nil {
		return 0, fmt.Errorf("read table: %w", err)
	}
	defer iter.Close()

	bw := bufio.NewWriterSize(f, 256*1024)
	var rows int64
	for iter.Next() {
		if _, err := bw.WriteString(RenderCSVRow(t.Columns, iter.Row())); err != nil {
			return 0, fmt.Errorf("write row: %w", err)
		}
		rows++
	}
	if err := iter.Err(); err != nil {
		return 0, fmt.Errorf("iterate: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return 0, fmt.Errorf("flush: %w", err)
	}
	return rows, nil
}

// RunLightningImport generates the Lightning TOML config and invokes
// tidb-lightning to import CSV files from tempDir into the TiDB target.
// Exported so the orchestrator can call it directly when CSV files are
// produced by an external tool (e.g., dumpling) rather than by exportTableCSV.
func RunLightningImport(ctx context.Context, tempDir string, target config.TargetConfig) error {
	bin := lightningpkg.FindBinary(tempDir)
	if bin == "" {
		return fmt.Errorf("tidb-lightning binary not found")
	}
	return runLightning(ctx, bin, tempDir, target)
}

// runLightning generates the Lightning TOML config (mirroring the PG path's
// [mydumper.csv] TSV contract + local backend + target) and invokes the
// tidb-lightning binary, returning an error with the output tail on failure.
func runLightning(ctx context.Context, bin, tempDir string, target config.TargetConfig) error {
	log := zap.L()
	absDir, err := filepath.Abs(tempDir)
	if err != nil {
		return fmt.Errorf("abs temp dir: %w", err)
	}
	sortedKVDir := filepath.Join(absDir, ".sorted-kv")
	if err := os.MkdirAll(sortedKVDir, 0o755); err != nil {
		return fmt.Errorf("create sorted-kv dir: %w", err)
	}
	// Clean stale Lightning checkpoints before import. A checkpoint left by a
	// prior (failed) run makes the next run fail with "illegal checkpoints" —
	// it records progress against data that no longer exists, which is
	// especially likely under the drop strategy (the target table is wiped but
	// the checkpoint still claims rows were imported). Cover every plausible
	// location: the configured sorted-kv-dir, the data dir, and the system temp
	// dirs (Lightning has been observed writing to /tmp when sorted-kv-dir was
	// absent or unwritable in older runs).
	cleanStaleLightningCheckpoints(absDir, sortedKVDir)

	pdAddr := target.PDAddr
	if pdAddr == "" {
		pdAddr = fmt.Sprintf("%s:2379", target.Host)
	}
	statusPort := target.StatusPort
	if statusPort == 0 {
		statusPort = 10080
	}

	configPath := filepath.Join(absDir, "lightning.toml")
	if err := os.WriteFile(configPath, []byte(buildLightningConfig(absDir, sortedKVDir, target, pdAddr, statusPort)), 0o644); err != nil {
		return fmt.Errorf("write lightning config: %w", err)
	}
	defer os.Remove(configPath)

	log.Info("LoadData: tidb-lightning import starting",
		zap.String("dir", absDir),
		zap.String("tidb_host", target.Host),
		zap.Int("tidb_port", target.Port))

	cmd := exec.CommandContext(ctx, bin, "--config", configPath, "--log-file=-")
	cmd.Dir = absDir
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "TERM=dumb")
	out, err := cmd.CombinedOutput()
	tail := strings.TrimSpace(string(out))
	if len(tail) > 4000 {
		tail = tail[len(tail)-4000:]
	}
	if err != nil {
		return fmt.Errorf("tidb-lightning failed: %w\n--- output tail ---\n%s", err, tail)
	}
	log.Info("LoadData: tidb-lightning import completed", zap.Int("output_bytes", len(out)))
	return nil
}

// buildLightningConfig renders the TiDB-Lightning TOML config. The [mydumper.csv]
// section mirrors the PG path exactly (separator="\t", backslash-escape,
// null=\N, no header) so the same format reads CIR-exported TSV.
func buildLightningConfig(absDir, sortedKVDir string, target config.TargetConfig, pdAddr string, statusPort int) string {
	return fmt.Sprintf(`[lightning]
level = "info"
check-requirements = false

[mydumper]
data-source-dir = "%s"
no-schema = true

[mydumper.csv]
separator = "\t"
delimiter = ""
header = false
not-null = false
null = "\\N"
backslash-escape = true
trim-last-separator = false

[tikv-importer]
backend = "local"
sorted-kv-dir = "%s"

[tidb]
host = "%s"
port = %d
user = "%s"
password = "%s"
status-port = %d
pd-addr = "%s"

[post-restore]
checksum = "optional"
analyze = "off"
`,
		toSlash(absDir), toSlash(sortedKVDir),
		target.Host, target.Port, target.User, target.Password,
		statusPort, pdAddr,
	)
}

func toSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// lightningCheckpointFile is the file-based checkpoint Lightning writes for the
// local backend. A stale copy from a prior failed run triggers
// "illegal checkpoints" on the next import.
const lightningCheckpointFile = "tidb_lightning_checkpoint.pb"

// cleanStaleLightningCheckpoints removes any leftover Lightning checkpoint from
// every plausible location. os.Remove ignores not-exist errors, so this is safe
// and cheap to run unconditionally before each import.
func cleanStaleLightningCheckpoints(absDir, sortedKVDir string) {
	log := zap.L()
	dirs := []string{sortedKVDir, absDir, os.TempDir(), "/tmp"}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, lightningCheckpointFile)
		if err := os.Remove(path); err == nil {
			log.Info("LoadData: removed stale lightning checkpoint", zap.String("path", path))
		}
	}
}
