// watermark.go is the MongoDB watermark-window layer (第9单「Mongo 水位化
// 对比」, seams ②③④⑤): an OPTIONAL filter dimension over the five-level
// compare pipeline, strictly isomorphic to the pg/mysql watermark mode
// (#t3/#wm-override in internal/validator/wmfilter.go).
//
// Resolution per collection (mirror of wmFilterFor): per-collection override
// entry (active form only) → the global group → full comparison. The filter
// group is the SAME config.WatermarkFilter the SQL side uses (seam ①: pure
// data structure, no dialect coupling) — single value (value + op ≤/<) or
// half-open interval [range_start, range_end), never both.
//
// Precheck (seam ②⑤): the watermark column is probed on the SOURCE side
// with a {col: {$exists: true}} sample; the probed BSON type drives value
// conversion (seam ③). Missing column / non-whitelisted type / value
// conversion failure are CONFIG conditions with pg/mysql Plan A/P2A
// semantics: global group → the collection degrades to full comparison with
// a disclosure note; an override entry → hard FAIL (例外水位列： prefix).
// Probe/cursor failures stay ordinary errors ⇒ collection error. Documents
// missing the field are excluded IDENTICALLY on both sides by the injected
// predicate itself (BSON comparison operators never match a missing field —
// the automatic analog of pg/mysql's "NULL rows excluded on both sides").
//
// Engine injection (seam ④): the predicate is $and-composed into count /
// hash-bucket / diff queries, and rides IDRange's initial $match so bucket
// planning is clamped inside the window; first/last open buckets intersect
// the window via the same $and — no loss, no duplication.
//
// Checkpoint fingerprint (seam ⑥): the predicate's canonical string is
// stamped into checkpoints; a checkpoint produced under a different
// fingerprint is stale and fully recomputed (same boundaries, different
// window ⇒ different content). Empty fingerprint = no watermark — old
// checkpoints stay reusable, and non-watermark runs are byte-identical to
// the legacy engine (red line).
package mongocompare

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/mongocompare/canon"
	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

// WmState values stamp CollectionResult.WmState (第9单 ⑦): the report/API
// layer reads them for the all-skipped gate (webapi) and disclosure.
const (
	WmStateWindowed = "windowed" // the collection ran inside its watermark window
	WmStateDegraded = "degraded" // global group precheck failed → full comparison
	WmStateFailed   = "failed"   // override precheck failed (or probe error) → error row
)

// wmExFailPrefix mirrors wmfilter.go's single-source producer prefix so the
// override hard-fail wording stays family-identical.
const wmExFailPrefix = "例外水位列："

// wmActive mirrors wmfilter.go's activation rule for an arbitrary filter
// group: single-value form or full half-open interval form; anything else
// reads as off (a half-configured entry never reaches the engine through
// the API, but hand-built configs are tolerated the same way).
func wmActive(wm *config.WatermarkFilter) bool {
	return wm != nil && wm.Column != "" && (wm.Value != "" || (wm.RangeStart != "" && wm.RangeEnd != ""))
}

// wmFor resolves the EFFECTIVE filter for one collection: an active
// override entry wins (hit=true); else the global group; else none. The
// returned pointer is a copy — callers never see the config slice element.
func (c Config) wmFor(coll string) (wm *config.WatermarkFilter, override bool) {
	for i := range c.TableWatermarks {
		tw := &c.TableWatermarks[i]
		if tw.Table == coll && wmActive(&tw.WatermarkFilter) {
			w := tw.WatermarkFilter
			return &w, true
		}
	}
	if wmActive(c.Watermark) {
		w := *c.Watermark
		return &w, false
	}
	return nil, false
}

// wmRequested reports whether ANY watermark is configured for the run
// (active global group or at least one active override) — the task-level
// watermark-mode switch mirrored from hasActiveWMOverrides.
func (c Config) wmRequested() bool {
	if wmActive(c.Watermark) {
		return true
	}
	for i := range c.TableWatermarks {
		if wmActive(&c.TableWatermarks[i].WatermarkFilter) {
			return true
		}
	}
	return false
}

// wmWindow is a resolved, engine-ready watermark window for one collection.
type wmWindow struct {
	filter   config.WatermarkFilter // effective group (override or global)
	override bool
	pred     bson.D // typed predicate on the BSON-typed column
	fp       string // checkpoint fingerprint (canonical pred string)
}

// wmOutcome is the precheck gate's verdict for one collection — the analog
// of wmPrecheckGate/wmPrecheckFailEx: at most one of the three is set.
type wmOutcome struct {
	window  *wmWindow // non-nil: run the collection inside the window
	degrade string    // non-empty: global group degraded to full comparison (reason)
	fail    string    // non-empty: hard FAIL message (override precheck, or probe error)
}

// wmDateTimeLayouts are the accepted watermark time spellings (the FE date
// pickers produce the first; ISO/RFC3339 and bare dates are tolerated for
// API callers). Parsed in the server's local zone — Mongo DateTime is a
// tz-naive epoch-millis instant, and the pg/mysql UTC-session convention
// has no analog here (disclosed in the FE informed-consent text).
var wmDateTimeLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	time.RFC3339,
	"2006-01-02",
}

// wmConvertValue converts one raw string endpoint into the BSON value the
// probed column type demands (seam ③). _id additionally admits ObjectId
// and double (the bucket planner's own type set); custom columns are
// whitelisted to date/long/int per the approved plan.
func wmConvertValue(colIsID bool, probed any, raw string) (any, error) {
	switch probed.(type) {
	case bson.ObjectID:
		if !colIsID { // custom ObjectId fields sit outside the plan whitelist
			return nil, fmt.Errorf("ObjectId 水位列仅支持 _id（自定义字段白名单：%s）", wmWhitelistLabel(false))
		}
		if !wmIsObjectIDHex(raw) {
			return nil, fmt.Errorf("ObjectId 水位列的值必须是 24 位十六进制（得到 %q）", raw)
		}
		return bson.ObjectIDFromHex(raw)
	case bson.DateTime:
		if t, ok := wmParseTime(raw); ok {
			return bson.DateTime(t.UnixMilli()), nil
		}
		return nil, fmt.Errorf("时间水位值 %q 无法解析（支持 YYYY-MM-DD HH:mm:ss / ISO / YYYY-MM-DD）", raw)
	case int32, int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("整数水位列的值必须是整数（得到 %q）", raw)
		}
		return n, nil // BSON compares numerically across int32/int64
	case float64:
		if colIsID { // _id only: the planner's own double support
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("数值水位列的值必须是数字（得到 %q）", raw)
			}
			return f, nil
		}
	}
	return nil, fmt.Errorf("水位列类型 %T 不可比（白名单：%s）", probed, wmWhitelistLabel(colIsID))
}

func wmWhitelistLabel(colIsID bool) string {
	if colIsID {
		return "ObjectId/date/int/long/double"
	}
	return "date/int/long"
}

func wmIsObjectIDHex(s string) bool {
	if len(s) != 24 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// wmParseTime parses one raw endpoint against the accepted time layouts
// (server-local zone — the disclosed convention).
func wmParseTime(raw string) (time.Time, bool) {
	for _, layout := range wmDateTimeLayouts {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func wmIntPair(lo, hi string) bool {
	_, errLo := strconv.ParseInt(lo, 10, 64)
	_, errHi := strconv.ParseInt(hi, 10, 64)
	return errLo == nil && errHi == nil
}

func wmTimePair(lo, hi string) bool {
	_, okLo := wmParseTime(lo)
	_, okHi := wmParseTime(hi)
	return okLo && okHi
}

// WmRangeOrderErr re-checks a RANGE-mode group's endpoint order in the
// TYPE domain (对抗票 P2 / 攻击面 d): the shared webapi normalizer orders
// raw STRINGS, but mixed-case ObjectId hex orders differently as ASCII
// than as bytes ("..0F" < "..0a" as strings, 0x0F > 0x0A as bytes) and
// int endpoints order differently as strings than numerically ("10" < "9"
// as strings, 10 > 9 as numbers) — both slip the string gate and yield a
// silently-empty half-open window that reports "equal". Creation time
// cannot know the probe type, so the domain is heuristic: 24-hex pairs
// compare lowercased (case-folded ASCII order == byte order for
// fixed-width hex), int64 pairs numerically, parseable time pairs by
// instant; anything else keeps the normalizer's string verdict — the
// engine's post-conversion guard (wmBsonOrdered) is the authoritative
// backstop. Non-range groups have no order to check.
func WmRangeOrderErr(wm *config.WatermarkFilter) error {
	if wm == nil || wm.RangeStart == "" || wm.RangeEnd == "" {
		return nil
	}
	lo, hi := wm.RangeStart, wm.RangeEnd
	empty := fmt.Errorf("watermark: range_start 不能大于等于 range_end（半开区间 [start,end) 恒为空；比较按实际类型序：ObjectId 十六进制字节序/整数数值序/时间时刻序）")
	switch {
	case wmIsObjectIDHex(lo) && wmIsObjectIDHex(hi):
		if strings.ToLower(lo) >= strings.ToLower(hi) {
			return empty
		}
	case wmIntPair(lo, hi):
		nlo, _ := strconv.ParseInt(lo, 10, 64)
		nhi, _ := strconv.ParseInt(hi, 10, 64)
		if nlo >= nhi {
			return empty
		}
	case wmTimePair(lo, hi):
		tlo, _ := wmParseTime(lo)
		thi, _ := wmParseTime(hi)
		if !tlo.Before(thi) {
			return empty
		}
	}
	return nil
}

// wmBsonOrdered reports whether two CONVERTED range endpoints order
// strictly ascending in the BSON type domain — the authoritative
// post-conversion backstop behind WmRangeOrderErr's creation-time
// heuristic (mixed time layouts and hand-built configs land here; a
// non-ascending verdict flows through the same Plan A/P2A gate as any
// other precheck condition). Types without a sound pairing keep the run.
func wmBsonOrdered(lo, hi any) bool {
	switch l := lo.(type) {
	case bson.ObjectID:
		h, ok := hi.(bson.ObjectID)
		return ok && bytes.Compare(l[:], h[:]) < 0
	case bson.DateTime:
		h, ok := hi.(bson.DateTime)
		return ok && l < h
	case int64:
		h, ok := hi.(int64)
		return ok && l < h
	case float64:
		h, ok := hi.(float64)
		return ok && l < h
	}
	return true
}

// resolveWMWindow runs the per-collection precheck + conversion and returns
// the gate verdict (seam ②⑤). Probe/cursor failures are ordinary errors
// (error row on BOTH global and override paths, mirroring wmPrecheckGate's
// ordinary-error branch); column/config conditions follow Plan A/P2A.
func resolveWMWindow(ctx context.Context, sc mongoconn.Coll, cfg Config, coll string) wmOutcome {
	wm, override := cfg.wmFor(coll)
	if wm == nil {
		return wmOutcome{} // no watermark for this collection
	}
	colIsID := wm.Column == "_id"

	// $exists sample probe (seam ②): the probed document both proves the
	// field exists somewhere in the collection and supplies its BSON type.
	probeQ := bson.D{{Key: wm.Column, Value: bson.D{{Key: "$exists", Value: true}}}}
	probe, err := sc.Find(ctx, probeQ, mongoconn.FindOpts{Batch: 1, Limit: 1})
	if err != nil {
		return wmOutcome{fail: fmt.Sprintf("水位列探针失败：%v", err)}
	}
	var probeDoc bson.D
	if probe.Next(ctx) {
		probeDoc = probe.Doc()
	}
	probeErr := probe.Err()
	_ = probe.Close(ctx)
	if probeErr != nil {
		return wmOutcome{fail: fmt.Sprintf("水位列探针失败：%v", probeErr)}
	}

	fail := func(format string, args ...any) wmOutcome {
		msg := fmt.Sprintf(format, args...)
		if override {
			return wmOutcome{fail: wmExFailPrefix + msg}
		}
		return wmOutcome{degrade: msg}
	}

	val, ok := wmFieldOf(probeDoc, wm.Column)
	if !ok {
		return fail("字段 %s 不存在（集合内没有文档包含该字段）", wm.Column)
	}
	if _, convOK := wmConvertibleType(colIsID, val); !convOK {
		return fail("字段 %s 类型 %T 不可比（白名单：%s）", wm.Column, val, wmWhitelistLabel(colIsID))
	}

	build := func(op, raw string) (any, bool) {
		v, err := wmConvertValue(colIsID, val, raw)
		if err != nil {
			return nil, false
		}
		return v, true
	}

	var pred bson.D
	if wm.RangeStart != "" && wm.RangeEnd != "" {
		lo, ok1 := build("$gte", wm.RangeStart)
		hi, ok2 := build("$lt", wm.RangeEnd)
		if !ok1 || !ok2 {
			_, errLo := wmConvertValue(colIsID, val, wm.RangeStart)
			reason := errLo
			if reason == nil {
				reason = fmt.Errorf("区间终点 %q 转换失败", wm.RangeEnd)
			} else {
				reason = fmt.Errorf("区间起点 %q 转换失败：%w", wm.RangeStart, errLo)
			}
			return fail("%s", reason.Error())
		}
		if !wmBsonOrdered(lo, hi) {
			return fail("水位区间 [%s, %s) 区间起点不小于终点（按值域比较）", wm.RangeStart, wm.RangeEnd)
		}
		pred = bson.D{{Key: wm.Column, Value: bson.D{
			{Key: "$gte", Value: lo},
			{Key: "$lt", Value: hi},
		}}}
	} else {
		op := "$lte"
		if wm.Op == "<" {
			op = "$lt"
		}
		v, okV := build(op, wm.Value)
		if !okV {
			_, errV := wmConvertValue(colIsID, val, wm.Value)
			return fail("水位值转换失败：%s", errV.Error())
		}
		pred = bson.D{{Key: wm.Column, Value: bson.D{{Key: op, Value: v}}}}
	}

	return wmOutcome{window: &wmWindow{
		filter:   *wm,
		override: override,
		pred:     pred,
		fp:       canon.Canonical(pred, cfg.Opts),
	}}
}

// wmConvertibleType reports whether the probed BSON type is whitelist-able
// (seam ②'s type gate — the same set wmConvertValue converts).
func wmConvertibleType(colIsID bool, v any) (any, bool) {
	switch v.(type) {
	case bson.ObjectID:
		return v, colIsID // a custom ObjectId field is outside the plan whitelist
	case bson.DateTime, int32, int64:
		return v, true
	case float64:
		return v, colIsID
	}
	return nil, false
}

// wmFieldOf extracts a top-level field from a probe document (column names
// passed the identifier allow-list upstream, so no dotted-path traversal).
func wmFieldOf(doc bson.D, col string) (any, bool) {
	for _, e := range doc {
		if e.Key == col {
			return e.Value, true
		}
	}
	return nil, false
}

// wmAnd composes a bucket/level query with the watermark predicate (seam
// ④): empty sides pass through verbatim; otherwise $and. The same composed
// document is injected on BOTH sides — the identical row-universe contract.
func wmAnd(query bson.D, w *wmWindow) bson.D {
	if w == nil {
		return query
	}
	if len(query) == 0 {
		return w.pred
	}
	return bson.D{{Key: "$and", Value: bson.A{query, w.pred}}}
}

// wmFallbackNote mirrors wmfilter.go's disclosure wording, collection-ized.
func wmFallbackNote(reason string) string {
	return fmt.Sprintf("水位列不可用（%s），该集合已退化为全量比对", reason)
}

// wmScopeNote mirrors wmTableNote's disclosure: the override tag, the
// effective condition, and — when the count level ran — the windowed row
// counts. haveCounts=false keeps the note shape but drops the tail.
func wmScopeNote(w *wmWindow, srcRows, dstRows int64, haveCounts bool) string {
	tag := ""
	if w.override {
		tag = "(例外)"
	}
	cond := func() string {
		if w.filter.RangeStart != "" && w.filter.RangeEnd != "" {
			return fmt.Sprintf("%s [%s, %s)", w.filter.Column, w.filter.RangeStart, w.filter.RangeEnd)
		}
		op := w.filter.Op
		if op != "<" {
			op = "<="
		}
		return fmt.Sprintf("%s %s %s", w.filter.Column, op, w.filter.Value)
	}()
	if !haveCounts {
		return fmt.Sprintf("水位过滤%s %s", tag, cond)
	}
	return fmt.Sprintf("水位过滤%s %s（源 %d 行/目标 %d 行）", tag, cond, srcRows, dstRows)
}

// wmStateRequestedLabel is not used by the engine; wmStateLabel renders the
// report-facing short state for a collection row (HTML badge text).
func wmStateLabel(s string) string {
	switch s {
	case WmStateWindowed:
		return "窗口"
	case WmStateDegraded:
		return "退化"
	case WmStateFailed:
		return "失败"
	}
	return ""
}

// WmAnyWindowed answers the all-skipped gate (seam ⑦, webapi side): did at
// least one collection actually run inside a watermark window?
func WmAnyWindowed(rows []CollectionResult) bool {
	for i := range rows {
		if rows[i].WmState == WmStateWindowed {
			return true
		}
	}
	return false
}

// wmTrim is a small helper kept for symmetry with the webapi normalize
// layer (values arrive pre-trimmed there; engine-side configs may be
// hand-built) — trims the string fields of a filter group in place copy.
func wmTrim(wm config.WatermarkFilter) config.WatermarkFilter {
	wm.Column = strings.TrimSpace(wm.Column)
	wm.Value = strings.TrimSpace(wm.Value)
	wm.RangeStart = strings.TrimSpace(wm.RangeStart)
	wm.RangeEnd = strings.TrimSpace(wm.RangeEnd)
	return wm
}
