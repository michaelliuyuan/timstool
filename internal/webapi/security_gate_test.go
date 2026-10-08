package webapi

// MS-11f 笔① anchors: destructive-endpoint token gate (three states),
// zero-echo, audit trail structure, anomaly-stop alarm (trigger /
// no-trigger / no-repeat), weak-token entropy advisory, CORS header.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
)

func doReqToken(method, path, body, token string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	return httptest.NewRecorder(), req
}

// Gate three states through the real router: unset = 403 fail-closed with a
// remediation hint; wrong/missing = 401; matching = handler runs.
func TestSecurityGateThreeStates(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetSecurityToken("") // unset: fail-closed

	w, req := doReqToken("POST", "/api/v1/cdc/stop", "", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unset token: %d (want 403): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "security.token") {
		t.Fatalf("403 must carry the remediation hint: %s", w.Body.String())
	}

	s.SetSecurityToken("gate-token-0123456789")
	w, req = doReqToken("POST", "/api/v1/cdc/stop", "", "wrong-token-value")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d (want 401): %s", w.Code, w.Body.String())
	}

	w, req = doReqToken("POST", "/api/v1/cdc/stop", "", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: %d (want 401): %s", w.Code, w.Body.String())
	}

	// supervisor nil => handleCDCStop answers 200 "not wired" — proves the
	// gate passed through to the handler on a matching token.
	w, req = doReqToken("POST", "/api/v1/cdc/stop", "", "gate-token-0123456789")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("matching token: %d (want 200): %s", w.Code, w.Body.String())
	}
}

// MS-11f 笔② 增补件 (adversarial P2, seq95 ruling): the seventh CDC write
// endpoint POST /cdc/replica-identity is gated too — same three states, with
// the 200 face driven through the real handler (config + fake executor).
func TestSecurityGateReplicaIdentitySeventhRoute(t *testing.T) {
	s, _, _ := newCDCServer(t)
	s.replicaIdentityExec = &fakeReplicaExec{canAlter: map[string]bool{"public.a": true}}
	body := `{"confirm":"ALTER","tables":["a"]}`

	s.SetSecurityToken("") // unset: fail-closed
	w, req := doReqToken("POST", "/api/v1/cdc/replica-identity", body, "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "security.token") {
		t.Fatalf("unset token: %d (want 403+hint): %s", w.Code, w.Body.String())
	}

	s.SetSecurityToken("gate-token-0123456789")
	w, req = doReqToken("POST", "/api/v1/cdc/replica-identity", body, "wrong-token-value")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d (want 401): %s", w.Code, w.Body.String())
	}
	w, req = doReqToken("POST", "/api/v1/cdc/replica-identity", body, "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: %d (want 401): %s", w.Code, w.Body.String())
	}

	// matching token reaches the handler: fake executor alters public.a => 200 ok
	w, req = doReqToken("POST", "/api/v1/cdc/replica-identity", body, "gate-token-0123456789")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("matching token: %d (want 200 ok:true): %s", w.Code, w.Body.String())
	}
}

// Reads stay open (ruling: GET/status faces carry little leakage): /cdc/status
// answers 200 with no token at all.
func TestSecurityGateReadsStayOpen(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetSecurityToken("gate-token-0123456789")
	w, req := doReqToken("GET", "/api/v1/cdc/status", "", "")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /cdc/status without token: %d (want 200)", w.Code)
	}
}

// Zero-echo (ruling seq83 #1): the token value must never appear in any
// response body — 401/403 faces, /cdc/status (with alarms), GET /cdc/config
// against a config.yaml that carries security.token.
func TestSecurityTokenZeroEcho(t *testing.T) {
	s, _, cfgFile := newCDCServer(t)
	const secret = "zero-echo-secret-0123456789"
	s.SetSecurityToken(secret)

	faces := func() []string {
		w1, r1 := doReqToken("POST", "/api/v1/cdc/stop", "", "wrong")
		s.router.ServeHTTP(w1, r1)
		w2, r2 := doReqToken("GET", "/api/v1/cdc/status", "", "")
		s.router.ServeHTTP(w2, r2)
		w3, r3 := doReqToken("GET", "/api/v1/cdc/config", "", "")
		s.router.ServeHTTP(w3, r3)
		return []string{w1.Body.String(), w2.Body.String(), w3.Body.String()}
	}()
	// seed an alarm so the .alarms face is exercised too
	s.pushAlarm("测试告警行")
	for _, body := range faces {
		if strings.Contains(body, secret) {
			t.Fatalf("token value echoed in response: %.200s", body)
		}
	}
	// config.yaml itself carries the token; GET /cdc/config must not leak it
	// (the summary struct has no security face at all).
	if err := os.WriteFile(cfgFile, []byte("source:\n  host: a\n  port: 1\ntarget:\n  host: b\n  port: 2\nsecurity:\n  token: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, req := doReqToken("GET", "/api/v1/cdc/config", "", "")
	s.router.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("GET /cdc/config leaked security.token: %s", w.Body.String())
	}
}

// Audit structure: denied and success both leave a JSONL line with the full
// field set; the direct peer IP is recorded; a successful stop advances
// lastStopAuditAt.
func TestAuditTrailStructure(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetSecurityToken("audit-token-0123456789")

	w, req := doReqToken("POST", "/api/v1/cdc/stop", "", "nope")
	s.router.ServeHTTP(w, req)
	w, req = doReqToken("POST", "/api/v1/cdc/stop", "", "audit-token-0123456789")
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
		t.Fatalf("audit entries = %d (want 2: denied + success)", len(entries))
	}
	denied, ok := entries[0], entries[1]
	if denied.Result != "denied" || denied.Action != "cdc.stop" {
		t.Fatalf("denied entry wrong: %+v", denied)
	}
	if denied.IP == "" || denied.Method != "POST" || denied.Endpoint != "/api/v1/cdc/stop" {
		t.Fatalf("denied entry fields missing: %+v", denied)
	}
	if denied.TS.IsZero() {
		t.Fatalf("denied entry has no timestamp")
	}
	if ok.Result != "success" {
		t.Fatalf("success entry wrong: %+v", ok)
	}
	if ts := s.lastStopAuditTime(); ts.IsZero() || ts.Before(denied.TS) {
		t.Fatalf("lastStopAuditAt must advance on a successful stop: %v", ts)
	}
}

// Entropy advisory (ruling seq85 #2): weak tokens WARN + audit, never refuse;
// strong tokens record nothing.
func TestWeakTokenEntropyAdvisory(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetSecurityToken("short")
	if !hasWarnAudit(s, "security.token_entropy") {
		t.Fatalf("short token must leave an entropy warn audit line")
	}
	s2, _ := newTestServer(t)
	s2.SetSecurityToken("12345678901234567890")
	if !hasWarnAudit(s2, "security.token_entropy") {
		t.Fatalf("pure-digit token must leave an entropy warn audit line")
	}
	s3, _ := newTestServer(t)
	s3.SetSecurityToken("strong-token-0123456789")
	if hasWarnAudit(s3, "security.token_entropy") {
		t.Fatalf("strong token must not warn")
	}
}

func hasWarnAudit(s *Server, action string) bool {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	for _, e := range s.auditRing {
		if e.Action == action && e.Result == "warn" {
			return true
		}
	}
	return false
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

	// operator stop through the gated route ⇒ success audit lands
	w, req := doReqToken("POST", "/api/v1/cdc/stop", "", testAuthToken)
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

// /cdc/status surfaces the alarm ring (.alarms) once one exists.
func TestCDCStatusSurfacesAlarms(t *testing.T) {
	s, _ := newTestServer(t)
	s.cdcEnabled = true // the disabled branch answers before the alarms face
	s.pushAlarm("行一")
	w, req := doReqToken("GET", "/api/v1/cdc/status", "", "")
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

// CORS form anchor: the preflight allow-list must include X-Auth-Token or
// the browser rejects the header before the gate ever sees it.
func TestCORSAllowsAuthTokenHeader(t *testing.T) {
	src := srcOf(t, "server.go")
	if !strings.Contains(src, `"X-Auth-Token"`) {
		t.Fatalf("AllowedHeaders must include X-Auth-Token")
	}
}

// tokenMatches shape: exact match passes; wrong value, wrong length and
// unicode variants fail (constant-time form is pinned by the hash-then-
// compare source anchor below).
func TestTokenMatches(t *testing.T) {
	s := &Server{authToken: "abcdef0123456789"}
	if !s.tokenMatches("abcdef0123456789") {
		t.Fatalf("exact match must pass")
	}
	for _, bad := range []string{"", "abcdef012345678", "abcdef0123456789x", "ABCDEF0123456789"} {
		if s.tokenMatches(bad) {
			t.Fatalf("mismatch %q must fail", bad)
		}
	}
	empty := &Server{}
	if empty.tokenMatches("") || empty.tokenMatches("anything") {
		t.Fatalf("unset gate: nobody passes")
	}
}

// Form anchor: the compare must hash BOTH sides first (flattening length)
// and then constant-time compare — a plain subtle.ConstantTimeCompare on the
// raw strings still short-circuits on length (adversarial seq84 #1).
func TestTokenCompareHashesBothSides(t *testing.T) {
	src := srcOf(t, "security_gate.go")
	i := strings.Index(src, "func (s *Server) tokenMatches")
	body := src[i:]
	for _, anchor := range []string{"sha256.Sum256([]byte(s.authToken))", "sha256.Sum256([]byte(presented))", "subtle.ConstantTimeCompare"} {
		if !strings.Contains(body, anchor) {
			t.Fatalf("tokenMatches missing form anchor: %s", anchor)
		}
	}
}
