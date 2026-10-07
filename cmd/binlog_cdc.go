package cmd

// binlog_cdc.go — MS-11 pen 3: `timstool cdc` routing for MySQL sources.
// Builds the BinlogRunner from the shared config (cdc.server_id etc.) and
// runs the same signal/stats lifecycle as the PG path.

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/michaelliuyuan/timstool/internal/cdc"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/logger"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func runBinlogCDC(cmd *cobra.Command, cfg *config.Config) error {
	if cfg.CDC.ServerID == 0 {
		return fmt.Errorf("MySQL binlog CDC requires cdc.server_id（复制拓扑内唯一非零 id）")
	}

	srcCfg := cdc.BinlogSourceConfig{
		Host:          cfg.Source.Host,
		Port:          cfg.Source.Port,
		User:          cfg.Source.User,
		Password:      cfg.Source.Password,
		Database:      cfg.Source.Database,
		ServerID:      cfg.CDC.ServerID,
		Tables:        cfg.CDC.Tables,
		ExcludeTables: cfg.CDC.ExcludeTables,
	}

	cpFile := cfg.CDC.CheckpointFile
	if cmd.Flags().Changed("checkpoint-file") {
		cpFile, _ = cmd.Flags().GetString("checkpoint-file")
	}
	dataDir, _ := cmd.Flags().GetString("data-dir")
	if dataDir == "" {
		dataDir = ".timstool"
	}
	statusFile, _ := cmd.Flags().GetString("status-file")
	if statusFile == "" {
		statusFile = filepath.Join(dataDir, "cdc", "status.json")
	}
	if abs, err := filepath.Abs(statusFile); err == nil {
		fmt.Fprintf(os.Stderr, "cdc status file (web must read this path): %s\n", abs)
	}

	batchCfg := cdc.DefaultBatchConfig()
	batchCfg.BatchSize = cfg.CDC.BatchSize
	batchCfg.Parallel = cfg.CDC.Parallel
	batchCfg.ConflictStrategy = cdc.ConflictStrategy(cfg.CDC.ConflictStrategy)
	if cmd.Flags().Changed("batch-size") {
		if v, _ := cmd.Flags().GetInt("batch-size"); v > 0 {
			batchCfg.BatchSize = v
		}
	}
	if cmd.Flags().Changed("parallel") {
		if v, _ := cmd.Flags().GetInt("parallel"); v > 0 {
			batchCfg.Parallel = v
		}
	}
	if cmd.Flags().Changed("conflict-strategy") {
		if v, _ := cmd.Flags().GetString("conflict-strategy"); v != "" {
			batchCfg.ConflictStrategy = cdc.ConflictStrategy(v)
		}
	}

	includeTables, _ := cmd.Flags().GetStringSlice("include-table")
	excludeTables, _ := cmd.Flags().GetStringSlice("exclude-table")
	includeSchemas, _ := cmd.Flags().GetStringSlice("include-schema")
	excludeSchemas, _ := cmd.Flags().GetStringSlice("exclude-schema")
	tblFilter := cdc.NewTableFilter().
		WithWhitelist(includeTables).
		WithBlacklist(excludeTables).
		WithSchemas(includeSchemas, excludeSchemas)

	targetDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&timeout=30s&readTimeout=300s&writeTimeout=300s",
		cfg.Target.User, cfg.Target.Password, cfg.Target.Host, cfg.Target.Port, cfg.Target.Database)

	logLevel, _ := cmd.Flags().GetString("log-level")
	logFormat, _ := cmd.Flags().GetString("log-format")
	logOutput, _ := cmd.Flags().GetString("log-output")
	if logLevel == "" {
		logLevel = cfg.Logging.Level
	}
	if logFormat == "" {
		logFormat = cfg.Logging.Format
	}
	logger.InitWithOutput(logLevel, logFormat, logOutput)
	defer logger.Sync()
	log := zap.L()

	runner, err := cdc.NewBinlogRunner(cdc.BinlogRunnerConfig{
		Source:         srcCfg,
		Batch:          batchCfg,
		Transformer:    cdc.DefaultTransformerConfig(),
		Filter:         tblFilter,
		TargetDSN:      targetDSN,
		CheckpointFile: cpFile,
		StatusFile:     statusFile,
	})
	if err != nil {
		return fmt.Errorf("create binlog cdc runner: %w", err)
	}
	runner.SetLogger(log)

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("received interrupt signal")
		cancel()
	}()

	log.Info("starting mysql binlog cdc",
		zap.String("source", fmt.Sprintf("%s:%d/%s", srcCfg.Host, srcCfg.Port, srcCfg.Database)),
		zap.Uint32("server_id", srcCfg.ServerID),
		zap.String("target", fmt.Sprintf("%s:%d/%s", cfg.Target.Host, cfg.Target.Port, cfg.Target.Database)),
	)

	if err := runner.Run(ctx); err != nil && err != context.Canceled {
		return fmt.Errorf("binlog cdc run: %w", err)
	}

	stats := runner.Stats()
	fmt.Fprintf(os.Stderr, "\n=== MySQL CDC Final Stats ===\n")
	for k, v := range stats {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", k, v)
	}
	return nil
}
