package canal

// MS-11j A-layer anchors. The pen-1 zero-node delivery point must stay
// silent for transaction-control / session-admin statements (upstream
// parity — BEGIN opens every row-based DML transaction; delivering it
// halted CDC on the first source write, te E2E U5 P0) while object
// statements (VIEW family) keep flowing to the host DDL gate.

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/siddontang/go-log/loggers"
)

// nopAdvanced satisfies loggers.Advanced for zero-noise Canal construction.
type nopAdvanced struct{}

func (nopAdvanced) Fatal(...interface{})          {}
func (nopAdvanced) Fatalf(string, ...interface{}) {}
func (nopAdvanced) Fatalln(...interface{})        {}
func (nopAdvanced) Panic(...interface{})          {}
func (nopAdvanced) Panicf(string, ...interface{}) {}
func (nopAdvanced) Panicln(...interface{})        {}
func (nopAdvanced) Print(...interface{})          {}
func (nopAdvanced) Printf(string, ...interface{}) {}
func (nopAdvanced) Println(...interface{})        {}
func (nopAdvanced) Debug(...interface{})          {}
func (nopAdvanced) Debugf(string, ...interface{}) {}
func (nopAdvanced) Debugln(...interface{})        {}
func (nopAdvanced) Error(...interface{})          {}
func (nopAdvanced) Errorf(string, ...interface{}) {}
func (nopAdvanced) Errorln(...interface{})        {}
func (nopAdvanced) Info(...interface{})           {}
func (nopAdvanced) Infof(string, ...interface{})  {}
func (nopAdvanced) Infoln(...interface{})         {}
func (nopAdvanced) Warn(...interface{})           {}
func (nopAdvanced) Warnf(string, ...interface{})  {}
func (nopAdvanced) Warnln(...interface{})         {}

var _ loggers.Advanced = nopAdvanced{}

// TestIsTxnControlStmtClassification: classification face — the ten
// txn-control/session-admin AST types classify true regardless of textual
// shape (BEGIN WORK / chained SET / ... variants are immune), object DDL
// stays false (delivery red line preserved).
func TestIsTxnControlStmtClassification(t *testing.T) {
	p := parser.New()
	yes := []string{
		"BEGIN",
		"START TRANSACTION",
		"COMMIT",
		"ROLLBACK",
		"SAVEPOINT sp1",
		"ROLLBACK TO sp1",
		"RELEASE SAVEPOINT sp1",
		"SET @a = 1",
		"SET autocommit = 1",
		"USE migration_test",
		"FLUSH TABLES",
		"LOCK TABLES t1 READ",
		"UNLOCK TABLES",
		// MS-11j ① full audit: session/read-only/utility zero-node shapes
		// (binlog logs rewritten statement text, not the EXECUTE form).
		"PREPARE stmt1 FROM 'SELECT 1'",
		"EXECUTE stmt1",
		"DEALLOCATE PREPARE stmt1",
		"DO SLEEP(1)",
		"SHOW TABLES",
		"HELP 'contents'",
		"BINLOG 'e90e'",
	}
	for _, q := range yes {
		stmts, _, err := p.Parse(q, "", "")
		if err != nil {
			t.Fatalf("parse %q: %v", q, err)
		}
		if len(stmts) != 1 {
			t.Fatalf("parse %q: got %d stmts, want 1", q, len(stmts))
		}
		if !isTxnControlStmt(stmts[0]) {
			t.Errorf("isTxnControlStmt(%q) = false, want true", q)
		}
	}
	no := []string{
		"CREATE VIEW v1 AS SELECT 1",
		"DROP VIEW v1",
		"CREATE TABLE t1 (id INT)",
		"DROP TABLE t1",
		"TRUNCATE TABLE t1",
		// MS-11j ① ruling: CALL stays delivered — procedure bodies are
		// opaque and may carry DDL (fail-closed).
		"CALL some_proc()",
	}
	// (ALTER VIEW parses as an ERROR in this parser build — it rides the
	// pen-1 parse_error delivery face, not the zero-node face; covered by
	// internal/cdc TestCanalHandlerUnrecognizedQueryTargetHalts. XA has no
	// AST type either — parse_error face, caught word-level by the
	// B-layer ddlAdminKind XA entry, internal/cdc TestGateAdminStmtTargetIgnored.)
	for _, q := range no {
		stmts, _, err := p.Parse(q, "", "")
		if err != nil {
			t.Fatalf("parse %q: %v", q, err)
		}
		if len(stmts) != 1 {
			t.Fatalf("parse %q: got %d stmts, want 1", q, len(stmts))
		}
		if isTxnControlStmt(stmts[0]) {
			t.Errorf("isTxnControlStmt(%q) = true, want false (object DDL must reach the gate)", q)
		}
	}
}

// captureHandler records gate-relevant deliveries; everything else keeps
// the Dummy (upstream-parity) no-op behavior.
type captureHandler struct {
	DummyEventHandler
	ddl          int
	unrecognized int
}

func (h *captureHandler) OnDDL(*replication.EventHeader, mysql.Position, *replication.QueryEvent) error {
	h.ddl++
	return nil
}

func (h *captureHandler) OnUnrecognizedQuery(*replication.EventHeader, mysql.Position, *replication.QueryEvent, string) error {
	h.unrecognized++
	return nil
}

func newCaptureCanal() (*Canal, *captureHandler) {
	h := &captureHandler{}
	c := &Canal{parser: parser.New(), master: &masterInfo{logger: nopAdvanced{}}, eventHandler: h}
	return c, h
}

func queryEvent(query, schema string) *replication.BinlogEvent {
	ev := &replication.BinlogEvent{}
	ev.Header = &replication.EventHeader{LogPos: 100, Timestamp: 1}
	ev.Event = &replication.QueryEvent{Schema: []byte(schema), Query: []byte(query)}
	return ev
}

// TestHandleEventTxnControlNotDelivered: the P0 regression anchor — a
// BEGIN/COMMIT Query event (row-based DML transaction boundaries, schema
// = the CDC target database, exactly te U5's first-incident shape) must
// trigger NO gate delivery and NO error (upstream silent parity).
func TestHandleEventTxnControlNotDelivered(t *testing.T) {
	for _, q := range []string{"BEGIN", "COMMIT", "SET autocommit = 1", "USE migration_test"} {
		c, h := newCaptureCanal()
		if err := c.handleEvent(queryEvent(q, "migration_test")); err != nil {
			t.Fatalf("handleEvent(%q) = %v, want nil", q, err)
		}
		if h.ddl != 0 || h.unrecognized != 0 {
			t.Fatalf("handleEvent(%q) delivered: ddl=%d unrecognized=%d, want 0/0 (MS-11j P0 regression)", q, h.ddl, h.unrecognized)
		}
	}
}

// TestHandleEventViewStillDelivered: the preserved red line — zero-node
// OBJECT statements (VIEW family) keep flowing to the gate (pen-1
// semantics untouched by the txn-control carve-out). Seven-type table-DDL
// OnDDL routing is unchanged by this patch and stays covered by the
// MS-11g anchors in internal/cdc (TestCanalHandlerDDLHardStop et al).
func TestHandleEventViewStillDelivered(t *testing.T) {
	c, h := newCaptureCanal()
	if err := c.handleEvent(queryEvent("CREATE VIEW v1 AS SELECT 1", "migration_test")); err != nil {
		t.Fatalf("handleEvent(CREATE VIEW) = %v, want nil (handler decides)", err)
	}
	if h.unrecognized != 1 || h.ddl != 0 {
		t.Fatalf("CREATE VIEW delivery: ddl=%d unrecognized=%d, want 0/1", h.ddl, h.unrecognized)
	}
}

// compile-time: the classifier consumes the same AST package as parseStmt.
var _ = isTxnControlStmt(nil)
var _ ast.StmtNode = (*ast.BeginStmt)(nil)
