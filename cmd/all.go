package cmd

import (
	"fmt"
	"os"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/orchestrator"
	"github.com/spf13/cobra"
)

var allCmd = &cobra.Command{
	Use:   "all",
	Short: "Run full migration pipeline (precheck -> schema -> data -> validate)",
	Long: `Execute the complete migration pipeline:
  1. Pre-check compatibility
  2. Migrate schema
  3. Migrate full data
  4. Validate data consistency`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// --source overrides the config's source type; the orchestrator routes
		// by it (#t79 dual-path: postgres→COPY→Lightning; non-PG→Source+CIR,
		// graceful "见 #t81" until the CIR→TiDB execution engine lands).
		sourceType, _ := cmd.Flags().GetString("source")

		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if sourceType != "" {
			cfg.Source.Type = sourceType
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}

		skipPrecheck, _ := cmd.Flags().GetBool("skip-precheck")
		skipSchema, _ := cmd.Flags().GetBool("skip-schema")
		skipData, _ := cmd.Flags().GetBool("skip-data")
		skipValidate, _ := cmd.Flags().GetBool("skip-validate")
		onErrorContinue, _ := cmd.Flags().GetBool("on-error-continue")

		o := orchestrator.NewOrchestrator(*cfg)
		results, err := o.Run(cmd.Context(), orchestrator.PipelineConfig{
			SkipPrecheck:    skipPrecheck,
			SkipSchema:      skipSchema,
			SkipData:        skipData,
			SkipValidate:    skipValidate,
			OnErrorContinue: onErrorContinue,
		})

		fmt.Fprintf(os.Stderr, "\n=== Pipeline Results ===\n")
		for _, r := range results {
			status := "PASS"
			if !r.Success {
				status = "FAIL"
			}
			fmt.Fprintf(os.Stderr, "  %s: %s\n", r.Phase, status)
		}

		return err
	},
}

func init() {
	rootCmd.AddCommand(allCmd)
	allCmd.Flags().Bool("skip-precheck", false, "skip pre-check step")
	allCmd.Flags().Bool("skip-schema", false, "skip schema migration step")
	allCmd.Flags().Bool("skip-data", false, "skip data migration step")
	allCmd.Flags().Bool("skip-validate", false, "skip data validation step")
	allCmd.Flags().Bool("on-error-continue", false, "continue pipeline on non-fatal errors")
}
