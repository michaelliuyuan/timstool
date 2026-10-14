package mongocompare

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

// MC-9 R1 anchors (seq=236): the TTL data-skip must never mask an
// index-level diff (A1). Three layers: main anchor (TTL + index diff →
// diff, not ttl_skip), reverse anchor (TTL with identical indexes →
// ttl_skip, guarding against over-flagging), and the target-only-TTL
// variant (TTL only on dst → index diff reported, source has no TTL so
// data levels still run).

func TestMC9TTLSemanticsLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	docs := []any{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "b"}},
	}
	ts := bson.D{{Key: "ts", Value: 1}}

	// 主锚 (seq=236): 源 TTL({ts:1},{expireAfterSeconds:3600}) vs 目标同
	// key 普通索引 {ts:1} → the TTL-vs-plain index difference must win
	// over ttl_skip; Skipped="ttl" stays as the reason the data levels
	// are absent.
	env.seed(env.db("md_mc9a_src"), "c", docs)
	env.seed(env.db("md_mc9a_dst"), "c", docs)
	env.index(env.db("md_mc9a_src"), "c", ts, 3600)
	env.index(env.db("md_mc9a_dst"), "c", ts, 0)
	s := env.compare(env.db("md_mc9a_src"), env.db("md_mc9a_dst"), Config{})
	if len(s.Collections) != 1 {
		t.Fatalf("main anchor: collections = %d, want 1", len(s.Collections))
	}
	c := s.Collections[0]
	if c.Overall != OverallDiff {
		t.Fatalf("main anchor: overall = %q, want diff (index diff must not be masked by ttl_skip)", c.Overall)
	}
	if c.Checks.Skipped != "ttl" {
		t.Fatalf("main anchor: skipped = %q, want ttl marker retained", c.Checks.Skipped)
	}
	if len(c.Checks.Index["diff"].([]string)) == 0 {
		t.Fatalf("main anchor: index diff list empty: %+v", c.Checks.Index)
	}
	if s.Counts["ttl_skip"] != 0 || s.Counts["diff"] != 1 {
		t.Fatalf("main anchor: counts = %v, want diff=1 ttl_skip=0 (four-state shape unchanged)", s.Counts)
	}

	// 反向锚: identical TTL indexes, identical docs → plain ttl_skip
	// (A1 must only demote from equal, never flag equal TTL pairs).
	env.seed(env.db("md_mc9b_src"), "c", docs)
	env.seed(env.db("md_mc9b_dst"), "c", docs)
	env.index(env.db("md_mc9b_src"), "c", ts, 3600)
	env.index(env.db("md_mc9b_dst"), "c", ts, 3600)
	s = env.compare(env.db("md_mc9b_src"), env.db("md_mc9b_dst"), Config{})
	if len(s.Collections) != 1 {
		t.Fatalf("reverse anchor: collections = %d, want 1", len(s.Collections))
	}
	if c := s.Collections[0]; c.Overall != OverallTTLSkip || c.Checks.Skipped != "ttl" {
		t.Fatalf("reverse anchor: overall=%q skipped=%q, want ttl_skip/ttl", c.Overall, c.Checks.Skipped)
	}

	// 仅目标端 TTL 变体锚: TTL only on dst → the TTL index is itself an
	// index diff (missing-in-source); src has no TTL so data levels run.
	env.seed(env.db("md_mc9c_src"), "c", docs)
	env.seed(env.db("md_mc9c_dst"), "c", docs)
	env.index(env.db("md_mc9c_dst"), "c", ts, 3600)
	s = env.compare(env.db("md_mc9c_src"), env.db("md_mc9c_dst"), Config{})
	if len(s.Collections) != 1 {
		t.Fatalf("target-only ttl: collections = %d, want 1", len(s.Collections))
	}
	c = s.Collections[0]
	if c.Overall != OverallDiff {
		t.Fatalf("target-only ttl: overall = %q, want diff", c.Overall)
	}
	if c.Checks.Skipped != "" {
		t.Fatalf("target-only ttl: skipped = %q, want empty (source has no TTL; data levels ran)", c.Checks.Skipped)
	}
	if c.Checks.Count == nil {
		t.Fatalf("target-only ttl: count check absent — data levels must run when source has no TTL")
	}
	idxDiff, _ := c.Checks.Index["diff"].([]string)
	if len(idxDiff) == 0 {
		t.Fatalf("target-only ttl: index diff list empty: %+v", c.Checks.Index)
	}
}

// MC-9 R2 anchors (seq=236): views are filtered from the compare (A2) on
// BOTH driver paths — the main anchor runs the v2 native path, then the
// same fixture is re-driven through the v1 bridge (dual-driver parity, the
// M5 convention). Skipped views land in Summary.Stats.skipped_views and
// never enter the four-state counts.
//
// 待验: the crossversion fixture (a real 3.x server, where listCollections
// may omit the type field — the empty-type guard) is not standing in this
// environment; the guard itself is anchored offline by the M1-era rule and
// needs a live 3.x matrix run to close.
func TestMC9ViewFilterLive(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	srcDB, dstDB := env.db("md_mc9v_src"), env.db("md_mc9v_dst")
	docs := []any{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "b"}},
	}
	env.seed(srcDB, "vc", docs)
	env.seed(dstDB, "vc", docs)
	// A view on the SOURCE side only: pre-filter, it must NOT surface as
	// missing-in-target.
	mkView := func(db string) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := env.client.Database(db).RunCommand(ctx, bson.D{
			{Key: "create", Value: "vview"},
			{Key: "viewOn", Value: "vc"},
			{Key: "pipeline", Value: bson.A{}},
		}).Err(); err != nil {
			t.Fatalf("createView %s: %v", db, err)
		}
	}
	mkView(srcDB)

	// 主锚 (v2 native).
	s := env.compare(srcDB, dstDB, Config{})
	assertViewFiltered(t, s, "v2 main anchor")

	// v1 桥双驱动: same fixture through the forced v1 bridge.
	addr := os.Getenv("TIMSTOOL_MONGO_SMOKE")
	host, port := splitHostPort(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v1c, err := mongoconn.DialV1(ctx, mongoconn.DialConfig{Host: host, Port: port})
	if err != nil || v1c.Driver() != "v1" {
		t.Fatalf("dial v1: %v", err)
	}
	defer v1c.Disconnect(context.Background())
	sB, err := CompareDatabases(ctx, v1c, v1c, srcDB, dstDB, Config{})
	if err != nil {
		t.Fatalf("compare v1 bridge: %v", err)
	}
	assertViewFiltered(t, sB, "v1 bridge driver")
}

func assertViewFiltered(t *testing.T, s *Summary, label string) {
	t.Helper()
	if len(s.Collections) != 1 || s.Collections[0].Coll != "vc" {
		t.Fatalf("%s: collections = %+v, want exactly the one real collection", label, s.Collections)
	}
	if s.Overall != OverallEqual {
		t.Fatalf("%s: overall = %q, want equal (view must not surface as missing-in-target)", label, s.Overall)
	}
	skipped, ok := s.Stats["skipped_views"].([]string)
	if !ok || len(skipped) != 1 {
		t.Fatalf("%s: stats.skipped_views = %#v, want one entry", label, s.Stats["skipped_views"])
	}
	if skipped[0] == "" || skipped[0][len(skipped[0])-len("vview"):] != "vview" {
		t.Fatalf("%s: skipped_views entry = %q, want db-qualified vview", label, skipped[0])
	}
	if s.Counts["equal"] != 1 || s.Counts["diff"] != 0 || s.Counts["error"] != 0 || s.Counts["ttl_skip"] != 0 {
		t.Fatalf("%s: counts = %v, want equal=1 only (views never enter the four-state counts)", label, s.Counts)
	}
}
