package webapi

import (
	"context"
	"sync"
	"testing"
	"time"
)

// compare_heartbeat_test.go anchors the #t95 P1 possibly-stuck heartbeat:
//
//	①  a running task whose LastProgressAt stalls beyond the threshold gets
//	   flagged possibly_stuck (persisted + broadcast).
//	②  a task whose progress keeps landing inside the threshold is NEVER
//	   mislabeled (normal progress doesn't trip the flag).
//	③  a task that left the running state stops the heartbeat immediately.

// TestCompareHeartbeatFlagsStuckTask (①): no progress within the threshold
// → possibly_stuck lands on the task (persisted via the server) within a
// bounded time.
func TestCompareHeartbeatFlagsStuckTask(t *testing.T) {
	s, _ := newTestServer(t)
	prev := compareHeartbeatTick
	compareHeartbeatTick = 5 * time.Millisecond
	defer func() { compareHeartbeatTick = prev }()

	task := &CompareTask{
		ID:             "abc12345",
		Status:         CompareStatusRunning,
		LastProgressAt: time.Now().Add(-1 * time.Minute), // stalled well past threshold
	}
	var mu sync.Mutex
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		s.compareHeartbeat(ctx, task, &mu, 50*time.Millisecond)
	}()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		stuck := task.PossiblyStuck
		mu.Unlock()
		if stuck {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("possibly_stuck flag never set within 5s")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

// TestCompareHeartbeatNoFalsePositiveOnFreshProgress (②): progress events
// landing inside the threshold keep the flag clear — a slow-but-moving run
// must never be mislabeled stuck.
func TestCompareHeartbeatNoFalsePositiveOnFreshProgress(t *testing.T) {
	s, _ := newTestServer(t)
	prev := compareHeartbeatTick
	compareHeartbeatTick = 5 * time.Millisecond
	defer func() { compareHeartbeatTick = prev }()

	task := &CompareTask{
		ID:             "abc12346",
		Status:         CompareStatusRunning,
		LastProgressAt: time.Now(),
	}
	var mu sync.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Simulate a healthy run: a progress event lands on every tick (the
	// onTableDone/onChunkProgress callbacks refresh LastProgressAt).
	stop := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		ticker := time.NewTicker(4 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				mu.Lock()
				task.LastProgressAt = time.Now()
				mu.Unlock()
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.compareHeartbeat(ctx, task, &mu, 50*time.Millisecond)
	}()

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	stuck := task.PossiblyStuck
	mu.Unlock()
	stopOnce.Do(func() { close(stop) })
	cancel()
	<-done

	if stuck {
		t.Fatalf("fresh-progress task misflagged possibly_stuck")
	}
}

// TestCompareHeartbeatStopsWhenTaskFinished (③): once the task leaves the
// running state the heartbeat goroutine returns on its next scan — it must
// not keep flagging a finished task.
func TestCompareHeartbeatStopsWhenTaskFinished(t *testing.T) {
	s, _ := newTestServer(t)
	prev := compareHeartbeatTick
	compareHeartbeatTick = 5 * time.Millisecond
	defer func() { compareHeartbeatTick = prev }()

	task := &CompareTask{ID: "abc12347", Status: CompareStatusCompleted}
	var mu sync.Mutex
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		s.compareHeartbeat(ctx, task, &mu, 50*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("heartbeat did not stop on a finished task within 5s")
	}
}
