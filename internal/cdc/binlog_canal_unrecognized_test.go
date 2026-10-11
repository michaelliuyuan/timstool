package cdc

// MS-11g 笔① anchors: the unclassifiable-Query gate. canalpatch (vendored
// go-mysql v1.11.0 canal) delivers two shapes upstream skipped SILENTLY —
// parse failures (CREATE TRIGGER, stored procedures) and parseable
// zero-table-node statements (VIEW family) — to canalHandler.
// OnUnrecognizedQuery, which runs the SAME red-line gate as OnDDL
// (target-db halt / non-target ignore). Anchors here are path-level only
// (canal mock infeasibility lesson, MS-11e pen 2); the live DDL matrix
// (TRIGGER/PROCEDURE/VIEW × target/non-target) belongs to te black-box.

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

// TestCanalHandlerUnrecognizedQueryTargetHalts: a parse-failed DDL on the
// TARGET database halts with the stop-chain wording family, the canal
// reason riding along, and the fatal recorded on the streamer.
func TestCanalHandlerUnrecognizedQueryTargetHalts(t *testing.T) {
	s := testStreamer()
	s.cfg.Database = "db"
	h := &canalHandler{s: s}
	q := &replication.QueryEvent{Schema: []byte("db"), Query: []byte("CREATE TRIGGER trg AFTER INSERT ON t1 FOR EACH ROW SET @x=1")}
	err := h.OnUnrecognizedQuery(&replication.EventHeader{}, mysql.Position{Name: "mysql-bin.000001", Pos: 42}, q,
		"parse_error: line 1 column 7 near \"TRIGGER\"")
	if !errors.Is(err, ErrMySQLDDLUnsupported) {
		t.Fatalf("err = %v, want ErrMySQLDDLUnsupported chain", err)
	}
	if !errors.Is(s.Err(), ErrMySQLDDLUnsupported) {
		t.Fatalf("fatal = %v, want recorded", s.Err())
	}
	msg := err.Error()
	for _, want := range []string{"不支持在线 DDL", "CREATE TRIGGER", "[canal 未识别:", "parse_error"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "%s") || strings.Contains(msg, "%!s") {
		t.Fatalf("literal format placeholder leaked: %q", msg)
	}

	// zero-node shape (VIEW family) on the target database: same halt.
	q2 := &replication.QueryEvent{Schema: []byte("db"), Query: []byte("CREATE VIEW v AS SELECT 1")}
	err = h.OnUnrecognizedQuery(&replication.EventHeader{}, mysql.Position{}, q2, "no_table_node")
	if !errors.Is(err, ErrMySQLDDLUnsupported) || !strings.Contains(err.Error(), "no_table_node") {
		t.Fatalf("zero-node target DDL must halt with reason, got %v", err)
	}
}

// TestCanalHandlerUnrecognizedQueryOutsideTargetIgnored: unclassifiable
// statements outside the target keep streaming (MS-11e A semantics — the
// ignore branch must not regress for the new face), and an empty-schema
// statement naming the target database halts (pen8 coarse scan).
func TestCanalHandlerUnrecognizedQueryOutsideTargetIgnored(t *testing.T) {
	s := testStreamer()
	s.cfg.Database = "db"
	h := &canalHandler{s: s}

	for _, tt := range []struct {
		schema string
		query  string
		reason string
	}{
		{"other_db", "CREATE TRIGGER trg AFTER INSERT ON x FOR EACH ROW SET @x=1", "parse_error: x"},
		{"other_db", "CREATE VIEW ov AS SELECT 1", "no_table_node"},
		{"", "CREATE VIEW ov AS SELECT 1", "no_table_node"},
	} {
		q := &replication.QueryEvent{Schema: []byte(tt.schema), Query: []byte(tt.query)}
		if err := h.OnUnrecognizedQuery(&replication.EventHeader{}, mysql.Position{Name: "mysql-bin.000001", Pos: 42}, q, tt.reason); err != nil {
			t.Fatalf("OnUnrecognizedQuery(schema=%q) = %v, want nil (ignored)", tt.schema, err)
		}
	}
	if s.Err() != nil {
		t.Fatalf("ignored shapes must not set fatal: %v", s.Err())
	}

	// pen8 red line on the new face: empty schema but the statement names
	// the target database — halt, never a silent pass.
	q := &replication.QueryEvent{Schema: []byte(""), Query: []byte("CREATE VIEW `db`.`v` AS SELECT 1")}
	if err := h.OnUnrecognizedQuery(&replication.EventHeader{}, mysql.Position{}, q, "no_table_node"); !errors.Is(err, ErrMySQLDDLUnsupported) {
		t.Fatalf("target-qualified unclassifiable DDL must halt, got %v", err)
	}
}

// TestCanalpatchFormAnchors pins the two delivery points in the vendored
// sync.go and the additive interface face — a go-mysql re-sync that drops
// or moves them fails here (drift alarm, see internal/canalpatch/README).
func TestCanalpatchFormAnchors(t *testing.T) {
	syncSrc := readVendored(t, "sync.go")
	handlerSrc := readVendored(t, "handler.go")

	for _, anchor := range []string{
		// delivery point 1: parse failure (upstream returned nil here)
		`uh.OnUnrecognizedQuery(ev.Header, pos, e, "parse_error: "+err.Error())`,
		// delivery point 2: parseable, zero table nodes overall; MS-11j —
		// all-txn-control events (BEGIN/COMMIT/...) stay silent (upstream
		// parity), only object statements reach the gate.
		`uh.OnUnrecognizedQuery(ev.Header, pos, e, "no_table_node")`,
		`if len(stmts) > 0 && nodesTotal == 0 && !allTxnControl {`,
		// MS-11j A-layer classifier (must exist in vendored copy)
		`func isTxnControlStmt(stmt ast.StmtNode) bool {`,
		// interface-assertion delivery (opt-in; Dummy keeps upstream behavior)
		`c.eventHandler.(UnrecognizedQueryHandler)`,
		// upstream skip log kept (parity for non-implementing handlers)
		`will skip this event`,
	} {
		if !strings.Contains(syncSrc, anchor) {
			t.Fatalf("canalpatch/sync.go missing form anchor: %s", anchor)
		}
	}
	if strings.Count(syncSrc, `c.eventHandler.(UnrecognizedQueryHandler)`) != 2 {
		t.Fatalf("canalpatch/sync.go must deliver from exactly two points")
	}
	for _, anchor := range []string{
		`type UnrecognizedQueryHandler interface {`,
		`OnUnrecognizedQuery(header *replication.EventHeader, nextPos mysql.Position, queryEvent *replication.QueryEvent, reason string) error`,
	} {
		if !strings.Contains(handlerSrc, anchor) {
			t.Fatalf("canalpatch/handler.go missing interface anchor: %s", anchor)
		}
	}
}

func readVendored(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../canalpatch/" + name)
	if err != nil {
		t.Fatalf("read canalpatch/%s: %v", name, err)
	}
	return string(data)
}
