package orchestrator

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/michaelliuyuan/timstool/internal/api"
	"github.com/michaelliuyuan/timstool/internal/common"
	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	cerrors "github.com/michaelliuyuan/timstool/internal/common/errors"
	"github.com/michaelliuyuan/timstool/internal/common/logger"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
	"github.com/michaelliuyuan/timstool/internal/data"
	"github.com/michaelliuyuan/timstool/internal/dumpling"
	"github.com/michaelliuyuan/timstool/internal/precheck"
	"github.com/michaelliuyuan/timstool/internal/schema"
	"github.com/michaelliuyuan/timstool/internal/source"
	"github.com/michaelliuyuan/timstool/internal/target"
	"github.com/michaelliuyuan/timstool/internal/validator"
	"go.uber.org/zap"
)

// Source-CIR test seams (MS-10d pen 1): package vars so unit anchors can
// drive runSourceCIR end-to-end with fakes — no real source adapter, TiDB
// connection, Lightning or dumpling binary in tests.
var (
	cirOpenSource        = source.Open
	cirOpenTargetDB      = func(cfg config.Config) (*sql.DB, error) { return sql.Open("mysql", cfg.Target.DSN()) }
	cirDropTables        = target.DropTables
	cirTruncateTables    = target.TruncateTables
	cirApplyDDL          = target.ApplyDDL
	cirLoadData          = target.LoadData
	cirRunLightning      = target.RunLightningImport
	cirValidateMigration = target.ValidateMigration
	cirFindDumpling      = dumpling.FindBinary
	cirPrecheck          = cirPrecheckProbe
)

// cirPrecheckProbe is the source-CIR precheck (MS-10d v1 minimal, leader
// ruling seq906): the source connection is live (Connect already succeeded),
// so stamp the server version when the adapter exposes its *sql.DB.
// Structural checks (privileges, charset, version floor) stay out of v1 —
// recorded as latent in docs/MS10D-WIZARD-MAP.md.
func cirPrecheckProbe(ctx context.Context, src source.Source) error {
	dc, ok := src.(interface{ DB() *sql.DB })
	if !ok {
		return nil
	}
	db := dc.DB()
	if db == nil {
		return nil
	}
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return fmt.Errorf("source-cir: precheck version probe: %w", err)
	}
	zap.L().Info("source-cir precheck: source reachable", zap.String("version", version))
	return nil
}

type Orchestrator struct {
	cfg        config.Config
	schemaMig  common.SchemaMigrator
	dataMig    common.DataMigrator
	validator  common.DataValidator
	prechecker common.Prechecker
	cpMgr      *checkpoint.Manager
	webServer  *api.Server
}

func NewOrchestrator(cfg config.Config) *Orchestrator {
	return &Orchestrator{
		cfg:        cfg,
		schemaMig:  schema.NewMigrator(cfg),
		dataMig:    data.NewMigrator(cfg),
		validator:  validator.NewValidator(cfg),
		prechecker: precheck.NewChecker(cfg),
	}
}

func (o *Orchestrator) Run(ctx context.Context, pipelineCfg PipelineConfig) ([]PipelineResult, error) {
	logger.InitWithOutput(o.cfg.Logging.Level, o.cfg.Logging.Format, o.cfg.Logging.Output)
	defer logger.Sync()

	log := zap.L()
	log.Info("starting timstool migration pipeline")

	var err error
	o.cpMgr, err = checkpoint.NewManager(o.cfg.Migration.CheckpointDir)
	if err != nil {
		return nil, cerrors.Wrap(cerrors.ErrCheckpointLoad, "init checkpoint", err)
	}

	// Schema progress: route the checkpoint manager into the schema
	// migrator (per-table SchemaState marks for the UI's schema phase).
	if sm, ok := o.schemaMig.(*schema.Migrator); ok {
		sm.SetProgressReporter(o.cpMgr)
	}

	// P1 phase lifecycle state machine: pre-seed pending/skipped for all
	// four phases so the UI can show 已跳过 from the very first poll.
	_ = o.cpMgr.InitPhases(map[string]bool{
		"precheck": pipelineCfg.SkipPrecheck,
		"schema":   pipelineCfg.SkipSchema,
		"data":     pipelineCfg.SkipData,
		"validate": pipelineCfg.SkipValidate,
	})

	if o.cfg.Web.Enable {
		stateAdapter := &checkpointStateReader{mgr: o.cpMgr}
		o.webServer = api.NewServer(stateAdapter, o.cfg.Web.Host, o.cfg.Web.Port)
		if err := o.webServer.Start(); err != nil {
			log.Warn("failed to start web server", zap.Error(err))
		} else {
			log.Info("web monitor started", zap.String("addr", fmt.Sprintf("%s:%d", o.cfg.Web.Host, o.cfg.Web.Port)))
		}
		defer o.webServer.Stop()
	}

	// Dual-path routing (#t79): PG -> existing COPY->Lightning (zero-regression);
	// non-PG -> Source+CIR execution (#t81). cpMgr + web monitor are started
	// above so BOTH paths report phase/progress to the UI.
	srcType := o.cfg.Source.SourceType()
	route := "pg-copy-lightning"
	if srcType != "postgres" {
		route = "source-cir"
	}
	log.Info("migration routing", zap.String("source", srcType), zap.String("path", route))
	if srcType != "postgres" {
		// MS-10d: the skip switches honor the same PipelineConfig as the
		// PG path; the phase seeds at Run entry (pipelineCfg values) are
		// the truth — no overriding "precheck always skipped" here anymore.
		return o.runSourceCIR(ctx, pipelineCfg)
	}

	var results []PipelineResult
	startTime := time.Now()

	if !pipelineCfg.SkipPrecheck {
		result := o.runPrecheck(ctx)
		results = append(results, result)
		if !result.Success && !pipelineCfg.OnErrorContinue {
			return results, result.Error
		}
	} else {
		log.Info("skipping precheck (user requested)")
	}

	if !pipelineCfg.SkipSchema {
		result := o.runSchema(ctx)
		results = append(results, result)
		if !result.Success && !pipelineCfg.OnErrorContinue {
			return results, result.Error
		}
	} else {
		log.Info("skipping schema migration (user requested)")
	}

	if !pipelineCfg.SkipData {
		if o.cfg.Migration.TargetPolicy == "drop" || o.cfg.Migration.TargetPolicy == "truncate" {
			cpDir := o.cfg.Migration.CheckpointDir
			if cpDir == "" {
				cpDir = ".checkpoint"
			}
			os.RemoveAll(filepath.Join(cpDir, "checkpoint.json"))
			log.Info("cleared checkpoint for drop/truncate policy to force fresh data migration")
		}
		result := o.runData(ctx)
		results = append(results, result)
		if !result.Success && !pipelineCfg.OnErrorContinue {
			return results, result.Error
		}
	} else {
		log.Info("skipping data migration (user requested)")
	}

	if !pipelineCfg.SkipValidate {
		result := o.runValidate(ctx)
		results = append(results, result)
		if !result.Success && !pipelineCfg.OnErrorContinue {
			return results, result.Error
		}
	} else {
		log.Info("skipping validation (user requested)")
	}

	o.cpMgr.SetPhaseWithReload("completed")
	log.Info("migration pipeline completed",
		zap.String("duration", time.Since(startTime).String()),
		zap.Int("phases", len(results)))

	return results, nil
}

// runSourceCIR executes the non-PG Source+CIR path (#t81). Steps:
//  0. Precheck (MS-10d v1 minimal) — connectivity probe + version stamp.
//  1. ApplyDDL — open the source adapter, read schema into CIR, CREATE TABLE on TiDB.
//  2. LoadData — dumpling fast-path (or stream fallback) → Lightning import.
//  3. Validate — row-count + value-level sample comparison (#t82, wired here).
//
// Source-agnostic: the target only sees CIR. PG is unaffected (COPY→Lightning path).
// MS-10d: every phase honors pipelineCfg.Skip* exactly like the PG path — a
// switch the user never set must never show "skipped" (silent-lie bug class).
func (o *Orchestrator) runSourceCIR(ctx context.Context, pipelineCfg PipelineConfig) ([]PipelineResult, error) {
	log := zap.L()
	srcType := o.cfg.Source.SourceType()

	srcCfg := source.SourceConfig{
		Kind:     srcType,
		Host:     o.cfg.Source.Host,
		Port:     o.cfg.Source.Port,
		User:     o.cfg.Source.User,
		Password: o.cfg.Source.Password,
		Database: o.cfg.Source.Database,
		Schema:   o.cfg.Source.Schema,
		Options:  map[string]string{"sslmode": o.cfg.Source.SSLMode},
	}
	src, err := cirOpenSource(srcType, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source-cir: open %s: %w", srcType, err)
	}
	defer src.Close()
	if err := src.Connect(ctx); err != nil {
		return nil, fmt.Errorf("source-cir: connect %s: %w", srcType, err)
	}

	// Phase: precheck (v1 minimal — probe only; runs after Connect so the
	// probe's "source reachable" claim is backed by a live connection).
	if pipelineCfg.SkipPrecheck {
		log.Info("skipping precheck (user requested)")
	} else {
		if o.cpMgr != nil {
			_ = o.cpMgr.SetPhase("precheck")
			_ = o.cpMgr.StartPhase("precheck")
		}
		log.Info("Phase: 预检查 (source-cir)", zap.String("source", srcType))
		if perr := cirPrecheck(ctx, src); perr != nil {
			o.finishPhase("precheck", perr, false)
			return nil, perr
		}
		o.finishPhase("precheck", nil, false)
	}

	cir, err := src.SchemaReader().ReadSchema(ctx, source.Filter{
		Tables:        o.cfg.Migration.Tables,
		ExcludeTables: o.cfg.Migration.ExcludeTables,
	})
	if err != nil {
		return nil, fmt.Errorf("source-cir: read schema: %w", err)
	}

	tidb, err := cirOpenTargetDB(o.cfg)
	if err != nil {
		return nil, fmt.Errorf("source-cir: open target: %w", err)
	}
	defer tidb.Close()

	if pipelineCfg.SkipSchema {
		log.Info("skipping schema migration (user requested)")
	} else {
		// Phase: schema (observability parity with the PG path — phase log + cpMgr).
		if o.cpMgr != nil {
			_ = o.cpMgr.SetPhase("schema")
			_ = o.cpMgr.StartPhase("schema")
			// Register the CIR tables up front (schema tables_total correct from
			// the start), then mark them per ApplyDDL outcome.
			names := make([]string, len(cir.Tables))
			for i, t := range cir.Tables {
				names[i] = t.Name
			}
			_ = o.cpMgr.RegisterSchemaTables(names)
		}
		log.Info("Phase: Schema 迁移", zap.String("source", srcType))

		// Apply the target data policy (mirrors the PG path). Lightning local-backend
		// requires EMPTY target tables, so drop/truncate empty them before import.
		policy := o.cfg.Migration.TargetPolicy
		if policy == "drop" {
			if err := cirDropTables(ctx, tidb, cir); err != nil {
				o.finishPhase("schema", err, false)
				return nil, fmt.Errorf("source-cir: drop tables (policy=drop): %w", err)
			}
			log.Info("source-cir: dropped target tables", zap.String("policy", policy))
		}
		if err := cirApplyDDL(ctx, tidb, cir); err != nil {
			if o.cpMgr != nil {
				for _, t := range cir.Tables {
					_ = o.cpMgr.MarkSchemaTableFailed(t.Name, err.Error())
				}
			}
			o.finishPhase("schema", err, false)
			return nil, fmt.Errorf("source-cir: apply ddl: %w", err)
		}
		if o.cpMgr != nil {
			for _, t := range cir.Tables {
				_ = o.cpMgr.MarkSchemaTableCompleted(t.Name)
			}
		}
		if policy == "truncate" {
			if err := cirTruncateTables(ctx, tidb, cir); err != nil {
				o.finishPhase("schema", err, false)
				return nil, fmt.Errorf("source-cir: truncate tables (policy=truncate): %w", err)
			}
			log.Info("source-cir: truncated target tables", zap.String("policy", policy))
		}
		o.finishPhase("schema", nil, false)
		log.Info("source-cir schema applied", zap.String("source", srcType), zap.Int("tables", len(cir.Tables)))
	}

	// Phase: data (#t81 Step 2 — CIR rows via DataReader -> TSV CSV -> lightning -> TiDB).
	if pipelineCfg.SkipData {
		log.Info("skipping data migration (user requested)")
	} else {
		if o.cpMgr != nil {
			_ = o.cpMgr.SetPhase("data")
			_ = o.cpMgr.StartPhase("data")
			_ = o.cpMgr.SetSubPhase("data", "data-export")
		}
		log.Info("Phase: 数据迁移", zap.String("source", srcType))
		tempDir, err := os.MkdirTemp("", "timstool-cir-load-*")
		if err != nil {
			o.finishPhase("data", err, false)
			return nil, fmt.Errorf("source-cir: create temp dir: %w", err)
		}
		defer os.RemoveAll(tempDir)

		// Export mode: dumpling (fast-path, concurrent+snapshot) if binary available;
		// otherwise stream (per-row CIR DataReader → CSV). Design §3.
		exportMode := "stream"
		if srcType == "mysql" {
			if bin := cirFindDumpling(""); bin != "" {
				exportMode = "dumpling"
				log.Info("source-cir: using dumpling export (fast-path)", zap.String("binary", bin))
				// Dumpling exports CSV directly to tempDir; LoadData then imports via lightning.
				tableNames := make([]string, len(cir.Tables))
				for i, t := range cir.Tables {
					tableNames[i] = o.cfg.Source.Database + "." + t.Name
				}
				if err := dumpling.Dump(ctx, dumpling.DumpFromConfig(o.cfg.Source, tempDir, bin, tableNames)); err != nil {
					// Fall back to stream on dumpling failure.
					log.Warn("source-cir: dumpling failed, falling back to stream", zap.Error(err))
					exportMode = "stream"
				}
			} else {
				log.Info("source-cir: dumpling binary not found, using stream export")
			}
		}

		if exportMode == "stream" {
			// Per-table start stamps: the CIR stream exporter walks tables
			// sequentially, so table k starts when table k-1 finishes —
			// stamp the first table now and each successor inside the
			// completion callback. Queue wait never inflates a table's own
			// duration.
			nextIdx := 1
			if o.cpMgr != nil && len(cir.Tables) > 0 {
				o.cpMgr.GetOrCreateTable(cir.Tables[0].Name, 0)
				_ = o.cpMgr.MarkTableRunning(cir.Tables[0].Name)
			}
			if err := cirLoadData(ctx, src, cir, o.cfg.Target, tempDir, func(name string, rows int64) {
				// progress parity: each exported table feeds tables_done/rows to the UI.
				if o.cpMgr != nil {
					o.cpMgr.GetOrCreateTable(name, rows)
					_ = o.cpMgr.MarkTableCompleted(name, rows)
					if nextIdx < len(cir.Tables) {
						o.cpMgr.GetOrCreateTable(cir.Tables[nextIdx].Name, 0)
						_ = o.cpMgr.MarkTableRunning(cir.Tables[nextIdx].Name)
						nextIdx++
					}
				}
			}); err != nil {
				o.finishPhase("data", err, false)
				return nil, fmt.Errorf("source-cir: load data: %w", err)
			}
		} else {
			// Dumpling produced CSVs directly; run lightning import (CSVs already in
			// tempDir). Report real per-table row counts from the dumped CSVs so the
			// progress layer/UI shows the true figure — the stream path gets counts
			// from exportTableCSV's callback, but dumpling writes files directly and
			// bypasses it, so without this the UI reports "0 rows migrated" even
			// though Lightning loaded everything. See #t83.
			bareTables := make([]string, len(cir.Tables))
			for i, t := range cir.Tables {
				bareTables[i] = t.Name
			}
			rowCounts := dumpling.CountExportedRows(tempDir, o.cfg.Source.Database, bareTables)
			for _, t := range cir.Tables {
				rows := rowCounts[t.Name]
				if o.cpMgr != nil {
					o.cpMgr.GetOrCreateTable(t.Name, rows)
					_ = o.cpMgr.MarkTableCompleted(t.Name, rows)
				}
				log.Info("source-cir: dumpling exported table", zap.String("table", t.Name), zap.Int64("rows", rows))
			}
			if o.cpMgr != nil {
				_ = o.cpMgr.SetSubPhase("data", "data-import")
				// Align the coarse phase + import mode with the PG data
				// migrator (data/migrator.go:199) so pollProgress and the
				// phases API normalize the CIR path identically.
				_ = o.cpMgr.SetPhase("data-import")
				_ = o.cpMgr.SetImportMode(checkpoint.ImportModeLightning)
			}
			if err := cirRunLightning(ctx, tempDir, o.cfg.Target); err != nil {
				o.finishPhase("data", err, false)
				return nil, fmt.Errorf("source-cir: lightning import (dumpling): %w", err)
			}
			// CIR has no per-table import callback; one honest terminal
			// write of N/N (same display the PG lightning path shows before
			// its first table) — progress jumps to 100% monotonically.
			if o.cpMgr != nil {
				_ = o.cpMgr.SetImportedTables(len(cir.Tables))
			}
		}
		o.finishPhase("data", nil, false)
		log.Info("source-cir data loaded", zap.String("source", srcType), zap.Int("tables", len(cir.Tables)), zap.String("export", exportMode))
	}

	// Phase: validate (#t81 Step 3 + #t82 value-level). CompareMode "quick" →
	// row-count only; otherwise value-level sample comparison (closes the
	// "row-count green but values corrupt" hole — e.g. a bad CSV separator
	// corrupts every value while row counts still match).
	validateSuccess := true
	var validateErr error
	if pipelineCfg.SkipValidate {
		log.Info("skipping validation (user requested)")
	} else {
		if o.cpMgr != nil {
			_ = o.cpMgr.SetPhase("validate")
			_ = o.cpMgr.StartPhase("validate")
		}
		log.Info("Phase: 数据验证", zap.String("source", srcType))
		sampleSize := 0
		if o.cfg.Compare.CompareMode != "quick" { // "" / "sample" / "checksum" → value-level
			sampleSize = o.cfg.Compare.SampleRows
			if sampleSize <= 0 {
				sampleSize = 20
			}
		}
		type dbConn interface{ DB() *sql.DB }
		if dc, ok := src.(dbConn); ok {
			// F-13: validate READ sessions on both sides pin UTC
			// (time_zone='+00:00', DSN-level so pooled semantics stay correct).
			// The data-path pools (dc.DB() / tidb) are deliberately untouched —
			// their wall-clock coupling with the write path is load-bearing;
			// pinning only one side would shift writes −8h and fabricate real
			// diffs (leader ruling seq640/641). MySQL source only; other kinds
			// keep the legacy pools unchanged.
			srcValDB, tgtValDB := dc.DB(), tidb
			if srcType == "mysql" {
				if db2, err := sql.Open("mysql", o.cfg.Source.DSNByType()); err == nil {
					defer db2.Close()
					srcValDB = db2
				} else {
					log.Warn("source-cir: pinned validate source pool failed, falling back to data pool", zap.Error(err))
				}
				if db2, err := sql.Open("mysql", o.cfg.Target.DSNPinnedUTC()); err == nil {
					defer db2.Close()
					tgtValDB = db2
				} else {
					log.Warn("source-cir: pinned validate target pool failed, falling back to data pool", zap.Error(err))
				}
			}
			vr, verr := cirValidateMigration(ctx, srcValDB, tgtValDB, cir, sampleSize)
			if verr != nil {
				log.Warn("source-cir: validation error", zap.Error(verr))
				validateSuccess = false
				validateErr = fmt.Errorf("source-cir: validation error: %w", verr)
			} else {
				log.Info("source-cir validation result",
					zap.Int("tables", vr.TotalTables), zap.Int("failed", vr.FailedTables),
					zap.Int("sample_size", sampleSize))
				if !vr.AllPassed {
					validateSuccess = false
					validateErr = fmt.Errorf("source-cir: validation failed: %d/%d tables failed", vr.FailedTables, vr.TotalTables)
					for _, tv := range vr.Tables {
						if !tv.Passed {
							log.Warn("validation mismatch",
								zap.String("table", tv.Name),
								zap.Int64("source", tv.SourceRows),
								zap.Int64("target", tv.TargetRows),
								zap.Int("sample_checked", tv.SampleChecked),
								zap.Int("sample_mismatches", tv.SampleMismatches))
						}
					}
				}
			}
		}
	}

	if o.cpMgr != nil {
		_ = o.cpMgr.SetPhaseWithReload("completed")
	}
	if !pipelineCfg.SkipValidate {
		if validateSuccess {
			o.finishPhase("validate", nil, false)
		} else {
			o.finishPhase("validate", validateErr, false)
		}
	}

	// Mirror the PG path: a result row only for phases actually executed.
	results := make([]PipelineResult, 0, 4)
	if !pipelineCfg.SkipPrecheck {
		results = append(results, PipelineResult{Phase: PhasePrecheck, Success: true})
	}
	if !pipelineCfg.SkipSchema {
		results = append(results, PipelineResult{Phase: PhaseSchema, Success: true})
	}
	if !pipelineCfg.SkipData {
		results = append(results, PipelineResult{Phase: PhaseData, Success: true})
	}
	if !pipelineCfg.SkipValidate {
		results = append(results, PipelineResult{Phase: PhaseValidate, Success: validateSuccess, Error: validateErr})
	}
	return results, nil
}

func (o *Orchestrator) runPrecheck(ctx context.Context) PipelineResult {
	log := zap.L()
	log.Info("Phase: 预检查")
	start := time.Now()

	if o.cpMgr != nil {
		_ = o.cpMgr.SetPhase("precheck")
		_ = o.cpMgr.StartPhase("precheck")
	}

	rpt, err := o.prechecker.Run(ctx, common.PrecheckOpts{
		ReportFile: "precheck-report.json",
	})

	result := PipelineResult{
		Phase:   PhasePrecheck,
		Success: err == nil,
		Error:   err,
	}

	if err != nil {
		log.Error("pre-check failed", zap.Error(err))
		o.finishPhase("precheck", err, false)
		return result
	}

	o.finishPhase("precheck", nil, false)

	if rpt != nil {
		log.Info("pre-check completed",
			zap.String("status", string(rpt.Status)),
			zap.String("duration", time.Since(start).String()))
	}

	return result
}

func (o *Orchestrator) runSchema(ctx context.Context) PipelineResult {
	log := zap.L()
	log.Info("Phase: Schema 迁移")
	start := time.Now()

	if o.cpMgr != nil {
		o.cpMgr.SetPhase("schema")
		_ = o.cpMgr.StartPhase("schema")
	}

	// Tables/ExcludeTables align the schema PROGRESS registration set with
	// the data migration set (①b): pre-registered entries for tables data
	// will never process would keep tables_done < tables_total forever.
	// DDL build/execute range inside schema.Run is unchanged.
	err := o.schemaMig.Run(ctx, common.SchemaOpts{
		Tables:        o.cfg.Migration.Tables,
		ExcludeTables: o.cfg.Migration.ExcludeTables,
	})

	result := PipelineResult{
		Phase:   PhaseSchema,
		Success: err == nil,
		Error:   err,
	}

	if err != nil {
		if cerrors.ShouldAbort(err, cerrors.StrategyAbort) {
			log.Error("schema migration failed", zap.Error(err))
			o.finishPhase("schema", err, false)
			return result
		}
		log.Warn("schema migration had errors (continuing)", zap.Error(err))
		result.Success = true
		// Error tolerated by OnError=continue: record completed-with-warn
		// so the UI shows 带警告完成 instead of a silent green.
		o.finishPhase("schema", nil, true)
		return result
	}

	// OnError=skip lets runDDL swallow per-statement failures (aggregate
	// err == nil) while individual tables sit failed in the checkpoint —
	// surface that as completed-with-warn instead of a silent green.
	// The orchestrator's checkpoint copy is fresh during the schema
	// phase (single writer), so a plain read is safe here.
	o.finishPhase("schema", nil, o.schemaHasFailedTables())

	log.Info("schema migration completed", zap.String("duration", time.Since(start).String()))
	return result
}

func (o *Orchestrator) schemaHasFailedTables() bool {
	if o.cpMgr == nil {
		return false
	}
	for _, tc := range o.cpMgr.GetAllTables() {
		if tc.SchemaState == checkpoint.StateFailed {
			return true
		}
	}
	return false
}

func (o *Orchestrator) runData(ctx context.Context) PipelineResult {
	log := zap.L()
	log.Info("Phase: 数据迁移")
	start := time.Now()

	if o.cpMgr != nil {
		o.cpMgr.SetPhase("data")
		_ = o.cpMgr.StartPhase("data")
	}

	dataResult, err := o.dataMig.Run(ctx, common.DataOpts{
		Parallel:      o.cfg.Migration.Parallel,
		BatchSize:     o.cfg.Migration.BatchSize,
		Tables:        o.cfg.Migration.Tables,
		ExcludeTables: o.cfg.Migration.ExcludeTables,
		UseLightning:  o.cfg.Migration.UseLightning,
		TempDir:       o.cfg.Migration.TempDir,
	})

	result := PipelineResult{
		Phase:   PhaseData,
		Success: err == nil,
		Error:   err,
	}

	if err != nil {
		log.Error("data migration failed", zap.Error(err))
		o.finishPhaseWithReload("data", err, false)
		return result
	}

	o.finishPhaseWithReload("data", nil, false)

	if dataResult != nil {
		log.Info("data migration completed",
			zap.Int64("rows", dataResult.TotalRows),
			zap.Int("tables", dataResult.TotalTables),
			zap.String("duration", time.Since(start).String()))
	}

	return result
}

func (o *Orchestrator) runValidate(ctx context.Context) PipelineResult {
	log := zap.L()
	log.Info("Phase: 数据验证")
	start := time.Now()

	if o.cpMgr != nil {
		// Use SetPhaseWithReload to reload checkpoint from disk first.
		// The data migrator writes table progress via its own cpMgr; if we
		// use SetPhase, the orchestrator's stale in-memory state (with no
		// tables) would overwrite the data migrator's progress.
		o.cpMgr.SetPhaseWithReload("validate")
		_ = o.cpMgr.StartPhase("validate")
	}

	// Resolve effective mode: never allow empty mode
	mode := o.cfg.Compare.CompareMode
	if mode == "" {
		mode = "sample"
	}

	// Determine validation level from resolved mode
	level := "L2" // default: sample
	switch mode {
	case "quick":
		level = "L1"
	case "checksum":
		level = "L3"
	}

	sampleRatio := o.cfg.Compare.SampleRatio
	if sampleRatio <= 0 {
		sampleRatio = 0.01
	}

	log.Info("data validation config",
		zap.String("mode", mode),
		zap.String("level", level),
		zap.Float64("sample_ratio", sampleRatio))

	rpt, err := o.validator.Run(ctx, common.ValidateOpts{
		Level:       level,
		Mode:        mode,
		SampleRatio: sampleRatio,
		Tables:      o.cfg.Migration.Tables,
		ReportFile:  "validation-report.json",
	})

	result := PipelineResult{
		Phase:   PhaseValidate,
		Success: err == nil,
		Error:   err,
	}

	if err != nil {
		log.Error("data validation failed", zap.Error(err))
		o.finishPhase("validate", err, false)
		return result
	}

	if rpt != nil {
		log.Info("data validation completed",
			zap.String("status", string(rpt.Status)),
			zap.String("duration", time.Since(start).String()))
		if rpt.Status == reporter.StatusFail {
			result.Success = false
			result.Error = fmt.Errorf("data validation failed: %d/%d tables failed", rpt.Stats.FailTables, rpt.Stats.TotalTables)
			log.Error("data validation failed",
				zap.Int("fail", rpt.Stats.FailTables),
				zap.Int("total", rpt.Stats.TotalTables))
			o.finishPhase("validate", result.Error, false)
			return result
		}
	}

	o.finishPhase("validate", nil, false)
	return result
}

type checkpointStateReader struct {
	mgr *checkpoint.Manager
}

// finishPhase is the nil-safe FinishPhase wrapper used on every return
// path so the phase machine can never be left "running" forever.
func (o *Orchestrator) finishPhase(name string, err error, warn bool) {
	if o.cpMgr == nil {
		return
	}
	_ = o.cpMgr.FinishPhase(name, err, warn)
}

// finishPhaseWithReload is the dual-instance-safe variant for the data
// phase: the data migrator writes through its own checkpoint.Manager, so
// this orchestrator's in-memory copy is stale by the time data finishes —
// reloading before the final save prevents overwriting the data-plane
// progress (tables/rows/imported) already persisted by the migrator.
func (o *Orchestrator) finishPhaseWithReload(name string, err error, warn bool) {
	if o.cpMgr == nil {
		return
	}
	_ = o.cpMgr.FinishPhaseWithReload(name, err, warn)
}

func (r *checkpointStateReader) GetPhase() string {
	return r.mgr.GetPhase()
}

func (r *checkpointStateReader) GetAllTables() map[string]api.TableState {
	tables := r.mgr.GetAllTables()
	result := make(map[string]api.TableState, len(tables))
	for name, tc := range tables {
		result[name] = api.TableState{
			TableName: tc.TableName,
			State:     string(tc.State),
			RowsDone:  tc.RowsDone,
			RowsTotal: tc.RowsTotal,
			Error:     tc.Error,
		}
	}
	return result
}

func (r *checkpointStateReader) Summary() (completed, failed, pending, running int) {
	return r.mgr.Summary()
}
