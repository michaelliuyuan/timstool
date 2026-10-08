package cdc

// MS-11 pen 2 anchors: canal adapter mapping semantics — row events map to
// source-neutral CDCEvents (no canal type leaks past binlogStreamer), the
// Query(DDL) hard-stop carries the explicit remediation message, the
// one-behind position rule holds across rotate/xid/row boundaries.

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/go-mysql-org/go-mysql/schema"
	"go.uber.org/zap"
)

func TestBinlogTableFilters(t *testing.T) {
	inc, exc, err := binlogTableFilters([]string{"db\\.t1", "db\\.t2"}, []string{"db\\.tmp_.*"})
	if err != nil {
		t.Fatalf("filters: %v", err)
	}
	if len(inc) != 2 || inc[0] != "^db\\.t1$" || exc[0] != "^db\\.tmp_.*$" {
		t.Fatalf("filters = %v / %v, want anchored regex forms", inc, exc)
	}
	if _, _, err := binlogTableFilters(nil, []string{"db\\.(["}); err == nil {
		t.Fatalf("invalid exclude regex must fail loud")
	}
}

func testStreamer() *canalStreamer {
	return &canalStreamer{
		log:    zap.NewNop(),
		events: make(chan *CDCEvent, 16),
		done:   make(chan struct{}),
	}
}

func testTable() *schema.Table {
	return &schema.Table{
		Schema:    "db",
		Name:      "t1",
		PKColumns: []int{0},
		Columns: []schema.TableColumn{
			{Name: "id"}, {Name: "name"},
		},
	}
}

func TestRowsEventInsertMapping(t *testing.T) {
	ev := &canal.RowsEvent{
		Table:  testTable(),
		Action: canal.InsertAction,
		Rows:   [][]interface{}{{int64(1), "a"}, {int64(2), "b"}},
		Header: &replication.EventHeader{LogPos: 157, Timestamp: 1700000000},
	}
	out, err := rowsEventToCDCEvents(ev, "mysql-bin.000001")
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("events = %d, want 2", len(out))
	}
	e := out[0]
	if e.Kind != EventInsert || e.Schema != "db" || e.Table != "t1" {
		t.Fatalf("shape = %+v", e)
	}
	if e.Binlog == nil || e.Binlog.String() != "mysql-bin.000001:157" {
		t.Fatalf("binlog position = %v, want mysql-bin.000001:157", e.Binlog)
	}
	if e.LSN != 0 {
		t.Fatalf("mysql event must not carry a PG LSN")
	}
	if len(e.Columns) != 2 || e.Columns[0].Name != "id" || !e.Columns[0].IsKey {
		t.Fatalf("columns = %+v, want id keyed", e.Columns)
	}
	if e.Columns[1].IsKey {
		t.Fatalf("name column must not be flagged key")
	}
}

func TestRowsEventUpdatePairsBeforeAfter(t *testing.T) {
	ev := &canal.RowsEvent{
		Table:  testTable(),
		Action: canal.UpdateAction,
		Rows:   [][]interface{}{{int64(1), "old"}, {int64(1), "new"}},
		Header: &replication.EventHeader{LogPos: 900, Timestamp: 1700000000},
	}
	out, err := rowsEventToCDCEvents(ev, "mysql-bin.000002")
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("events = %d, want 1 (one pair)", len(out))
	}
	e := out[0]
	if e.Kind != EventUpdate {
		t.Fatalf("kind = %s", e.Kind)
	}
	if len(e.OldColumns) != 2 || e.OldColumns[1].Value != "old" {
		t.Fatalf("old columns = %+v, want before-image", e.OldColumns)
	}
	if e.Columns[1].Value != "new" {
		t.Fatalf("columns = %+v, want after-image", e.Columns)
	}
	if e.Binlog.String() != "mysql-bin.000002:900" {
		t.Fatalf("binlog = %v", e.Binlog)
	}

	// Odd row count (row-image v0) must fail loud, never mis-pair.
	ev.Rows = [][]interface{}{{int64(1), "only"}}
	if _, err := rowsEventToCDCEvents(ev, "f"); err == nil {
		t.Fatalf("odd update rows must fail loud")
	}
}

func TestRowsEventMissingMetaFailsLoud(t *testing.T) {
	ev := &canal.RowsEvent{Action: canal.InsertAction, Rows: [][]interface{}{{int64(1)}}}
	if _, err := rowsEventToCDCEvents(ev, "f"); err == nil {
		t.Fatalf("rows event without table meta must fail loud")
	}
}

func TestCanalHandlerDDLHardStop(t *testing.T) {
	s := testStreamer()
	s.cfg.Database = "db" // the target database — DDL here halts (v1 red line)
	h := &canalHandler{s: s}
	q := &replication.QueryEvent{Schema: []byte("db"), Query: []byte("ALTER TABLE db.t1 ADD COLUMN c INT")}
	err := h.OnDDL(&replication.EventHeader{}, mysql.Position{Name: "mysql-bin.000001", Pos: 42}, q)
	if !errors.Is(err, ErrMySQLDDLUnsupported) {
		t.Fatalf("OnDDL err = %v, want ErrMySQLDDLUnsupported", err)
	}
	if !errors.Is(s.Err(), ErrMySQLDDLUnsupported) {
		t.Fatalf("fatal = %v, want recorded ErrMySQLDDLUnsupported", s.Err())
	}
	// Remediation message must spell out the v1 procedure.
	msg := err.Error()
	for _, want := range []string{"不支持在线 DDL", "重跑全量", "ALTER TABLE db.t1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
	// The hard stop must not regress to a nil error (MS-11e A kept the
	// target-database red line while adding the cross-database ignore).
	if err := h.OnDDL(&replication.EventHeader{}, mysql.Position{}, &replication.QueryEvent{
		Schema: []byte("db"), Query: []byte("DROP TABLE db.t1"),
	}); err == nil || s.Err() == nil {
		t.Fatalf("target-db DDL must keep halting")
	}
}

// TestCanalHandlerDDLOutsideTargetIgnored anchors the MS-11e A filter: DDL
// on a different database (or with an empty schema) cannot affect the
// replicated tables and must be logged-and-ignored — the stream keeps
// flowing and NO fatal is recorded.
func TestCanalHandlerDDLOutsideTargetIgnored(t *testing.T) {
	s := testStreamer()
	s.cfg.Database = "db"
	h := &canalHandler{s: s}

	for _, schema := range []string{"other_db", ""} {
		q := &replication.QueryEvent{Schema: []byte(schema), Query: []byte("ALTER TABLE t ADD COLUMN c INT")}
		if err := h.OnDDL(&replication.EventHeader{}, mysql.Position{Name: "mysql-bin.000001", Pos: 42}, q); err != nil {
			t.Fatalf("OnDDL(schema=%q) = %v, want nil (ignored)", schema, err)
		}
	}
	if s.Err() != nil {
		t.Fatalf("fatal = %v, want nil — cross-database DDL must not halt the stream", s.Err())
	}
	// The stream must still deliver row events afterwards (not closed).
	ev := &canal.RowsEvent{
		Table:  testTable(),
		Action: canal.InsertAction,
		Rows:   [][]interface{}{{int64(1), "a"}},
		Header: &replication.EventHeader{LogPos: 99, Timestamp: 1},
	}
	if err := h.OnRow(ev); err != nil {
		t.Fatalf("OnRow after ignored DDL: %v", err)
	}
	// (no rotate in this fixture — currentFile is empty, so the coordinate
	// renders as ":99"; only the pos matters: the stream still delivers.)
	if got := <-s.events; got.Binlog.String() != ":99" {
		t.Fatalf("row event after ignored DDL = %v", got.Binlog)
	}
}

func TestOneBehindPositionAcrossBoundaries(t *testing.T) {
	s := testStreamer()
	if got := s.CurrentPosition(); got != nil {
		t.Fatalf("pre-start tracker = %v, want nil", got)
	}
	h := &canalHandler{s: s}

	// Rotate advances the file.
	rot := &replication.RotateEvent{}
	rot.NextLogName = []byte("mysql-bin.000007")
	rot.Position = 4
	if err := h.OnRotate(nil, rot); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got := s.CurrentPosition().String(); got != "mysql-bin.000007:4" {
		t.Fatalf("after rotate = %s", got)
	}

	// Row event advances to the event end coordinate.
	ev := &canal.RowsEvent{
		Table:  testTable(),
		Action: canal.InsertAction,
		Rows:   [][]interface{}{{int64(1), "a"}},
		Header: &replication.EventHeader{LogPos: 555, Timestamp: 1},
	}
	if err := h.OnRow(ev); err != nil {
		t.Fatalf("row: %v", err)
	}
	// The delivered event carries the same coordinate as the tracker.
	got := <-s.events
	if got.Binlog.String() != "mysql-bin.000007:555" {
		t.Fatalf("event binlog = %v", got.Binlog)
	}
	if s.CurrentPosition().String() != "mysql-bin.000007:555" {
		t.Fatalf("tracker = %s, want event end coordinate", s.CurrentPosition())
	}

	// XID (commit boundary) advances to the committed coordinate.
	if err := h.OnXID(nil, mysql.Position{Name: "mysql-bin.000007", Pos: 620}); err != nil {
		t.Fatalf("xid: %v", err)
	}
	if s.CurrentPosition().String() != "mysql-bin.000007:620" {
		t.Fatalf("after xid = %s", s.CurrentPosition())
	}
}
