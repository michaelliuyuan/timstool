package webapi

// MS-11g 笔② anchors: three small faces —
//   🟢1 audited-start seeding of the anomaly checker (narrow first-tick
//      kill window, te seq118 / adversarial seq130 shape)
//   🟢2 reconcileStatusFace (read-face/control-truth reconciliation)
//   🟡  PUT /cdc/config source.database empty/whitespace → 400 (te seq143,
//      ruling seq144 — field-conditional, partial updates stay legal)

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
)

// 🟢1: an audited operator start seeds the attribution clock — a kill
// before ANY checker tick observed running still alarms (the pre-pen2
// behavior was silence: te two-round zero-alarm observation).
func TestAnomalyAlarmSeededByAuditedStart(t *testing.T) {
	s, sup, prov := newAnomalyServer(t)
	c := newCDCAnomalyChecker(s, time.Hour, nil)
	// the fixture wires the supervisor after NewServer, so the server's own
	// checker slot is nil — wire our driven checker so the appendAudit hook
	// seeds exactly what we assert (the prod path: NewServer(sup≠nil) owns
	// both).
	s.cdcAnomaly = c
	if !c.lastSeen().IsZero() {
		t.Fatalf("precondition: checker has observed nothing yet")
	}

	// operator start through the gated route ⇒ success audit lands ⇒ seed
	w, req := doReqToken("POST", "/api/v1/cdc/start", "", testAuthToken)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("audited start failed: %d %s", w.Code, w.Body.String())
	}
	if c.lastSeen().IsZero() {
		t.Fatalf("a successful cdc.start audit must seed the attribution clock")
	}

	// killed before any checker tick: pin the terminal shape directly
	prov.state = string(cdc.LivenessNotRunning)
	sup.mu.Lock()
	sup.state = StateFailed
	sup.mu.Unlock()
	c.check() // grace tick
	c.check() // un-audited death ⇒ alarm
	if alarms := s.cdcAlarms(); len(alarms) != 1 {
		t.Fatalf("seeded narrow-window kill must alarm exactly once, got %d", len(alarms))
	}
}

// 🟢1 zero-regression companion: with NO start audit (the startup guard's
// own shape), the same kill stays silent — seeding must not weaken the
// zero-false-positive startup semantics.
func TestAnomalyAlarmStartupSilenceWithoutStartAuditKept(t *testing.T) {
	s, sup, prov := newAnomalyServer(t)
	c := newCDCAnomalyChecker(s, time.Hour, nil)
	// no c.check(), no audited start — nothing attributed
	prov.state = string(cdc.LivenessNotRunning)
	sup.mu.Lock()
	sup.state = StateFailed
	sup.mu.Unlock()
	c.check()
	c.check()
	if got := len(s.cdcAlarms()); got != 0 {
		t.Fatalf("un-attributed startup death must stay silent: %d", got)
	}
}

// 🟢2: reconcileStatusFace table — the four ruled forms plus the two
// leader edge points (first-write window display, adoption respect).
func TestReconcileStatusFace(t *testing.T) {
	live := cdcStatusView{State: string(cdc.LivenessRunning), Running: true, PID: 7, LSN: "0/1"}
	stale := cdcStatusView{State: string(cdc.LivenessStale), PID: 7}
	halted := cdcStatusView{State: string(cdc.LivenessHalted), FatalError: "ddl halt"}
	noFile := cdcStatusView{State: string(cdc.LivenessNotRunning)}
	oldPid := cdcStatusView{State: string(cdc.LivenessStale), PID: 7} // previous incarnation

	stopped := &CDCControlStatus{State: StateStopped}
	starting := &CDCControlStatus{State: StateStarting, PID: 9}
	running := &CDCControlStatus{State: StateRunning, PID: 9}
	adopted := &CDCControlStatus{State: StateAdopted, PID: 7}

	// halted is never overridden (red-line face)
	if got := reconcileStatusFace(halted, stopped); got.State != string(cdc.LivenessHalted) || got.FatalError == "" {
		t.Fatalf("halted must keep its face: %+v", got)
	}
	// stopped control + lingering (stale) file reads not_running
	if got := reconcileStatusFace(stale, stopped); got.State != string(cdc.LivenessNotRunning) || got.PID != 0 || got.Running {
		t.Fatalf("stopped chain must read not_running: %+v", got)
	}
	// stopped control + LIVE file keeps the file truth (adoption respect)
	if got := reconcileStatusFace(live, stopped); got.State != string(cdc.LivenessRunning) || !got.Running {
		t.Fatalf("outside-supervisor live chain must keep the file truth: %+v", got)
	}
	// running control + old-pid file → convergence window: starting + control pid
	if got := reconcileStatusFace(oldPid, running); got.State != "starting" || got.PID != 9 {
		t.Fatalf("old-pid file under running control must converge: %+v", got)
	}
	// leader edge ①: first-write window (no file yet) → starting + control pid
	if got := reconcileStatusFace(noFile, starting); got.State != "starting" || got.PID != 9 {
		t.Fatalf("first-write window must show starting + control pid: %+v", got)
	}
	// converged: same pid running file under running control → untouched
	if got := reconcileStatusFace(live, adopted); got.State != string(cdc.LivenessRunning) || got.LSN != "0/1" {
		t.Fatalf("adopted/untouched shapes must pass through: %+v", got)
	}
	// nil control → untouched
	if got := reconcileStatusFace(stale, nil); got.State != string(cdc.LivenessStale) {
		t.Fatalf("nil control must pass through: %+v", got)
	}
}

// 🟡: PUT /cdc/config with an explicitly empty / whitespace source.database
// → 400 and nothing lands in config.yaml; partial updates without the
// field stay 200 (ruling: no partial-update false rejections).
func TestPutCDCConfigRejectsEmptyDatabase(t *testing.T) {
	s, _, cfgFile := newCDCServer(t)
	before := readFileOrEmpty(t, cfgFile)

	for _, body := range []string{
		`{"source":{"database":""}}`,
		`{"source":{"database":"   "}}`,
	} {
		w, req := doReq("PUT", "/api/v1/cdc/config", body)
		s.router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "database") {
			t.Fatalf("PUT %s = %d %s, want 400 naming database", body, w.Code, w.Body.String())
		}
	}
	// rejected PUTs must not persist anything
	if after := readFileOrEmpty(t, cfgFile); after != before {
		t.Fatalf("rejected PUT must not touch config.yaml")
	}

	// partial update WITHOUT the database field stays legal (field-conditional guard)
	w, req := doReq("PUT", "/api/v1/cdc/config", `{"source":{"password":"newsecret"}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial PUT without database field = %d %s, want 200", w.Code, w.Body.String())
	}

	// legal explicit database stays 200
	w, req = doReq("PUT", "/api/v1/cdc/config", `{"source":{"database":"proddb"}}`)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legal database PUT = %d %s, want 200", w.Code, w.Body.String())
	}
}

func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
