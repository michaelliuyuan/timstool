package validator

// table_duration_test.go anchors FEAT-CMP-TBL-DURATION (每表对比耗时):
//
//	①  duration is stamped for EVERY mode's ending — quick/sample fail at
//	    the refused TiDB dial, checksum+wm fails at the watermark precheck
//	    probe, and an unknown mode fails before any query: all three carry
//	    a non-empty, FormatDuration-shaped Duration (fail 表也带).
//	②  the unknown-mode ending (no query at all) is stamped too.
//	③  OnTableDone receives the SAME stamped tr (带时长) with the done
//	    counter ascending 1..N.
//	④  report.json on disk carries the per-table duration end-to-end.
//	⑤  parallel slow>fast (sleep injection at the wm precheck): the slow
//	    table's wall clock exceeds the fast one's — D1=A semantics where
//	    the whole goroutine processing window is the measurement.
//
// The seam under test is the REAL Run() goroutine loop (validator.go):
// clock started at goroutine entry, stopped at the AddTableReport
// collection seam. Run() opens the source pool by driver NAME — the real
// "pgx" driver is always registered by the blank import, so this file
// registers its scripted driver under a private name and points
// srcDriverOverride at it (same override pattern the Validator already
// uses for DSNs/compare knobs). The target pool opens the real "mysql"
// driver against a refused localhost port with a 2s dial timeout — the
// quick/sample arms fail exactly at getTiDBConn, which is itself one of
// the anchored endings.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/common"
	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// --- scripted "pgx" driver -------------------------------------------------

var (
	durScriptMu sync.Mutex
	durSleep    = map[string]time.Duration{}    // table -> wm-precheck sleep
	durProbeErr = error(nil)                    // wm precheck probe result
)

type durConn struct{}

func (durConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (durConn) Close() error                        { return nil }
func (durConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (durConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

func (durConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "information_schema.columns") {
		table := ""
		if len(args) >= 2 {
			if s, ok := args[1].Value.(string); ok {
				table = s
			}
		}
		durScriptMu.Lock()
		sleep := durSleep[table]
		err := durProbeErr
		durScriptMu.Unlock()
		if sleep > 0 {
			time.Sleep(sleep)
		}
		if err != nil {
			return nil, err
		}
		return &scriptRows{cols: []string{"data_type"}, data: [][]driver.Value{{"timestamp with time zone"}}}, nil
	}
	return &scriptRows{cols: []string{"x"}, data: nil}, nil
}

type durDriver struct{}

func (durDriver) Open(string) (driver.Conn, error) { return durConn{}, nil }

func init() { sql.Register("pgx-duration-script", durDriver{}) }

// --- helpers ---------------------------------------------------------------

// durRe accepts every FormatDuration form: "0.300s" / "1m23.456s" / "1h2m3.000s".
var durRe = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(\d+(?:\.\d+)?)s$`)

// durMillis parses a FormatDuration string into milliseconds.
func durMillis(t *testing.T, s string) float64 {
	t.Helper()
	m := durRe.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("duration %q is not FormatDuration-shaped", s)
	}
	h, min, sec := 0.0, 0.0, 0.0
	if m[1] != "" {
		fmt.Sscanf(m[1], "%f", &h)
	}
	if m[2] != "" {
		fmt.Sscanf(m[2], "%f", &min)
	}
	fmt.Sscanf(m[3], "%f", &sec)
	return h*3600000 + min*60000 + sec*1000
}

// durRun drives the REAL Run() with the scripted source driver and a
// refused-localhost target. onDone (may be nil) rides OnTableDone.
func durRun(t *testing.T, mode string, tables []string, wm *config.WatermarkFilter, onDone func(int, int, reporter.TableReport)) (*reporter.Report, error) {
	t.Helper()
	v := NewValidator(config.Config{})
	// In-package overrides: scripted source driver by name, refused target
	// dial with a hard 2s cap so a hostile network stack can never hang a
	// test.
	v.srcDriverOverride = "pgx-duration-script"
	v.pgDSNOverride = "dur-fake-source"
	v.tidbDSNOverride = "u:p@tcp(127.0.0.1:1)/d?charset=utf8mb4&timeout=2s"
	v.compareOverride = &config.CompareConfig{Concurrency: 4, Watermark: wm}
	if onDone != nil {
		v.OnTableDone(onDone)
	}
	return v.Run(context.Background(), common.ValidateOpts{
		Level: "data", Mode: mode, Tables: tables,
	})
}

func durByTable(rpt *reporter.Report) map[string]reporter.TableReport {
	out := make(map[string]reporter.TableReport, len(rpt.Tables))
	for _, tr := range rpt.Tables {
		out[tr.TableName] = tr
	}
	return out
}

// --- anchors ---------------------------------------------------------------

// TestTableDurationStampedAllModes (①): quick/sample (fail at getTiDBConn)
// and checksum+wm (fail at the precheck probe) all stamp a
// FormatDuration-shaped, non-empty Duration on every table — the fail
// ending is the whole point (pass/fail/预检失败 alike carry timing).
func TestTableDurationStampedAllModes(t *testing.T) {
	durScriptMu.Lock()
	durSleep = map[string]time.Duration{}
	durProbeErr = errors.New("scripted wm probe failure")
	durScriptMu.Unlock()
	wm := &config.WatermarkFilter{Column: "updated_at", Value: "2026-01-01", Op: "<=", BaseMode: "checksum"}

	for _, tc := range []struct {
		mode       string
		wm         *config.WatermarkFilter
		wantErrSub string
	}{
		{"quick", nil, "get TiDB connection"},
		{"sample", nil, "get TiDB connection"},
		{"checksum", wm, "校验水位列"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			rpt, err := durRun(t, tc.mode, []string{"public.a", "public.b"}, tc.wm, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(rpt.Tables) != 2 {
				t.Fatalf("tables=%d want 2", len(rpt.Tables))
			}
			for _, tr := range rpt.Tables {
				if tr.Status != reporter.StatusFail {
					t.Fatalf("%s: table %s status=%s want fail (scripted ending)", tc.mode, tr.TableName, tr.Status)
				}
				if !strings.Contains(tr.Error, tc.wantErrSub) {
					t.Fatalf("%s: table %s error=%q want substring %q", tc.mode, tr.TableName, tr.Error, tc.wantErrSub)
				}
				if !durRe.MatchString(tr.Duration) {
					t.Errorf("%s: table %s duration=%q not FormatDuration-shaped (fail tables must carry timing)", tc.mode, tr.TableName, tr.Duration)
				}
				if durMillis(t, tr.Duration) < 0 {
					t.Errorf("%s: table %s negative duration %q", tc.mode, tr.TableName, tr.Duration)
				}
			}
		})
	}
}

// TestTableDurationUnknownModeStamped (②): the switch-default ending fails
// before any query, and the one collection seam still stamps it.
func TestTableDurationUnknownModeStamped(t *testing.T) {
	rpt, err := durRun(t, "bogus-mode", []string{"public.x"}, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := rpt.Tables[0]
	if tr.Status != reporter.StatusFail || !strings.Contains(tr.Error, "unknown validation mode") {
		t.Fatalf("unknown mode ending wrong: %+v", tr)
	}
	if !durRe.MatchString(tr.Duration) {
		t.Errorf("unknown-mode table duration=%q not stamped", tr.Duration)
	}
}

// TestTableDurationOnTableDoneCarriesDuration (③): every OnTableDone
// callback sees the STAMPED tr (带时长), with done ascending 1..N and
// total==N — the WS face (compare.go tableDoneProgress) consumes exactly
// this callback.
func TestTableDurationOnTableDoneCarriesDuration(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []reporter.TableReport
		dones []int
	)
	rpt, err := durRun(t, "quick", []string{"public.a", "public.b"}, nil, func(done, total int, tr reporter.TableReport) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, tr)
		dones = append(dones, done)
		if total != 2 {
			t.Errorf("callback total=%d want 2", total)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || dones[0] != 1 || dones[1] != 2 {
		t.Fatalf("callbacks=%v dones=%v want 2 ascending", calls, dones)
	}
	byTable := durByTable(rpt)
	for _, tr := range calls {
		if !durRe.MatchString(tr.Duration) {
			t.Errorf("onTableDone tr for %s duration=%q not stamped", tr.TableName, tr.Duration)
		}
		if want := byTable[tr.TableName]; want.Duration != tr.Duration {
			t.Errorf("onTableDone tr for %s duration=%q != report entry %q", tr.TableName, tr.Duration, want.Duration)
		}
	}
}

// TestTableDurationReportJSONEndToEnd (④): opts.ReportFile writes
// report.json; every table object carries a non-empty duration string —
// the FE result table/CSV and the server HTML face all read this file.
func TestTableDurationReportJSONEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")

	v := NewValidator(config.Config{})
	v.srcDriverOverride = "pgx-duration-script"
	v.pgDSNOverride = "dur-fake-source"
	v.tidbDSNOverride = "u:p@tcp(127.0.0.1:1)/d?charset=utf8mb4&timeout=2s"
	v.compareOverride = &config.CompareConfig{Concurrency: 4}
	if _, err := v.Run(context.Background(), common.ValidateOpts{
		Level: "data", Mode: "quick", Tables: []string{"public.a", "public.b"}, ReportFile: path,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Run() persisted the report via opts.ReportFile (atomic temp+rename).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tables []map[string]any `json:"tables"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode report.json: %v", err)
	}
	if len(doc.Tables) != 2 {
		t.Fatalf("tables=%d want 2", len(doc.Tables))
	}
	for _, tm := range doc.Tables {
		d, ok := tm["duration"].(string)
		if !ok || !durRe.MatchString(d) {
			t.Errorf("table %v report.json duration=%v not a FormatDuration string", tm["table_name"], tm["duration"])
		}
	}
}

// TestTableDurationSlowTableGreaterThanFastParallel (⑤): two tables run in
// PARALLEL goroutines (budget 4); the slow table's wm-precheck probe sleeps
// 300ms, the fast one returns instantly. D1=A wall clock must rank the slow
// table above the fast one — the whole point of the feature (慢表定位).
func TestTableDurationSlowTableGreaterThanFastParallel(t *testing.T) {
	durScriptMu.Lock()
	durSleep = map[string]time.Duration{"public.slow": 300 * time.Millisecond, "public.fast": 0}
	durProbeErr = errors.New("scripted wm probe failure")
	durScriptMu.Unlock()
	defer func() {
		durScriptMu.Lock()
		durSleep = map[string]time.Duration{}
		durScriptMu.Unlock()
	}()

	rpt, err := durRun(t, "checksum", []string{"public.slow", "public.fast"},
		&config.WatermarkFilter{Column: "updated_at", Value: "2026-01-01", Op: "<=", BaseMode: "checksum"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byTable := durByTable(rpt)
	slow, fast := byTable["public.slow"], byTable["public.fast"]
	if slow.Status != reporter.StatusFail || fast.Status != reporter.StatusFail {
		t.Fatalf("scripted wm-precheck-fail ending wrong: slow=%s fast=%s", slow.Status, fast.Status)
	}
	// wm 预检失败结局也带耗时.
	for name, tr := range map[string]reporter.TableReport{"slow": slow, "fast": fast} {
		if !durRe.MatchString(tr.Duration) {
			t.Errorf("%s duration=%q not stamped", name, tr.Duration)
		}
	}
	slowMs := durMillis(t, slow.Duration)
	fastMs := durMillis(t, fast.Duration)
	if slowMs < fastMs+200 {
		t.Errorf("parallel slow table must outrank fast: slow=%vms fast=%vms (sleep 300ms injected)", slowMs, fastMs)
	}
}
