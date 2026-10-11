package mongocompare

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// smokeAddrFor parses TIMSTOOL_MONGO_SMOKE (host[:port], default
// localhost:27017) — same convention as the source adapter's live smoke.
func smokeAddrFor(v string) string {
	if v == "" {
		return "mongodb://localhost:27017"
	}
	return "mongodb://" + v
}

// liveTestEnv is the shared env-gated live harness: one client to the
// standing smoke container, md_* fixture databases, per-run OutDir.
type liveTestEnv struct {
	t        *testing.T
	client   *mongo.Client
	tmp      string
	lastOut  string
	dbs      []string
	runCount int64
	uniq     string
}

func liveEnv(t *testing.T) *liveTestEnv {
	t.Helper()
	addr := os.Getenv("TIMSTOOL_MONGO_SMOKE")
	if addr == "" {
		t.Skip("TIMSTOOL_MONGO_SMOKE not set; live tests require the standing mongod")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(smokeAddrFor(addr)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		t.Fatalf("ping: %v", err)
	}
	tmp := t.TempDir()
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	sweepOnce.Do(func() { (&liveTestEnv{t: t, client: client}).sweepLegacyMd() })
	return &liveTestEnv{t: t, client: client, tmp: tmp,
		uniq: fmt.Sprintf("%dx%d", os.Getpid(), atomic.AddInt64(&envSeq, 1))}
}

// sweepOnce runs the legacy md_* cleanup once per test process.
var sweepOnce sync.Once

// legacyMdSuffix matches the per-run suffix appended by db() (`_<pid>x<seq>`).
var legacyMdSuffix = regexp.MustCompile(`_\d+x\d+$`)

// sweepLegacyMd implements the F1 main fix (seq=55/56): force-drop every
// pre-existing UNSUFFIXED md_* fixture database left behind by harness
// versions before per-run names existed — e.g. a crashed run whose teardown
// never executed. Suffixed names are deliberately untouched: on this shared
// container they may belong to a concurrently running suite process, and the
// sweep must be effective against legacy dirty state without ever killing a
// live neighbor's fixtures.
func (e *liveTestEnv) sweepLegacyMd() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := e.client.ListDatabases(ctx, bson.D{{Key: "name", Value: bson.Regex{Pattern: "^md_"}}})
	if err != nil {
		e.t.Logf("legacy md_* sweep skipped: %v", err)
		return
	}
	for _, d := range res.Databases {
		if legacyMdSuffix.MatchString(d.Name) {
			continue
		}
		_ = e.client.Database(d.Name).Drop(ctx)
		e.t.Logf("swept legacy fixture db %s", d.Name)
	}
}

// envSeq makes fixture namespaces unique per liveEnv (per test) across
// processes: the standing smoke container is shared by design, and multiple
// sessions (implementer / verification / review) legitimately run this suite
// concurrently against it. With fixed names, one process's teardown dropAll
// races another's in-flight compare and deletes its fixtures mid-scan (the
// seq=53 cross-process state leak: dst sides scanning as empty, cursors
// killed → overall="error"). Suffixed names keep every run's fixtures
// process- and test-private.
var envSeq int64

// db returns this run's private fixture database name for base.
func (e *liveTestEnv) db(base string) string { return base + "_" + e.uniq }

func (e *liveTestEnv) track(db string) {
	for _, d := range e.dbs {
		if d == db {
			return
		}
	}
	e.dbs = append(e.dbs, db)
}

// seed (re)creates db.coll with the given documents.
func (e *liveTestEnv) seed(db, coll string, docs []any) {
	e.t.Helper()
	e.track(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.client.Database(db).Drop(ctx); err != nil {
		e.t.Fatalf("drop %s: %v", db, err)
	}
	if len(docs) > 0 {
		if _, err := e.client.Database(db).Collection(coll).InsertMany(ctx, docs); err != nil {
			e.t.Fatalf("seed %s.%s: %v", db, coll, err)
		}
	}
}

// index creates a secondary index (ttlSecs > 0 makes it a TTL index).
func (e *liveTestEnv) index(db, coll string, keys bson.D, ttlSecs int32) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	idxOpts := options.Index()
	if ttlSecs > 0 {
		idxOpts = idxOpts.SetExpireAfterSeconds(ttlSecs)
	}
	if _, err := e.client.Database(db).Collection(coll).Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys, Options: idxOpts}); err != nil {
		e.t.Fatalf("index %s.%s: %v", db, coll, err)
	}
}

// compare runs CompareDatabases on the single client. A fresh OutDir is
// allocated only when the caller did not set one (the resume test needs a
// stable OutDir across runs). NOTE: compared databases are NOT tracked for
// teardown — only seed() marks ownership (a compared db may be externally
// owned fixture data, e.g. the restored testdb sample).
func (e *liveTestEnv) compare(srcDB, dstDB string, cfg Config) *Summary {
	e.t.Helper()
	if cfg.OutDir == "" {
		n := atomic.AddInt64(&e.runCount, 1)
		cfg.OutDir = filepath.Join(e.tmp, fmt.Sprintf("out%d", n))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := CompareDatabases(ctx, e.client, e.client, srcDB, dstDB, cfg)
	if err != nil {
		e.t.Fatalf("compare %s vs %s: %v", srcDB, dstDB, err)
	}
	e.lastOut = cfg.OutDir
	return s
}

// readDeltas parses the drill-down ndjson of db.coll from the last run.
func (e *liveTestEnv) readDeltas(db, coll string) []DocDelta {
	e.t.Helper()
	name := db + "__" + coll + ".ndjson"
	b, err := os.ReadFile(filepath.Join(e.lastOut, "diff", name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		e.t.Fatalf("read deltas: %v", err)
	}
	var out []DocDelta
	for _, line := range splitLines(string(b)) {
		if line == "" {
			continue
		}
		var d DocDelta
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			e.t.Fatalf("unmarshal delta %q: %v", line, err)
		}
		out = append(out, d)
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// dropAll removes every tracked fixture database.
func (e *liveTestEnv) dropAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, db := range e.dbs {
		_ = e.client.Database(db).Drop(ctx)
	}
}

// TestLegacyMdSuffixRule locks the F1 sweep contract: every legacy fixed
// fixture name is swept (no suffix match), every per-run suffixed name is
// left alone (it may belong to a concurrently running process).
func TestLegacyMdSuffixRule(t *testing.T) {
	swept := []string{"md_gap_src", "md_gap_dst", "md_eq_src", "md_eq_dst",
		"md_ttl_src", "md_ttl_dst", "md_miss_src", "md_miss_dst", "md_ct_src",
		"md_ct_dst", "md_cp_src", "md_cp_dst", "md_real_dst", "md_real_eq",
		"md_val_src", "md_val_dst"}
	for _, n := range swept {
		if legacyMdSuffix.MatchString(n) {
			t.Errorf("legacy name %q must be swept but matches the suffix rule", n)
		}
	}
	kept := []string{"md_gap_src_4321x1", "md_real_eq_4321x7", "md_val_src_999x12"}
	for _, n := range kept {
		if !legacyMdSuffix.MatchString(n) {
			t.Errorf("suffixed name %q must be kept but misses the suffix rule", n)
		}
	}
}

func (e *liveTestEnv) close() {
	e.dropAll()
}
