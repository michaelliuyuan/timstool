package mongocompare

import (
	"context"
	"math"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/michaelliuyuan/timstool/internal/mongocompare/canon"
)

// TestValidateCanonicalLive is the Go port of mongo-dbcheck
// test/validate-canonical.js (M2 acceptance piece 2 of 3): the ~50-seed
// regression matrix, rebuilt as fixtures seeded through the Go driver into
// two databases of the standing container and asserted against the canon
// package on server-roundtripped documents (type fidelity through a real
// mongod is the point of the live variant).
func TestValidateCanonicalLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	src := env.client.Database(env.db("md_val_src"))
	dst := env.client.Database(env.db("md_val_dst"))

	pass, fail := 0, 0
	check := func(name string, got, want any) {
		t.Helper()
		if got != want {
			fail++
			t.Errorf("FAIL %s: got %v, want %v", name, got, want)
		} else {
			pass++
		}
	}

	// ---- eq.types: one doc per BSON type, canonical tag locked ----
	oid, _ := bson.ObjectIDFromHex("000000000000000000000001")
	typeDoc := func(k string, v any) any {
		return bson.D{{Key: "_id", Value: k}, {Key: "k", Value: k}, {Key: "v", Value: v}}
	}
	env.seed(env.db("md_val_src"), "eq_types", []any{
		typeDoc("double", 1.0),
		typeDoc("int32", int32(1)),
		typeDoc("int64", int64(1)),
		typeDoc("decimal", mustDec(t, "1.5")),
		typeDoc("string", "1"),
		typeDoc("oid", oid),
		typeDoc("bool", true),
		typeDoc("date", bson.DateTime(0)),
		typeDoc("null", nil),
		typeDoc("array", bson.A{int32(1), "1", 1.0}),
		typeDoc("object", bson.D{{Key: "a", Value: bson.D{{Key: "x", Value: 3.0}, {Key: "y", Value: 2.0}}}, {Key: "z", Value: 1.0}}),
		typeDoc("regex", bson.Regex{Pattern: "^a$", Options: "mi"}),
		typeDoc("bindata", bson.Binary{Subtype: 0, Data: []byte("hello")}),
		typeDoc("timestamp", bson.Timestamp{T: 1, I: 2}),
		typeDoc("minkey", bson.MinKey{}),
		typeDoc("maxkey", bson.MaxKey{}),
	})
	fetchOne(t, src, "eq_types", "v", func(k string, v any) {
		switch k {
		case "double":
			check("eq.types double", canon.Canonical(v, canon.Opts{}), "d:1")
		case "int32":
			check("eq.types int32", canon.Canonical(v, canon.Opts{}), "c:1")
		case "int64":
			check("eq.types int64", canon.Canonical(v, canon.Opts{}), "g:1")
		case "decimal":
			check("eq.types decimal", canon.Canonical(v, canon.Opts{}), "m:1.5")
		case "string":
			check("eq.types string", canon.Canonical(v, canon.Opts{}), "s:1")
		case "oid":
			check("eq.types oid", canon.Canonical(v, canon.Opts{}), "i:000000000000000000000001")
		case "bool":
			check("eq.types bool", canon.Canonical(v, canon.Opts{}), "l:true")
		case "date":
			check("eq.types date", canon.Canonical(v, canon.Opts{}), "D:0")
		case "null":
			check("eq.types null", canon.Canonical(v, canon.Opts{}), "z:null")
		case "array":
			check("eq.types array", canon.Canonical(v, canon.Opts{}), "a:[c:1,s:1,d:1]")
		case "object":
			check("eq.types object", canon.Canonical(v, canon.Opts{}), "o:{a:o:{x:d:3,y:d:2},z:d:1}")
		case "regex":
			check("eq.types regex prefix", len(canon.Canonical(v, canon.Opts{})) > 3 && canon.Canonical(v, canon.Opts{})[:3] == "r:/", true)
		case "bindata":
			check("eq.types bindata prefix", canon.Canonical(v, canon.Opts{})[:4] == "b:0:", true)
		case "timestamp":
			check("eq.types timestamp", canon.Canonical(v, canon.Opts{}), "t:1:2")
		case "minkey":
			check("eq.types minkey", canon.Canonical(v, canon.Opts{}), "n:MinKey")
		case "maxkey":
			check("eq.types maxkey", canon.Canonical(v, canon.Opts{}), "x:MaxKey")
		}
	})

	// ---- eq.keyorder / eq.nest: key order is not semantic (explicit _id so
	// the server does not add a random one) ----
	env.seed(env.db("md_val_src"), "eq_keyorder", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}}})
	env.seed(env.db("md_val_dst"), "eq_keyorder", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "b", Value: int32(2)}, {Key: "a", Value: int32(1)}}})
	check("eq.keyorder equal", canon.Canonical(fetchAll(t, src, "eq_keyorder")[0], canon.Opts{}) ==
		canon.Canonical(fetchAll(t, dst, "eq_keyorder")[0], canon.Opts{}), true)

	env.seed(env.db("md_val_src"), "eq_nest", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "a", Value: int32(1)}, {Key: "b", Value: bson.D{{Key: "y", Value: int32(2)}, {Key: "x", Value: int32(3)}}}}})
	env.seed(env.db("md_val_dst"), "eq_nest", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "b", Value: bson.D{{Key: "x", Value: int32(3)}, {Key: "y", Value: int32(2)}}}, {Key: "a", Value: int32(1)}}})
	check("eq.nest equal", canon.Canonical(fetchAll(t, src, "eq_nest")[0], canon.Opts{}) ==
		canon.Canonical(fetchAll(t, dst, "eq_nest")[0], canon.Opts{}), true)

	// spec §12: Decimal128 trailing zeros (1.50 ≡ 1.5 through a server roundtrip).
	env.seed(env.db("md_val_src"), "eq_dec", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: mustDec(t, "1.50")}}})
	env.seed(env.db("md_val_dst"), "eq_dec", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: mustDec(t, "1.5")}}})
	check("decimal trailing zeros equal",
		canon.Canonical(vOf(fetchAll(t, src, "eq_dec")[0]), canon.Opts{}) ==
			canon.Canonical(vOf(fetchAll(t, dst, "eq_dec")[0]), canon.Opts{}), true)

	// ---- diff.numtype: default strict diff, numericTolerance equal ----
	numDoc := func(id int32, v any) any { return bson.D{{Key: "_id", Value: id}, {Key: "v", Value: v}} }
	env.seed(env.db("md_val_src"), "diff_numtype", []any{
		numDoc(1, int32(1)), numDoc(2, int64(1)), numDoc(3, 1.5),
	})
	env.seed(env.db("md_val_dst"), "diff_numtype", []any{
		numDoc(1, 1.0), numDoc(2, mustDec(t, "1.0")), numDoc(3, int64(1)),
	})
	ns := fetchAll(t, src, "diff_numtype")
	nd := fetchAll(t, dst, "diff_numtype")
	for i := 0; i < 3; i++ {
		check(numName(i)+" default diff", canon.Canonical(vOf(ns[i]), canon.Opts{}) == canon.Canonical(vOf(nd[i]), canon.Opts{}), false)
	}
	tol := canon.Opts{NumericTolerance: true}
	check("numtype tolerance _id1", canon.Canonical(vOf(ns[0]), tol) == canon.Canonical(vOf(nd[0]), tol), true)
	check("numtype tolerance _id2", canon.Canonical(vOf(ns[1]), tol) == canon.Canonical(vOf(nd[1]), tol), true)
	check("numtype tolerance _id3 stays diff", canon.Canonical(vOf(ns[2]), tol) == canon.Canonical(vOf(nd[2]), tol), false)

	// ---- eq.float: NaN==NaN, -0==0, ±Inf, 0.1 ----
	env.seed(env.db("md_val_src"), "eq_float", []any{
		numDoc(1, nan()), numDoc(2, negZero()), numDoc(3, posInf()), numDoc(4, negInf()), numDoc(5, 0.1),
	})
	env.seed(env.db("md_val_dst"), "eq_float", []any{
		numDoc(1, nan()), numDoc(2, 0.0), numDoc(3, posInf()), numDoc(4, negInf()), numDoc(5, 0.1),
	})
	fs := fetchAll(t, src, "eq_float")
	fd := fetchAll(t, dst, "eq_float")
	for i := 0; i < 5; i++ {
		check(floatName(i)+" equal", canon.Canonical(fs[i], canon.Opts{}) == canon.Canonical(fd[i], canon.Opts{}), true)
	}

	// ---- diff.missing: strict diff, missingAsNull equal (recursive) ----
	env.seed(env.db("md_val_src"), "diff_missing", []any{
		numDoc(1, bson.D{{Key: "a", Value: int32(1)}}),
		numDoc(2, bson.D{{Key: "v", Value: nil}}),
		numDoc(3, bson.D{{Key: "o", Value: bson.D{{Key: "a", Value: nil}}}}),
	})
	env.seed(env.db("md_val_dst"), "diff_missing", []any{
		numDoc(1, bson.D{{Key: "a", Value: int32(1)}, {Key: "v", Value: nil}}),
		numDoc(2, bson.D{}),
		numDoc(3, bson.D{{Key: "o", Value: bson.D{}}}),
	})
	ms := fetchAll(t, src, "diff_missing")
	md := fetchAll(t, dst, "diff_missing")
	asNull := canon.Opts{MissingAsNull: true}
	for i := 0; i < 3; i++ {
		check(missName(i)+" default diff", canon.DiffDocs(ms[i], md[i], canon.Opts{}).Equal, false)
		check(missName(i)+" asNull equal", canon.DiffDocs(ms[i], md[i], asNull).Equal, true)
	}

	// ---- eq.missing: identical on both sides ----
	env.seed(env.db("md_val_src"), "eq_missing", []any{numDoc(1, bson.D{{Key: "v", Value: nil}}), numDoc(2, bson.D{{Key: "a", Value: int32(1)}})})
	env.seed(env.db("md_val_dst"), "eq_missing", []any{numDoc(1, bson.D{{Key: "v", Value: nil}}), numDoc(2, bson.D{{Key: "a", Value: int32(1)}})})
	es := fetchAll(t, src, "eq_missing")
	ed := fetchAll(t, dst, "eq_missing")
	for i := 0; i < 2; i++ {
		check("eq.missing equal", canon.Canonical(es[i], canon.Opts{}) == canon.Canonical(ed[i], canon.Opts{}), true)
	}

	// ---- diff.array: order semantic; positional null; nested-key equal ----
	env.seed(env.db("md_val_src"), "diff_array", []any{
		numDoc(1, bson.A{int32(1), int32(2)}),
		numDoc(2, bson.A{bson.D{{Key: "a", Value: int32(1)}}, bson.D{{Key: "b", Value: int32(2)}}}),
		numDoc(3, bson.A{bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}}}),
		numDoc(4, bson.A{int32(1), nil}),
	})
	env.seed(env.db("md_val_dst"), "diff_array", []any{
		numDoc(1, bson.A{int32(2), int32(1)}),
		numDoc(2, bson.A{bson.D{{Key: "b", Value: int32(2)}}, bson.D{{Key: "a", Value: int32(1)}}}),
		numDoc(3, bson.A{bson.D{{Key: "b", Value: int32(2)}, {Key: "a", Value: int32(1)}}}),
		numDoc(4, bson.A{int32(1)}),
	})
	as := fetchAll(t, src, "diff_array")
	ad := fetchAll(t, dst, "diff_array")
	check("array _id1 diff", canon.Canonical(vOf(as[0]), canon.Opts{}) == canon.Canonical(vOf(ad[0]), canon.Opts{}), false)
	check("array _id2 diff", canon.Canonical(vOf(as[1]), canon.Opts{}) == canon.Canonical(vOf(ad[1]), canon.Opts{}), false)
	check("array _id3 equal", canon.Canonical(vOf(as[2]), canon.Opts{}) == canon.Canonical(vOf(ad[2]), canon.Opts{}), true)
	check("array _id4 diff (default)", canon.DiffDocs(as[3], ad[3], canon.Opts{}).Equal, false)
	check("array _id4 diff (asNull, positional)", canon.DiffDocs(as[3], ad[3], asNull).Equal, false)
	r := canon.DiffDocs(as[3], ad[3], canon.Opts{})
	check("array _id4 leaf path v.1", len(r.Deltas) == 1 && r.Deltas[0].Key == "v.1" && r.Deltas[0].Type == "missing-in-b", true)

	// ---- diff.oid: ObjectId vs string hex _id ----
	env.seed(env.db("md_val_src"), "diff_oid", []any{bson.D{{Key: "_id", Value: oid}, {Key: "v", Value: int32(1)}}})
	env.seed(env.db("md_val_dst"), "diff_oid", []any{bson.D{{Key: "_id", Value: oid.Hex()}, {Key: "v", Value: int32(1)}}})
	o1 := fetchAll(t, src, "diff_oid")[0]
	o2 := fetchAll(t, dst, "diff_oid")[0]
	check("oid typeTag", canon.Canonical(idOfAny(o1), canon.Opts{})[:2] == "i:" && canon.Canonical(idOfAny(o2), canon.Opts{})[:2] == "s:", true)
	check("oid diff", canon.Canonical(o1, canon.Opts{}) == canon.Canonical(o2, canon.Opts{}), false)

	// ---- diff.big: identical blob, differing note ----
	blob := make([]byte, 1<<20)
	for i := range blob {
		blob[i] = byte(i % 251)
	}
	env.seed(env.db("md_val_src"), "diff_big", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "data", Value: blob}, {Key: "note", Value: "src"}}})
	env.seed(env.db("md_val_dst"), "diff_big", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "data", Value: blob}, {Key: "note", Value: "dst"}}})
	b1 := fetchAll(t, src, "diff_big")[0]
	b2 := fetchAll(t, dst, "diff_big")[0]
	bigRes := canon.DiffDocs(b1, b2, canon.Opts{})
	check("big diff overall", bigRes.Equal, false)
	check("big diff only note", len(bigRes.Deltas) == 1 && bigRes.Deltas[0].Key == "note", true)

	// ---- eq.ttl: TTL index present (engine skip predicate source) ----
	env.seed(env.db("md_val_src"), "eq_ttl", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "ts", Value: bson.DateTime(1)}}})
	env.seed(env.db("md_val_dst"), "eq_ttl", []any{bson.D{{Key: "_id", Value: int32(1)}, {Key: "ts", Value: bson.DateTime(1)}}})
	ttlKeys := bson.D{{Key: "ts", Value: 1}}
	env.index(env.db("md_val_src"), "eq_ttl", ttlKeys, 3600)
	env.index(env.db("md_val_dst"), "eq_ttl", ttlKeys, 3600)
	idxs := listIndexes(t, src, "eq_ttl")
	hasTTL := false
	for _, ix := range idxs {
		if ix.ExpireAfterSeconds != nil {
			hasTTL = true
		}
	}
	check("ttl index present", hasTTL, true)
	ttlRes := env.compare(env.db("md_val_src"), env.db("md_val_dst"), Config{})
	if len(ttlRes.Collections) == 0 {
		t.Fatalf("eq.ttl engine skip: no collections compared (fixture vanished mid-run?)")
	}
	check("eq.ttl engine skip", ttlRes.Collections[0].Overall, any(OverallTTLSkip))

	t.Logf("validate-canonical matrix: pass=%d fail=%d", pass, fail)
	if fail > 0 {
		t.Fatalf("%d matrix checks failed", fail)
	}
	if pass < 50 {
		t.Fatalf("matrix too small: pass=%d, want >= 50 (port completeness)", pass)
	}
}

func mustDec(t *testing.T, s string) bson.Decimal128 {
	t.Helper()
	d, err := bson.ParseDecimal128(s)
	if err != nil {
		t.Fatalf("ParseDecimal128(%q): %v", s, err)
	}
	return d
}

func fetchAll(t *testing.T, db *mongo.Database, coll string) []bson.D {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cur, err := db.Collection(coll).Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		t.Fatalf("find %s: %v", coll, err)
	}
	defer cur.Close(ctx)
	var out []bson.D
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("decode %s: %v", coll, err)
	}
	return out
}

func fetchOne(t *testing.T, db *mongo.Database, coll string, field string, fn func(k string, v any)) {
	t.Helper()
	for _, doc := range fetchAll(t, db, coll) {
		var k string
		var v any
		for _, e := range doc {
			if e.Key == "k" {
				k, _ = e.Value.(string)
			}
			if e.Key == field {
				v = e.Value
			}
		}
		fn(k, v)
	}
}

func listIndexes(t *testing.T, db *mongo.Database, coll string) []mongo.IndexSpecification {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	idxs, err := db.Collection(coll).Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("listIndexes %s: %v", coll, err)
	}
	return idxs
}

func nan() float64     { return math.NaN() }
func negZero() float64 { return math.Copysign(0, -1) }
func posInf() float64  { return math.Inf(1) }
func negInf() float64  { return math.Inf(-1) }

func numName(i int) string { return []string{"numtype _id1", "numtype _id2", "numtype _id3"}[i] }
func floatName(i int) string {
	return []string{"eq.float NaN", "eq.float -0", "eq.float +Inf", "eq.float -Inf", "eq.float 0.1"}[i]
}
func missName(i int) string { return []string{"missing _id1", "missing _id2", "missing _id3"}[i] }

func vOf(doc bson.D) any {
	for _, e := range doc {
		if e.Key == "v" {
			return e.Value
		}
	}
	return nil
}

func idOfAny(doc bson.D) any {
	for _, e := range doc {
		if e.Key == "_id" {
			return e.Value
		}
	}
	return nil
}
