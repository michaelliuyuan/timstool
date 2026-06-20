package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "timstool",
	Short: "Multi-source heterogeneous DB to TiDB migration/sync tool",
	Long: `timstool is a CLI tool for migrating data from heterogeneous databases
(PostgreSQL, MySQL, Oracle, SqlServer, DB2, ...) to TiDB.

Currently PostgreSQL is fully implemented (full migration + CDC + DDL replication);
MySQL adapter is in development; other sources are stubbed.

It provides:
  - Schema migration (structure, indexes, views)
  - Full data migration (high-performance parallel export/import)
  - Data validation (row count, sampling, checksum)
  - CDC incremental sync (optional module, default OFF — enable via cdc.enable
    in config, or run 'timstool cdc --enable-cdc')`,
	SilenceUsage: true,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "configs/config.yaml", "config file path")
	rootCmd.PersistentFlags().String("source", "postgres", "data source type: postgres (default) | mysql | oracle | mssql | db2")
	rootCmd.PersistentFlags().String("log-level", "info", "log level: debug, info, warn, error")
	rootCmd.PersistentFlags().String("log-format", "console", "log format: console, json")
}
