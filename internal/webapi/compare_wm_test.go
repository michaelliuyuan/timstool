package webapi

import (
	"net/http"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
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

// A2: create endpoint rejects invalid watermark groups pre-run (400) and
// maps an absent mode to the filter's base_mode.
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
