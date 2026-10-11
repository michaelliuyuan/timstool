package webapi

// MS-11o A2 anchors: negative durations clamp to 0 on BOTH display faces —
// the migration report's per-table duration and the task-phases API's
// per-phase duration. Legacy rows can carry FinishedAt before StartedAt
// (clock oddities); the display side must render 0 (parity with
// taskElapsedSeconds), never a negative duration.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeMS11oClampCheckpoint hand-builds a checkpoint.json for taskID with:
//   - table "inv_ok":  completed, FinishedAt 1h BEFORE StartedAt (inverted)
//   - table "normal":  completed, honest 1s duration
//   - phases.data:     finished, StartedAt 1h AFTER FinishedAt (inverted)
func writeMS11oClampCheckpoint(t *testing.T, taskID string) {
	t.Helper()
	now := time.Now()
	doc := map[string]any{
		"version":    "1.0",
		"created_at": now.Format(time.RFC3339Nano),
		"updated_at": now.Format(time.RFC3339Nano),
		"phase":      "validate",
		"tables": map[string]any{
			"inv_ok": map[string]any{
				"table_name":  "inv_ok",
				"state":       "completed",
				"rows_total":  100,
				"rows_done":   100,
				"started_at":  now.Format(time.RFC3339Nano),
				"finished_at": now.Add(-time.Hour).Format(time.RFC3339Nano),
			},
			"normal": map[string]any{
				"table_name":  "normal",
				"state":       "completed",
				"rows_total":  50,
				"rows_done":   50,
				"started_at":  now.Add(-2 * time.Second).Format(time.RFC3339Nano),
				"finished_at": now.Add(-time.Second).Format(time.RFC3339Nano),
			},
		},
		"phases": map[string]any{
			"data": map[string]any{
				"name":        "data",
				"status":      "completed",
				"started_at":  now.Add(time.Hour).Format(time.RFC3339Nano),
				"finished_at": now.Format(time.RFC3339Nano),
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	cpDir := filepath.Join(".checkpoint", taskID)
	if err := os.MkdirAll(cpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cpDir, "checkpoint.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTaskReportClampsNegativeTableDuration: the inverted table renders
// "0.000s", never "-59m59...s"; the honest table keeps its real duration.
func TestTaskReportClampsNegativeTableDuration(t *testing.T) {
	s, taskID := createPhaseTestTask(t, "ms11o-clamp")
	t.Chdir(t.TempDir())
	writeMS11oClampCheckpoint(t, taskID)

	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		t.Fatalf("GetTask: %v %v", task, err)
	}
	rep := s.buildTaskReport(task)

	var inv, normal string
	for _, tr := range rep.Tables {
		switch tr.TableName {
		case "inv_ok":
			inv = tr.Duration
		case "normal":
			normal = tr.Duration
		}
	}
	if inv == "" {
		t.Fatalf("inv_ok missing from report tables: %+v", rep.Tables)
	}
	if inv != "0.000s" {
		t.Errorf("inverted table duration = %q, want \"0.000s\" (clamped, not negative)", inv)
	}
	if normal != "1.000s" {
		t.Errorf("normal table duration = %q, want \"1.000s\" (real duration preserved)", normal)
	}
}

// TestTaskPhasesClampsNegativePhaseDuration: the inverted phase record
// reports duration 0 (raw Sub would be -3600), never a negative number.
func TestTaskPhasesClampsNegativePhaseDuration(t *testing.T) {
	s, taskID := createPhaseTestTask(t, "ms11o-phase")
	t.Chdir(t.TempDir())
	writeMS11oClampCheckpoint(t, taskID)

	phases := fetchPhases(t, s, taskID)
	for _, p := range phases {
		if p.Name == "data" {
			if p.Duration != 0 {
				t.Errorf("data phase duration = %v, want exactly 0 (raw would be -3600)", p.Duration)
			}
			return
		}
	}
	t.Fatal("data phase missing from phases payload")
}
