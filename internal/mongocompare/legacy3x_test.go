package mongocompare

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

// The 3.x legacy matrix (M5 gates ②③): the v1 bridge must run the FULL
// compare pipeline against a pre-4.4 server (probe wire 4 through 8), with
// the 3.x type coverage annotated explicitly rather than silently narrowed:
//
//   - The standing instance (3.2.4) predates Decimal128 (numberDecimal
//     arrives in 3.4), so the real dataset under test cannot contain the
//     type. Decimal128 parity is covered by the bridge-equivalence corpus on
//     the 7.x smoke container — its absence HERE is a server-version fact,
//     not a test gap (explicit annotation, gate ②).
//   - The v1 driver's official floor is 3.6; the bridge relaxes it to wire 4
//     (see v1.go init) — support below 3.6 is best-effort by declaration
//     (docs) and this test is the empirical authority.
//
// Gate: TIMSTOOL_MONGO_27017 (host[:port]) — the standing 3.x instance.
// The compare is a self-comparison of the instance's real testdb dataset
// (same db both sides → must report equal): the engine is read-only, and the
// v1 driver's WRITE commands misbehave on 3.2's OP_REPLY path, so fixture
// seeding is off the table — the standing 100k-doc sample is a better
// workload anyway.

func legacyAddr(t *testing.T) (string, int) {
	t.Helper()
	addr := os.Getenv("TIMSTOOL_MONGO_27017")
	if addr == "" {
		t.Skip("TIMSTOOL_MONGO_27017 not set; legacy-matrix tests require the standing 3.x mongod")
	}
	return splitHostPort(addr)
}

// TestDialSelectsV1OnLegacy pins the probe's second state (gate ④): a pre-4.4
// server must land on the v1 bridge (the v2 handshake cannot complete below
// wire 9; Dial falls through to v1, which connects).
func TestDialSelectsV1OnLegacy(t *testing.T) {
	host, port := legacyAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := mongoconn.Dial(ctx, mongoconn.DialConfig{Host: host, Port: port})
	if err != nil {
		t.Fatalf("Dial to 3.x: %v", err)
	}
	defer c.Disconnect(context.Background())
	if c.Driver() != "v1" {
		t.Errorf("Driver() = %q on legacy server, want v1", c.Driver())
	}
	if v, err := c.Version(ctx); err != nil {
		t.Errorf("Version: %v", err)
	} else {
		t.Logf("legacy server: %s", v)
	}
}

// TestV1BridgeFullPipelineOn3x runs the whole engine against the standing
// 3.x server through the v1 bridge: meta surfaces (ListDatabases/
// ListCollections with stats + index detail), then a self-compare of the
// real sample database (Find streaming, IDRange aggregate, Count, IndexSpecs,
// canonical hashing at 100k scale, checkpoints).
func TestV1BridgeFullPipelineOn3x(t *testing.T) {
	host, port := legacyAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c, err := mongoconn.Dial(ctx, mongoconn.DialConfig{Host: host, Port: port})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Disconnect(context.Background())
	if c.Driver() != "v1" {
		t.Fatalf("Driver() = %q, want v1 on 3.x", c.Driver())
	}

	// Meta surfaces with browse detail (v1 bridge path).
	dbs, err := c.ListDatabases(ctx)
	if err != nil {
		t.Fatalf("ListDatabases via bridge: %v", err)
	}
	target := ""
	for _, d := range dbs {
		if d.Name != "admin" && d.Name != "local" {
			target = d.Name
			break
		}
	}
	if target == "" {
		t.Skip("no user database on the legacy instance")
	}
	if _, err := c.ListCollections(ctx, target); err != nil {
		t.Fatalf("ListCollections via bridge: %v", err)
	}

	out := filepath.Join(t.TempDir(), "legacy_out")
	summary, err := CompareDatabases(ctx, c, c, target, target, Config{OutDir: out})
	if err != nil {
		t.Fatalf("compare via bridge: %v", err)
	}
	if len(summary.Collections) == 0 {
		t.Fatal("no collections compared")
	}
	for _, r := range summary.Collections {
		if r.Overall == OverallError {
			t.Errorf("collection %s: engine error: %s", r.Coll, r.Checks.Error)
		}
	}
	// A self-comparison must not report diffs (ttl_skip is fine: TTL
	// collections legitimately expire).
	bad := 0
	for _, r := range summary.Collections {
		if r.Overall != OverallEqual && r.Overall != OverallTTLSkip {
			bad++
			t.Errorf("self-compare: %s = %s (%+v)", r.Coll, r.Overall, r.Checks)
		}
	}
	if bad > 0 {
		t.Fatalf("%d collections diverged on a self-comparison", bad)
	}
	if _, err := os.Stat(filepath.Join(out, ".checkpoints")); err != nil {
		t.Errorf("checkpoints missing: %v", err)
	}
	n := 0
	for _, r := range summary.Collections {
		n += len(r.Checks.Hash)
	}
	t.Logf("3.x self-compare: %d collections, %d bucket results, all equal; Decimal128 absent on 3.2.4 by server fact (gate ② annotation)",
		len(summary.Collections), n)
}
