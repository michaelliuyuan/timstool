package webapi

// MS-11f 笔① anchors (post gate removal): destructive endpoints run
// ungated, audit trail structure, anomaly-stop alarm (trigger /
// no-trigger / no-repeat).

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
)

// Destructive endpoints run ungated: POST /cdc/stop with no token at all
// reaches the handler (supervisor nil => 200 "not wired").
func TestDestructiveRoutesUngated(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/cdc/stop", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /cdc/stop without token: %d (want 200): %s", w.Code, w.Body.String())
	}
}

// POST /cdc/replica-identity through the real handler (config + fake
// executor), no token: 200 ok:true.
func TestReplicaIdentityRouteUngated(t *testing.T) {
	s, _, _ := newCDCServer(t)
	s.replicaIdentityExec = &fakeReplicaExec{canAlter: map[string]bool{"public.a": true}}
	body := `{"confirm":"ALTER","tables":["a"]}`

	w, req := doReq("POST", "/api/v1/cdc/replica-identity", body)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("replica-identity without token: %d (want 200 ok:true): %s", w.Code, w.Body.String())
	}
}

// Reads stay open (ruling: GET/status faces carry little leakage): /cdc/status
// answers 200 with no token at all.
func TestSecurityGateReadsStayOpen(t *testing.T) {
	s, _ := newTestServer(t)
	w, req := doReq("GET", "/api/v1/cdc/status", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /cdc/status without token: %d (want 200)", w.Code)
	}
}

// Audit structure: every audited op leaves a JSONL line with the full field
// set; the direct peer IP is recorded; a successful stop advances
// lastStopAuditAt.
func TestAuditTrailStructure(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/cdc/stop", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stop failed: %d %s", w.Code, w.Body.String())
	}
	w, req = doReq("POST", "/api/v1/cdc/stop", "")
	s.router.ServeHTTP(w, req)

	f, err := os.Open(filepath.Join(s.dataDir, "audit", "cdc-ops.jsonl"))
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	defer f.Close()
	var entries []auditEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e auditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("audit line not json: %v", err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 2 {
		t.Fatalf("audit entries = %d (want 2)", len(entries))
	}
	first := entries[0]
	if first.Result != "success" || first.Action != "cdc.stop" {
		t.Fatalf("success entry wrong: %+v", first)
	}
	if first.IP == "" || first.Method != "POST" || first.Endpoint != "/api/v1/cdc/stop" {
		t.Fatalf("entry fields missing: %+v", first)
	}
	if first.TS.IsZero() {
		t.Fatalf("entry has no timestamp")
	}
	if ts := s.lastStopAuditTime(); ts.IsZero() || ts.Before(first.TS) {
		t.Fatalf("lastStopAuditAt must advance on a successful stop: %v", ts)
	}
}

// stubCDCProvider pins the status-file view the anomaly checker reads.
type stubCDCProvider struct{ state string }

func (p stubCDCProvider) StatusView() cdcStatusView {
	return cdcStatusView{State: p.state, Running: p.state == string(cdc.LivenessRunning)}
}

func newAnomalyServer(t *testing.T) (*Server, *CDCSupervisor, *stubCDCProvider) {
	t.Helper()
	s, _ := newTestServer(t)
	s.cdcEnabled = true
	sup := newTestSupervisor(t, true)
	sup.SetFactory(func() (supervisedProcess, error) { return newFakeProc(4242), nil })
	prov := &stubCDCProvider{state: string(cdc.LivenessRunning)}
	s.SetCDCStatusProvider(prov)
	s.cdcSupervisor = sup
	if _, err := sup.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Start flips to Running on the supervise goroutine — pin it before the
	// checker's first observation or the arm step races the state flip.
	if !waitForState(t, sup, StateRunning, 2*time.Second) {
		t.Fatalf("supervisor never reached running")
	}
	return s, sup, prov
}

// Trigger form: running → stopped with NO audit stop ⇒ exactly one alarm
// after the one-tick grace, and a still-stopped CDC does not re-alarm
// (ruling seq83 #3: transition-edge, no periodic repeat).
func TestAnomalyAlarmFiresOncePerEdge(t *testing.T) {
	s, sup, prov := newAnomalyServer(t)
	c := newCDCAnomalyChecker(s, time.Hour, nil) // interval irrelevant: check() driven directly

	c.check() // observes running
	if c.lastSeenRunningAt.IsZero() {
		t.Fatalf("running observation must arm the checker")
	}
	sup.Stop(context.Background())
	prov.state = string(cdc.LivenessNotRunning)

	c.check() // grace tick
	if got := len(s.cdcAlarms()); got != 0 {
		t.Fatalf("grace tick must not alarm yet: %d", got)
	}
	c.check() // still un-audited ⇒ alarm
	alarms := s.cdcAlarms()
	if len(alarms) != 1 {
		t.Fatalf("alarms = %d (want exactly 1)", len(alarms))
	}
	if !strings.Contains(alarms[0].Message, "异常停止") {
		t.Fatalf("alarm wording: %s", alarms[0].Message)
	}
	c.check() // still stopped: no repeat
	c.check()
	if got := len(s.cdcAlarms()); got != 1 {
		t.Fatalf("still-stopped must not re-alarm: %d", got)
	}
}

// No-trigger form A: an AUDITED operator stop covers the transition.
func TestAnomalyAlarmSilentWhenStopIsAudited(t *testing.T) {
	s, sup, prov := newAnomalyServer(t)
	c := newCDCAnomalyChecker(s, time.Hour, nil)
	c.check() // running observed

	// operator stop through the audited route ⇒ success audit lands
	w, req := doReq("POST", "/api/v1/cdc/stop", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("audited stop failed: %d %s", w.Code, w.Body.String())
	}
	_ = sup
	prov.state = string(cdc.LivenessNotRunning)
	c.check()
	c.check()
	if got := len(s.cdcAlarms()); got != 0 {
		t.Fatalf("audited stop must not alarm: %d", got)
	}
}

// No-trigger form B: a fail-closed DDL halt is a known cause (its own
// fatal_error surface), never an anomaly.
func TestAnomalyAlarmSilentOnHalt(t *testing.T) {
	s, sup, prov := newAnomalyServer(t)
	c := newCDCAnomalyChecker(s, time.Hour, nil)
	c.check()
	sup.Stop(context.Background())
	prov.state = string(cdc.LivenessHalted)
	c.check()
	c.check()
	if got := len(s.cdcAlarms()); got != 0 {
		t.Fatalf("halt must not alarm: %d", got)
	}
}

// No-trigger form C: a fresh web process that never observed running must
// not alarm (zero false positives at startup).
func TestAnomalyAlarmSilentAtStartup(t *testing.T) {
	s, sup, prov := newAnomalyServer(t)
	c := newCDCAnomalyChecker(s, time.Hour, nil)
	sup.Stop(context.Background())
	prov.state = string(cdc.LivenessNotRunning)
	c.check()
	c.check()
	if got := len(s.cdcAlarms()); got != 0 {
		t.Fatalf("startup without a running observation must not alarm: %d", got)
	}
}

// pen3 trigger form (ruling seq109 #2, te seq102/107 🔴1): a NON-TERMINAL
// supervisor state ({Starting, Stopping}) that lingers past 3×interval is a
// dead chain in limbo — it joins the dead cluster and alarms exactly once;
// the limbo persisting must NOT re-enter the budget window (no periodic
// re-alarm). Both leak shapes are pinned: the Starting leak (pure kill +
// failed revival) and the Stopping leak (stop stuck on an orphaned done).
func TestAnomalyAlarmFiresOnLingeringNonTerminal(t *testing.T) {
	for _, shape := range []struct {
		name  string
		state CDCState
	}{
		{"starting_leak", StateStarting}, // te seq107: pure kill+revival-fail
		{"stopping_leak", StateStopping}, // te seq102: stop in the limbo
	} {
		t.Run(shape.name, func(t *testing.T) {
			s, sup, prov := newAnomalyServer(t)
			c := newCDCAnomalyChecker(s, time.Hour, nil)
			c.check() // running observed

			// pin the limbo shape: control pinned, liveness stale
			sup.mu.Lock()
			sup.state = shape.state
			sup.mu.Unlock()
			prov.state = "stale"

			c.check() // first non-terminal tick: arms the clock, running cluster
			if c.nonTerminalSince.IsZero() {
				t.Fatalf("non-terminal tick must arm the linger clock")
			}
			if got := len(s.cdcAlarms()); got != 0 {
				t.Fatalf("within budget must not alarm: %d", got)
			}

			// age the linger clock past 3×interval (4× to be unambiguous)
			c.nonTerminalSince = time.Now().Add(-4 * c.interval)
			c.check() // past budget: enters the judgment — grace tick
			if got := len(s.cdcAlarms()); got != 0 {
				t.Fatalf("grace tick must not alarm yet: %d", got)
			}
			c.check() // still un-audited ⇒ alarm (wording names the state)
			alarms := s.cdcAlarms()
			if len(alarms) != 1 || !strings.Contains(alarms[0].Message, "滞留非终态") ||
				!strings.Contains(alarms[0].Message, string(shape.state)) {
				t.Fatalf("lingering %s must alarm once with the state named: %+v", shape.state, alarms)
			}
			c.check() // limbo persists: no re-alarm (aged clock, no re-entry)
			c.check()
			if got := len(s.cdcAlarms()); got != 1 {
				t.Fatalf("persistent limbo must not re-alarm: %d", got)
			}
		})
	}
}

// pen3 no-trigger form: NORMAL transitions pass through the non-terminal
// pair well inside the budget — still counting toward the running cluster,
// and the audited stop keeps the alarm silent; leaving the pair clears the
// clock (a Starting segment followed by Stopping keeps it armed — the clock
// spans the pair).
func TestAnomalyAlarmSilentWhenNonTerminalWithinBudget(t *testing.T) {
	s, sup, prov := newAnomalyServer(t)
	c := newCDCAnomalyChecker(s, time.Hour, nil)
	c.check() // running observed

	// restart in flight: Starting observed within the budget
	sup.mu.Lock()
	sup.state = StateStarting
	sup.mu.Unlock()
	c.check()
	if c.nonTerminalSince.IsZero() {
		t.Fatalf("non-terminal tick must arm the linger clock")
	}

	// graceful stop right after (Starting → Stopping: same clock, no reset)
	sup.mu.Lock()
	sup.state = StateStopping
	sup.mu.Unlock()
	c.check()
	if c.nonTerminalSince.IsZero() {
		t.Fatalf("clock must span the non-terminal pair")
	}

	// audit lands + process exits: stopped, covered by the audited stop
	s.appendAudit(auditEntry{Action: "cdc.stop", Result: "success"})
	sup.mu.Lock()
	sup.state = StateStopped
	sup.mu.Unlock()
	prov.state = string(cdc.LivenessNotRunning)
	c.check()
	c.check()
	if got := len(s.cdcAlarms()); got != 0 {
		t.Fatalf("budgeted transition + audited stop must not alarm: %d", got)
	}
	if !c.nonTerminalSince.IsZero() {
		t.Fatalf("leaving the non-terminal pair must clear the linger clock")
	}
}

// /cdc/status surfaces the alarm ring (.alarms) once one exists.
func TestCDCStatusSurfacesAlarms(t *testing.T) {
	s, _ := newTestServer(t)
	s.cdcEnabled = true // the disabled branch answers before the alarms face
	s.pushAlarm("行一")
	w, req := doReq("GET", "/api/v1/cdc/status", "")
	s.router.ServeHTTP(w, req)
	var resp struct {
		Alarms []CDCAlarm `json:"alarms"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Alarms) != 1 || resp.Alarms[0].Message != "行一" {
		t.Fatalf("alarms face: %+v", resp.Alarms)
	}
}
