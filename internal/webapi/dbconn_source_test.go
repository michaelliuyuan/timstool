package webapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// TestSourceConnSpecDispatch (MS-09): the connection layer dispatches on the
// NORMALIZED source type. mysql must take the mysql driver with DSNByType (the
// MS-08d time_zone='+00:00' UTC session pin rides on the DSN); postgres,
// legacy-empty and unknown kinds keep the pgx pair byte-identical to the old
// openPGTestConn(sc.DSN()) call shape.
func TestSourceConnSpecDispatch(t *testing.T) {
	base := config.SourceConfig{Host: "h", Port: 5432, User: "u", Password: "p", Database: "d", Schema: "public", SSLMode: "disable"}

	pg := base
	if d, dsn := sourceConnSpec(pg); d != "pgx" || dsn != pg.DSN() {
		t.Fatalf("postgres: got (%q, %q), want (pgx, sc.DSN())", d, dsn)
	}

	legacy := base // empty Type normalizes to postgres
	if d, dsn := sourceConnSpec(legacy); d != "pgx" || dsn != legacy.DSN() {
		t.Fatalf("legacy empty type: got (%q, %q), want (pgx, sc.DSN())", d, dsn)
	}

	my := base
	my.Type = "mysql"
	d, dsn := sourceConnSpec(my)
	if d != "mysql" {
		t.Fatalf("mysql: driver %q, want mysql", d)
	}
	if dsn != my.DSNByType() {
		t.Fatalf("mysql: dsn %q, want sc.DSNByType() %q (MS-08d pin must ride on the DSN)", dsn, my.DSNByType())
	}
	// Negative anchor, mirroring TestSourceDSNByType_MySQLSessionTimeZonePinned:
	// the UTC session pin must survive the dispatch.
	if !strings.Contains(dsn, "time_zone=") || !strings.Contains(dsn, "%27%2B00%3A00%27") {
		t.Fatalf("mysql: DSNByType lost the time_zone='+00:00' UTC pin: %q", dsn)
	}
	if strings.Contains(dsn, "sslmode") {
		t.Fatalf("mysql: PG-only sslmode leaked into the mysql DSN: %q", dsn)
	}
}

// TestSourceConnDirectCallTripwire (MS-09): the raw openers stay sealed behind
// the dispatch. openPGTestConn may only be referenced inside dbconn.go; the
// openMySQLTestConn direct-call whitelist is the two TargetConfig-side sites
// (server.go target test, incremental.go UTC-pinned write session) plus
// dbconn.go itself. New source-side consumers must go through
// openSourceTestConn so the driver/DSN pair (and the MS-08d pin) cannot drift.
func TestSourceConnDirectCallTripwire(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	pgAllowed := map[string]bool{"dbconn.go": true}
	myAllowed := map[string]bool{"dbconn.go": true, "server.go": true, "incremental.go": true}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "openPGTestConn") && !pgAllowed[name] {
			t.Errorf("openPGTestConn referenced outside dbconn.go in %s: route source-side connections through openSourceTestConn (MS-09)", name)
		}
		if strings.Contains(string(b), "openMySQLTestConn") && !myAllowed[name] {
			t.Errorf("openMySQLTestConn referenced outside the TargetConfig-side whitelist in %s: source-side connections must go through openSourceTestConn so the MS-08d UTC pin cannot be bypassed", name)
		}
	}
}
