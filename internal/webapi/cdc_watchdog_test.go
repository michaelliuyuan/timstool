package webapi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
)

func newTestWatchdog(sv *CDCSupervisor, statusFile string) *cdcWatchdog {
	w := newCDCWatchdog(sv, statusFile, 30*time.Second, 20*time.Millisecond, nil)
	// Deterministic liveness in tests: the sentinel 4194303 is "dead",
	// every other pid (the fake children) "alive".
	w.pidAlive = func(pid int) bool { return pid != 4194303 }
	w.revCh = make(chan struct{}, 8)
	return w
}

// P1 gap 1: an adopted child that dies must be revived (adoption dropped,
// fresh child spawned).
func TestWatchdogRevivesDeadAdoptedChild(t *testing.T) {
	sv := newTestSupervisor(t, true)
	spawned := 0
	sv.SetFactory(func() (supervisedProcess, error) {
		spawned++
		return newFakeProc(100 + spawned), nil
	})

	// Simulate an adoption of a now-dead PID (no real process 4194303).
	sv.mu.Lock()
	sv.state = StateAdopted
	sv.adopted = true
	sv.pid = 4194303 // reserved-ish high pid, not alive
	sv.mu.Unlock()

	w := newTestWatchdog(sv, "")
	w.NotifyStarted() // desired=true (adoption implies desired)
	go w.Run()
	defer w.Stop()

	select {
	case <-w.revCh:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never revived the dead adopted child")
	}
	if st := sv.Status(); st.State != StateRunning || st.Adopted {
		t.Fatalf("after revive: state=%v adopted=%v, want running/not-adopted", st.State, st.Adopted)
	}
	ws := w.Status()
	if ws.Revives != 1 || ws.LastReviveState != string(StateAdopted) || ws.LastReviveAt.IsZero() {
		t.Fatalf("watchdog status = %+v, want 1 revive from adopted", ws)
	}
}

// P1 gap 2: restart-cap exhaustion (state=failed) is revived while desired.
func TestWatchdogRevivesFailedSupervisor(t *testing.T) {
	sv := newTestSupervisor(t, true)
	crashes := 0
	sv.SetFactory(func() (supervisedProcess, error) {
		crashes++
		return &crashingProc{pid: crashes}, nil // instant crash loop → failed
	})
	if _, err := sv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !waitForState(t, sv, StateFailed, 5*time.Second) {
		t.Fatalf("supervisor never reached failed (state=%v)", sv.Status().State)
	}

	w := newTestWatchdog(sv, "")
	w.NotifyStarted()
	go w.Run()
	defer w.Stop()

	select {
	case <-w.revCh:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never revived the failed supervisor")
	}
	ws := w.Status()
	if ws.Revives != 1 || ws.LastReviveState != string(StateFailed) {
		t.Fatalf("watchdog status = %+v, want 1 revive from failed", ws)
	}
}

// Explicit Stop clears desired: the watchdog must NOT revive.
func TestWatchdogHonorsExplicitStop(t *testing.T) {
	sv := newTestSupervisor(t, true)
	spawned := 0
	sv.SetFactory(func() (supervisedProcess, error) {
		spawned++
		return newFakeProc(200 + spawned), nil
	})
	w := newTestWatchdog(sv, "")
	w.NotifyStarted()
	w.NotifyStopped() // operator pressed stop

	for i := 0; i < 5; i++ {
		w.tick()
	}
	if sv.Status().State != StateStopped || spawned != 0 {
		t.Fatalf("watchdog revived after explicit stop: state=%v spawned=%d", sv.Status().State, spawned)
	}
	if ws := w.Status(); ws.Revives != 0 {
		t.Fatalf("revives = %d, want 0", ws.Revives)
	}
}

// P1 gap 3 (startup rebuild): a fresh web whose status file says CDC was
// running recently (not halted, not adopted because the PID is dead) must
// compute desired=true and revive on the first tick.
func TestWatchdogRebuildsAtStartupFromStatusFile(t *testing.T) {
	dir := t.TempDir()
	statusFile := filepath.Join(dir, "status.json")
	st := cdc.CDCStatusFile{
		State:     cdc.CDCSelfRunning,
		PID:       4194303, // dead: adoption will not take it
		Timestamp: time.Now(),
	}
	if err := cdc.WriteStatusFile(statusFile, st); err != nil {
		t.Fatal(err)
	}

	sv := newTestSupervisor(t, true)
	spawned := 0
	sv.SetFactory(func() (supervisedProcess, error) {
		spawned++
		return newFakeProc(300 + spawned), nil
	})
	// No adoption happened (PID dead) — the watchdog must still decide
	// desired from the status file.
	w := newTestWatchdog(sv, statusFile)
	w.mu.Lock()
	desired := w.desired
	w.mu.Unlock()
	if !desired {
		t.Fatal("watchdog did not mark desired from a recent running status file")
	}
	w.tick()
	if spawned != 1 {
		t.Fatalf("startup rebuild did not spawn (spawned=%d)", spawned)
	}
	if ws := w.Status(); ws.Revives != 1 {
		t.Fatalf("revives = %d, want 1", ws.Revives)
	}
}

// A halted (self-fatal) CDC must not be rebuilt at startup — human action
// is required; same for an absent status file.
func TestWatchdogDoesNotRebuildHaltedOrMissing(t *testing.T) {
	dir := t.TempDir()
	haltedFile := filepath.Join(dir, "halted.json")
	st := cdc.CDCStatusFile{State: cdc.CDCSelfHalted, PID: 4194303, Timestamp: time.Now()}
	if err := cdc.WriteStatusFile(haltedFile, st); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"halted": haltedFile, "missing": filepath.Join(dir, "nope.json")} {
		sv := newTestSupervisor(t, true)
		spawned := 0
		sv.SetFactory(func() (supervisedProcess, error) {
			spawned++
			return newFakeProc(400 + spawned), nil
		})
		w := newTestWatchdog(sv, file)
		if w.desired {
			t.Fatalf("%s: watchdog marked desired", name)
		}
		w.tick()
		if spawned != 0 || w.Status().Revives != 0 {
			t.Fatalf("%s: watchdog revived (spawned=%d revives=%d)", name, spawned, w.Status().Revives)
		}
	}
}

// Backoff guard: at most one revive per interval even while the child keeps
// dying instantly.
func TestWatchdogReviveBackoff(t *testing.T) {
	sv := newTestSupervisor(t, true)
	sv.SetFactory(func() (supervisedProcess, error) { return &crashingProc{pid: 1}, nil })
	w := newTestWatchdog(sv, "")
	w.interval = 500 * time.Millisecond // long backoff for the test
	w.NotifyStarted()
	w.revive("failed")
	first := w.Status().LastReviveAt
	w.revive("failed") // within the backoff window: must be a no-op
	if ws := w.Status(); ws.Revives != 1 || !ws.LastReviveAt.Equal(first) {
		t.Fatalf("backoff violated: %+v", ws)
	}
}

// /cdc/status exposes the watchdog face (revives/last fields) end to end.
func TestCDCStatusExposesWatchdog(t *testing.T) {
	s, _ := newTestServer(t)
	sv := newTestSupervisor(t, true)
	spawned := 0
	sv.SetFactory(func() (supervisedProcess, error) {
		spawned++
		return newFakeProc(500 + spawned), nil
	})
	s.cdcEnabled = true
	s.cdcSupervisor = sv
	w := newTestWatchdog(sv, "")
	w.NotifyStarted()
	w.revive("failed")
	s.cdcWatchdog = w

	rr, req := doReq("GET", "/api/v1/cdc/status", "")
	s.handleCDCStatus(rr, req)
	body := rr.Body.String()
	for _, want := range []string{`"watchdog"`, `"revives":1`, `"last_revive_state":"failed"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status body missing %s: %s", want, body)
		}
	}
}
