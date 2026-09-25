package webapi

import (
	"context"
	"fmt"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/orchestrator"
	"github.com/michaelliuyuan/timstool/internal/store"
)

func TestZZDebugDemotion(t *testing.T) {
	s, _, _ := newCDCServer(t)
	body := `{"name":"dbg","source":{"host":"pg","port":5432,"user":"u","password":"p","database":"d"},
		"target":{"host":"t","port":4000,"user":"u","password":"p","database":"d"},
		"opts":{"cdc_chain":true}}`
	w, req := doReq("POST", "/api/v1/tasks", body)
	s.router.ServeHTTP(w, req)
	taskID := ExtractTaskID(t, w.Body.String())

	orig := runPipeline
	defer func() { runPipeline = orig }()
	runPipeline = func(ctx context.Context, cfg config.Config, pc orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
		rs := []orchestrator.PipelineResult{
			{Phase: "schema", Success: true},
			{Phase: "data", Success: true},
			{Phase: orchestrator.PhaseValidate, Success: false},
		}
		return rs, fmt.Errorf("data validation failed: 1/2 tables failed")
	}

	cfg := config.Config{}
	cfg.Migration.CDCChain = true
	cfg.Migration.ChainStartLSN = "0/1"
	cfg.Migration.CheckpointDir = t.TempDir()

	before, _ := s.store.GetTask(taskID)
	t.Logf("before: status=%q", before.Status)

	s.runMigration(context.Background(), taskID, cfg, 1)

	after, err := s.store.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after:  status=%q err=%q", after.Status, after.Error)

	for _, e := range s.logCollector.GetBuffer(taskID).GetAll() {
		t.Logf("log[%s]: %s", e.Level, e.Message)
	}

	// also check store direct
	st2 := s.store
	t.Logf("store type: %T", st2)

	_ = store.TaskStatusCreated
}
