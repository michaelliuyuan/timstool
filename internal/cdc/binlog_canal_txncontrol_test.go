package cdc

// MS-11j B-layer anchors (病灶 B): gateDDLQuery must plain-ignore
// administrative / transaction-control statements on ALL paths. Before
// MS-11j the ddlAdminKind whitelist only ran inside the off-target scan
// (ddlMentionsTargetDB); an admin statement at schema==target fell
// straight to the halt — te E2E U5's first incident (BEGIN halted CDC on
// the first source write). These anchors are red before the fix, green
// after.

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

// TestGateAdminStmtTargetIgnored: admin/txn-control queries with schema ==
// TARGET (the 病灶 B shape) must pass the gate nil — both the direct gate
// face and the OnUnrecognizedQuery delivery face.
func TestGateAdminStmtTargetIgnored(t *testing.T) {
	s := testStreamer()
	s.cfg.Database = "db"
	h := &canalHandler{s: s}

	for _, q := range []string{
		"BEGIN",
		"START TRANSACTION",
		"COMMIT",
		"ROLLBACK",
		"SAVEPOINT sp1",
		"RELEASE SAVEPOINT sp1",
		"SET autocommit = 1",
		"USE db",
		"FLUSH TABLES",
		"LOCK TABLES t1 READ",
		"UNLOCK TABLES",
		// word-interstitial comments must not defeat the whitelist
		"BEGIN /* txn */",
		// MS-11j ②: XA has no AST type — parse_error face, caught by the
		// B-layer word whitelist (leader seq278 discretionary add).
		"XA START 'xid1'",
		"XA END 'xid1'",
		"XA COMMIT 'xid1'",
	} {
		if err := h.gateDDLQuery("db", q); err != nil {
			t.Fatalf("gateDDLQuery(target, %q) = %v, want nil (admin plain-ignore)", q, err)
		}
		qe := &replication.QueryEvent{Schema: []byte("db"), Query: []byte(q)}
		if err := h.OnUnrecognizedQuery(&replication.EventHeader{}, mysql.Position{Name: "mysql-bin.000001", Pos: 42}, qe, "no_table_node"); err != nil {
			t.Fatalf("OnUnrecognizedQuery(target, %q) = %v, want nil (admin plain-ignore)", q, err)
		}
	}
	if s.Err() != nil {
		t.Fatalf("admin statements must not set fatal: %v", s.Err())
	}
}

// TestGateAdminStmtOutsideTargetStillIgnored: off-target admin statements
// keep the plain ignore (MS-11e semantics — no regression from moving the
// whitelist earlier).
func TestGateAdminStmtOutsideTargetStillIgnored(t *testing.T) {
	s := testStreamer()
	s.cfg.Database = "db"
	h := &canalHandler{s: s}
	for _, tt := range []struct{ schema, q string }{
		{"other_db", "BEGIN"},
		{"", "SET autocommit = 1"},
	} {
		if err := h.gateDDLQuery(tt.schema, tt.q); err != nil {
			t.Fatalf("gateDDLQuery(%q, %q) = %v, want nil", tt.schema, tt.q, err)
		}
	}
	if s.Err() != nil {
		t.Fatalf("fatal must stay clean: %v", s.Err())
	}
}

// TestGateDDLRedLineUnchanged: non-admin object statements keep the full
// red-line semantics — target halts, off-target without target mention
// ignores, target-qualified halts (pen8/pen9 anchors re-asserted after
// the whitelist moved to the gate entry).
func TestGateDDLRedLineUnchanged(t *testing.T) {
	s := testStreamer()
	s.cfg.Database = "db"
	h := &canalHandler{s: s}

	if err := h.gateDDLQuery("db", "CREATE VIEW v AS SELECT 1"); !errors.Is(err, ErrMySQLDDLUnsupported) {
		t.Fatalf("target object DDL must halt, got %v", err)
	}
	if err := h.gateDDLQuery("other_db", "CREATE VIEW ov AS SELECT 1"); err != nil {
		t.Fatalf("off-target unqualified DDL must ignore, got %v", err)
	}
	if err := h.gateDDLQuery("", "CREATE VIEW `db`.`v` AS SELECT 1"); !errors.Is(err, ErrMySQLDDLUnsupported) {
		t.Fatalf("target-qualified DDL must halt, got %v", err)
	}
	// admin-word prefix confusion must NOT slide object DDL through: the
	// whitelist anchors at statement start, so a leading SET/USE inside a
	// multi-statement shape is fine, but CREATE USER at target is admin
	// (no table objects) — assert the family boundary explicitly.
	if err := h.gateDDLQuery("db", "CREATE USER u1 IDENTIFIED BY 'x'"); err != nil {
		t.Fatalf("CREATE USER is admin (no table objects), want ignore, got %v", err)
	}
}

// TestGateAdminFormAnchor: form anchor for the B-layer — the gate entry
// must run the admin whitelist before any path (drift alarm if a refactor
// moves it back inside the off-target scan).
func TestGateAdminFormAnchor(t *testing.T) {
	data, err := os.ReadFile("binlog_canal.go")
	if err != nil {
		t.Fatalf("read binlog_canal.go: %v", err)
	}
	src := string(data)
	for _, anchor := range []string{
		"if ddlAdminKind.MatchString(ddlStripComments(query)) {",
		"admin/txn-control query ignored at gate",
	} {
		if !strings.Contains(src, anchor) {
			t.Fatalf("binlog_canal.go missing B-layer form anchor: %s", anchor)
		}
	}
	// the entry check must precede the target/schema branching
	entry := strings.Index(src, "if ddlAdminKind.MatchString(ddlStripComments(query)) {")
	branch := strings.Index(src, "if schema == \"\" || !strings.EqualFold(schema, target) {")
	if entry == -1 || branch == -1 || entry > branch {
		t.Fatalf("B-layer entry check must precede schema branching (entry=%d branch=%d)", entry, branch)
	}
}
