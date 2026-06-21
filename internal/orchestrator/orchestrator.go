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
	"github.com/michaelliuyuan/timstool/internal/precheck"
	"github.com/michaelliuyuan/timstool/internal/schema"
	"github.com/michaelliuyuan/timstool/internal/source"
	"github.com/michaelliuyuan/timstool/internal/target"
	"github.com/michaelliuyuan/timstool/internal/validator"
	"go.uber.org/zap"
)

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
		cfg:       cfg,
		schemaMig: schema.NewMigrator(cfg),
		dataMig:   data.NewMigrator(cfg),
		validator: validator.NewValidator(cfg),
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
		return o.runSourceCIR(ctx)
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

// runSourceCIR executes the non-PG Source+CIR path (#t81). Step 1 (ApplyDDL):
// open the source adapter, read its schema into CIR, and create the tables on
// the TiDB target — source-agnostic (the target only sees CIR). Data load
// (Step 2) and validate (Step 3) are pending; until then this returns an honest
// "schema applied, data pending" so a non-PG migration is never reported
// complete without data. PG is unaffected (it takes the COPY->Lightning path).
func (o *Orchestrator) runSourceCIR(ctx context.Context) ([]PipelineResult, error) {
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
	src, err := source.Open(srcType, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source-cir: open %s: %w", srcType, err)
	}
	defer src.Close()
	if err := src.Connect(ctx); err != nil {
		return nil, fmt.Errorf("source-cir: connect %s: %w", srcType, err)
	}
	cir, err := src.SchemaReader().ReadSchema(ctx, source.Filter{
		Tables:        o.cfg.Migration.Tables,
		ExcludeTables: o.cfg.Migration.ExcludeTables,
	})
	if err != nil {
		return nil, fmt.Errorf("source-cir: read schema: %w", err)
	}

	tidb, err := sql.Open("mysql", o.cfg.Target.DSN())
	if err != nil {
		return nil, fmt.Errorf("source-cir: open target: %w", err)
	}
	defer tidb.Close()

	// Phase: schema (observability parity with the PG path — phase log + cpMgr).
	if o.cpMgr != nil {
		_ = o.cpMgr.SetPhase("schema")
	}
	log.Info("Phase: Schema 迁移", zap.String("source", srcType))

	// Apply the target data policy (mirrors the PG path). Lightning local-backend
	// requires EMPTY target tables, so drop/truncate empty them before import.
	policy := o.cfg.Migration.TargetPolicy
	if policy == "drop" {
		if err := target.DropTables(ctx, tidb, cir); err != nil {
			return nil, fmt.Errorf("source-cir: drop tables (policy=drop): %w", err)
		}
		log.Info("source-cir: dropped target tables", zap.String("policy", policy))
	}
	if err := target.ApplyDDL(ctx, tidb, cir); err != nil {
		return nil, fmt.Errorf("source-cir: apply ddl: %w", err)
	}
	if policy == "truncate" {
		if err := target.TruncateTables(ctx, tidb, cir); err != nil {
			return nil, fmt.Errorf("source-cir: truncate tables (policy=truncate): %w", err)
		}
		log.Info("source-cir: truncated target tables", zap.String("policy", policy))
	}
	log.Info("source-cir schema applied", zap.String("source", srcType), zap.Int("tables", len(cir.Tables)))

	// Phase: data (#t81 Step 2 — CIR rows via DataReader -> TSV CSV -> lightning -> TiDB).
	if o.cpMgr != nil {
		_ = o.cpMgr.SetPhase("data")
	}
	log.Info("Phase: 数据迁移", zap.String("source", srcType))
	tempDir, err := os.MkdirTemp("", "timstool-cir-load-*")
	if err != nil {
		return nil, fmt.Errorf("source-cir: create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)
	if err := target.LoadData(ctx, src, cir, o.cfg.Target, tempDir, func(name string, rows int64) {
		// progress parity: each exported table feeds tables_done/rows to the UI.
		if o.cpMgr != nil {
			o.cpMgr.GetOrCreateTable(name, rows)
			_ = o.cpMgr.MarkTableCompleted(name, rows)
		}
	}); err != nil {
		return nil, fmt.Errorf("source-cir: load data: %w", err)
	}
	log.Info("source-cir data loaded", zap.String("source", srcType), zap.Int("tables", len(cir.Tables)))
	if o.cpMgr != nil {
		_ = o.cpMgr.SetPhaseWithReload("completed")
	}

	return []PipelineResult{
		{Phase: PhaseSchema, Success: true},
		{Phase: PhaseData, Success: true},
	}, nil
}

func (o *Orchestrator) runPrecheck(ctx context.Context) PipelineResult {
	log := zap.L()
	log.Info("Phase: 预检查")
	start := time.Now()

	if o.cpMgr != nil {
		o.cpMgr.SetPhase("precheck")
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
		return result
	}

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
	}

	err := o.schemaMig.Run(ctx, common.SchemaOpts{})

	result := PipelineResult{
		Phase:   PhaseSchema,
		Success: err == nil,
		Error:   err,
	}

	if err != nil {
		if cerrors.ShouldAbort(err, cerrors.StrategyAbort) {
			log.Error("schema migration failed", zap.Error(err))
			return result
		}
		log.Warn("schema migration had errors (continuing)", zap.Error(err))
		result.Success = true
	}

	log.Info("schema migration completed", zap.String("duration", time.Since(start).String()))
	return result
}

func (o *Orchestrator) runData(ctx context.Context) PipelineResult {
	log := zap.L()
	log.Info("Phase: 数据迁移")
	start := time.Now()

	if o.cpMgr != nil {
		o.cpMgr.SetPhase("data")
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
		return result
	}

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
		}
	}

	return result
}

type checkpointStateReader struct {
	mgr *checkpoint.Manager
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
