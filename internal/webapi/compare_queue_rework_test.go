package webapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A1 rework regressions (P0-1 and P1-1 from the adversarial review).
//
// P0-1: handleCancelCompare's running path used to return while still holding
// compare.mu, permanently deadlocking the whole compare domain.
// P1-1: a corrupt queued task.json must fail loudly, not vanish silently.

// TestCompareTask_CancelRunningReleasesSlot is the P0-1 guard: a successful
// running-cancel answers 200 "cancelling" AND releases compare.mu, so the very
// next compare operation (a POST here) can proceed. On the buggy code the
// POST below blocks forever and the watchdog fires.
func TestCompareTask_CancelRunningReleasesSlot(t *testing.T) {
	s, _ := newTestServer(t)

	// Make the slot LOOK occupied by a live run we can observe: a fake
	// cancel func plus a matching runningID is exactly the state
	// handleCancelCompare's running branch keys off.
	s.compare.cancel = func() {}
	s.compare.runningID = "abcd1234"

	// Cancel through the real router (chi supplies {compareID}).
	w, req := doReq("POST", compareURL("abcd1234")+"/cancel", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("running cancel status = %d body=%s", w.Code, w.Body.String())
	}
	if !containsJSONField(w.Body.String(), "status", "cancelling") {
		t.Fatalf("running cancel body = %s, want status=cancelling", w.Body.String())
	}

	// THE ASSERTION THAT CATCHES P0-1: compare.mu must be free right now.
	// A watchdog converts the deadlock into a bounded failure instead of a
	// hung suite.
	slotFree := make(chan struct{})
	go func() {
		s.compare.mu.Lock()
		s.compare.mu.Unlock()
		close(slotFree)
	}()
	select {
	case <-slotFree:
	case <-time.After(3 * time.Second):
		t.Fatalf("compare.mu still held after a running cancel — P0-1 lock leak")
	}

	// And the domain still works: a POST now succeeds (201 running, since the
	// fake slot was cleared by nothing here — drop it first).
	s.compare.cancel = nil
	s.compare.runningID = ""
	w2, req2 := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"127.0.0.1","port":1},"target":{"host":"127.0.0.1","port":2}}`)
	s.handleCreateCompare(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("post after running cancel = %d body=%s (compare domain still wedged?)",
			w2.Code, w2.Body.String())
	}
	compareWaitSlotIdle(t, s)
}

// TestCompareTask_CancelUnknownStillConflict pins that the non-running path
// still 409s and still releases the lock (no behaviour change for unknown ids).
func TestCompareTask_CancelUnknownStillConflict(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", compareURL("abcd1234")+"/cancel", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("unknown cancel status = %d body=%s", w.Code, w.Body.String())
	}
	slotFree := make(chan struct{})
	go func() {
		s.compare.mu.Lock()
		s.compare.mu.Unlock()
		close(slotFree)
	}()
	select {
	case <-slotFree:
	case <-time.After(3 * time.Second):
		t.Fatalf("compare.mu held after a 409 cancel")
	}
}

// TestCompareTask_CorruptQueuedRecordFailsLoudly is the P1-1 guard: a queued
// task.json that will not decode must become an explicit failed record (it has
// no status field to read, so recovery cannot know it was queued — the point
// is that it is NEVER silently absent from the history).
func TestCompareTask_CorruptQueuedRecordFailsLoudly(t *testing.T) {
	s, _ := newTestServer(t)

	dir := s.compareDir("deadbeef")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Truncated JSON: unparseable, and — crucially — carries no status.
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(`{"id":"deadbeef","name":`), 0o644); err != nil {
		t.Fatal(err)
	}

	s.recoverStaleCompares()

	task, err := s.loadCompareTask("deadbeef")
	if err != nil || task == nil {
		t.Fatalf("corrupt task not repaired into a readable record: err=%v task=%v", err, task)
	}
	if task.Status != CompareStatusFailed {
		t.Fatalf("corrupt task status = %q, want failed", task.Status)
	}
	if task.Error == "" {
		t.Fatalf("corrupt task carries no explanatory error")
	}
	if task.FinishedAt == nil {
		t.Fatalf("corrupt task carries no finished_at")
	}
}

// TestCompareTask_FifoQueueSeqStrictOrder pins that QueueSeq — not the random
// uuid tie-break — decides same-tick FIFO order (P2-1).
func TestCompareTask_FifoQueueSeqStrictOrder(t *testing.T) {
	now := time.Now()
	a := &CompareTask{ID: "zzzzzzzz", CreatedAt: now, QueueSeq: 1}
	b := &CompareTask{ID: "aaaaaaaa", CreatedAt: now, QueueSeq: 2}
	if !queuedBefore(a, b) {
		t.Fatalf("QueueSeq order violated: seq1 should precede seq2 regardless of id")
	}
	// Legacy records (no QueueSeq) still fall back to CreatedAt then id.
	older := &CompareTask{ID: "zzzzzzzz", CreatedAt: now.Add(-time.Second)}
	newer := &CompareTask{ID: "aaaaaaaa", CreatedAt: now}
	if !queuedBefore(older, newer) {
		t.Fatalf("legacy CreatedAt ordering violated")
	}
}

// containsJSONField reports whether body is a JSON object whose field equals
// want (the cancel handlers answer small string maps).
func containsJSONField(body, field, want string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false
	}
	v, ok := m[field].(string)
	return ok && v == want
}
