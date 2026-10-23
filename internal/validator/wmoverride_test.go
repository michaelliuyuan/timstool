package validator

import (
	"context"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// #wm-override anchors (P1A/P2A/P3A): the per-table resolution chain
// (override entry → global group → none), the override precheck gate (hard
// FAIL, never fallback), the override-tagged scope note, and the
// watermark-mode activation switch (no global + overrides ⇒ still a
// watermark task). Live three-mode fallback e2e stays with isolation
// testing / te smoke per the wmfilter_test.go header convention.

// O1: resolution chain — an ACTIVE override entry wins for its table; a
// table without an entry falls to the global group; a half-configured
// (inactive) entry is ignored (that table falls back to the global group,
// mirroring the activation rule).
func TestWmOverrideResolution(t *testing.T) {
	base := config.Config{
		Compare: config.CompareConfig{
			Watermark: wmTestFilter(""),
			TableWatermarks: []config.TableWatermark{
				{Table: "orders", WatermarkFilter: config.WatermarkFilter{Column: "ts", Value: "2026-10-01 00:00:00"}},
				// half-configured interval: NOT an active form → ignored
				{Table: "logs", WatermarkFilter: config.WatermarkFilter{Column: "ts", RangeStart: "2026-09-01 00:00:00"}},
			},
		},
	}
	v := NewValidator(base)

	if ov, hit := v.wmOverrideFor("orders"); !hit || ov == nil || ov.Column != "ts" {
		t.Fatalf("orders must resolve its override, got hit=%v wm=%+v", hit, ov)
	}
	if ov, hit := v.wmOverrideFor("logs"); hit || ov != nil {
		t.Fatalf("half-configured entry must be ignored, got hit=%v wm=%+v", hit, ov)
	}
	if ov, hit := v.wmOverrideFor("other"); hit || ov != nil {
		t.Fatalf("unlisted table must not resolve an override, got hit=%v wm=%+v", hit, ov)
	}
	// The returned override is a COPY of the slice element: callers must
	// never be able to mutate the config snapshot through it.
	if ov, _ := v.wmOverrideFor("orders"); ov != nil {
		ov.Column = "mutated"
		if again, _ := v.wmOverrideFor("orders"); again.Column != "ts" {
			t.Fatal("override resolution must return a defensive copy")
		}
	}
}

// O2: wmFilterFor resolves via the ctx table key — override table sees its
// entry, non-override table (same ctx-less global path) sees the global
// group, and the global group stays nil-safe with overrides present.
func TestWmFilterForTableKey(t *testing.T) {
	v := NewValidator(config.Config{
		Compare: config.CompareConfig{
			TableWatermarks: []config.TableWatermark{
				{Table: "orders", WatermarkFilter: config.WatermarkFilter{Column: "ts", Value: "1"}},
			},
		},
	})
	ctxOrders := ctxWithWMTable(context.Background(), "orders")
	ctxOther := ctxWithWMTable(context.Background(), "other")
	if got := v.wmFilterFor(ctxOrders); got == nil || got.Column != "ts" {
		t.Fatalf("override table must see its filter, got %+v", got)
	}
	if got := v.wmFilterFor(ctxOther); got != nil {
		t.Fatalf("no global group + no override must yield nil, got %+v", got)
	}
	if v.wmFilterFor(context.Background()) != nil {
		t.Fatal("untagged ctx with no global group must yield nil")
	}
}

// O3: pure-override activation (P3A) — no global group but active overrides
// ⇒ hasActiveWMOverrides flips true (the task-level watermark-mode switch).
func TestWmHasActiveOverrides(t *testing.T) {
	if NewValidator(config.Config{}).hasActiveWMOverrides() {
		t.Fatal("zero config must not activate watermark mode")
	}
	v := NewValidator(config.Config{
		Compare: config.CompareConfig{
			TableWatermarks: []config.TableWatermark{
				{Table: "logs", WatermarkFilter: config.WatermarkFilter{Column: "ts", RangeStart: "only-one-endpoint"}},
			},
		},
	})
	if v.hasActiveWMOverrides() {
		t.Fatal("inactive-only overrides must not activate watermark mode")
	}
	v2 := NewValidator(config.Config{
		Compare: config.CompareConfig{
			TableWatermarks: []config.TableWatermark{
				{Table: "orders", WatermarkFilter: config.WatermarkFilter{Column: "ts", Value: "1"}},
			},
		},
	})
	if !v2.hasActiveWMOverrides() {
		t.Fatal("one active override must activate watermark mode")
	}
}

// O4: override precheck gate — sentinel AND ordinary errors are BOTH hard
// FAILs (never a full-comparison fallback), with explicit attribution.
func TestWmPrecheckFailEx(t *testing.T) {
	sentinel := wmSkipf("watermark column updated_at not found in table orders")
	tr := wmPrecheckFailEx("orders", sentinel)
	if tr == nil || tr.Status != "fail" || tr.TableName != "orders" {
		t.Fatalf("sentinel on an override table must hard FAIL, got %+v", tr)
	}
	if !strings.HasPrefix(tr.Error, "例外水位列：") || !strings.Contains(tr.Error, "updated_at") {
		t.Fatalf("override FAIL must carry explicit attribution, got %q", tr.Error)
	}
	ordinary := context.Canceled
	tr2 := wmPrecheckFailEx("orders", ordinary)
	if tr2 == nil || tr2.Status != "fail" {
		t.Fatalf("ordinary error on an override table must hard FAIL too, got %+v", tr2)
	}
}

// O5: wmTableNote override tagging — an override table's scope note carries
// the (例外) marker; a global-scope note stays untagged (byte-identical to
// the pre-override wording); the fallback note keeps priority over both.
func TestWmTableNoteOverrideTag(t *testing.T) {
	wm := wmTestFilter("")
	ov := wmTableNote(wm, "", 5, 5, true)
	if !strings.Contains(ov, "水位过滤(例外) updated_at <=") {
		t.Fatalf("override scope note must be tagged: %q", ov)
	}
	g := wmTableNote(wm, "", 5, 5, false)
	if strings.Contains(g, "例外") || !strings.Contains(g, "水位过滤 updated_at <=") {
		t.Fatalf("global scope note must stay untagged: %q", g)
	}
	fb := wmTableNote(wm, "some reason", 5, 5, true)
	if !strings.Contains(fb, "退化为全量比对") || strings.Contains(fb, "水位过滤") {
		t.Fatalf("fallback note must win over the override scope note: %q", fb)
	}
}

// O6 (Obs2): an override precheck FAIL row is recognized by isWmExFail ⇒ the
// stamp site skips the watermark scope note (the row never ran, so its zero
// counts would read as "the filter matched nothing"); a global-group gate
// FAIL row and a passing row are NOT recognized (their notes stay as-is).
func TestWmExFailStampSkip(t *testing.T) {
	ex := *wmPrecheckFailEx("orders", wmSkipf("watermark column updated_at not found in table orders"))
	if !isWmExFail(ex) {
		t.Fatal("a wmPrecheckFailEx product must be recognized (scope note skipped)")
	}
	if ex.Suggestion != "" {
		t.Fatalf("producer must leave Suggestion empty for the stamp site to decide, got %q", ex.Suggestion)
	}
	_, gateFail := wmPrecheckGate("orders", context.Canceled)
	if gateFail == nil || isWmExFail(*gateFail) {
		t.Fatalf("a global-group FAIL row must NOT be recognized (its note stays as-is): %+v", gateFail)
	}
	pass := reporter.TableReport{TableName: "orders", Status: reporter.StatusPass}
	if isWmExFail(pass) {
		t.Fatal("a passing row must never be recognized")
	}
}
