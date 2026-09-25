package store

import (
	"testing"
)

// F-08-2 item 7 anchor ("Plan A"): run-conditioned writes succeed only for
// the generation stamped by SetTaskRun; a superseded generation sees
// affected==0 and must skip — the DB itself rejects the clobber even if the
// in-memory ownership check raced.
func TestRunConditionedWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	if err := s.CreateTask(&Task{ID: "t7", Name: "cond", Status: TaskStatusCreated, ConfigJSON: `{}`}); err != nil {
		t.Fatal(err)
	}

	// Generation 1 takes the row and writes Running.
	if err := s.SetTaskRun("t7", 1); err != nil {
		t.Fatal(err)
	}
	if task, _ := s.GetTask("t7"); task.Status != TaskStatusRunning {
		t.Fatalf("status = %s, want running", task.Status)
	}

	// Generation 2 supersedes (resume swap → SetTaskRun stamps 2).
	if err := s.SetTaskRun("t7", 2); err != nil {
		t.Fatal(err)
	}

	// Stale generation 1 terminal writes: all rejected (affected==0).
	ok, err := s.UpdateTaskStatusIfRun("t7", TaskStatusCompleted, 1)
	if err != nil || ok {
		t.Errorf("stale Completed write: ok=%v err=%v, want false/nil", ok, err)
	}
	ok, err = s.SetTaskErrorIfRun("t7", "boom", 1)
	if err != nil || ok {
		t.Errorf("stale error write: ok=%v err=%v, want false/nil", ok, err)
	}
	ok, err = s.SetTaskResultIfRun("t7", `{"x":1}`, 1)
	if err != nil || ok {
		t.Errorf("stale result write: ok=%v err=%v, want false/nil", ok, err)
	}
	ok, err = s.UpdateTaskProgressIfRun("t7", "data", 50, 5, 10, 500, 1000, 1)
	if err != nil || ok {
		t.Errorf("stale progress write: ok=%v err=%v, want false/nil", ok, err)
	}

	// The row must still reflect the fresh generation's Running state.
	task, _ := s.GetTask("t7")
	if task.Status != TaskStatusRunning || task.Error != "" || task.ResultJSON != "" || task.Phase != "" {
		t.Fatalf("stale writes leaked: %+v", task)
	}

	// Fresh generation 2 writes land.
	if ok, err := s.UpdateTaskProgressIfRun("t7", "data", 10, 1, 10, 100, 1000, 2); err != nil || !ok {
		t.Fatalf("fresh progress write: ok=%v err=%v", ok, err)
	}
	if ok, err := s.UpdateTaskStatusIfRun("t7", TaskStatusCompleted, 2); err != nil || !ok {
		t.Fatalf("fresh Completed write: ok=%v err=%v", ok, err)
	}
	task, _ = s.GetTask("t7")
	if task.Status != TaskStatusCompleted || task.Progress != 10 {
		t.Fatalf("fresh writes did not land: %+v", task)
	}
}
