package mongocompare

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// MC-10 B1 anchors (seq=259 card ②): bidirectional existence at the schema
// level. Main anchor: a dst-only COLLECTION surfaces as a synthetic
// missing-in-source row (diff counts, four-state shape unchanged) while a
// dst-only VIEW stays unreported (symmetric with the source-side A2 view
// filter). dst-view fix anchor: a same-named target VIEW no longer masks a
// missing target collection (dstNames counts real collections only).
// Schema-off anchor: with the schema level disabled the dst side is not
// enumerated at all — no dst-only rows.
func TestMC10BidirectionalExistenceLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	docs := []any{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "b"}},
	}

	// 主锚: src {c1} vs dst {c1, extra(collection), vview(view on c1)} —
	// extra must surface as missing-in-source; vview must NOT. NOTE: seed()
	// recreates the whole database, so the second dst collection goes in
	// via a direct insert (harness convention: seed() owns the db).
	srcDB, dstDB := env.db("md_mc10a_src"), env.db("md_mc10a_dst")
	env.seed(srcDB, "c1", docs)
	env.seed(dstDB, "c1", docs)
	insertColl(t, env, dstDB, "extra", docs)
	mkView(t, env, dstDB, "vview", "c1")

	s := env.compare(srcDB, dstDB, Config{})
	if len(s.Collections) != 2 {
		t.Fatalf("main anchor: collections = %+v, want exactly c1 + extra (dst-only view unreported)", s.Collections)
	}
	var extra *CollectionResult
	for i := range s.Collections {
		if s.Collections[i].Coll == "extra" {
			extra = &s.Collections[i]
		}
	}
	if extra == nil {
		t.Fatalf("main anchor: dst-only collection extra missing from rows: %+v", s.Collections)
	}
	if extra.Overall != OverallDiff {
		t.Fatalf("main anchor: extra overall = %q, want diff", extra.Overall)
	}
	d, _ := extra.Checks.Schema["diff"].([]string)
	if len(d) != 1 || d[0] != "missing-in-source" {
		t.Fatalf("main anchor: extra schema diff = %#v, want [missing-in-source]", extra.Checks.Schema["diff"])
	}
	if s.Overall != OverallDiff || s.Counts["equal"] != 1 || s.Counts["diff"] != 1 {
		t.Fatalf("main anchor: overall=%s counts=%v, want diff with equal=1 diff=1 (four-state shape unchanged)", s.Overall, s.Counts)
	}

	// dst-view 修正锚: src has collection vc; dst has NO collection vc —
	// only a same-named VIEW (plus its base). vc must be reported
	// missing-in-target (pre-fix the view name masked the check), and the
	// dst-only base surfaces as missing-in-source.
	srcDB2, dstDB2 := env.db("md_mc10b_src"), env.db("md_mc10b_dst")
	env.seed(srcDB2, "vc", docs)
	env.seed(dstDB2, "base", docs)
	mkView(t, env, dstDB2, "vc", "base")

	s2 := env.compare(srcDB2, dstDB2, Config{})
	byColl := map[string]CollectionResult{}
	for _, r := range s2.Collections {
		byColl[r.Coll] = r
	}
	vc, hasVC := byColl["vc"]
	if !hasVC || vc.Overall != OverallDiff {
		t.Fatalf("dst-view anchor: vc row = %+v, want diff (view must not mask missing-in-target)", byColl)
	}
	if d, _ := vc.Checks.Schema["diff"].([]string); len(d) != 1 || d[0] != "missing-in-target" {
		t.Fatalf("dst-view anchor: vc schema diff = %#v, want [missing-in-target]", vc.Checks.Schema["diff"])
	}
	base, hasBase := byColl["base"]
	if !hasBase || base.Overall != OverallDiff {
		t.Fatalf("dst-view anchor: base row = %+v, want diff missing-in-source", byColl)
	}
	if s2.Counts["diff"] != 2 || s2.Overall != OverallDiff {
		t.Fatalf("dst-view anchor: counts=%v overall=%s, want diff=2 overall=diff", s2.Counts, s2.Overall)
	}

	// schema 关级锚: without the schema level the dst side is not
	// enumerated — dst-only collections stay unreported.
	s3 := env.compare(srcDB, dstDB, Config{Levels: []Level{LevelCount}})
	if len(s3.Collections) != 1 || s3.Collections[0].Coll != "c1" {
		t.Fatalf("schema-off anchor: collections = %+v, want exactly c1 (no dst enumeration)", s3.Collections)
	}
	if s3.Counts["diff"] != 0 {
		t.Fatalf("schema-off anchor: counts = %v, want diff=0", s3.Counts)
	}
}

// mkView creates a read-only view (MC-9 A2 harness convention).
func mkView(t *testing.T, env *liveTestEnv, db, view, viewOn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := env.client.Database(db).RunCommand(ctx, bson.D{
		{Key: "create", Value: view},
		{Key: "viewOn", Value: viewOn},
		{Key: "pipeline", Value: bson.A{}},
	}).Err(); err != nil {
		t.Fatalf("createView %s.%s: %v", db, view, err)
	}
}

// insertColl adds documents to an additional collection of an
// already-seeded database (seed() would drop the db).
func insertColl(t *testing.T, env *liveTestEnv, db, coll string, docs []any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := env.client.Database(db).Collection(coll).InsertMany(ctx, docs); err != nil {
		t.Fatalf("insert %s.%s: %v", db, coll, err)
	}
}
