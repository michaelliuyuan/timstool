package source_test

// MS-01 anchors:
//   A1 鈥?capability-bit snapshots per registered kind (postgres all-true,
//        mysql schema/data only, stubs all-false). Pins the matrix the UI
//        greying and the API 400-guards read.
//   A2 鈥?NormalizeKind single-default semantics: ""鈫抪ostgres, unknown
//        non-empty鈫抏rror (never silently downgraded), known passes through.
//   A3 鈥?repo-wide "zero out-of-registry type behavior branch" grep anchor:
//        equality compares against source-kind literals are allowed ONLY in
//        the explicit grandfather list (orchestrator dual-path routing #t79,
//        the two legacy PG-direct endpoints in webapi/server.go). Anything
//        else fails, so a new 'if type == "mysql"' cannot sneak in unnoticed.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// A1: capability snapshots (three states: full / partial / stub).
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
	if k, err := source.NormalizeKind("mysql"); err != nil || k != "mysql" {
		t.Errorf(`NormalizeKind("mysql") = %q, %v; want passthrough`, k, err)
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
	if _, err := source.Capable("nosuch", source.CapCompare); err == nil {
		t.Error(`Capable("nosuch", compare) succeeded; want error for unknown kind`)
	}
	if _, err := source.Capable("postgres", source.Capability("bogus")); err == nil {
		t.Error(`Capable("postgres", "bogus") succeeded; want error for unknown capability`)
	}
}

// A3: grandfather list 鈥?the ONLY non-test sites allowed to branch on a
// source-kind string literal. Counts are exact: removing a grandfathered
// branch also fails, forcing this list to stay honest.
var typeBranchGrandfather = map[string]int{
	// #t79 dual-path routing: non-PG sources go through the Source+CIR engine
	// (architecture v1.0 pin #3: routing, not feature gating).
	"internal/orchestrator/orchestrator.go": 3,
	// Legacy PG-direct endpoints (/test-connection and /config/list-tables
	// source_ref paths speak the PG wire protocol only; retirement tracked
	// with the MS-06+ interface work).
	"internal/webapi/server.go": 2,
}

// Source kinds only — "tidb" is a TARGET-side datasource type routed by the
// target-ref gates, not a member of the source registry this anchor protects.
var typeBranchRe = regexp.MustCompile(`[!=]= "(postgres|mysql|oracle|mssql|db2)"`)

func TestNoTypeBehaviorBranchOutsideRegistry(t *testing.T) {
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
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // comment lines don't branch
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
		if allowed, ok := typeBranchGrandfather[file]; !ok {
			t.Errorf("out-of-registry type branch in %s (x%d) 鈥?route through source.Capable or the registry", file, n)
		} else if n != allowed {
			t.Errorf("grandfathered %s has %d type branches, list says %d 鈥?update the grandfather list if this change is intentional", file, n, allowed)
		}
	}
	for file, allowed := range typeBranchGrandfather {
		if found[file] == 0 {
			t.Errorf("grandfathered %s has no type branches left (list says %d) 鈥?prune the entry", file, allowed)
		}
	}
}
