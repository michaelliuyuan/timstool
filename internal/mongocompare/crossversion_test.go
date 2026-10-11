package mongocompare

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

// The cross-version regression (seq=175/176): a 3.x↔modern compare once
// reported every numeric index key as a diff — 3.2.4's listIndexes reports
// key values as double 1.0/-1.0 while modern servers report int32, and the
// canonical index form is type-tag sensitive. The fix folds the ±1 integer
// forms at the mongoconn decode point; this test is its standing guard.
//
// THIS TEST CLASS IS ONLY EXPOSABLE CROSS-INSTANCE: the same-server dual-path
// bridge-equivalence and self-compare suites are structurally immune (both
// sides share one server's serialization), so do not remove this in favor of
// synthetic fixtures — the bug cannot reproduce on a single server.
//
// Gates: TIMSTOOL_MONGO_27017 (host[:port], standing 3.x instance) and
// TIMSTOOL_MONGO_SMOKE (host[:port], modern smoke container); either missing
// skips. The compared pair is the naturally same-origin users sample
// (10k-doc real dataset restored from the 3.x origin into the smoke
// container): cross-version canonical equality on real data is the strongest
// available provenance proof. Assertions target the users row by name —
// testdb may legitimately carry other collections on either side, and the
// engine is right to report those; the regression guard is users' overall.

func TestCrossVersionIndexCanonParity(t *testing.T) {
	legacy := os.Getenv("TIMSTOOL_MONGO_27017")
	if legacy == "" {
		t.Skip("TIMSTOOL_MONGO_27017 not set; cross-version tests require the standing 3.x mongod")
	}
	smoke := os.Getenv("TIMSTOOL_MONGO_SMOKE")
	if smoke == "" {
		t.Skip("TIMSTOOL_MONGO_SMOKE not set; cross-version tests require the modern smoke container")
	}
	lh, lp := splitHostPort(legacy)
	sh, sp := splitHostPort(smoke)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src, err := mongoconn.Dial(ctx, mongoconn.DialConfig{Host: lh, Port: lp})
	if err != nil {
		t.Fatalf("Dial legacy: %v", err)
	}
	defer src.Disconnect(context.Background())
	dst, err := mongoconn.Dial(ctx, mongoconn.DialConfig{Host: sh, Port: sp})
	if err != nil {
		t.Fatalf("Dial smoke: %v", err)
	}
	defer dst.Disconnect(context.Background())
	if src.Driver() != "v1" || dst.Driver() != "v2" {
		t.Fatalf("driver pair = %s/%s, want v1/v2 (cross-version arms require both paths)", src.Driver(), dst.Driver())
	}

	out := filepath.Join(t.TempDir(), "xver_out")
	sum, err := CompareDatabases(ctx, src, dst, "testdb", "testdb", Config{Buckets: 64, OutDir: out})
	if err != nil {
		t.Fatalf("CompareDatabases: %v", err)
	}
	row := findColl(t, sum, "users")
	if row.Overall != OverallEqual {
		t.Errorf("users overall = %q, want equal (the index-canon fold regression resurfaces as index diffs here)", row.Overall)
		if d, ok := row.Checks.Index["diff"].([]string); ok {
			for _, s := range d {
				t.Errorf("index diff: %s", s)
			}
		}
	}
	nMatch := 0
	for _, b := range row.Checks.Hash {
		if b.Match && b.SrcHash == b.DstHash {
			nMatch++
		}
	}
	if nMatch != len(row.Checks.Hash) || nMatch == 0 {
		t.Errorf("hash buckets matched %d/%d, want all", nMatch, len(row.Checks.Hash))
	}
	t.Logf("cross-version users: overall=%s, %d/%d hash buckets byte-equal (v1@3.x vs v2@modern)", row.Overall, nMatch, len(row.Checks.Hash))
}
