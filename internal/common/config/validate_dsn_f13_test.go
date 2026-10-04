package config

import (
	"strings"
	"testing"
)

// F-13 anchors: the validate read session pins time_zone='+00:00' on BOTH
// sides; the data-path DSNs stay unpinned (their wall-clock coupling with
// the write path is load-bearing — leader ruling seq640/641).
func TestValidateSessionDSNPinnedUTC(t *testing.T) {
	tgt := TargetConfig{Host: "tidb", Port: 4000, User: "root", Password: "pw", Database: "db"}

	pinned := tgt.DSNPinnedUTC()
	if !strings.Contains(pinned, "time_zone=%27%2B00%3A00%27") && !strings.Contains(pinned, "time_zone='+00:00'") {
		t.Fatalf("target pinned DSN missing UTC time_zone: %s", pinned)
	}
	if got := tgt.DSN(); strings.Contains(got, "time_zone") {
		t.Fatalf("data-path target DSN() must stay unpinned: %s", got)
	}

	// Source side reuses DSNByType (MS-08d): mysql pins UTC, postgres is
	// byte-identical to the legacy DSN().
	my := SourceConfig{Type: "mysql", Host: "my", Port: 3306, User: "u", Password: "pw", Database: "db"}
	myDSN := my.DSNByType()
	if !strings.Contains(myDSN, "time_zone=%27%2B00%3A00%27") && !strings.Contains(myDSN, "time_zone='+00:00'") {
		t.Fatalf("mysql DSNByType missing UTC time_zone: %s", myDSN)
	}
	pg := SourceConfig{Type: "postgres", Host: "pg", Port: 5432, User: "u", Password: "pw", Database: "d", SSLMode: "disable"}
	if pg.DSNByType() != pg.DSN() {
		t.Fatalf("postgres DSNByType must equal DSN() byte-identically")
	}
}
