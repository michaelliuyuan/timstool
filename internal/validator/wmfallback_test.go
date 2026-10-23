package validator

import (
	"context"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// Plan A fallback anchors: the per-table ctx override forces wm=nil for the
// degraded table only, and the report-layer note picks the fallback
// disclosure over the scope note (mutual exclusion — a scope note on a
// full-table comparison would be misleading).

// F1: wmFilterFor — fallback-marked ctx yields nil (this table compares in
// full), unmarked ctx yields the global filter.
func TestWmFilterForCtxOverride(t *testing.T) {
	v := NewValidator(config.Config{Compare: config.CompareConfig{Watermark: wmTestFilter("")}})
	if v.wmFilterFor(context.Background()) == nil {
		t.Fatal("unmarked ctx must see the global filter")
	}
	if got := v.wmFilterFor(ctxWithWMFallback(context.Background())); got != nil {
		t.Fatalf("fallback ctx must yield nil filter, got %+v", got)
	}
}

// F2: fallback and zero-config coexistence — the override is the only signal;
// even a fully-configured global filter is neutralized for the fallback table.
func TestWmFilterForZeroConfigUnchanged(t *testing.T) {
	v := NewValidator(config.Config{})
	if v.wmFilterFor(context.Background()) != nil {
		t.Fatal("zero config stays nil without override")
	}
	if v.wmFilterFor(ctxWithWMFallback(context.Background())) != nil {
		t.Fatal("zero config + override stays nil")
	}
}

// F3: wmTableNote mutual exclusion — a fallback table gets the fallback note
// (never the scope note), a non-fallback watermarked table gets the scope
// note, an unfiltered table gets "".
func TestWmTableNoteMutualExclusion(t *testing.T) {
	wm := wmTestRangeFilter()
	note := wmTableNote(wm, "水位列 updated_at 不存在于表 orders", 10, 10, false)
	if !strings.Contains(note, "退化为全量比对") || !strings.Contains(note, "不存在于表 orders") {
		t.Fatalf("fallback note wrong: %q", note)
	}
	if strings.Contains(note, "水位过滤") {
		t.Fatalf("fallback table must NOT carry the scope note: %q", note)
	}

	scope := wmTableNote(wm, "", 10, 10, false)
	if !strings.Contains(scope, "水位过滤 updated_at [") {
		t.Fatalf("scope note missing for non-fallback table: %q", scope)
	}
	if got := wmTableNote(nil, "", 0, 0, false); got != "" {
		t.Fatalf("no watermark and no fallback must yield empty note: %q", got)
	}
}
