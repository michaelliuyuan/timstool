package canon

import (
	"math"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestUnitCanonical is the Go port of mongo-dbcheck test/unit-canonical.js
// (M2 acceptance piece 1 of 3). It locks the canonical semantics offline,
// before any live-DB tests.
func TestUnitCanonical(t *testing.T) {
	d1 := bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: 1.0}}

	eq := func(name string, got, want any) {
		if got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	eqStr := func(name string, got, want string) { eq(name, got, want) }
	eqBool := func(name string, got, want bool) { eq(name, got, want) }

	eqBool("canonical(x) equals canonical(x,{})", Canonical(d1, Opts{}) == Canonical(d1, Opts{}), true)
	eqBool("canonicalHash deterministic", CanonicalHash(d1, Opts{}) == CanonicalHash(d1, Opts{}), true)
	eqBool("zero-opts equals defaulted", Canonical(d1, Opts{}) == Canonical(d1, Opts{MaxDepth: 100}), true)

	eqStr("int32", Canonical(int32(1), Opts{}), "c:1")
	eqStr("double", Canonical(1.0, Opts{}), "d:1")
	eqStr("long", Canonical(int64(1), Opts{}), "g:1")
	eqStr("decimal 1.50", Canonical(mustDecimal(t, "1.50"), Opts{}), "m:1.5")
	eqStr("decimal NaN", Canonical(mustDecimal(t, "NaN"), Opts{}), "m:NaN")
	eqStr("double NaN", Canonical(math.NaN(), Opts{}), "d:NaN")
	eqStr("double -0", Canonical(math.Copysign(0, -1), Opts{}), "d:0")
	eqStr("double +Inf", Canonical(math.Inf(1), Opts{}), "d:Infinity")
	eqStr("double 0.1", Canonical(0.1, Opts{}), "d:0.1")
	oid, err := bson.ObjectIDFromHex("000000000000000000000001")
	if err != nil {
		t.Fatalf("ObjectIDFromHex: %v", err)
	}
	eqStr("oid", Canonical(oid, Opts{}), "i:000000000000000000000001")
	eqStr("date", Canonical(bson.DateTime(0), Opts{}), "D:0")
	eqStr("regex sorted flags", Canonical(bson.Regex{Pattern: "^a$", Options: "mi"}, Opts{}), "r:/^a$/im")
	eqStr("timestamp t1i2", Canonical(bson.Timestamp{T: 1, I: 2}, Opts{}), "t:1:2")
	eqStr("minkey", Canonical(bson.MinKey{}, Opts{}), "n:MinKey")
	eqStr("maxkey", Canonical(bson.MaxKey{}, Opts{}), "x:MaxKey")
	eqStr("binary", Canonical(bson.Binary{Subtype: 0, Data: []byte("hello")}, Opts{}), "b:0:aGVsbG8=")

	// Key-order insensitivity (spec §2), nested.
	eqBool("keyorder",
		Canonical(bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: bson.D{{Key: "c", Value: int32(2)}, {Key: "d", Value: int32(3)}}}}, Opts{}) ==
			Canonical(bson.D{{Key: "b", Value: bson.D{{Key: "d", Value: int32(3)}, {Key: "c", Value: int32(2)}}}, {Key: "a", Value: int32(1)}}, Opts{}), true)

	// Array order is semantic (spec §3).
	eqBool("array ordered diff",
		Canonical(bson.D{{Key: "v", Value: bson.A{int32(1), int32(2), int32(3)}}}, Opts{}) !=
			Canonical(bson.D{{Key: "v", Value: bson.A{int32(3), int32(2), int32(1)}}}, Opts{}), true)

	// Missing vs null (spec §7).
	eqBool("missing vs null (canonical)",
		Canonical(bson.D{{Key: "a", Value: int32(1)}}, Opts{}) !=
			Canonical(bson.D{{Key: "a", Value: int32(1)}, {Key: "v", Value: nil}}, Opts{}), true)
	eqBool("diffDocs missingAsNull a-side-null",
		DiffDocs(bson.D{{Key: "a", Value: int32(1)}}, bson.D{{Key: "a", Value: int32(1)}, {Key: "v", Value: nil}}, Opts{MissingAsNull: true}).Equal, true)
	eqBool("diffDocs missingAsNull b-side-null",
		DiffDocs(bson.D{{Key: "v", Value: nil}}, bson.D{}, Opts{MissingAsNull: true}).Equal, true)
	eqBool("diffDocs strict missing diff",
		DiffDocs(bson.D{{Key: "a", Value: int32(1)}}, bson.D{{Key: "a", Value: int32(1)}, {Key: "v", Value: nil}}, Opts{}).Equal, false)

	// numericTolerance (spec §10).
	eqBool("tolerance int32==long",
		Canonical(int32(1), Opts{NumericTolerance: true}) == Canonical(int64(1), Opts{NumericTolerance: true}), true)
	eqBool("tolerance double==decimal 1",
		Canonical(1.0, Opts{NumericTolerance: true}) == Canonical(mustDecimal(t, "1.0"), Opts{NumericTolerance: true}), true)
	eqBool("tolerance 1.5 != 1",
		Canonical(1.5, Opts{NumericTolerance: true}) != Canonical(int64(1), Opts{NumericTolerance: true}), true)
	eqBool("strict int32!=long", Canonical(int32(1), Opts{}) != Canonical(int64(1), Opts{}), true)

	// Cross-type tolerance equality at exponent-switch magnitudes (M2 review
	// seq=49 defect anchors): 'f' expansion must align with int64/decimal
	// plain decimal forms at the 2^53 boundary and the 1e-7 neighborhood.
	tol := Opts{NumericTolerance: true}
	eqBool("tolerance 2^53 double==long",
		Canonical(float64(1<<53), tol) == Canonical(int64(1)<<53, tol), true)
	eqBool("tolerance 1e-7 double==decimal",
		Canonical(1e-7, tol) == Canonical(mustDecimal(t, "0.0000001"), tol), true)
	eqBool("tolerance 1e18 double==long",
		Canonical(1e18, tol) == Canonical(int64(1e18), tol), true)

	// Bucket hash determinism (order-independence is covered separately).
	rows := []any{
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: int32(1)}},
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}},
	}
	eqBool("bucketHash deterministic", BucketHash(rows, Opts{}) == BucketHash(rows, Opts{}), true)
}

func mustDecimal(t *testing.T, s string) bson.Decimal128 {
	t.Helper()
	d, err := bson.ParseDecimal128(s)
	if err != nil {
		t.Fatalf("ParseDecimal128(%q): %v", s, err)
	}
	return d
}

// TestBucketStreamingMatchesSorted locks the spec §9 v1-revision equivalence:
// streaming mod-2^256 sum ≡ sorted-concat hash on equal multisets, and both
// detect actual multiset differences (no non-trivial pairwise cancellation).
func TestBucketStreamingMatchesSorted(t *testing.T) {
	mk := func(id int32, v int32) any {
		return bson.D{{Key: "_id", Value: id}, {Key: "v", Value: v}}
	}
	rows := []any{mk(2, 1), mk(1, 1), mk(5, 9), mk(3, 0)}

	streaming := NewBucketHasher(Opts{})
	for _, r := range rows {
		streaming.Update(r)
	}
	d := streaming.Digest()
	if d.Count != int64(len(rows)) {
		t.Fatalf("streaming count = %d, want %d", d.Count, len(rows))
	}

	// Same multiset, different order → identical streaming hash. The spec
	// §9 equivalence is as an EQUALITY PREDICATE (streaming and sorted-concat
	// agree on equal multisets and both detect differences), not identical
	// output strings — checked by the difference tests below.
	reordered := []any{rows[3], rows[1], rows[0], rows[2]}
	h2 := NewBucketHasher(Opts{})
	for _, r := range reordered {
		h2.Update(r)
	}
	if d.Hash != h2.Digest().Hash {
		t.Fatalf("order dependence: %s vs %s", d.Hash, h2.Digest().Hash)
	}

	// Actual multiset difference must be detected by BOTH forms.
	changed := []any{mk(2, 1), mk(1, 1), mk(5, 9), mk(3, 7)}
	hc := NewBucketHasher(Opts{})
	for _, r := range changed {
		hc.Update(r)
	}
	if hc.Digest().Hash == d.Hash {
		t.Fatal("value change not detected by streaming hash")
	}
	if BucketHash(changed, Opts{}) == BucketHash(rows, Opts{}) {
		t.Fatal("value change not detected by sorted-concat hash")
	}

	// Multiset cardinality difference ({a,a,b} vs {a,b,b}) must be detected.
	aa := []any{mk(1, 1), mk(1, 1), mk(2, 2)}
	bb := []any{mk(1, 1), mk(2, 2), mk(2, 2)}
	if BucketHash(aa, Opts{}) == BucketHash(bb, Opts{}) {
		t.Fatal("multiset cardinality difference not detected")
	}
}

// TestDiffDocsPaths locks the delta path vocabulary (leaf paths, array
// positions) and the type tags.
func TestDiffDocsPaths(t *testing.T) {
	a := bson.D{{Key: "x", Value: bson.D{{Key: "c", Value: int32(2)}}}, {Key: "arr", Value: bson.A{int32(1), int32(2)}}}
	b := bson.D{{Key: "x", Value: bson.D{{Key: "c", Value: int32(3)}}}, {Key: "arr", Value: bson.A{int32(1)}}}
	r := DiffDocs(a, b, Opts{})
	if r.Equal {
		t.Fatal("expected diff")
	}
	want := map[string]string{
		"x.c":   "modified",
		"arr.1": "missing-in-b",
	}
	if len(r.Deltas) != len(want) {
		t.Fatalf("deltas = %+v, want %v", r.Deltas, want)
	}
	for _, d := range r.Deltas {
		if want[d.Key] != d.Type {
			t.Errorf("delta %q type = %q, want %q", d.Key, d.Type, want[d.Key])
		}
	}
}

// TestCompareIDsOrderLocks locks the BSON canonical rank order used by the
// merge join, including mixed-type numeric ordering by value.
func TestCompareIDsOrderLocks(t *testing.T) {
	oid, _ := bson.ObjectIDFromHex("000000000000000000000001")
	// Linearly ordered chain (Regex excluded: within-rank not comparable).
	vals := []any{
		bson.MinKey{},
		nil,
		bson.Undefined{},
		int32(1),
		1.5,
		int64(9),
		"a",
		bson.Binary{Subtype: 0, Data: []byte{1}},
		oid,
		false,
		bson.DateTime(5),
		bson.Timestamp{T: 1, I: 1},
		bson.MaxKey{},
	}
	for i := 0; i < len(vals); i++ {
		for j := 0; j < len(vals); j++ {
			c, ok := CompareIDs(vals[i], vals[j])
			if !ok {
				t.Fatalf("CompareIDs(%T, %T) not ok", vals[i], vals[j])
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if c != want {
				t.Fatalf("CompareIDs(pos %d %T, pos %d %T) = %d, want %d", i, vals[i], j, vals[j], c, want)
			}
		}
	}
	// Regex: rank-positioned between Timestamp and MaxKey across types, but
	// not comparable within its own rank (map-join fallback signal).
	rx := bson.Regex{Pattern: "a"}
	if c, ok := CompareIDs(bson.Timestamp{T: 1, I: 1}, rx); !ok || c != -1 {
		t.Errorf("CompareIDs(ts, regex) = (%d, %v), want (-1, true)", c, ok)
	}
	if c, ok := CompareIDs(rx, bson.MaxKey{}); !ok || c != -1 {
		t.Errorf("CompareIDs(regex, maxkey) = (%d, %v), want (-1, true)", c, ok)
	}
	if _, ok := CompareIDs(rx, bson.Regex{Pattern: "b"}); ok {
		t.Error("CompareIDs(regex, regex) should not be ok")
	}
	// Mixed numeric by value: int32(2) < double(2.5) < long(3).
	if c, _ := CompareIDs(int32(2), 2.5); c != -1 {
		t.Errorf("CompareIDs(int32(2), 2.5) = %d, want -1", c)
	}
	if c, _ := CompareIDs(int64(3), 2.5); c != 1 {
		t.Errorf("CompareIDs(long(3), 2.5) = %d, want 1", c)
	}
	// Not precisely comparable → ok=false.
	if _, ok := CompareIDs(bson.D{}, bson.D{}); ok {
		t.Error("CompareIDs(doc, doc) should not be ok")
	}
}

// TestMaxDepthTruncation: over-deep branches serialize as u:undefined instead
// of recursing forever.
func TestMaxDepthTruncation(t *testing.T) {
	deep := any(int32(1))
	for i := 0; i < 200; i++ {
		deep = bson.D{{Key: "k", Value: deep}}
	}
	got := Canonical(deep, Opts{})
	if got == "" {
		t.Fatal("empty canonical")
	}
	// Both over-deep truncations agree → equal (top levels differ in depth parity).
	d1 := any(int32(1))
	for i := 0; i < 150; i++ {
		d1 = bson.D{{Key: "k", Value: d1}}
	}
	d2 := any(int32(1))
	for i := 0; i < 152; i++ {
		d2 = bson.D{{Key: "k", Value: d2}}
	}
	if Canonical(d1, Opts{}) != Canonical(d2, Opts{}) {
		t.Errorf("two over-maxDepth docs should both truncate:\n%s\n%s", Canonical(d1, Opts{}), Canonical(d2, Opts{}))
	}
}
