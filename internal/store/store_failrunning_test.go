package store

import (
	"testing"
)

// F-08-2 ride b anchor: tasks left "running" by a dead process must be
// failed in one shot at startup; other statuses untouched.
func TestFailAllRunning(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	for _, tc := range []struct {
		id     string
		status TaskStatus
	}{
		{"r1", TaskStatusRunning}, {"r2", TaskStatusRunning},
		{"p1", TaskStatusPaused}, {"c1", TaskStatusCompleted}, {"n1", TaskStatusCreated},
	} {
		if err := s.CreateTask(&Task{ID: tc.id, Name: tc.id, Status: tc.status, ConfigJSON: `{}`}); err != nil {
			t.Fatalf("CreateTask %s: %v", tc.id, err)
		}
	}

	n, err := s.FailAllRunning("interrupted by restart")
	if err != nil {
		t.Fatalf("FailAllRunning: %v", err)
	}
	if n != 2 {
		t.Fatalf("recovered count = %d, want 2", n)
	}

	for _, id := range []string{"r1", "r2"} {
		task, _ := s.GetTask(id)
		if task.Status != TaskStatusFailed {
			t.Errorf("%s status = %s, want failed", id, task.Status)
		}
		if task.Error == "" || task.FinishedAt == nil {
			t.Errorf("%s must carry an error and a finish time", id)
		}
	}
	for _, id := range []string{"p1", "c1", "n1"} {
		task, _ := s.GetTask(id)
		if task.Status == TaskStatusFailed {
			t.Errorf("%s must keep its status", id)
		}
	}
}
