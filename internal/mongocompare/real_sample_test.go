package mongocompare

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestRealSampleLive exercises the engine on the 100k-doc real sample
// (testdb.users, double _id — the remote-origin dataset restored into the
// smoke container). Skips when testdb.users is absent. Server-side $out
// clones the collection, three known docs get modified, and the engine must
// find exactly those three (bucket planning over a double _id range + L6
// drill-down at real-data scale).
func TestRealSampleLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srcColl := env.client.Database("testdb").Collection("users")
	n, err := srcColl.CountDocuments(ctx, bson.D{})
	if err != nil || n == 0 {
		t.Skipf("testdb.users not populated (count=%d err=%v); requires the restored real sample", n, err)
	}

	// Server-side clone into this run's private md_real_dst db (uniq suffix:
	// concurrent suite runs on the shared container must not collide).
	dstDB := env.db("md_real_dst")
	env.track(dstDB)
	if err := env.client.Database(dstDB).Drop(ctx); err != nil {
		t.Fatalf("drop %s: %v", dstDB, err)
	}
	cursor, err := srcColl.Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{}}},
		bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: dstDB}, {Key: "coll", Value: "users"}}}},
	})
	if err != nil {
		t.Fatalf("aggregate $out: %v", err)
	}
	_ = cursor.Close(ctx)

	// Modify three existing docs (min / middle / max _id) in the clone.
	nDst, err := env.client.Database(dstDB).Collection("users").CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count clone: %v", err)
	}
	pick := func(sortDir int, skip int64) float64 {
		var doc bson.D
		err := env.client.Database(dstDB).Collection("users").FindOne(ctx, bson.D{},
			options.FindOne().SetSort(bson.D{{Key: "_id", Value: sortDir}}).SetSkip(skip)).Decode(&doc)
		if err != nil {
			t.Fatalf("pick doc (sort=%d skip=%d): %v", sortDir, skip, err)
		}
		for _, e := range doc {
			if e.Key == "_id" {
				if id, ok := e.Value.(float64); ok {
					return id
				}
				t.Fatalf("non-float _id: %#v", e.Value)
			}
		}
		t.Fatal("no _id")
		return 0
	}
	modified := []float64{pick(1, 0), pick(1, nDst/2), pick(-1, 0)}
	sortFloats(modified)
	if modified[0] == modified[1] || modified[1] == modified[2] {
		t.Fatalf("modified ids not distinct: %v", modified)
	}
	for _, id := range modified {
		if _, err := env.client.Database(dstDB).Collection("users").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: id}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "city", Value: "MOD-CITY"}}}}); err != nil {
			t.Fatalf("update _id=%v: %v", id, err)
		}
	}

	// Equal baseline first: clone before modification compared to source.
	// (Re-clone is wasteful; instead verify the pre-modification state by
	// hashing a no-op: skip — the modified run below is the real assertion.)

	s := env.compare("testdb", dstDB, Config{Buckets: 8, ParallelBuckets: 4})
	c := findColl(t, s, "users")
	if c.Overall != OverallDiff {
		t.Fatalf("users overall = %q, want diff (err=%q count-check=%+v)",
			c.Overall, c.Checks.Error, c.Checks.Count)
	}
	got := c.Checks.Diff["count"]
	if got != 3 {
		t.Fatalf("diff count = %v, want 3 (ids %v)", got, modified)
	}
	deltas := env.readDeltas("testdb", "users")
	if len(deltas) != 3 {
		t.Fatalf("ndjson deltas = %d, want 3", len(deltas))
	}
	found := map[float64]bool{}
	for _, d := range deltas {
		if d.Type != "modified" {
			t.Errorf("delta %v type = %q, want modified", d.ID, d.Type)
			continue
		}
		for _, f := range d.Fields {
			if f.Key != "city" {
				t.Errorf("delta %v field = %q, want city", d.ID, f.Key)
			}
		}
		id, _ := d.ID.(float64)
		found[id] = true
	}
	for _, want := range modified {
		if !found[want] {
			t.Errorf("modified _id %v not detected at real-data scale", want)
		}
	}
	t.Logf("real sample: %d docs, %d buckets, 3/3 modifications detected", n, len(c.Checks.Hash))
}

// TestRealSampleEqualLive: the unmodified clone must compare equal (the
// positive control for the real sample — run before mutations in a separate
// db pair).
func TestRealSampleEqualLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srcColl := env.client.Database("testdb").Collection("users")
	n, err := srcColl.CountDocuments(ctx, bson.D{})
	if err != nil || n == 0 {
		t.Skipf("testdb.users not populated (count=%d err=%v)", n, err)
	}

	dstDB := env.db("md_real_eq")
	env.track(dstDB)
	if err := env.client.Database(dstDB).Drop(ctx); err != nil {
		t.Fatalf("drop: %v", err)
	}
	cursor, err := srcColl.Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{}}},
		bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: dstDB}, {Key: "coll", Value: "users"}}}},
	})
	if err != nil {
		t.Fatalf("aggregate $out: %v", err)
	}
	_ = cursor.Close(ctx)

	// $out replaces the collection WITHOUT secondary indexes — recreate them
	// from the source so the index level compares equal.
	if err := copyIndexes(ctx, t, env, "testdb", "users", dstDB, "users"); err != nil {
		t.Fatalf("copy indexes: %v", err)
	}

	s := env.compare("testdb", dstDB, Config{Buckets: 8})
	c := findColl(t, s, "users")
	if c.Overall != OverallEqual {
		for _, b := range c.Checks.Hash {
			if !b.Match {
				t.Logf("mismatch bucket %d: src(count=%d hash=%s) dst(count=%d hash=%s)",
					b.Bucket, b.SrcCount, b.SrcHash[:12], b.DstCount, b.DstHash[:12])
			}
		}
		// A vanished dst makes every src doc a delta — cap the log at a
		// sample instead of 100k lines; the ndjson keeps the full detail.
		ds := env.readDeltas("testdb", "users")
		for i, d := range ds {
			if i == 10 {
				t.Logf("... %d further deltas suppressed (full detail in ndjson)", len(ds)-i)
				break
			}
			t.Logf("delta: type=%s id=%v fields=%+v", d.Type, d.ID, d.Fields)
		}
		t.Fatalf("users overall = %q, want equal (err=%q count-check=%+v deltas=%d)",
			c.Overall, c.Checks.Error, c.Checks.Count, len(ds))
	}
	for _, b := range c.Checks.Hash {
		if !b.Match {
			t.Errorf("bucket %d mismatch on identical clone (src=%d dst=%d)", b.Bucket, b.SrcCount, b.DstCount)
		}
	}
}

// findColl locates one collection's result row. The real-sample tests assert
// on the users row specifically: testdb is a real restored sample database
// and may legitimately carry other collections (e.g. a hand-made types_probe
// of unknown provenance) that no clone target reproduces — the engine is
// right to report them missing; the assertions must not depend on the
// library's full collection list.
func findColl(t *testing.T, s *Summary, coll string) CollectionResult {
	t.Helper()
	for _, c := range s.Collections {
		if c.Coll == coll {
			return c
		}
	}
	t.Fatalf("collection %q not compared (rows: %d)", coll, len(s.Collections))
	return CollectionResult{}
}

// copyIndexes recreates srcColl's secondary indexes (name, keys, unique) on
// dstColl — $out clones carry only _id_.
func copyIndexes(ctx context.Context, t *testing.T, env *liveTestEnv, srcDB, srcColl, dstDB, dstColl string) error {
	t.Helper()
	idxs, err := env.client.Database(srcDB).Collection(srcColl).Indexes().ListSpecifications(ctx)
	if err != nil {
		return err
	}
	dst := env.client.Database(dstDB).Collection(dstColl)
	for _, ix := range idxs {
		if ix.Name == "_id_" {
			continue
		}
		var keys bson.D
		if err := bson.Unmarshal(ix.KeysDocument, &keys); err != nil {
			return err
		}
		idxOpts := options.Index()
		if ix.Unique != nil && *ix.Unique {
			idxOpts = idxOpts.SetUnique(true)
		}
		if _, err := dst.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys, Options: idxOpts}); err != nil {
			return err
		}
	}
	return nil
}

func sortFloats(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
