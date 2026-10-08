package webapi

// MS-11f 笔① security core (P0): destructive-endpoint token gate + operation
// audit trail + anomaly-stop alarm. The incident being root-fixed: an
// external IP POSTed /cdc/stop unauthenticated and CDC sat silent-dead for 2h.
//
// Three planes:
//   1. gate — six destructive routes require X-Auth-Token == the STARTUP
//      SNAPSHOT of config security.token (a mid-run config.yaml edit never
//      affects the running gate; changing the token requires a web restart).
//      Unset token = fail-closed 403; missing/wrong token = 401 (the FE uses
//      the distinction: 403 shows a hint, 401 opens the token prompt).
//   2. audit — every gated request leaves a JSONL line (ts/ip/method/
//      endpoint/action/result/detail), DENIED attempts included. IP is the
//      direct peer RemoteAddr (ruling seq83 #2: XFF is forgeable while the
//      listener is 0.0.0.0 direct). An audit write failure never bricks
//      operations: retry once, then ERROR alarm + proceed with the record
//      kept in the in-memory ring (ruling seq85 #3).
//   3. alarm — a checker observes CDC liveness; a running→stopped transition
//      with NO successful stop audit newer than the last running observation
//      raises exactly one ERROR alarm (transition-edge semantics, ruling
//      seq83 #3). Deliberately independent of the watchdog: the incident
//      shape is an API stop that CLEARS desired, and the watchdog's
//      !desired early-return would skip exactly that case.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/michaelliuyuan/timstool/internal/cdc"
	"go.uber.org/zap"
)

// CDCAlarm is one anomaly-stop alarm row surfaced on GET /cdc/status
// (.alarms) for the dashboard to render as an ERROR line.
type CDCAlarm struct {
	TS      time.Time `json:"ts"`
	Message string    `json:"message"`
}

// auditEntry is one destructive-op record. Result: success (2xx) | denied
// (401/403 at the gate) | error (handler answered non-2xx) | warn (startup
// entropy advisory — not a request).
type auditEntry struct {
	TS       time.Time `json:"ts"`
	IP       string    `json:"ip"`
	Method   string    `json:"method"`
	Endpoint string    `json:"endpoint"`
	Action   string    `json:"action"`
	Result   string    `json:"result"`
	Detail   string    `json:"detail,omitempty"`
}

const (
	auditRingCap = 256 // in-memory backstop ring (audit-plane-failure fallback)
	alarmRingCap = 20  // /cdc/status .alarms ring
)

// SetSecurityToken wires the startup snapshot of security.token (called once
// from cmd/web). Snapshot semantics: later config.yaml edits do not affect
// the running gate — PUT /cdc/config cannot touch the security section and a
// hand edit needs a web restart. A weak token (short / pure digits) warns
// and audits but never refuses startup (ruling seq85 #2: fail-loud).
func (s *Server) SetSecurityToken(token string) {
	s.authToken = token
	if token == "" {
		zap.L().Warn("security.token 未配置：破坏性端点将全部拒绝（fail-closed）。请在 config.yaml 的 security.token 配置后重启 web。")
		return
	}
	if reason := weakTokenReason(token); reason != "" {
		msg := "security.token 弱令牌（" + reason + "）：公网暴露面下可被暴力尝试，建议 ≥16 字符且字母数字混合。"
		zap.L().Warn(msg)
		s.appendAudit(auditEntry{
			IP: "localhost", Method: "-", Endpoint: "-",
			Action: "security.token_entropy", Result: "warn", Detail: msg,
		})
	}
}

// weakTokenReason returns the advisory reason when the token is brute-force
// susceptible (ruling seq85 #2), or "". Byte length is deliberate: the gate
// compares raw bytes.
func weakTokenReason(token string) string {
	if len(token) < 16 {
		return "长度不足 16 字符"
	}
	if isAllDigits(token) {
		return "纯数字"
	}
	return ""
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// tokenMatches compares the presented header against the startup snapshot in
// constant time. Both sides are SHA-256 hashed FIRST so a candidate of the
// wrong length cannot be separated by an early return — every comparison,
// equal length or not, costs one hash + one 32-byte compare (adversarial
// seq84 #1, ruling seq85 #1).
func (s *Server) tokenMatches(presented string) bool {
	if s.authToken == "" {
		return false // unset gate: nobody passes
	}
	have := sha256.Sum256([]byte(s.authToken))
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(have[:], got[:]) == 1
}

// statusRecorder captures the handler's response code for the audit trail.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// peerIP returns the DIRECT peer address (host part of RemoteAddr). XFF is
// deliberately not consulted: with the listener still 0.0.0.0 direct,
// X-Forwarded-For is attacker-controlled (ruling seq83 #2).
func peerIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// requireCDCOpToken gates one destructive CDC endpoint: 403 while no token
// is configured (fail-closed + remediation hint), 401 on a missing/wrong
// token (FE opens the token prompt on exactly this code). Every outcome —
// denied, error, success — leaves an audit line.
func (s *Server) requireCDCOpToken(action string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		e := auditEntry{
			IP: peerIP(r), Method: r.Method, Endpoint: r.URL.Path, Action: action,
		}
		if s.authToken == "" {
			e.Result, e.Detail = "denied", "security.token 未配置（fail-closed 全拒）"
			s.appendAudit(e)
			s.writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "服务端未配置 security.token：破坏性操作已全部拒绝。请在 config.yaml 设置 security.token 后重启 web 再操作。",
			})
			return
		}
		if !s.tokenMatches(r.Header.Get("X-Auth-Token")) {
			e.Result, e.Detail = "denied", "X-Auth-Token 缺失或错误"
			s.appendAudit(e)
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "unauthorized：X-Auth-Token 缺失或错误。",
			})
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		if rec.status >= 400 {
			e.Result = "error"
			e.Detail = fmt.Sprintf("http %d", rec.status)
		} else {
			e.Result = "success"
		}
		s.appendAudit(e)
	}
}

// appendAudit records one entry: always into the in-memory ring (the
// anomaly checker's source AND the fallback when the disk trail fails), and
// durably into <dataDir>/audit/cdc-ops.jsonl with one retry. A persist
// failure must neither brick the operation nor stay silent (ruling seq85
// #3): ERROR log + alarm, record survives in the ring.
func (s *Server) appendAudit(e auditEntry) {
	e.TS = time.Now()
	s.auditMu.Lock()
	s.auditRing = append(s.auditRing, e)
	if len(s.auditRing) > auditRingCap {
		s.auditRing = s.auditRing[len(s.auditRing)-auditRingCap:]
	}
	if e.Action == "cdc.stop" && e.Result == "success" {
		s.lastStopAuditAt = e.TS
	}
	s.auditMu.Unlock()

	if s.dataDir == "" {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	f := filepath.Join(s.dataDir, "audit", "cdc-ops.jsonl")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		s.alarmAuditWriteFailure(f, err)
		return
	}
	if err := appendLineRetry(f, line); err != nil {
		s.alarmAuditWriteFailure(f, err)
	}
}

// alarmAuditWriteFailure raises the audit-plane's own alarm: the observability
// plane must never break silently — a dead audit trail is precisely the 2h
// silence this batch eliminates (adversarial seq84 #3).
func (s *Server) alarmAuditWriteFailure(path string, err error) {
	zap.L().Error("cdc audit write failed", zap.String("file", path), zap.Error(err))
	s.pushAlarm("审计日志写入失败：" + path + "：" + err.Error() + "（操作仍放行，记录暂存内存，请检查磁盘）")
}

// appendLineRetry appends one JSONL line, retrying exactly once on failure.
func appendLineRetry(path string, line []byte) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			lastErr = err
			continue
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			lastErr = err
			continue
		}
		return f.Close()
	}
	return lastErr
}

// pushAlarm appends one alarm row to the /cdc/status ring.
func (s *Server) pushAlarm(msg string) CDCAlarm {
	a := CDCAlarm{TS: time.Now(), Message: msg}
	s.alarmsMu.Lock()
	s.alarms = append(s.alarms, a)
	if len(s.alarms) > alarmRingCap {
		s.alarms = s.alarms[len(s.alarms)-alarmRingCap:]
	}
	s.alarmsMu.Unlock()
	return a
}

// cdcAlarms returns a copy of the alarm ring (oldest first).
func (s *Server) cdcAlarms() []CDCAlarm {
	s.alarmsMu.Lock()
	defer s.alarmsMu.Unlock()
	if len(s.alarms) == 0 {
		return nil
	}
	out := make([]CDCAlarm, len(s.alarms))
	copy(out, s.alarms)
	return out
}

// lastStopAuditTime reads the last SUCCESSFUL audited stop (zero when none).
func (s *Server) lastStopAuditTime() time.Time {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	return s.lastStopAuditAt
}

// cdcAnomalyChecker watches for CDC stops that no audited operator stop
// accounts for (P0-1c). Runs on its own ticker so it observes state even
// when the watchdog has stood down (!desired — exactly the incident shape:
// the unauthenticated API stop cleared desired and NOTHING was watching).
type cdcAnomalyChecker struct {
	s        *Server
	interval time.Duration
	log      *zap.Logger

	// lastSeenRunningAt is the last tick that observed a live CDC in this
	// web process; zero until then (a fresh web start never alarms — no
	// observed transition, zero false positives).
	lastSeenRunningAt time.Time
	// pendingSince is the first tick that observed an un-audited stop; the
	// alarm fires on the NEXT tick still un-audited (one-tick grace: the
	// audit line lands milliseconds after the state flip, the checker ticks
	// seconds later — but the race is real and must not false-alarm).
	pendingSince time.Time

	stop chan struct{}
}

func newCDCAnomalyChecker(s *Server, interval time.Duration, log *zap.Logger) *cdcAnomalyChecker {
	if log == nil {
		log = zap.L()
	}
	return &cdcAnomalyChecker{s: s, interval: interval, log: log, stop: make(chan struct{})}
}

func (c *cdcAnomalyChecker) run() {
	if c.interval <= 0 {
		c.interval = 30 * time.Second
	}
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.check()
		}
	}
}

// check performs one observation. Halted is excluded — a DDL-gate halt is an
// INTENTIONAL fail-closed stop with its own fatal_error surface, never an
// anomaly. Alarm semantics: exactly one per transition edge (running-cluster
// → stopped-cluster without a covering audit stop); a still-stopped CDC does
// not re-alarm, a new running→stopped edge re-arms (ruling seq83 #3).
func (c *cdcAnomalyChecker) check() {
	s := c.s
	if !s.cdcEnabled || s.cdcSupervisor == nil {
		return
	}
	if v := s.cdcStatus(); v.State == string(cdc.LivenessHalted) {
		// Known cause: the fail-closed DDL halt. Edge consumed silently.
		c.lastSeenRunningAt = time.Time{}
		c.pendingSince = time.Time{}
		return
	}
	st := s.cdcSupervisor.Status()
	switch st.State {
	case StateRunning, StateAdopted, StateStarting, StateStopping:
		c.lastSeenRunningAt = time.Now()
		c.pendingSince = time.Time{}
		return
	case StateStopped, StateFailed:
		// fall through to the un-audited-stop judgment
	default:
		return
	}
	if c.lastSeenRunningAt.IsZero() {
		return // never saw it running in this process: nothing to attribute
	}
	// A tie counts as covered: the audit line lands strictly after the
	// arming observation in wall order, but time.Now() can return the SAME
	// tick for both — After() would then false-alarm on an audited stop.
	if lastStop := s.lastStopAuditTime(); !lastStop.IsZero() && !lastStop.Before(c.lastSeenRunningAt) {
		c.pendingSince = time.Time{} // an audited operator stop covers it
		return
	}
	if c.pendingSince.IsZero() {
		c.pendingSince = time.Now() // grace tick: audit may land milliseconds late
		return
	}
	c.pushAnomalyAlarm(st.State, st.PID)
	c.lastSeenRunningAt = time.Time{} // edge consumed; re-arms on next running
	c.pendingSince = time.Time{}
}

func (c *cdcAnomalyChecker) pushAnomalyAlarm(state CDCState, pid int) {
	msg := fmt.Sprintf("CDC 异常停止（状态=%s，PID=%d）：无对应的 stop 审计记录——不是操作员经 API 停止，进程被外部终止或链路异常退出，请立即核查。", state, pid)
	c.s.pushAlarm(msg)
	if c.log != nil {
		c.log.Error("cdc anomaly: stopped without an audited stop",
			zap.String("state", string(state)), zap.Int("pid", pid))
	}
}
