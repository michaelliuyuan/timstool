package orchestrator

import (
	"path/filepath"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
)

func TestPhaseConstants(t *testing.T) {
	if PhasePrecheck != "precheck" {
		t.Errorf("expected precheck, got %s", PhasePrecheck)
	}
	if PhaseSchema != "schema" {
		t.Errorf("expected schema, got %s", PhaseSchema)
	}
	if PhaseData != "data" {
		t.Errorf("expected data, got %s", PhaseData)
	}
	if PhaseValidate != "validate" {
		t.Errorf("expected validate, got %s", PhaseValidate)
	}
}

func TestPipelineConfig(t *testing.T) {
	cfg := PipelineConfig{
		SkipPrecheck:    true,
		SkipSchema:      false,
		SkipData:        false,
		SkipValidate:    true,
		OnErrorContinue: false,
	}
	if !cfg.SkipPrecheck {
		t.Error("SkipPrecheck should be true")
	}
	if cfg.SkipSchema {
		t.Error("SkipSchema should be false")
	}
}

func TestPipelineResult(t *testing.T) {
	r := PipelineResult{
		Phase:   PhaseSchema,
		Success: true,
		Error:   nil,
	}
	if r.Phase != PhaseSchema {
		t.Error("phase mismatch")
	}
	if !r.Success {
		t.Error("should be success")
	}
}

// TestSchemaHasFailedTables anchors the OnError=skip warn surface (🟡1
// supplement): runDDL can aggregate err == nil while individual tables sit
// failed in the checkpoint — the schema phase must finish warn-completed,
// not silently green.
func TestSchemaHasFailedTables(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, err := checkpoint.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{cpMgr: m}

	if o.schemaHasFailedTables() {
		t.Error("empty checkpoint must report no failures")
	}
	if err := m.RegisterSchemaTables([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if o.schemaHasFailedTables() {
		t.Error("pending tables must not count as failures")
	}
	if err := m.MarkSchemaTableCompleted("a"); err != nil {
		t.Fatal(err)
	}
	if o.schemaHasFailedTables() {
		t.Error("one completed table must not count as failures")
	}
	if err := m.MarkSchemaTableFailed("b", "create index boom"); err != nil {
		t.Fatal(err)
	}
	if !o.schemaHasFailedTables() {
		t.Error("failed schema table must surface as warn")
	}

	// nil-safe: no checkpoint manager at all.
	if (&Orchestrator{}).schemaHasFailedTables() {
		t.Error("nil cpMgr must be false")
	}
}
