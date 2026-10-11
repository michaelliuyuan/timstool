package mongocompare

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

// bridgeDocs is the byte-parity corpus: every BSON type family the canonical
// serializer distinguishes (type-tag sensitivity, spec §4), including
// Decimal128 — the hardest raw-bytes case for the v1 bridge (numberDecimal
// wire bytes -> v2 decode -> canonical string must match the v2-native path
// exactly; 3.2.4 predates the type, so the legacy-matrix test explicitly
// annotates its absence there instead of silently omitting it).
func bridgeDocs() []any {
	return []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "i32", Value: int32(7)}, {Key: "i64", Value: int64(1 << 40)}, {Key: "f64", Value: 3.14}},
		bson.D{{Key: "_id", Value: 2}, {Key: "s", Value: "字符串 with specials \x00\xff"}, {Key: "b", Value: true}, {Key: "nil", Value: nil}},
		bson.D{{Key: "_id", Value: 3}, {Key: "dt", Value: bson.DateTime(1700000000000)}, {Key: "oid", Value: bson.NewObjectID()}},
		bson.D{{Key: "_id", Value: 4}, {Key: "dec", Value: mustParseDecimal("1.05")}, {Key: "arr", Value: bson.A{int32(1), "two", 3.5, nil, true}}},
		bson.D{{Key: "_id", Value: 5}, {Key: "nested", Value: bson.D{{Key: "in", Value: bson.D{{Key: "deep", Value: int32(-2)}}}, {Key: "arr", Value: bson.A{bson.D{{Key: "k", Value: "v"}}}}}}},
	}
}

func mustParseDecimal(s string) bson.Decimal128 {
	d, err := bson.ParseDecimal128(s)
	if err != nil {
		panic("bad decimal fixture " + s + ": " + err.Error())
	}
	return d
}

// TestDialSelectsV2OnModern pins the probe's first state (gate ④): a modern
// server (the smoke container) selects the v2 native path.
func TestDialSelectsV2OnModern(t *testing.T) {
	addr := os.Getenv("TIMSTOOL_MONGO_SMOKE")
	if addr == "" {
		t.Skip("TIMSTOOL_MONGO_SMOKE not set; live tests require the standing mongod")
	}
	host, port := splitHostPort(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := mongoconn.Dial(ctx, mongoconn.DialConfig{Host: host, Port: port})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Disconnect(context.Background())
	if c.Driver() != "v2" {
		t.Errorf("Driver() = %q on modern server, want v2", c.Driver())
	}
}

// TestBridgeEquivalenceCanonicalByteParity is M5 gate ① (the hard one): the
// same dataset through the v2 native path and the v1 bridge must produce
// byte-identical checkpoints and hash results. This proves the bridge's
// byte fidelity end to end — any decode drift (type tags, Decimal128
// mantissa, key order) diverges the canonical hash immediately.
func TestBridgeEquivalenceCanonicalByteParity(t *testing.T) {
	env := liveEnv(t)
	defer env.close()

	src := env.db("md_br_src")
	dst := env.db("md_br_dst")
	docs := bridgeDocs()
	env.seed(src, "c1", docs)
	env.seed(dst, "c1", docs)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	addr := os.Getenv("TIMSTOOL_MONGO_SMOKE")
	host, port := splitHostPort(addr)
	cfg := mongoconn.DialConfig{Host: host, Port: port}

	// Force both paths against the SAME server (the smoke container is 7.x:
	// v2 native legal, v1 bridge legal — parity is exactly the bridge's
	// contribution).
	v2c, err := mongoconn.Dial(ctx, cfg)
	if err != nil || v2c.Driver() != "v2" {
		t.Fatalf("dial v2: %v (driver=%q)", err, v2c.Driver())
	}
	defer v2c.Disconnect(context.Background())
	v1c, err := mongoconn.DialV1(ctx, cfg)
	if err != nil || v1c.Driver() != "v1" {
		t.Fatalf("dial v1: %v", err)
	}
	defer v1c.Disconnect(context.Background())

	outA := filepath.Join(env.tmp, "parity_v2")
	outB := filepath.Join(env.tmp, "parity_v1")
	cfgA := Config{OutDir: outA}
	cfgB := Config{OutDir: outB}

	sA, err := CompareDatabases(ctx, v2c, v2c, src, dst, cfgA)
	if err != nil {
		t.Fatalf("compare v2 path: %v", err)
	}
	sB, err := CompareDatabases(ctx, v1c, v1c, src, dst, cfgB)
	if err != nil {
		t.Fatalf("compare v1 bridge: %v", err)
	}
	if sA.Overall != sB.Overall {
		t.Errorf("overall %s vs %s", sA.Overall, sB.Overall)
	}
	for i := range sA.Collections {
		a, b := sA.Collections[i], sB.Collections[i]
		if a.Overall != b.Overall {
			t.Errorf("coll overall %s vs %s (v1 error: %s)", a.Overall, b.Overall, b.Checks.Error)
		}
		if len(a.Checks.Hash) != len(b.Checks.Hash) {
			t.Fatalf("hash levels differ: %d vs %d", len(a.Checks.Hash), len(b.Checks.Hash))
		}
		for j := range a.Checks.Hash {
			ha, hb := a.Checks.Hash[j], b.Checks.Hash[j]
			if ha.SrcHash != hb.SrcHash || ha.DstHash != hb.DstHash ||
				ha.SrcCount != hb.SrcCount || ha.DstCount != hb.DstCount {
				t.Errorf("bucket %d diverged: v2=%+v v1=%+v", ha.Bucket, ha, hb)
			}
		}
	}

	// Checkpoint byte parity (gate ① verbatim): same algo version, same
	// canonical boundaries, same hashes — byte for byte.
	cpA := readAllFiles(t, filepath.Join(outA, ".checkpoints"))
	cpB := readAllFiles(t, filepath.Join(outB, ".checkpoints"))
	if len(cpA) == 0 {
		t.Fatal("no checkpoints produced")
	}
	for name, a := range cpA {
		b, ok := cpB[name]
		if !ok {
			t.Errorf("checkpoint %s missing on v1 path", name)
			continue
		}
		if string(a) != string(b) {
			t.Errorf("checkpoint %s byte-diverged (v2 %d bytes, v1 %d bytes)", name, len(a), len(b))
		}
	}
}

func splitHostPort(addr string) (string, int) {
	host, port := addr, 27017
	for i := 0; i < len(addr); i++ {
		if addr[i] == ':' {
			host, port = addr[:i], mustAtoi(addr[i+1:])
			break
		}
	}
	if host == "" {
		host = "localhost"
	}
	return host, port
}

func mustAtoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 27017
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func readAllFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string][]byte{}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		out[n] = b
	}
	return out
}
