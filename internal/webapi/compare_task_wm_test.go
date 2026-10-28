// compare_task_wm_test.go locks the 第9单 watermark plumbing on the webapi
// side: the create path reuses the pg/mysql normalize validators verbatim
// (mutual exclusion / interval completeness / op enum / identifier allow-list
// / duplicate overrides), BaseMode is stripped (the mongo engine has no
// quick/sample/checksum dimension), the group round-trips into ConfigJSON,
// and the HTML report carries the 水位过滤 mode tag + per-collection cell.
package webapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/mongocompare"
	"github.com/michaelliuyuan/timstool/internal/store"
)

// TestCreateCompareTaskWatermarkValidation: every reject the shared
// normalizers produce must 400 BEFORE any store write.
func TestCreateCompareTaskWatermarkValidation(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name string
		body string
	}{
		{"value and range mutually exclusive", `{
			"source":{"uri":"mongodb://s","database":"a"},
			"watermark":{"column":"_id","value":"670d2b0f0000000000000001","range_start":"2026-10-01 00:00:00","range_end":"2026-10-02 00:00:00"}}`},
		{"half interval", `{
			"source":{"uri":"mongodb://s","database":"a"},
			"watermark":{"column":"_id","range_start":"2026-10-01 00:00:00"}}`},
		{"neither value form", `{"source":{"uri":"mongodb://s","database":"a"},"watermark":{"column":"_id"}}`},
		{"bad op", `{"source":{"uri":"mongodb://s","database":"a"},"watermark":{"column":"_id","value":"1","op":">="}}`},
		{"bad column identifier", `{"source":{"uri":"mongodb://s","database":"a"},"watermark":{"column":"a; drop","value":"1"}}`},
		{"empty column", `{"source":{"uri":"mongodb://s","database":"a"},"watermark":{"value":"1"}}`},
		{"range start >= end", `{
			"source":{"uri":"mongodb://s","database":"a"},
			"watermark":{"column":"ts","range_start":"2026-10-02 00:00:00","range_end":"2026-10-01 00:00:00"}}`},
		{"override duplicate collections", `{
			"source":{"uri":"mongodb://s","database":"a"},
			"table_watermarks":[
				{"table":"orders","column":"ts","value":"1"},
				{"table":"orders","column":"ts","value":"2"}]}`},
		{"override empty table", `{
			"source":{"uri":"mongodb://s","database":"a"},
			"table_watermarks":[{"table":"  ","column":"ts","value":"1"}]}`},
		{"override bad filter", `{
			"source":{"uri":"mongodb://s","database":"a"},
			"table_watermarks":[{"table":"orders","column":"ts"}]}`},
		// 对抗票 P2 (攻击面 d): both pairs pass the SHARED string-order
		// gate ("..0F" < "..0a" and "10" < "9" as ASCII) yet are empty
		// windows in the type domain — the mongo creation gate must 400.
		{"type-domain range order: mixed-case hex", `{
			"source":{"uri":"mongodb://s","database":"a"},
			"watermark":{"column":"_id","range_start":"670d2b0f000000000000000F","range_end":"670d2b0f000000000000000a"}}`},
		{"type-domain range order: override int 10 vs 9", `{
			"source":{"uri":"mongodb://s","database":"a"},
			"table_watermarks":[{"table":"orders","column":"n","range_start":"10","range_end":"9"}]}`},
	}
	for _, c := range cases {
		rec := postCompareSpec(t, s, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d want 400, body=%s", c.name, rec.Code, rec.Body.String())
		}
	}
}

// TestCreateCompareTaskWatermarkRoundTrip: a normalized group (defaults
// filled, BaseMode stripped on BOTH the global group and overrides) is what
// the persisted ConfigJSON carries.
func TestCreateCompareTaskWatermarkRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	rec := postCompareSpec(t, s, `{
		"source": {"uri":"mongodb://s","database":"a"},
		"target": {"uri":"mongodb://t","database":"b"},
		"watermark": {"column":" ts ","value":" 670d2b0f0000000000000001 ","base_mode":"quick"},
		"table_watermarks": [{"table":" orders ","column":"ts","range_start":"2026-10-01 00:00:00","range_end":"2026-10-02 00:00:00","base_mode":"quick"}]
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := dsBody(t, rec)["id"].(string)
	task, err := s.store.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := parseCompareTask(task.ConfigJSON)
	if !ok {
		t.Fatalf("not a compare config: %s", task.ConfigJSON)
	}
	if spec.Watermark == nil || spec.Watermark.Column != "ts" || spec.Watermark.Value != "670d2b0f0000000000000001" {
		t.Fatalf("global group not normalized into the persisted spec: %+v", spec.Watermark)
	}
	if spec.Watermark.Op != "<=" || spec.Watermark.BaseMode != "" {
		t.Fatalf("op default/base_mode strip broken: %+v", spec.Watermark)
	}
	if len(spec.TableWatermarks) != 1 || spec.TableWatermarks[0].Table != "orders" {
		t.Fatalf("override not normalized: %+v", spec.TableWatermarks)
	}
	if spec.TableWatermarks[0].BaseMode != "" || spec.TableWatermarks[0].RangeStart != "2026-10-01 00:00:00" {
		t.Fatalf("override base_mode must be stripped, endpoints trimmed: %+v", spec.TableWatermarks[0])
	}
}

// TestCompareReportWatermarkFace: the HTML family report keys the 水位过滤
// mode tag and the per-collection 水位 column off the spec/engine state.
func TestCompareReportWatermarkFace(t *testing.T) {
	task := &store.Task{ID: "wm1", Name: "wm report"}
	spec := &compareTaskSpec{
		Watermark: &config.WatermarkFilter{Column: "ts", RangeStart: "2026-10-24 00:00:00", RangeEnd: "2026-10-25 00:00:00", Op: "<="},
	}
	sum := &mongocompare.Summary{Overall: mongocompare.OverallEqual}
	sum.Counts = map[string]int{"equal": 2, "diff": 0, "error": 0, "ttl_skip": 0}
	var win, deg mongocompare.CollectionResult
	win.DB, win.Coll, win.Overall, win.WmState = "sdb", "orders", mongocompare.OverallEqual, mongocompare.WmStateWindowed
	win.Note = "水位过滤 ts [2026-10-24 00:00:00, 2026-10-25 00:00:00)（源 5 行/目标 5 行）"
	deg.DB, deg.Coll, deg.Overall, deg.WmState = "sdb", "logs", mongocompare.OverallEqual, mongocompare.WmStateDegraded
	deg.Note = "水位列不可用（字段 absent 不存在（集合内没有文档包含该字段）），该集合已退化为全量比对"
	sum.Collections = []mongocompare.CollectionResult{win, deg}

	html := renderCompareReportHTML(task, spec, sum, "")
	for _, want := range []string{
		`<div>模式 <span>水位过滤</span></div>`,
		"<th>水位</th>",
		`<span class="badge badge-pass" title="`, // windowed badge carries the note
		`<span class="badge badge-skip" title="`, // degraded badge carries the note
		"窗口", "退化全量",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("watermark face missing %q", want)
		}
	}

	// No watermark in the spec: no mode tag (the per-row notes above may
	// legitimately repeat the phrase — the TAG is the precise anchor).
	plain := renderCompareReportHTML(task, &compareTaskSpec{}, sum, "")
	if strings.Contains(plain, `<div>模式 <span>水位过滤</span></div>`) {
		t.Errorf("non-watermark spec must not carry the mode tag")
	}
}
