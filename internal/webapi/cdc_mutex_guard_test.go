package webapi

// MS-11 pen 3 anchors: the dual-increment mutex (ruling seq 953 #3) —
// identity keying is stable, a PostgreSQL source never blocks, and the
// remediation message names the choose-one rule.

import (
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

func TestMySQLDualIncrementKeyStable(t *testing.T) {
	a := config.SourceConfig{Type: "mysql", Host: "h", Port: 3306, User: "u", Database: "db"}
	b := config.SourceConfig{Type: "mysql", Host: "h", Port: 3306, User: "u", Database: "db"}
	c := config.SourceConfig{Type: "mysql", Host: "other", Port: 3306, User: "u", Database: "db"}
	if mysqlDualIncrementKey(a) != mysqlDualIncrementKey(b) {
		t.Fatalf("same source must key equal")
	}
	if mysqlDualIncrementKey(a) == mysqlDualIncrementKey(c) {
		t.Fatalf("different host must key different")
	}
	if !strings.Contains(mysqlDualIncrementKey(a), "h:3306:u:db") {
		t.Fatalf("key shape = %q", mysqlDualIncrementKey(a))
	}
}

func TestDualIncrementNeverBlocksPG(t *testing.T) {
	s := &Server{}
	pg := config.SourceConfig{Type: "postgres", Host: "h", Database: "db"}
	if s.blockIncrementalRunForCDC(pg) {
		t.Fatalf("PG source must never hit the mysql mutex")
	}
	// MySQL with no wired CDC config (loadCDCConfig errors) also must not
	// block — the guard fails open only when CDC isn't configured at all.
	my := config.SourceConfig{Type: "mysql", Host: "h", Port: 3306, User: "u", Database: "db"}
	if s.blockIncrementalRunForCDC(my) {
		t.Fatalf("unwired CDC config must not block the polling engine")
	}
}

func TestDualIncrementMessageChoosesOne(t *testing.T) {
	for _, want := range []string{"互斥", "二选一", "binlog CDC"} {
		if !strings.Contains(dualIncrementMsg, want) {
			t.Fatalf("message %q missing %q", dualIncrementMsg, want)
		}
	}
}
