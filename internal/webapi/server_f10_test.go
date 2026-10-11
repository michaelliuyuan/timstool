package webapi

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/orchestrator"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// F-10 group 1 (Plan B) anchor: a resumed task must re-run with the
// truncate target policy — the data migrator then clears the task's own
// table set before re-importing (the 1062 re-insert debt). A user-configured
// stronger policy (drop) is preserved.
func TestResumeForcesTruncatePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, configured, want string
	}{
		{"default-insert-becomes-truncate", ``, "truncate"},
		{"drop-is-preserved", `"TargetPolicy":"drop"`, "drop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := newTestServer(t)

			taskID := "t-f10-trunc"
			cfgJSON := `{"migration":{` + tc.configured + `}}`
			if err := st.CreateTask(&store.Task{ID: taskID, Name: "f10", Status: store.TaskStatusPaused, ConfigJSON: cfgJSON}); err != nil {
				t.Fatal(err)
			}

			got := make(chan string, 1)
			release := make(chan struct{})
			var entered int32
			oldPipeline := runPipeline
			defer func() { runPipeline = oldPipeline }()
			runPipeline = func(ctx context.Context, cfg config.Config, _ orchestrator.PipelineConfig) ([]orchestrator.PipelineResult, error) {
				if atomic.AddInt32(&entered, 1) == 1 {
					got <- cfg.Migration.TargetPolicy
				}
				<-release
				return nil, nil
			}

			w, req := doReq("POST", "/api/v1/tasks/"+taskID+"/resume", "")
			s.handleResumeTask(w, withChiParam(req, "taskID", taskID))
			if w.Code != http.StatusOK {
				t.Fatalf("resume: %d %s", w.Code, w.Body.String())
			}

			select {
			case policy := <-got:
				if policy != tc.want {
					t.Errorf("resumed run TargetPolicy = %q, want %q", policy, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("resumed run never entered its pipeline stub")
			}

			// The stored task config must stay UNTOUCHED (truncate is a
			// per-run override, not a persistent config mutation).
			task, _ := st.GetTask(taskID)
			if task == nil || (tc.configured != "" && task.ConfigJSON != cfgJSON) {
				t.Errorf("stored task config was mutated: %q", task.ConfigJSON)
			}

			close(release)
			deadline := time.Now().Add(2 * time.Second)
			for atomic.LoadInt32(&entered) < 1 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond) // let runMigration wind down
		})
	}
}
