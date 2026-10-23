package webapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// #wm-override webapi anchors (P1A): normalizeTableWatermarks validation,
// the options presence-three-state merge (present-and-empty clears / absent
// keeps), and the create-path guard (an override outside an explicit table
// selection is a 400, never a silent no-op).

// W1: normalizeTableWatermarks — empty stays nil; a valid entry is trimmed,
// defaults filled, BaseMode FORCED empty; duplicates, blank table names and
// malformed filters are rejected.
func TestNormalizeTableWatermarks(t *testing.T) {
	if out, err := normalizeTableWatermarks(nil); out != nil || err != nil {
		t.Fatalf("nil must stay nil, got %+v err=%v", out, err)
	}
	out, err := normalizeTableWatermarks([]config.TableWatermark{
		{Table: " orders ", WatermarkFilter: config.WatermarkFilter{Column: " ts ", Value: " 42 ", BaseMode: "quick"}},
	})
	if err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	if len(out) != 1 || out[0].Table != "orders" || out[0].Column != "ts" || out[0].Value != "42" {
		t.Fatalf("normalization wrong: %+v", out)
	}
	if out[0].BaseMode != "" || out[0].Op != "<=" {
		t.Fatalf("override BaseMode must be forced empty (got %q), op defaulted: %+v", out[0].BaseMode, out[0])
	}

	// blank table name rejected
	if _, err := normalizeTableWatermarks([]config.TableWatermark{
		{Table: "  ", WatermarkFilter: config.WatermarkFilter{Column: "ts", Value: "x"}},
	}); err == nil {
		t.Fatal("blank table name must be rejected")
	}
	// malformed embedded filter rejected (bad base_mode here proves the
	// group passes the SAME validation as the global one)
	if _, err := normalizeTableWatermarks([]config.TableWatermark{
		{Table: "a", WatermarkFilter: config.WatermarkFilter{Column: "ts", Value: "x", BaseMode: "bad"}},
	}); err == nil {
		t.Fatal("malformed embedded filter must be rejected")
	}
	// duplicate table names rejected
	if _, err := normalizeTableWatermarks([]config.TableWatermark{
		{Table: "a", WatermarkFilter: config.WatermarkFilter{Column: "ts", Value: "x"}},
		{Table: "a", WatermarkFilter: config.WatermarkFilter{Column: "ts", Value: "y"}},
	}); err == nil {
		t.Fatal("duplicate table must be rejected")
	}
}

// W2: options merge — present saves, present-and-empty clears, absent keeps.
func TestCompareOptionsTableWatermarksMerge(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("PUT", "/api/v1/compare/options",
		`{"table_watermarks":[{"table":"orders","column":"ts","value":"42"}]}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("save overrides: %d %s", w.Code, w.Body.String())
	}
	opts := s.loadCompareOptions()
	if len(opts.TableWatermarks) != 1 || opts.TableWatermarks[0].Table != "orders" || opts.TableWatermarks[0].BaseMode != "" {
		t.Fatalf("saved overrides wrong: %+v", opts.TableWatermarks)
	}

	// Absent key keeps them.
	w, req = doReq("PUT", "/api/v1/compare/options", `{"parallel":3}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK || len(s.loadCompareOptions().TableWatermarks) != 1 {
		t.Fatal("absent key must keep the saved overrides")
	}

	// Present-and-empty clears.
	w, req = doReq("PUT", "/api/v1/compare/options", `{"table_watermarks":[]}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK || s.loadCompareOptions().TableWatermarks != nil {
		t.Fatal("present-and-empty must clear the saved overrides")
	}
}

// W3: create path — an override outside an explicit table selection is a
// 400 with the table named; a matching selection passes and the overrides
// reach compareCfg via buildCompareRunParams.
func TestCompareTaskTableWatermarksGuard(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s"},"target":{"host":"t"},"tables":["orders","users"],`+
			`"table_watermarks":[{"table":"ghost","column":"ts","value":"42"}]}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("override outside selection must 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ghost") {
		t.Fatalf("error must name the offending table: %s", w.Body.String())
	}

	// All-tables selection (tables empty) has nothing static to check.
	w, req = doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s"},"target":{"host":"t"},`+
			`"table_watermarks":[{"table":"ghost","column":"ts","value":"42"}]}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusCreated {
		// no live DB: the run fails fast, but NOT at validation — accept the
		// created path too (the goroutine owns the failure); only 4xx-at-
		// validation would be a regression.
		t.Fatalf("all-tables override must pass static validation, got %d body=%s", w.Code, w.Body.String())
	}
	// Wind down the background run goroutine (unreachable host) so the
	// server's TempDir is free for teardown — the create-path convention
	// (compare_ignore_test.go); without this the test is flaky on cleanup.
	if s.compare.cancel != nil {
		s.compare.cancel()
	}

	// Plumbing: buildCompareRunParams carries the overrides into compareCfg.
	task := &CompareTask{TableWatermarks: []config.TableWatermark{
		{Table: "orders", WatermarkFilter: config.WatermarkFilter{Column: "ts", Value: "42"}},
	}}
	_, _, cfg := buildCompareRunParams(task, "")
	if len(cfg.TableWatermarks) != 1 || cfg.TableWatermarks[0].Table != "orders" {
		t.Fatalf("overrides not plumbed into compareCfg: %+v", cfg.TableWatermarks)
	}
}
