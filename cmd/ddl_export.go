package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/logger"
	"github.com/michaelliuyuan/timstool/internal/ddlexport"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	ddlExportOut     string
	ddlExportSchemas string
	ddlExportTypes   string
	ddlExportTiDB    bool
)

var ddlExportCmd = &cobra.Command{
	Use:   "export-ddl",
	Short: "Export source PostgreSQL DDL per schema into .sql files",
	Long: `Export native PostgreSQL DDL for the configured source database.

Each selected schema becomes a self-contained folder under --out containing
one .sql file per object type: tables.sql, indexes.sql, views.sql,
sequences.sql, functions.sql, procedures.sql, triggers.sql, types.sql.
Every schema folder also carries its own README.txt and manifest.json
(object counts, skipped objects). With --tidb an additional
tidb-tables.sql (TiDB-converted CREATE TABLE reference) is written.

Examples:
  timstool export-ddl --config config.yaml --out ./ddl
  timstool export-ddl -c config.yaml -o ./ddl --schemas public,sales
  timstool export-ddl -c config.yaml -o ./ddl --types tables,indexes,views --tidb`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}

		logLevel, _ := cmd.Flags().GetString("log-level")
		logFormat, _ := cmd.Flags().GetString("log-format")
		logger.InitWithOutput(logLevel, logFormat, "")

		if ddlExportOut == "" {
			return fmt.Errorf("--out directory is required")
		}

		pgDB, err := sql.Open("pgx", cfg.Source.DSN())
		if err != nil {
			return fmt.Errorf("connect to PostgreSQL: %w", err)
		}
		defer pgDB.Close()

		ts := ddlexport.DefaultTypes()
		if ddlExportTypes != "" {
			ts, err = parseDDLTypes(ddlExportTypes)
			if err != nil {
				return err
			}
		}

		var schemas []string
		if ddlExportSchemas != "" {
			for _, s := range strings.Split(ddlExportSchemas, ",") {
				if s = strings.TrimSpace(s); s != "" {
					if err := ddlexport.ValidateSchemaName(s); err != nil {
						return err
					}
					schemas = append(schemas, s)
				}
			}
		}

		ctx := context.Background()
		exporter := ddlexport.NewExporter(pgDB, ddlexport.Options{
			Schemas:     schemas,
			Types:       ts,
			IncludeTiDB: ddlExportTiDB,
		})

		manifest, err := exporter.ExportDir(ctx, ddlExportOut)
		if err != nil {
			return fmt.Errorf("export ddl: %w", err)
		}

		zap.L().Info("ddl export completed",
			zap.String("out", ddlExportOut),
			zap.Int("objects", manifest.Total),
			zap.Int("skipped", len(manifest.Skipped)))
		fmt.Fprintf(os.Stderr, "Exported %d objects to %s (%d skipped, see manifest.json)\n",
			manifest.Total, ddlExportOut, len(manifest.Skipped))
		return nil
	},
}

func parseDDLTypes(spec string) (ddlexport.TypeSet, error) {
	ts := ddlexport.TypeSet{}
	valid := map[string]*bool{
		"tables": &ts.Tables, "indexes": &ts.Indexes, "views": &ts.Views,
		"sequences": &ts.Sequences, "functions": &ts.Functions,
		"procedures": &ts.Procedures, "triggers": &ts.Triggers, "types": &ts.Types,
	}
	for _, t := range strings.Split(spec, ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		p, ok := valid[t]
		if !ok {
			return ts, fmt.Errorf("unknown object type %q (valid: tables, indexes, views, sequences, functions, procedures, triggers, types)", t)
		}
		*p = true
	}
	return ts, nil
}

func init() {
	ddlExportCmd.Flags().StringVarP(&ddlExportOut, "out", "o", "", "Output directory (required)")
	ddlExportCmd.Flags().StringVar(&ddlExportSchemas, "schemas", "", "Comma-separated schema list (default: public)")
	ddlExportCmd.Flags().StringVar(&ddlExportTypes, "types", "", "Comma-separated object types (default: all)")
	ddlExportCmd.Flags().BoolVar(&ddlExportTiDB, "tidb", false, "Also export a TiDB-converted tables.sql reference")
	rootCmd.AddCommand(ddlExportCmd)
}
