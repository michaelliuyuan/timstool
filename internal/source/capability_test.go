package source_test

// MS-01 anchors (ruling seq 82 final):
//   A1 — capability-bit snapshots per registered kind: postgres all-true,
//        mysql schema/data only, tidb ALL-false (target-only status quo),
//        stubs all-false. Pins the matrix the UI greying reads.
//   A2 — NormalizeKind single-default semantics: ""→postgres, unknown
//        non-empty→error (never silently downgraded), known (incl. tidb)
//        passes through.
//   A3 — frozen-baseline type-branch inventory ("only-decrease, zero at
//        MS-12"): the ruling pattern is scanned repo-wide (non-test, outside
//        internal/source — the registry/parse layer is the one allowed
//        place); context-excluded false positives (column types, role
//        routing, nil checks...) are filtered; what remains must equal the
//        frozen fixture EXACTLY — in both directions: a new branch fails,
//        and so does a removed one whose fixture entry was not pruned.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// A1: capability snapshots.
func TestCapabilityMatrixSnapshot(t *testing.T) {
	pg, err := source.Describe("postgres")
	if err != nil {
		t.Fatalf("describe postgres: %v", err)
	}
	pgc := pg.Capabilities
	if !(pgc.Schema && pgc.Data && pgc.CDC && pgc.Compare && pgc.Watermark && pgc.Assess && pgc.DDLExport) {
		t.Errorf("postgres capabilities = %+v, want all seven bits true", pgc)
	}

	my, err := source.Describe("mysql")
	if err != nil {
		t.Fatalf("describe mysql: %v", err)
	}
	myc := my.Capabilities
	if !myc.Schema || !myc.Data {
		t.Errorf("mysql schema/data = %v/%v, want true/true", myc.Schema, myc.Data)
	}
	for _, off := range []struct {
		name string
		bit  bool
	}{
		{"cdc", myc.CDC}, {"compare", myc.Compare}, {"watermark", myc.Watermark},
		{"assess", myc.Assess}, {"ddl_export", myc.DDLExport},
	} {
		if off.bit {
			t.Errorf("mysql capability %q = true, want false until MS-08..MS-11 land", off.name)
		}
	}

	// Ruling seq 82 ①: tidb is a registered kind with EVERY bit false —
	// target-only today; the flip point is tidb_meta.go and must never be
	// flipped without removing the three tidb-as-source 400 guards too.
	td, err := source.Describe("tidb")
	if err != nil {
		t.Fatalf("describe tidb: %v", err)
	}
	if td.Capabilities != (source.Capabilities{}) {
		t.Errorf("tidb capabilities = %+v, want all-false (target-only status quo)", td.Capabilities)
	}

	or, err := source.Describe("oracle")
	if err != nil {
		t.Fatalf("describe oracle: %v", err)
	}
	if or.Capabilities != (source.Capabilities{}) {
		t.Errorf("stub oracle capabilities = %+v, want all-false zero value", or.Capabilities)
	}
}

// A2: NormalizeKind single-default semantics.
func TestNormalizeKindDefaultAndUnknown(t *testing.T) {
	if k, err := source.NormalizeKind(""); err != nil || k != "postgres" {
		t.Errorf(`NormalizeKind("") = %q, %v; want "postgres", nil (legacy default)`, k, err)
	}
	for _, kind := range []string{"mysql", "tidb", "postgres"} {
		if k, err := source.NormalizeKind(kind); err != nil || k != kind {
			t.Errorf("NormalizeKind(%q) = %q, %v; want passthrough", kind, k, err)
		}
	}
	if k, err := source.NormalizeKind("cockroachdb"); err == nil {
		t.Errorf(`NormalizeKind("cockroachdb") = %q, nil; want error (no silent downgrade)`, k)
	}
}

// Capable reads the bits + rejects unknown kinds/capabilities.
func TestCapableSingleTruth(t *testing.T) {
	ok, err := source.Capable("", source.CapCompare)
	if err != nil || !ok {
		t.Errorf(`Capable("", compare) = %v, %v; want true (legacy default = postgres)`, ok, err)
	}
	ok, err = source.Capable("mysql", source.CapCompare)
	if err != nil || ok {
		t.Errorf(`Capable("mysql", compare) = %v, %v; want false until MS-08`, ok, err)
	}
	if ok, err := source.Capable("tidb", source.CapData); err != nil || ok {
		t.Errorf(`Capable("tidb", data) = %v, %v; want false/nil (target-only)`, ok, err)
	}
	if _, err := source.Capable("nosuch", source.CapCompare); err == nil {
		t.Error(`Capable("nosuch", compare) succeeded; want error for unknown kind`)
	}
	if _, err := source.Capable("postgres", source.Capability("bogus")); err == nil {
		t.Error(`Capable("postgres", "bogus") succeeded; want error for unknown capability`)
	}
}

// A3 frozen baseline (ruling seq 82 ②). Baseline was 27 sites; MS-01 absorbed
// the two source_handler ""→postgres defaults into NormalizeKind. The fixture
// below is the post-MS-01 inventory: 26 sites (the ruling's enumeration of 27
// plus orchestrator.go's dumpling fast-path routing at :269, same #t79
// dual-path class — pattern-true, so it is frozen in).
//
// Only-decrease: MS-03..MS-07 each swap their guards for capability reads and
// prune their fixture lines; MS-12 asserts the map is empty.
var typeBranchFixture = map[string]int{
	// config-level legacy default (unification deferred; data-shaping only).
	"internal/common/config/config.go": 1,
	// #t79 dual-path routing (legitimate routing, not feature gating).
	"internal/orchestrator/orchestrator.go": 3,
	// CDC source guard + target-tidb guard (M4 binlog work).
	"internal/webapi/cdc_config.go": 2,
	// compare source guards (MS-03) + target-tidb guard.
	"internal/webapi/compare.go": 3,
	// connection-test target-style routing for tidb profiles.
	"internal/webapi/datasource.go": 1,
	// DDL export source guard (MS-07).
	"internal/webapi/ddl_export_handler.go": 1,
	// incremental source guards absorbed into the WatermarkDialect capability
	// read (MS-04, incSourceWatermarkCapable: empty kind rejected explicitly
	// inside the helper); target-tidb guards remain.
	"internal/webapi/incremental.go": 2,
	// legacy PG-direct endpoints + tidb-as-source guard + target guard +
	// cdc_chain guard (M4) + assess guard (MS-06).
	"internal/webapi/server.go": 4,
	// tidb-as-source guards (kept until the tidb flip point).
	"internal/webapi/source_handler.go": 2,
}

// Ruling pattern (seq 82): catches .Type/srcType/SourceType() compares plus
// the `XxxType == ""` legacy-default shape.
var typeBranchRe = regexp.MustCompile(`\.Type ==|\.Type !=|SourceType\(\) ==|SourceType\(\) !=|srcType ==|srcType !=|Type == ""`)

// Context exclusions (ruling seq 82 enumeration) — lines that match the
// pattern but are NOT source-kind behavior branches.
var typeBranchExclusions = []string{
	"col.Type ==",          // cdc/transformer.go: column type mapping
	"mysqlType == ",        // schema/ddl.go: type-name default
	"wmType == ",           // incremental.go: watermark kind default
	".Type != nil",         // cdc_config.go: pointer nil passthrough
	`req.Type != ""`,       // datasource.go:320 update consistency check
	`req.Type == "source"`, // server.go: connection-test role routing
	`req.Type != "source"`, // server.go: connection-test role routing
}

func isExcluded(line string) bool {
	for _, ex := range typeBranchExclusions {
		if strings.Contains(line, ex) {
			return true
		}
	}
	return false
}

func TestTypeBranchFrozenBaseline(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist", "frontend":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		// The source package IS the registry/parse layer — the one allowed
		// place for kind branches (five-pin #3).
		if rel == "internal/source" || strings.HasPrefix(rel, "internal/source/") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || isExcluded(trimmed) {
				continue
			}
			if typeBranchRe.MatchString(line) {
				found[rel]++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for file, n := range found {
		if allowed, ok := typeBranchFixture[file]; !ok {
			t.Errorf("NEW type branch in %s (x%d) — route through source.Capable / the registry and prune or justify the fixture", file, n)
		} else if n != allowed {
			t.Errorf("%s has %d type branches, fixture says %d — if you removed guards, prune the fixture (only-decrease); if you added one, reroute it", file, n, allowed)
		}
	}
	for file, allowed := range typeBranchFixture {
		if found[file] == 0 {
			t.Errorf("fixture entry %s (x%d) has no branches left — prune it (only-decrease bookkeeping)", file, allowed)
		}
	}
}
