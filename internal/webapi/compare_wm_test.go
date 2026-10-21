package webapi

import (
	"net/http"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// #t3 watermark compare anchors: filter-group normalization/defaults,
// create-time validation, mode←base_mode mapping, options persistence
// (set / clear-by-null / preserve-when-absent), and run-param plumbing.

// A1: normalizeWatermarkFilter — nil stays nil; defaults fill ("<=",
// checksum); explicit values pass through; every invalid shape is rejected.
func TestWatermarkNormalize(t *testing.T) {
	if out, err := normalizeWatermarkFilter(nil); out != nil || err != nil {
		t.Fatalf("nil filter must stay nil, got %v err=%v", out, err)
	}

	out, err := normalizeWatermarkFilter(&config.WatermarkFilter{Column: "updated_at", Value: "2026-10-01 00:00:00"})
	if err != nil {
		t.Fatalf("minimal filter must pass: %v", err)
	}
	if out.Op != "<=" || out.BaseMode != "checksum" {
		t.Fatalf("defaults wrong: op=%q base=%q", out.Op, out.BaseMode)
	}

	out, err = normalizeWatermarkFilter(&config.WatermarkFilter{Column: " id ", Value: " 42 ", Op: "<", BaseMode: "quick"})
	if err != nil || out.Column != "id" || out.Value != "42" || out.Op != "<" || out.BaseMode != "quick" {
		t.Fatalf("explicit filter wrong: %+v err=%v", out, err)
	}

	for i, bad := range []*config.WatermarkFilter{
		{Column: "", Value: "x"},            // missing column
		{Column: "updated_at", Value: "  "}, // missing value
		{Column: `upd"; DROP`, Value: "x"},  // identifier injection
		{Column: "updated_at", Value: "x", Op: ">="},
		{Column: "updated_at", Value: "x", Op: "="},
		{Column: "updated_at", Value: "x", BaseMode: "full"},
		{Column: "updated_at", Value: "x", BaseMode: "watermark"},
	} {
		if _, err := normalizeWatermarkFilter(bad); err == nil {
			t.Errorf("case %d: %+v must be rejected", i, bad)
		}
	}
}

// A1b: interval-form normalize — a full [range_start, range_end) group passes
// (endpoints preserved, Value empty); every malformed shape is rejected
// (single endpoint, start>=end, value+range mixed, neither value form).
func TestWatermarkNormalizeRange(t *testing.T) {
	out, err := normalizeWatermarkFilter(&config.WatermarkFilter{
		Column: "updated_at", RangeStart: " 2026-09-01 00:00:00 ", RangeEnd: "2026-10-01 00:00:00",
	})
	if err != nil {
		t.Fatalf("full interval must pass: %v", err)
	}
	if out.Value != "" || out.RangeStart != "2026-09-01 00:00:00" || out.RangeEnd != "2026-10-01 00:00:00" {
		t.Fatalf("range fields wrong: %+v", out)
	}
	if out.BaseMode != "checksum" {
		t.Fatalf("range default base_mode wrong: %q", out.BaseMode)
	}

	for i, bad := range []*config.WatermarkFilter{
		{Column: "updated_at", RangeStart: "2026-09-01 00:00:00"},                                                                // missing range_end
		{Column: "updated_at", RangeEnd: "2026-10-01 00:00:00"},                                                                  // missing range_start
		{Column: "updated_at", RangeStart: "2026-10-01 00:00:00", RangeEnd: "2026-09-01 00:00:00"},                               // start > end
		{Column: "updated_at", RangeStart: "2026-10-01 00:00:00", RangeEnd: "2026-10-01 00:00:00"},                               // start == end → empty [start,end)
		{Column: "updated_at", RangeStart: "2026-09-01 00:00:00", RangeEnd: "2026-10-01 00:00:00", Value: "2026-10-01 00:00:00"}, // mixed
		{Column: "updated_at", RangeStart: "2026-09-01 00:00:00", RangeEnd: "2026-10-01 00:00:00", Value: "x"},
		{Column: "updated_at"},                       // neither value nor range
		{Column: "updated_at", Value: "  "},          // whitespace-only value ≠ present
		{Column: "", RangeStart: "a", RangeEnd: "b"}, // missing column
	} {
		if _, err := normalizeWatermarkFilter(bad); err == nil {
			t.Errorf("range case %d: %+v must be rejected", i, bad)
		}
	}
}

// A1c: interval-form options round-trip — a range watermark saved via
// PUT /compare/options persists unchanged (RangeStart/RangeEnd preserved,
// defaults filled) and comes back through loadCompareOptions.
func TestCompareOptionsWatermarkRangeRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("PUT", "/api/v1/compare/options",
		`{"watermark":{"column":"updated_at","range_start":"2026-09-01 00:00:00","range_end":"2026-10-01 00:00:00"}}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set range watermark: %d %s", w.Code, w.Body.String())
	}
	opts := s.loadCompareOptions()
	if opts.Watermark == nil {
		t.Fatalf("range watermark must be saved")
	}
	if opts.Watermark.RangeStart != "2026-09-01 00:00:00" || opts.Watermark.RangeEnd != "2026-10-01 00:00:00" || opts.Watermark.Value != "" {
		t.Fatalf("saved range watermark wrong: %+v", opts.Watermark)
	}
	if opts.Watermark.BaseMode != "checksum" || opts.Watermark.Column != "updated_at" {
		t.Fatalf("range defaults wrong: %+v", opts.Watermark)
	}

	// Malformed range rejected without touching the saved group.
	w, req = doReq("PUT", "/api/v1/compare/options",
		`{"watermark":{"column":"updated_at","range_start":"2026-10-01 00:00:00","range_end":"2026-09-01 00:00:00"}}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("start>end range must 400, got %d", w.Code)
	}
	if s.loadCompareOptions().Watermark == nil {
		t.Fatalf("rejected range must not clobber the saved group")
	}
}
func TestCompareTaskWatermarkValidation(t *testing.T) {
	s, _ := newTestServer(t)
	for i, body := range []string{
		`{"source":{"host":"s"},"target":{"host":"t"},"watermark":{"column":"updated_at","value":"x","op":">="}}`,
		`{"source":{"host":"s"},"target":{"host":"t"},"watermark":{"column":"updated_at","value":"x","base_mode":"full"}}`,
		`{"source":{"host":"s"},"target":{"host":"t"},"watermark":{"column":"updated_at"}}`,
		`{"source":{"host":"s"},"target":{"host":"t"},"watermark":{"column":"bad col","value":"x"}}`,
	} {
		w, req := doReq("POST", "/api/v1/compare/tasks", body)
		s.handleCreateCompare(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("case %d: expected 400, got %d body=%s", i, w.Code, w.Body.String())
		}
	}

	// mode←base_mode mapping: a watermark task with base quick runs quick.
	task := &CompareTask{Watermark: &config.WatermarkFilter{Column: "updated_at", Value: "x", Op: "<", BaseMode: "quick"}}
	_, opts, cfg := buildCompareRunParams(task, "")
	if opts.Mode != "quick" || cfg.CompareMode != "quick" {
		t.Fatalf("mode mapping failed: opts=%q cfg=%q", opts.Mode, cfg.CompareMode)
	}
	if cfg.Watermark != task.Watermark {
		t.Fatalf("watermark not plumbed into compareCfg")
	}
}

// A3: options persistence — set / clear-by-null / preserve-when-absent.
func TestCompareOptionsWatermarkMerge(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("PUT", "/api/v1/compare/options",
		`{"watermark":{"column":"updated_at","value":"2026-10-01 00:00:00"}}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set watermark: %d %s", w.Code, w.Body.String())
	}
	opts := s.loadCompareOptions()
	if opts.Watermark == nil || opts.Watermark.Column != "updated_at" || opts.Watermark.Op != "<=" || opts.Watermark.BaseMode != "checksum" {
		t.Fatalf("saved watermark wrong: %+v", opts.Watermark)
	}

	// Absent key keeps the saved group (partial-merge by presence).
	w, req = doReq("PUT", "/api/v1/compare/options", `{"parallel":3}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("absent-key put: %d", w.Code)
	}
	if s.loadCompareOptions().Watermark == nil {
		t.Fatalf("absent watermark key must preserve saved group")
	}

	// Present-and-null clears.
	w, req = doReq("PUT", "/api/v1/compare/options", `{"watermark":null}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("null-clear put: %d", w.Code)
	}
	if s.loadCompareOptions().Watermark != nil {
		t.Fatalf("watermark:null must clear the saved group")
	}

	// Invalid group rejected without touching the (now nil) saved value.
	w, req = doReq("PUT", "/api/v1/compare/options",
		`{"watermark":{"column":"c","value":"v","op":"!="}}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid group must 400, got %d", w.Code)
	}
}

// A4: runtime per-table skip — a watermark run where EVERY table hit a
// residual skip path (no comparable columns / all excluded; all StatusSkip)
// is an explicit task failure, never a misleading "completed"; a mixed
// report (≥1 non-skip) is NOT all-skipped.
func TestCompareAllTablesSkipped(t *testing.T) {
	if compareAllTablesSkipped(nil) {
		t.Fatal("nil report must not read as all-skipped")
	}
	rpt := reporter.NewReport("data")
	if compareAllTablesSkipped(rpt) {
		t.Fatal("empty report must not read as all-skipped")
	}
	rpt.AddTableReport(reporter.TableReport{TableName: "a", Status: reporter.StatusSkip})
	rpt.AddTableReport(reporter.TableReport{TableName: "b", Status: reporter.StatusSkip})
	if !compareAllTablesSkipped(rpt) {
		t.Fatal("all-skip report must be detected")
	}
	rpt.AddTableReport(reporter.TableReport{TableName: "c", Status: reporter.StatusPass})
	if compareAllTablesSkipped(rpt) {
		t.Fatal("mixed report (pass present) must NOT be all-skipped")
	}
	// Non-skip statuses all break the all-skip reading.
	rpt.Tables = []reporter.TableReport{
		{TableName: "a", Status: reporter.StatusSkip},
		{TableName: "b", Status: reporter.StatusFail},
	}
	if compareAllTablesSkipped(rpt) {
		t.Fatal("fail table must NOT count as skipped")
	}
}
