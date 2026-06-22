// Package dumpling provides the tidb-dumpling binary discovery and invocation
// for MySQL data export (#t83). Dumpling is PingCAP's official MySQL-protocol
// data export tool — concurrent sharding + consistency snapshot, the MySQL→TiDB
// equivalent of PostgreSQL COPY. It produces CSV files that feed the existing
// LoadData/lightning import path.
package dumpling

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"go.uber.org/zap"
)

// FindBinary locates the tidb-dumpling binary: configured path → PATH → common
// deployment locations. Returns "" if not found (caller should fall back to
// stream mode + warn).
func FindBinary(configured string) string {
	// 1. Configured path.
	if configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return configured
		}
	}
	// 2. PATH.
	for _, name := range []string{"tidb-dumpling", "dumpling"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	// 3. Common deployment locations.
	for _, p := range []string{"/home/tidb/dumpling", "/usr/local/bin/tidb-dumpling"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// DumpConfig controls the dumpling subprocess invocation.
type DumpConfig struct {
	Binary      string // path to tidb-dumpling
	Host        string
	Port        int
	User        string
	Password    string
	Database    string
	TablesList  string // comma-separated table names (empty = all tables in database)
	OutputDir   string // where CSV files are written
	Threads     int    // concurrent sharding threads (0 = dumpling default)
	Consistency string // snapshot | lock | none (empty = snapshot)
	FileType    string // csv | sql (empty = csv)
}

// Dump invokes tidb-dumpling to export MySQL data to CSV files in OutputDir.
// The CSV files are named {db}.{table}.{shard}.csv by dumpling's file router,
// which matches Lightning's expected naming. Returns an error with the output
// tail on failure.
func Dump(ctx context.Context, cfg DumpConfig) error {
	log := zap.L()

	args := []string{
		"--host=" + cfg.Host,
		fmt.Sprintf("--port=%d", cfg.Port),
		"--user=" + cfg.User,
		"--password=" + cfg.Password,
		"--output=" + cfg.OutputDir,
		"--filetype=" + firstNonEmpty(cfg.FileType, "csv"),
		"--no-header", // lightning expects no header (header=false in config)
		"--separator", "\\t",
		"--consistency=" + firstNonEmpty(cfg.Consistency, "lock"),
	}
	if cfg.Database != "" {
		args = append(args, "--database="+cfg.Database)
	}
	if cfg.TablesList != "" {
		args = append(args, "--tables-list="+cfg.TablesList)
	}
	if cfg.Threads > 0 {
		args = append(args, fmt.Sprintf("--threads=%d", cfg.Threads))
	}

	log.Info("dumpling export starting",
		zap.String("binary", cfg.Binary),
		zap.String("host", cfg.Host),
		zap.Int("port", cfg.Port),
		zap.String("database", cfg.Database),
		zap.String("output", cfg.OutputDir))

	cmd := exec.CommandContext(ctx, cfg.Binary, args...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "TERM=dumb")
	out, err := cmd.CombinedOutput()
	tail := strings.TrimSpace(string(out))
	if len(tail) > 4000 {
		tail = tail[len(tail)-4000:]
	}
	if err != nil {
		return fmt.Errorf("tidb-dumpling failed: %w\n--- output tail ---\n%s", err, tail)
	}
	log.Info("dumpling export completed", zap.Int("output_bytes", len(out)))
	return nil
}

// DumpFromConfig is a convenience that builds DumpConfig from a config.SourceConfig.
func DumpFromConfig(src config.SourceConfig, outputDir, binaryPath string, tables []string) DumpConfig {
	return DumpConfig{
		Binary:     binaryPath,
		Host:       src.Host,
		Port:       src.Port,
		User:       src.User,
		Password:   src.Password,
		Database:   src.Database,
		TablesList: strings.Join(tables, ","),
		OutputDir:  outputDir,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
