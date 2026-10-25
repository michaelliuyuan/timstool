// watermark_test.go locks the 第9单 watermark-window semantics offline (no
// Docker): value conversion per probed BSON type, override/global resolution
// (the wmFilterFor mirror), Plan A/P2A precheck outcomes ($exists probe),
// predicate composition ($and), checkpoint fingerprinting, and the
// engine-level injection anchors (count filter / IDRange match / scope note).
package mongocompare

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/mongoconn"
	"github.com/michaelliuyuan/timstool/internal/source"
)

// ---- scripted fixtures ----

// wmIter streams a fixed batch once (the probe uses Limit 1).
type wmIter struct {
	docs []bson.D
	idx  int
}

func (it *wmIter) Next(ctx context.Context) bool { return it.idx < len(it.docs) }
func (it *wmIter) Doc() bson.D {
	d := it.docs[it.idx]
	it.idx++
	return d
}
func (it *wmIter) Err() error                      { return nil }
func (it *wmIter) Close(ctx context.Context) error { return nil }

// wmFakeColl captures the engine-facing calls; only the surfaces the
// watermark layer touches carry behavior.
type wmFakeColl struct {
	probeDocs []bson.D // streamed for ANY Find (the layer only probes)
	findErr   error
	findCalls []bson.D
	countN    int64
	countErr  error
	countFlts []bson.D
	rangeFlts []bson.D
	rangeStat mongoconn.RangeStats
}

func (c *wmFakeColl) Find(ctx context.Context, q bson.D, opts mongoconn.FindOpts) (mongoconn.DocIter, error) {
	c.findCalls = append(c.findCalls, q)
	if c.findErr != nil {
		return nil, c.findErr
	}
	return &wmIter{docs: c.probeDocs}, nil
}
func (c *wmFakeColl) IDRange(ctx context.Context, match bson.D) (mongoconn.RangeStats, error) {
	c.rangeFlts = append(c.rangeFlts, match)
	return c.rangeStat, nil
}
func (c *wmFakeColl) CountDocuments(ctx context.Context, filter bson.D) (int64, error) {
	c.countFlts = append(c.countFlts, filter)
	if c.countErr != nil {
		return 0, c.countErr
	}
	return c.countN, nil
}
func (c *wmFakeColl) IndexSpecs(ctx context.Context) ([]source.IndexInfo, error) { return nil, nil }

// wmFakeClient adapts wmFakeColl to the Client surface compareCollection
// consumes: the embedded interface satisfies the method set (only
// Collection is ever called on this path) and the override routes it to the
// scripted coll.
type wmFakeClient struct {
	mongoconn.Client
	coll mongoconn.Coll
}

func (c *wmFakeClient) Collection(db, coll string) mongoconn.Coll { return c.coll }

func wmClientOf(c *wmFakeColl) *wmFakeClient { return &wmFakeClient{coll: c} }

// ---- conversion (seam ③) ----

func TestWmConvertValue(t *testing.T) {
	oidHex := "670d2b0f0000000000000001"
	oid, err := wmConvertValue(true, bson.ObjectID{}, oidHex)
	if err != nil || oid.(bson.ObjectID).Hex() != oidHex {
		t.Fatalf("objectid conversion: %v %+v", err, oid)
	}
	if _, err := wmConvertValue(true, bson.ObjectID{}, "not-hex"); err == nil {
		t.Fatalf("non-hex objectid value must fail")
	}
	if _, err := wmConvertValue(false, bson.ObjectID{}, oidHex); err == nil {
		t.Fatalf("custom ObjectId field is outside the whitelist")
	}

	dt, err := wmConvertValue(false, bson.DateTime(0), "2026-10-24 00:00:00")
	if err != nil {
		t.Fatalf("datetime conversion: %v", err)
	}
	want := time.Date(2026, 10, 24, 0, 0, 0, 0, time.Local).UnixMilli()
	if int64(dt.(bson.DateTime)) != want {
		t.Fatalf("datetime = %d, want %d", dt, want)
	}
	for _, s := range []string{"2026-10-24T08:00:00", "2026-10-24T08:00:00+08:00", "2026-10-24"} {
		if _, err := wmConvertValue(false, bson.DateTime(0), s); err != nil {
			t.Fatalf("datetime layout %q must parse: %v", s, err)
		}
	}
	if _, err := wmConvertValue(false, bson.DateTime(0), "yesterday"); err == nil {
		t.Fatalf("unparseable datetime must fail")
	}

	n, err := wmConvertValue(false, int32(0), "42")
	if err != nil || n.(int64) != 42 {
		t.Fatalf("int conversion: %v %+v", err, n)
	}
	if _, err := wmConvertValue(false, int64(0), "4.5"); err == nil {
		t.Fatalf("non-integer on an int column must fail")
	}
	f, err := wmConvertValue(true, float64(0), "1.5")
	if err != nil || f.(float64) != 1.5 {
		t.Fatalf("double _id conversion: %v %+v", err, f)
	}
	if _, err := wmConvertValue(false, float64(0), "1.5"); err == nil {
		t.Fatalf("custom double field is outside the whitelist")
	}
	if _, err := wmConvertValue(false, "string", "x"); err == nil {
		t.Fatalf("string field must fail (whitelist)")
	}
}

// ---- resolution (seam ① mirror) ----

func TestWmForResolution(t *testing.T) {
	global := &config.WatermarkFilter{Column: "_id", Value: "670d2b0f0000000000000001"}
	override := config.TableWatermark{Table: "orders", WatermarkFilter: config.WatermarkFilter{Column: "ts", RangeStart: "2026-10-01 00:00:00", RangeEnd: "2026-10-02 00:00:00"}}
	half := config.TableWatermark{Table: "half", WatermarkFilter: config.WatermarkFilter{Column: "ts"}}

	cfg := Config{Watermark: global, TableWatermarks: []config.TableWatermark{override, half}}

	if wm, ov := cfg.wmFor("orders"); !ov || wm.Column != "ts" {
		t.Fatalf("override must win for orders: %+v ov=%v", wm, ov)
	}
	if wm, ov := cfg.wmFor("half"); ov || wm.Column != "_id" {
		t.Fatalf("inactive override must fall back to global: %+v ov=%v", wm, ov)
	}
	if wm, _ := cfg.wmFor("other"); wm == nil || wm.Column != "_id" {
		t.Fatalf("global must apply to other: %+v", wm)
	}
	if wm, _ := (Config{}).wmFor("x"); wm != nil {
		t.Fatalf("no watermark configured must resolve nil")
	}
	if !cfg.wmRequested() || (Config{}).wmRequested() {
		t.Fatalf("wmRequested mirror broken")
	}
}

// ---- precheck outcomes (seam ②⑤) ----

func TestResolveWMWindowOutcomes(t *testing.T) {
	ctx := context.Background()
	oidDoc := bson.D{{Key: "_id", Value: bson.ObjectID{}}, {Key: "ts", Value: bson.DateTime(0)}}

	// happy path, global single-value on _id (ObjectId): windowed + pred shape.
	sc := &wmFakeColl{probeDocs: []bson.D{oidDoc}}
	out := resolveWMWindow(ctx, sc, Config{Watermark: &config.WatermarkFilter{Column: "_id", Value: "670d2b0f0000000000000001"}}, "orders")
	if out.window == nil || out.degrade != "" || out.fail != "" {
		t.Fatalf("expected windowed, got %+v", out)
	}
	if got := out.window.pred[0].Key; got != "_id" {
		t.Fatalf("pred key = %q", got)
	}
	if _, ok := out.window.pred[0].Value.(bson.D)[0].Value.(bson.ObjectID); !ok {
		t.Fatalf("pred value must be typed ObjectID: %#v", out.window.pred)
	}
	if op := out.window.pred[0].Value.(bson.D)[0].Key; op != "$lte" {
		t.Fatalf("default op must be $lte, got %q", op)
	}
	// the probe rode $exists on the configured column
	if len(sc.findCalls) != 1 || sc.findCalls[0][0].Key != "_id" {
		t.Fatalf("probe query = %#v", sc.findCalls)
	}
	if _, ok := sc.findCalls[0][0].Value.(bson.D)[0].Value.(bool); !ok || sc.findCalls[0][0].Value.(bson.D)[0].Key != "$exists" {
		t.Fatalf("probe must be {col:{$exists:true}}: %#v", sc.findCalls[0])
	}

	// strict op '<'.
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{oidDoc}}, Config{Watermark: &config.WatermarkFilter{Column: "_id", Op: "<", Value: "670d2b0f0000000000000001"}}, "orders")
	if op := out.window.pred[0].Value.(bson.D)[0].Key; op != "$lt" {
		t.Fatalf("explicit < must map to $lt, got %q", op)
	}

	// range form: [$gte, $lt) on a custom DateTime field.
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{oidDoc}}, Config{Watermark: &config.WatermarkFilter{Column: "ts", RangeStart: "2026-10-24 00:00:00", RangeEnd: "2026-10-25 00:00:00"}}, "orders")
	if out.window == nil {
		t.Fatalf("range window missing: %+v", out)
	}
	ops := out.window.pred[0].Value.(bson.D)
	if ops[0].Key != "$gte" || ops[1].Key != "$lt" {
		t.Fatalf("range ops = %#v", ops)
	}

	// missing field: global → degrade note path; override → 例外水位列 prefix.
	cfgNo := Config{Watermark: &config.WatermarkFilter{Column: "absent", Value: "1"}}
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{oidDoc}}, cfgNo, "orders")
	if out.window != nil || out.fail != "" || out.degrade == "" {
		t.Fatalf("missing field global must degrade: %+v", out)
	}
	cfgOv := Config{TableWatermarks: []config.TableWatermark{{Table: "orders", WatermarkFilter: config.WatermarkFilter{Column: "absent", Value: "1"}}}}
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{oidDoc}}, cfgOv, "orders")
	if out.degrade != "" || out.window != nil || len(out.fail) < len(wmExFailPrefix) || out.fail[:len(wmExFailPrefix)] != wmExFailPrefix {
		t.Fatalf("missing field override must hard-fail with prefix: %+v", out)
	}

	// non-whitelisted type (string field): same split.
	strDoc := bson.D{{Key: "_id", Value: bson.ObjectID{}}, {Key: "s", Value: "text"}}
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{strDoc}}, cfgNo, "orders")
	if out.degrade == "" || out.fail != "" {
		t.Fatalf("string column global must degrade: %+v", out)
	}
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{strDoc}}, Config{TableWatermarks: []config.TableWatermark{{Table: "orders", WatermarkFilter: config.WatermarkFilter{Column: "s", Value: "x"}}}}, "orders")
	if out.fail == "" {
		t.Fatalf("string column override must fail: %+v", out)
	}

	// conversion failure on a whitelisted type: value "zzz" for int column.
	intDoc := bson.D{{Key: "_id", Value: bson.ObjectID{}}, {Key: "n", Value: int32(7)}}
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{intDoc}}, Config{Watermark: &config.WatermarkFilter{Column: "n", Value: "zzz"}}, "orders")
	if out.degrade == "" {
		t.Fatalf("conversion failure global must degrade: %+v", out)
	}

	// empty collection (probe finds nothing): treated as column-unavailable.
	out = resolveWMWindow(ctx, &wmFakeColl{}, cfgNo, "orders")
	if out.degrade == "" {
		t.Fatalf("empty collection must degrade the global group: %+v", out)
	}

	// probe error: ordinary error (fail) on both paths.
	out = resolveWMWindow(ctx, &wmFakeColl{findErr: os.ErrDeadlineExceeded}, cfgNo, "orders")
	if out.fail == "" || out.degrade != "" || out.window != nil {
		t.Fatalf("probe error must be an ordinary fail: %+v", out)
	}
}

// ---- range order in the TYPE domain (对抗票 P2 / 攻击面 d) ----

// TestWmRangeOrderErr: the creation-time gate must catch pairs the shared
// string-order normalizer waves through — mixed-case ObjectId hex ("..0F"
// < "..0a" as ASCII, 0x0F > 0x0A as bytes) and int endpoints ("10" < "9"
// as strings, 10 > 9 as numbers) — while sound windows of every domain
// (including cross-layout datetime pairs a bare string compare would
// false-reject) stay green.
func TestWmRangeOrderErr(t *testing.T) {
	bad := []struct {
		name string
		wm   config.WatermarkFilter
	}{
		{"mixed-case hex inverts as bytes", config.WatermarkFilter{Column: "_id",
			RangeStart: "670d2b0f000000000000000F", RangeEnd: "670d2b0f000000000000000a"}},
		{"same hex digit different case = equal bytes", config.WatermarkFilter{Column: "_id",
			RangeStart: "670d2b0f000000000000000f", RangeEnd: "670d2b0f000000000000000F"}},
		{"int 10 vs 9 inverts numerically", config.WatermarkFilter{Column: "n",
			RangeStart: "10", RangeEnd: "9"}},
		{"datetime chrono-inverted", config.WatermarkFilter{Column: "ts",
			RangeStart: "2026-10-25 00:00:00", RangeEnd: "2026-10-24T00:00:00Z"}},
	}
	for _, c := range bad {
		if err := WmRangeOrderErr(&c.wm); err == nil {
			t.Errorf("%s: must reject", c.name)
		}
	}
	good := []struct {
		name string
		wm   config.WatermarkFilter
	}{
		{"lowercase hex ascending", config.WatermarkFilter{Column: "_id",
			RangeStart: "670d2b0f000000000000000a", RangeEnd: "670d2b0f000000000000000f"}},
		{"uppercase hex ascending", config.WatermarkFilter{Column: "_id",
			RangeStart: "670d2b0f000000000000000A", RangeEnd: "670d2b0f000000000000000F"}},
		{"int 9 vs 10 ascending", config.WatermarkFilter{Column: "n",
			RangeStart: "9", RangeEnd: "10"}},
		{"cross-layout datetime ascending (string compare alone would false-reject)", config.WatermarkFilter{Column: "ts",
			RangeStart: "2026-10-24T00:00:00Z", RangeEnd: "2026-10-25 00:00:00"}},
		{"single value has no order", config.WatermarkFilter{Column: "_id", Value: "670d2b0f0000000000000001"}},
	}
	for _, c := range good {
		if err := WmRangeOrderErr(&c.wm); err != nil {
			t.Errorf("%s: must pass, got %v", c.name, err)
		}
	}
	if err := WmRangeOrderErr(nil); err != nil {
		t.Errorf("nil group must pass, got %v", err)
	}
}

// TestResolveWMWindowEmptyRangeGuard: the engine's post-conversion guard —
// a range that inverts in the BSON type domain NEVER runs as a silent
// empty window: the global group degrades to full comparison (disclosed),
// an override hard-fails with the family prefix; a sound window passes.
func TestResolveWMWindowEmptyRangeGuard(t *testing.T) {
	ctx := context.Background()
	oidDoc := bson.D{{Key: "_id", Value: bson.ObjectID{}}}
	cfgG := Config{Watermark: &config.WatermarkFilter{Column: "_id",
		RangeStart: "670d2b0f000000000000000F", RangeEnd: "670d2b0f000000000000000a"}}

	out := resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{oidDoc}}, cfgG, "orders")
	if out.window != nil || out.fail != "" || out.degrade == "" || !strings.Contains(out.degrade, "区间起点不小于终点（按值域比较）") {
		t.Fatalf("type-domain empty range must degrade the global group: %+v", out)
	}
	cfgOv := Config{TableWatermarks: []config.TableWatermark{{Table: "orders", WatermarkFilter: *cfgG.Watermark}}}
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{oidDoc}}, cfgOv, "orders")
	if out.degrade != "" || out.window != nil || len(out.fail) < len(wmExFailPrefix) || out.fail[:len(wmExFailPrefix)] != wmExFailPrefix {
		t.Fatalf("type-domain empty range override must hard-fail with prefix: %+v", out)
	}
	out = resolveWMWindow(ctx, &wmFakeColl{probeDocs: []bson.D{oidDoc}}, Config{Watermark: &config.WatermarkFilter{Column: "_id",
		RangeStart: "670d2b0f000000000000000a", RangeEnd: "670d2b0f000000000000000f"}}, "orders")
	if out.window == nil || out.degrade != "" || out.fail != "" {
		t.Fatalf("ascending lowercase hex range must stay windowed: %+v", out)
	}
}

// ---- composition + fingerprint (seam ④⑥) ----

func TestWmAnd(t *testing.T) {
	pred := bson.D{{Key: "ts", Value: bson.D{{Key: "$lt", Value: 9}}}}
	bucket := bson.D{{Key: "_id", Value: bson.D{{Key: "$gte", Value: 1}, {Key: "$lt", Value: 2}}}}
	and := wmAnd(bucket, &wmWindow{pred: pred})
	if and[0].Key != "$and" {
		t.Fatalf("composed query must lead with $and: %#v", and)
	}
	arr := and[0].Value.(bson.A)
	if len(arr) != 2 {
		t.Fatalf("$and must carry both sides: %#v", arr)
	}
	if got := wmAnd(bucket, nil); len(got) != len(bucket) {
		t.Fatalf("nil window must pass the query through: %#v", got)
	}
	if got := wmAnd(bson.D{}, &wmWindow{pred: pred}); got[0].Key != "ts" {
		t.Fatalf("empty bucket query must become the bare pred: %#v", got)
	}
}

func TestCheckpointWmFingerprint(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".checkpoints"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Resume: true, OutDir: dir}
	// A legacy checkpoint (no wm_fp) written under no watermark.
	legacy := checkpoint{AlgoVersion: AlgoVersion, Done: true, Buckets: map[int]cpBucket{0: {Bucket: 0, Match: true}}}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(cpPath(dir, "db", "coll"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	// Same fingerprint ("" == ""): reusable — backward compatible.
	cp := loadCheckpoint(cfg, "db", "coll", "")
	if !cp.Done || len(cp.Buckets) != 1 {
		t.Fatalf("legacy checkpoint must stay reusable without watermark: %+v", cp)
	}
	// Different fingerprint (watermark now active): discarded wholesale.
	cp = loadCheckpoint(cfg, "db", "coll", "fp1")
	if cp.Done || len(cp.Buckets) != 0 {
		t.Fatalf("fingerprint mismatch must discard: %+v", cp)
	}
	// Stored fingerprint round-trips when it matches.
	stored := checkpoint{AlgoVersion: AlgoVersion, Done: true, WmFP: "fp1", Buckets: map[int]cpBucket{0: {Bucket: 0}}}
	b, _ = json.Marshal(stored)
	if err := os.WriteFile(cpPath(dir, "db", "coll"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if cp := loadCheckpoint(cfg, "db", "coll", "fp1"); !cp.Done || len(cp.Buckets) != 1 {
		t.Fatalf("matching fingerprint must reuse: %+v", cp)
	}
	if cp := loadCheckpoint(cfg, "db", "coll", "fp2"); cp.Done || len(cp.Buckets) != 0 {
		t.Fatalf("fp2 vs fp1 must discard: %+v", cp)
	}
}

// ---- engine-level injection anchors (seam ④⑤⑦) ----

func TestCompareCollectionInjectsPredicates(t *testing.T) {
	ctx := context.Background()
	oidDoc := bson.D{{Key: "_id", Value: bson.ObjectID{}}, {Key: "ts", Value: bson.DateTime(0)}}
	pred := bson.D{{Key: "ts", Value: bson.D{
		{Key: "$gte", Value: bson.DateTime(time.Date(2026, 10, 24, 0, 0, 0, 0, time.Local).UnixMilli())},
		{Key: "$lt", Value: bson.DateTime(time.Date(2026, 10, 25, 0, 0, 0, 0, time.Local).UnixMilli())},
	}}}

	// Windowed run: count filter and IDRange match both ride the predicate;
	// the row carries the scope note with the windowed counts.
	sc := &wmFakeColl{probeDocs: []bson.D{oidDoc}, countN: 5, rangeStat: mongoconn.RangeStats{N: 0}}
	tc := &wmFakeColl{probeDocs: []bson.D{oidDoc}, countN: 5, rangeStat: mongoconn.RangeStats{N: 0}}
	cfg := Config{
		Levels:    []Level{LevelCount, LevelHash},
		Watermark: &config.WatermarkFilter{Column: "ts", RangeStart: "2026-10-24 00:00:00", RangeEnd: "2026-10-25 00:00:00"},
	}
	r := compareCollection(ctx, wmClientOf(sc), wmClientOf(tc), "sdb", "ddb", "orders", nil, cfg)
	if r.WmState != WmStateWindowed {
		t.Fatalf("wm_state = %q, want windowed", r.WmState)
	}
	for _, side := range []*wmFakeColl{sc, tc} {
		if len(side.countFlts) != 1 || !reflect.DeepEqual(side.countFlts[0], pred) {
			t.Fatalf("count filter must deep-equal the watermark pred: %#v", side.countFlts)
		}
	}
	if len(sc.rangeFlts) != 1 || !reflect.DeepEqual(sc.rangeFlts[0], pred) {
		t.Fatalf("IDRange match must deep-equal the watermark pred: %#v", sc.rangeFlts)
	}
	want := "水位过滤 ts [2026-10-24 00:00:00, 2026-10-25 00:00:00)（源 5 行/目标 5 行）"
	if r.Note != want {
		t.Fatalf("note = %q, want %q", r.Note, want)
	}

	// Degraded run (global group, column absent): full-comparison counts
	// (empty filter) + fallback note.
	sc = &wmFakeColl{probeDocs: []bson.D{oidDoc}, countN: 9}
	tc = &wmFakeColl{probeDocs: []bson.D{oidDoc}, countN: 9}
	cfgAbs := Config{Levels: []Level{LevelCount}, Watermark: &config.WatermarkFilter{Column: "absent", Value: "1"}}
	r = compareCollection(ctx, wmClientOf(sc), wmClientOf(tc), "sdb", "ddb", "orders", nil, cfgAbs)
	if r.WmState != WmStateDegraded {
		t.Fatalf("wm_state = %q, want degraded", r.WmState)
	}
	if len(sc.countFlts) != 1 || len(sc.countFlts[0]) != 0 {
		t.Fatalf("degraded count must run unfiltered: %#v", sc.countFlts)
	}
	if r.Note == "" {
		t.Fatalf("degraded row must carry the fallback note")
	}

	// Override hard fail: error row, 例外水位列 prefix, no count at all.
	sc = &wmFakeColl{probeDocs: []bson.D{oidDoc}}
	cfgOv := Config{Levels: []Level{LevelCount}, TableWatermarks: []config.TableWatermark{
		{Table: "orders", WatermarkFilter: config.WatermarkFilter{Column: "absent", Value: "1"}},
	}}
	r = compareCollection(ctx, wmClientOf(sc), wmClientOf(&wmFakeColl{probeDocs: []bson.D{oidDoc}}), "sdb", "ddb", "orders", nil, cfgOv)
	if r.Overall != OverallError || r.WmState != WmStateFailed || len(r.Checks.Error) < len(wmExFailPrefix)+1 {
		t.Fatalf("override precheck fail must be an error row: %+v", r)
	}
	if r.Checks.Error[:len(wmExFailPrefix)] != wmExFailPrefix {
		t.Fatalf("error prefix = %q", r.Checks.Error)
	}
	if len(sc.countFlts) != 0 {
		t.Fatalf("failed precheck must not count: %#v", sc.countFlts)
	}

	// Red line: no watermark → nil filters, no state, no note.
	sc = &wmFakeColl{probeDocs: []bson.D{oidDoc}, countN: 3}
	tc = &wmFakeColl{probeDocs: []bson.D{oidDoc}, countN: 3}
	r = compareCollection(ctx, wmClientOf(sc), wmClientOf(tc), "sdb", "ddb", "orders", nil, Config{Levels: []Level{LevelCount}})
	if r.WmState != "" || r.Note != "" {
		t.Fatalf("no watermark must leave state/note empty: %+v", r)
	}
	if len(sc.countFlts) != 1 || len(sc.countFlts[0]) != 0 {
		t.Fatalf("no watermark must count unfiltered: %#v", sc.countFlts)
	}
	if len(sc.findCalls) != 0 {
		t.Fatalf("no watermark must not probe: %#v", sc.findCalls)
	}
}

func TestWmAnyWindowedGate(t *testing.T) {
	rows := []CollectionResult{
		{WmState: WmStateDegraded},
		{WmState: WmStateFailed},
		{WmState: ""},
	}
	if WmAnyWindowed(rows) {
		t.Fatalf("no windowed row ⇒ gate closed")
	}
	rows[1].WmState = WmStateWindowed
	if !WmAnyWindowed(rows) {
		t.Fatalf("one windowed row ⇒ gate open")
	}
}
