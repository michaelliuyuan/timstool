package webapi

import (
	"context"
	"sync"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
	"go.uber.org/zap"
)

// CDCWatchdog is the in-process liveness guard for the CDC child (P1 ruling,
// #t5): the supervisor's own restart loop only covers children it spawned;
// three gaps let CDC stay dead for hours until a human noticed —
//  1. an ADOPTED child dies (no supervise loop owns it),
//  2. the supervisor exhausts its restart cap (state=failed) and gives up,
//  3. web restarts while CDC was previously running but neither alive
//     (stale status) nor adopted — nothing ever starts it again.
//
// The watchdog ticks every interval and, while CDC is DESIRED running
// (started or adopted at least once, not explicitly stopped), revives it by
// reusing the supervisor's Start path with a fixed backoff between revive
// attempts (crash-storm guard). Halted (self-fatal) CDC is never revived —
// that needs a human. Status is surfaced via /cdc/status (.watchdog).

// CDCWatchdogStatus is the watchdog face on the status endpoint.
type CDCWatchdogStatus struct {
	Revives         int       `json:"revives"`
	LastReviveAt    time.Time `json:"last_revive_at,omitempty"`
	LastReviveState string    `json:"last_revive_state,omitempty"` // supervisor state that triggered the revive
}

// cdcWatchdog guards one CDCSupervisor.
type cdcWatchdog struct {
	sv       *CDCSupervisor
	interval time.Duration // tick; also the min spacing between revives (backoff)
	pidAlive func(int) bool
	log      *zap.Logger

	statusFile string // for the startup rebuild decision (P1 gap 3)
	stale      time.Duration

	mu      sync.Mutex
	desired bool // CDC should be running: started/adopted at least once, not stopped
	revives int
	lastAt  time.Time
	lastSt  string

	stop  chan struct{}
	revCh chan struct{} // signals a revive happened (tests)
}

// newCDCWatchdog builds the watchdog and computes the initial desired flag
// (P1 gap 3: rebuild at web startup). Call after Supervisor.Adopt.
func newCDCWatchdog(sv *CDCSupervisor, statusFile string, stale time.Duration, interval time.Duration, log *zap.Logger) *cdcWatchdog {
	w := &cdcWatchdog{
		sv:         sv,
		interval:   interval,
		pidAlive:   pidAlive,
		log:        log,
		statusFile: statusFile,
		stale:      stale,
		stop:       make(chan struct{}),
	}
	st := sv.Status()
	switch st.State {
	case StateRunning, StateStarting, StateAdopted, StateStopping:
		// Adopt/start already happened in this process: desired.
		w.desired = true
	case StateStopped, StateFailed:
		// Was CDC running before this web start? Adopt only takes live
		// children; a stale-but-not-halted record means the previous CDC
		// died with the previous web — rebuild it (ruling ③).
		if sv.cfg.Enable && statusFile != "" {
			if f, err := cdc.ReadStatusFile(statusFile); err == nil && f.PID > 0 &&
				f.State != cdc.CDCSelfHalted && time.Since(f.Timestamp) <= stale {
				w.desired = true
				if w.log != nil {
					w.log.Info("cdc watchdog: previous CDC died with the web process; will rebuild",
						zap.Int("prev_pid", f.PID))
				}
			}
		}
	}
	return w
}

// NotifyStarted marks CDC desired (called on successful Start).
func (w *cdcWatchdog) NotifyStarted() {
	w.mu.Lock()
	w.desired = true
	w.mu.Unlock()
}

// NotifyStopped clears desired (called on Stop — explicit operator intent).
func (w *cdcWatchdog) NotifyStopped() {
	w.mu.Lock()
	w.desired = false
	w.mu.Unlock()
}

// Status returns the watchdog face (thread-safe).
func (w *cdcWatchdog) Status() CDCWatchdogStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return CDCWatchdogStatus{
		Revives:         w.revives,
		LastReviveAt:    w.lastAt,
		LastReviveState: w.lastSt,
	}
}

// Run is the watchdog loop; blocks until stop is closed.
func (w *cdcWatchdog) Run() {
	if w.interval <= 0 {
		w.interval = 30 * time.Second
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.tick()
		}
	}
}

// Stop terminates the loop.
func (w *cdcWatchdog) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
}

// tick performs one liveness check + revive decision.
func (w *cdcWatchdog) tick() {
	w.mu.Lock()
	desired := w.desired
	w.mu.Unlock()
	if !desired || !w.sv.cfg.Enable {
		return
	}
	st := w.sv.Status()
	switch st.State {
	case StateRunning, StateStarting, StateStopping:
		return // supervise loop owns these
	case StateAdopted:
		if w.pidAlive(st.PID) {
			return // adopted child still alive
		}
		// Adopted child died: drop the adoption so Start can spawn fresh.
		w.sv.abandonAdopted(w.pidAlive)
	case StateStopped, StateFailed:
		// failed = restart cap exhausted (gap 2); stopped-while-desired is
		// transient (crash between Wait and restart) or post-failed.
	default:
		return
	}
	w.revive(string(st.State))
}

// revive respawns CDC, honoring the min spacing between attempts.
func (w *cdcWatchdog) revive(fromState string) {
	w.mu.Lock()
	if !w.lastAt.IsZero() && time.Since(w.lastAt) < w.interval {
		w.mu.Unlock()
		return // backoff: at most one revive per interval (crash-storm guard)
	}
	w.mu.Unlock()

	if _, err := w.sv.Start(context.Background()); err != nil {
		if w.log != nil {
			w.log.Warn("cdc watchdog: revive failed", zap.String("from_state", fromState), zap.Error(err))
		}
		return
	}
	w.mu.Lock()
	w.revives++
	w.lastAt = time.Now()
	w.lastSt = fromState
	w.desired = true
	w.mu.Unlock()
	if w.log != nil {
		w.log.Info("cdc watchdog: revived CDC child", zap.String("from_state", fromState))
	}
	if w.revCh != nil {
		select {
		case w.revCh <- struct{}{}:
		default:
		}
	}
}
