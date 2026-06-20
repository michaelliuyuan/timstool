package source_test

import (
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source"
	// Trigger adapter init() so postgres/mysql RegisterMeta runs alongside the
	// stubs (which live in package source). The real binary imports these via
	// main; this test must do the same to see all sources.
	_ "github.com/michaelliuyuan/timstool/internal/source/mysql"
	_ "github.com/michaelliuyuan/timstool/internal/source/postgres"
)

// TestDescribeAllIncludesAllSources: DescribeAll lists every source incl. stubs,
// without opening any connection (doc §4/§10.1).
func TestDescribeAllIncludesAllSources(t *testing.T) {
	all := source.DescribeAll()
	if len(all) < 5 {
		t.Fatalf("DescribeAll() = %d sources, want >= 5 (pg/mysql/oracle/mssql/db2)", len(all))
	}
	byName := map[string]source.SourceMeta{}
	for _, m := range all {
		byName[m.Name] = m
	}
	for _, name := range []string{"postgres", "mysql", "oracle", "mssql", "db2"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("DescribeAll missing %q", name)
		}
	}
}

// TestDescribeImplementedVsStub: implemented sources declare capabilities +
// default port; stubs describe themselves with Implemented=false + a hint, with
// no working Open (doc §4/§10.1).
func TestDescribeImplementedVsStub(t *testing.T) {
	pg, err := source.Describe("postgres")
	if err != nil {
		t.Fatalf("Describe(postgres) err = %v", err)
	}
	if !pg.Implemented || pg.DefaultPort != 5432 || !pg.Capabilities.CDC {
		t.Errorf("postgres meta = %+v, want Implemented/5432/CDC=true", pg)
	}
	my, _ := source.Describe("mysql")
	if !my.Implemented || my.DefaultPort != 3306 || my.Capabilities.CDC {
		t.Errorf("mysql meta = %+v, want Implemented/3306/CDC=false", my)
	}
	ora, err := source.Describe("oracle")
	if err != nil {
		t.Fatalf("Describe(oracle) err = %v", err)
	}
	if ora.Implemented {
		t.Errorf("oracle Implemented = true, want false (stub)")
	}
	if ora.NotImplMsg == "" {
		t.Error("oracle NotImplMsg empty, want a hint")
	}
}

func TestDescribeUnknown(t *testing.T) {
	_, err := source.Describe("definitely-not-a-source")
	if err == nil || !strings.Contains(err.Error(), "unknown source") {
		t.Fatalf("Describe(unknown) err = %v, want unknown-source error", err)
	}
}
