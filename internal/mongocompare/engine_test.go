package mongocompare

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestPlanBucketsNumeric locks the boundary arithmetic offline: _id 0..100
// with 4 buckets must yield [0,25,50,75,100] with the first bucket open
// below and the last bucket closed above (the gap-regression danger zone
// [75,100) must be covered by bucket 3's $gte, and everything above max by
// the same open end).
func TestPlanBucketsNumeric(t *testing.T) {
	plan := PlanBuckets(int32(0), int32(100), 4)
	if len(plan) != 4 {
		t.Fatalf("len(plan) = %d, want 4", len(plan))
	}
	wantLo := []int32{0, 25, 50, 75}
	for i, b := range plan {
		if b.Index != i {
			t.Errorf("plan[%d].Index = %d", i, b.Index)
		}
		if lo, ok := b.Lo.(int32); !ok || lo != wantLo[i] {
			t.Errorf("plan[%d].Lo = %v, want %d", i, b.Lo, wantLo[i])
		}
	}
	// First bucket: _id < 25.
	q0 := plan[0].Query
	if getOp(t, q0) != "$lt" || getInt(t, q0) != 25 {
		t.Errorf("bucket0 query = %v, want $lt 25", q0)
	}
	// Last bucket: _id >= 75 (NOT > 75 — the historical off-by-one).
	q3 := plan[3].Query
	if getOp(t, q3) != "$gte" || getInt(t, q3) != 75 {
		t.Errorf("bucket3 query = %v, want $gte 75", q3)
	}
	// Middle bucket 1: 25 <= _id < 50.
	q1 := plan[1].Query
	idDoc := q1[0].Value.(bson.D)
	if idDoc[0].Key != "$gte" || idDoc[0].Value.(int32) != 25 ||
		idDoc[1].Key != "$lt" || idDoc[1].Value.(int32) != 50 {
		t.Errorf("bucket1 query = %v, want $gte 25 $lt 50", q1)
	}
}

func getOp(t *testing.T, q bson.D) string {
	t.Helper()
	idDoc, ok := q[0].Value.(bson.D)
	if !ok || len(idDoc) == 0 {
		t.Fatalf("query %v has no _id operator doc", q)
	}
	return idDoc[0].Key
}

func getInt(t *testing.T, q bson.D) int32 {
	t.Helper()
	idDoc := q[0].Value.(bson.D)
	v, _ := idDoc[0].Value.(int32)
	return v
}

// TestPlanBucketsFallbacks: string _ids, n<2, and degenerate ranges all fall
// back to a single full-collection bucket.
func TestPlanBucketsFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		min  any
		max  any
		n    int
	}{
		{"string id", "a", "z", 8},
		{"n<2", int32(0), int32(100), 1},
		{"degenerate", int32(7), int32(7), 8},
		{"max<min", int32(9), int32(1), 8},
	} {
		plan := PlanBuckets(tc.min, tc.max, tc.n)
		if len(plan) != 1 || plan[0].Index != 0 || len(plan[0].Query) != 0 {
			t.Errorf("%s: plan = %+v, want single empty-query bucket", tc.name, plan)
		}
	}
}

// TestPlanBucketsOIDInterpolation: ObjectId range interpolation is exact over
// the 96-bit space (BigInt, no float drift).
func TestPlanBucketsOIDInterpolation(t *testing.T) {
	lo := bson.ObjectID{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	hi := bson.ObjectID{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	plan := PlanBuckets(lo, hi, 4)
	if len(plan) != 4 {
		t.Fatalf("len = %d, want 4", len(plan))
	}
	want := []bson.ObjectID{
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		{0x40, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // 1 + (2^96-2)/4 = 2^94 exactly (BigInt floor)
		{0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // 1 + 2*(2^96-2)/4 = 2^95
		{0xBF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
	}
	for i := 0; i <= 4; i++ {
		var got any
		if i < 4 {
			got = plan[i].Lo
		} else {
			got = plan[3].Hi
		}
		oid, ok := got.(bson.ObjectID)
		if !ok || oid != want[i] {
			t.Errorf("bound[%d] = %v, want %s", i, got, want[i].Hex())
		}
	}
}

// --- live tests (env-gated: TIMSTOOL_MONGO_SMOKE, standing container) ---

// TestGapRegressionLive is the Go port of test/gap-regression.sh (M2
// acceptance piece 3 of 3): _id 0..100, 4 buckets, modified docs at 10
// (control) and 80/99 (the danger zone [75,100) that the historical
// off-by-one missed) must ALL be detected.
func TestGapRegressionLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	seed := func(mod10, mod80, mod99 string) []any {
		v := func(id int32, val string) any {
			return bson.D{{Key: "_id", Value: id}, {Key: "v", Value: val}}
		}
		return []any{
			v(0, "base"), v(10, mod10), v(30, "base"),
			v(60, "base"), v(80, mod80), v(99, mod99), v(100, "base"),
		}
	}
	env.seed(env.db("md_gap_src"), "gap", seed("x", "gap", "gap"))
	env.seed(env.db("md_gap_dst"), "gap", seed("X-MOD", "GAP-MOD", "GAP2"))

	s := env.compare(env.db("md_gap_src"), env.db("md_gap_dst"), Config{Buckets: 4, ParallelColls: 1})
	if s.Overall != OverallDiff {
		t.Fatalf("overall = %q, want diff", s.Overall)
	}
	if len(s.Collections) != 1 {
		t.Fatalf("collections = %d, want 1", len(s.Collections))
	}
	c := s.Collections[0]
	if c.Checks.Diff["count"] != 3 {
		t.Fatalf("diff count = %v, want 3 (all of 10, 80, 99)", c.Checks.Diff["count"])
	}

	deltas := env.readDeltas(env.db("md_gap_src"), "gap")
	if len(deltas) != 3 {
		t.Fatalf("ndjson deltas = %d, want 3", len(deltas))
	}
	modified := map[float64]bool{}
	for _, d := range deltas {
		if d.Type != "modified" {
			t.Errorf("delta type = %q, want modified", d.Type)
		}
		id, ok := d.ID.(float64)
		if !ok {
			t.Errorf("delta _id = %#v, want numeric", d.ID)
			continue
		}
		modified[id] = true
	}
	for _, want := range []float64{10, 80, 99} {
		if !modified[want] {
			t.Errorf("modified _id %v not detected (gap-zone regression?)", want)
		}
	}
}

// TestEngineEqualLive: identical collections compare equal with all buckets
// matching; a TTL collection is skipped; missing docs and cross-type _id
// pairs are detected.
func TestEngineEqualLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	// equal: identical docs both sides.
	same := []any{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: bson.D{{Key: "x", Value: 1.5}, {Key: "y", Value: int64(9)}}}},
	}
	env.seed(env.db("md_eq_src"), "eq", same)
	env.seed(env.db("md_eq_dst"), "eq", same)
	s := env.compare(env.db("md_eq_src"), env.db("md_eq_dst"), Config{Buckets: 2})
	if len(s.Collections) == 0 {
		t.Fatalf("equal case: no collections compared (fixture vanished mid-run?)")
	}
	if s.Overall != OverallEqual || s.Collections[0].Overall != OverallEqual {
		t.Fatalf("equal case: overall = %q/%q, want equal", s.Overall, s.Collections[0].Overall)
	}
	for _, b := range s.Collections[0].Checks.Hash {
		if !b.Match {
			t.Errorf("equal case: bucket %d mismatch", b.Bucket)
		}
	}

	// ttl: TTL index → ttl_skip (index level still compared).
	env.seed(env.db("md_ttl_src"), "ttl", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "ts", Value: bson.DateTime(1)}}})
	env.seed(env.db("md_ttl_dst"), "ttl", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "ts", Value: bson.DateTime(1)}}})
	ttlKeys := bson.D{{Key: "ts", Value: 1}}
	env.index(env.db("md_ttl_src"), "ttl", ttlKeys, 3600)
	env.index(env.db("md_ttl_dst"), "ttl", ttlKeys, 3600)
	s = env.compare(env.db("md_ttl_src"), env.db("md_ttl_dst"), Config{})
	if len(s.Collections) == 0 {
		t.Fatalf("ttl case: no collections compared (fixture vanished mid-run?)")
	}
	if c := s.Collections[0]; c.Overall != OverallTTLSkip || c.Checks.Skipped != "ttl" {
		t.Fatalf("ttl case: overall = %q skipped = %v, want ttl_skip/ttl", c.Overall, c.Checks.Skipped)
	}

	// missing doc: one side only.
	env.seed(env.db("md_miss_src"), "miss", []any{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "b"}},
	})
	env.seed(env.db("md_miss_dst"), "miss", []any{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a"}},
	})
	s = env.compare(env.db("md_miss_src"), env.db("md_miss_dst"), Config{Buckets: 1})
	deltas := env.readDeltas(env.db("md_miss_src"), "miss")
	if s.Overall != OverallDiff || len(deltas) != 1 || deltas[0].Type != "missing-in-target" {
		t.Fatalf("missing case: overall=%q deltas=%+v, want diff + 1 missing-in-target", s.Overall, deltas)
	}

	// cross-type _id: int32(1) vs double(1.0) — same BSON rank, different
	// canonical tag ⇒ cross missing pair.
	env.seed(env.db("md_ct_src"), "ct", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a"}}})
	env.seed(env.db("md_ct_dst"), "ct", []any{bson.D{{Key: "_id", Value: 1.0}, {Key: "v", Value: "a"}}})
	s = env.compare(env.db("md_ct_src"), env.db("md_ct_dst"), Config{Buckets: 1})
	deltas = env.readDeltas(env.db("md_ct_src"), "ct")
	if s.Overall != OverallDiff || len(deltas) != 2 {
		t.Fatalf("cross-type case: overall=%q deltas=%+v, want diff + cross pair", s.Overall, deltas)
	}
	types := map[string]bool{}
	for _, d := range deltas {
		types[d.Type] = true
	}
	if !types["missing-in-target"] || !types["missing-in-source"] {
		t.Errorf("cross-type deltas = %+v, want one of each side", deltas)
	}
}

// TestResumeCheckpointLive: after a full run the checkpoint holds all bucket
// results; deleting one entry and rerunning with Resume on UNCHANGED data
// must reproduce the identical outcome (reuse path executes, boundaries
// recomputed — never deserialized).
func TestResumeCheckpointLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	docs := []any{}
	for i := int32(0); i < 40; i++ {
		docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "v", Value: "v" + strconv.Itoa(int(i))}})
	}
	env.seed(env.db("md_cp_src"), "cp", docs)
	dst := append([]any{}, docs...)
	dst[7] = bson.D{{Key: "_id", Value: int32(7)}, {Key: "v", Value: "CHANGED"}}
	env.seed(env.db("md_cp_dst"), "cp", dst)

	cfg := Config{Buckets: 4, OutDir: filepath.Join(env.tmp, "out")}
	s1 := env.compare(env.db("md_cp_src"), env.db("md_cp_dst"), cfg)
	if s1.Overall != OverallDiff {
		t.Fatalf("run1 overall = %q, want diff", s1.Overall)
	}

	cpFile := filepath.Join(cfg.OutDir, ".checkpoints", env.db("md_cp_src")+"__cp.json")
	b, err := os.ReadFile(cpFile)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	var cp map[string]any
	if err := json.Unmarshal(b, &cp); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	buckets, _ := cp["buckets"].(map[string]any)
	if len(buckets) != 4 {
		t.Fatalf("checkpoint buckets = %d, want 4", len(buckets))
	}
	delete(buckets, "2") // simulate an interrupted bucket
	b2, _ := json.Marshal(cp)
	if err := os.WriteFile(cpFile, b2, 0o644); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}

	cfg.Resume = true
	s2 := env.compare(env.db("md_cp_src"), env.db("md_cp_dst"), cfg)
	if len(s2.Collections) == 0 {
		t.Fatalf("run2: no collections compared (fixture vanished mid-run?)")
	}
	if s2.Overall != OverallDiff || s2.Collections[0].Checks.Diff["count"] != 1 {
		t.Fatalf("run2 (resume) overall=%q diffcount=%v, want diff/1", s2.Overall, s2.Collections[0].Checks.Diff["count"])
	}
}
