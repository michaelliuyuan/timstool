// Package dumpling provides the tidb-dumpling binary discovery and invocation
// for MySQL data export (#t83). Dumpling is PingCAP's official MySQL-protocol
// data export tool — concurrent sharding + consistency snapshot, the MySQL→TiDB
// equivalent of PostgreSQL COPY. It produces CSV files that feed the existing
// LoadData/lightning import path.
package dumpling

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

	args := buildArgs(cfg)

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

// buildArgs renders the tidb-dumpling CLI argument vector for cfg. Separated
// from Dump so the separator/delimiter contract with Lightning can be unit-
// tested without invoking the binary.
func buildArgs(cfg DumpConfig) []string {
	args := []string{
		"--host=" + cfg.Host,
		fmt.Sprintf("--port=%d", cfg.Port),
		"--user=" + cfg.User,
		"--password=" + cfg.Password,
		"--output=" + cfg.OutputDir,
		"--filetype=" + firstNonEmpty(cfg.FileType, "csv"),
		"--no-header", // lightning expects no header (header=false in config)
		// NOTE: "\t" is a single real TAB byte (0x09) in Go — exec.Command
		// passes it verbatim to dumpling (no shell). Do NOT write "\\t" (a
		// 2-char backslash-t literal): dumpling v7.1.9 treats that as a 2-byte
		// separator (0x5c 0x74), which mismatches Lightning's separator="\t"
		// (real TAB) and corrupts every value while row counts still pass.
		// csv-delimiter="" matches Lightning delimiter="" (no quote wrapping).
		"--csv-separator", "\t",
		"--csv-delimiter", "",
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
	return args
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

// CountExportedRows counts the data rows dumpling wrote to dir for each table,
// keyed by bare table name. It sums record lines across every matching CSV file
// ({db}.{table}.*.csv — dumpling shards large tables into .000000000000.csv
// etc.). Used so the dumpling fast-path can report real row counts to the
// progress layer: the stream path gets counts from exportTableCSV's callback,
// but dumpling writes files directly and bypasses that, so without this the UI
// shows "0 rows migrated" even though Lightning loaded everything.
//
// Each row is exactly one line: csv-separator is a real TAB and backslash-
// escape is on, so any newline inside a field is escaped (\n) and never appears
// as a raw line break in the file.
func CountExportedRows(dir, database string, tables []string) map[string]int64 {
	counts := make(map[string]int64, len(tables))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return counts
	}
	// Bare table name -> filename prefix "{db}.{table}.".
	prefix := make(map[string]string, len(tables))
	for _, t := range tables {
		prefix[t] = database + "." + t + "."
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".csv") {
			continue
		}
		for table, p := range prefix {
			if strings.HasPrefix(e.Name(), p) {
				counts[table] += countCSVRecords(filepath.Join(dir, e.Name()))
				break
			}
		}
	}
	return counts
}

// countCSVRecords returns the number of newline-terminated records in path.
// dumpling terminates every row with '\n'; if the final record lacks one (non-
// empty file not ending in '\n'), it is still counted.
func countCSVRecords(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	var count int64
	var total int64
	var last byte
	buf := make([]byte, 64*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			total += int64(n)
			last = buf[n-1]
			for i := 0; i < n; i++ {
				if buf[i] == '\n' {
					count++
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return count
		}
	}
	// Non-empty file whose last record had no trailing newline.
	if total > 0 && last != '\n' {
		count++
	}
	return count
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
