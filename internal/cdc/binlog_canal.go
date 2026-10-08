package cdc

// binlog_canal.go — MS-11 pen 2: the MySQL binlog collection layer on top of
// go-mysql canal (ruling seq 955, option A with three hard conditions):
//
//   1. Position truth = OUR checkpoint. canal v1.11's master info is
//      memory-only (no disk persistence), and we never read positions back
//      from canal — CurrentPosition reports what this streamer observed.
//   2. canal types stay inside this adapter file. The contract surface is
//      binlogStreamer/CDCEvent (source-neutral) — no canal type leaks.
//   3. Fail-loud error mapping: canal auto-retry is disabled
//      (DisableRetrySync) and DiscardNoMetaRowEvent stays false, so
//      disconnect / permission / missing-table-schema errors surface as a
//      fatal stream error, never a silent skip.
//
// v1 hard semantics (ruling seq 953 #2): a binlog Query(DDL) event on the
// TARGET database halts the task with an explicit remediation message —
// silent DDL skipping is forbidden. DDL outside the target database is
// logged and ignored (MS-11e A).

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"go.uber.org/zap"
)

// ErrMySQLDDLUnsupported is the v1 hard-stop error for online DDL. The task
// must transition to error state with this message (no silent skip). The
// DDL statement is appended by ddlUnsupportedError's %w wrap — this
// sentinel itself carries no format placeholder (MS-11e C: a bare %s here
// would render literally, since the wrap layer does not re-format).
var ErrMySQLDDLUnsupported = fmt.Errorf(
	"MySQL CDC v1 不支持在线 DDL（binlog Query 事件）：请停止 CDC 链 → 在源/目标执行 DDL → 重跑全量后重建链。GTID 与在线 DDL 支持为 v2 候选")

// ddlUnsupportedError wraps the DDL statement into the hard-stop error.
func ddlUnsupportedError(query string) error {
	return fmt.Errorf("%w: %s", ErrMySQLDDLUnsupported, query)
}

// canalStreamer implements binlogStreamer over canal.
type canalStreamer struct {
	cfg BinlogSourceConfig
	log *zap.Logger

	events chan *CDCEvent
	done   chan struct{}

	canal *canal.Canal

	// curFile is the binlog file the stream is currently reading (advanced on
	// Rotate). curMu also guards the one-behind delivered position.
	curMu   sync.Mutex
	curFile string
	lastPos uint32

	errMu sync.Mutex
	fatal error
}

// binlogTableFilters compiles the table include/exclude lists into canal
// regex form ("^db\\.table$"). Empty include = all tables.
func binlogTableFilters(tables, exclude []string) (inc, exc []string, err error) {
	for _, t := range tables {
		if _, e := regexp.Compile(t); e != nil {
			return nil, nil, fmt.Errorf("binlog include table %q: %w", t, e)
		}
		inc = append(inc, "^"+t+"$")
	}
	for _, t := range exclude {
		if _, e := regexp.Compile(t); e != nil {
			return nil, nil, fmt.Errorf("binlog exclude table %q: %w", t, e)
		}
		exc = append(exc, "^"+t+"$")
	}
	return inc, exc, nil
}

func newCanalStreamer(cfg BinlogSourceConfig) (*canalStreamer, error) {
	inc, exc, err := binlogTableFilters(cfg.Tables, cfg.ExcludeTables)
	if err != nil {
		return nil, err
	}
	if cfg.ServerID == 0 {
		return nil, fmt.Errorf("binlog source: ServerID must be non-zero (unique in the replication topology)")
	}

	ccfg := canal.NewDefaultConfig()
	ccfg.Addr = cfg.Host + ":" + strconv.Itoa(cfg.Port)
	ccfg.User = cfg.User
	ccfg.Password = cfg.Password
	ccfg.ServerID = cfg.ServerID
	ccfg.Flavor = mysql.MySQLFlavor
	ccfg.Dump.ExecutionPath = "" // binlog only — never mysqldump
	ccfg.IncludeTableRegex = inc
	ccfg.ExcludeTableRegex = exc
	ccfg.DisableRetrySync = true       // fail-loud: no silent reconnect looping (hard condition 3)
	ccfg.DiscardNoMetaRowEvent = false // missing table schema = error, not skip

	c, err := canal.NewCanal(ccfg)
	if err != nil {
		return nil, fmt.Errorf("binlog source: canal init: %w", err)
	}

	s := &canalStreamer{
		cfg:    cfg,
		log:    zap.NewNop(),
		events: make(chan *CDCEvent, 4096),
		done:   make(chan struct{}),
		canal:  c,
	}
	c.SetEventHandler(&canalHandler{s: s})
	return s, nil
}

func (s *canalStreamer) SetLogger(log *zap.Logger) { s.log = log }

// masterPosition reads the current master binlog coordinate (chain-seed
// equivalent of PG's consistent-point, ruling seq 953).
func (s *canalStreamer) masterPosition() (*BinlogPosition, error) {
	rr, err := s.canal.Execute("SHOW MASTER STATUS")
	if err != nil {
		return nil, fmt.Errorf("binlog source: SHOW MASTER STATUS: %w", err)
	}
	file, _ := rr.GetString(0, 0)
	pos, _ := rr.GetString(0, 1)
	off, err := strconv.ParseUint(pos, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("binlog source: master position %q: %w", pos, err)
	}
	return &BinlogPosition{File: file, Pos: uint32(off)}, nil
}

// Start begins the binlog dump from the given position (nil = current master
// position, i.e. a fresh chain start) and returns the event stream.
func (s *canalStreamer) Start(ctx context.Context, from *BinlogPosition) (<-chan *CDCEvent, error) {
	if from == nil {
		p, err := s.masterPosition()
		if err != nil {
			return nil, err
		}
		from = p
	}
	s.curMu.Lock()
	s.curFile = from.File
	s.lastPos = from.Pos
	s.curMu.Unlock()

	pos := mysql.Position{Name: from.File, Pos: from.Pos}
	go func() {
		defer close(s.done)
		// Closing the events channel when the stream ends (fatal or clean)
		// lets the applier drain and exit — mirrors the PG source's
		// streamLoop exit semantics; the runner then surfaces streamer.Err().
		defer close(s.events)
		s.log.Info("binlog source: dumping from", zap.String("position", from.String()))
		if err := s.canal.RunFrom(pos); err != nil {
			s.setFatal(fmt.Errorf("binlog source: stream stopped: %w", err))
		}
	}()
	return s.events, nil
}

// CurrentPosition returns the one-behind delivered position: the end
// coordinate of the last row event pushed onto the channel (mirrors the PG
// source's "only positions whose events have been delivered may be
// reported" rule).
func (s *canalStreamer) CurrentPosition() *BinlogPosition {
	s.curMu.Lock()
	defer s.curMu.Unlock()
	if s.curFile == "" {
		return nil
	}
	return &BinlogPosition{File: s.curFile, Pos: s.lastPos}
}

func (s *canalStreamer) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.fatal
}

func (s *canalStreamer) Close() error {
	s.canal.Close()
	return nil
}

func (s *canalStreamer) setFatal(err error) {
	s.errMu.Lock()
	if s.fatal == nil {
		s.fatal = err
	}
	s.errMu.Unlock()
	s.log.Error("binlog source: fatal", zap.Error(err))
}

func (s *canalStreamer) advance(file string, pos uint32) {
	s.curMu.Lock()
	s.curFile = file
	s.lastPos = pos
	s.curMu.Unlock()
}

// canalHandler adapts canal's EventHandler onto the streamer. All canal
// types live here (hard condition 2).
type canalHandler struct {
	s *canalStreamer
}

func (h *canalHandler) String() string { return "timstool-binlog" }

func (h *canalHandler) OnRotate(header *replication.EventHeader, rotateEvent *replication.RotateEvent) error {
	// nextPos names the file the stream rotates to.
	h.s.advance(string(rotateEvent.NextLogName), uint32(rotateEvent.Position))
	return nil
}

func (h *canalHandler) OnTableChanged(header *replication.EventHeader, schemaName, table string) error {
	// Schema cache invalidation is canal's job; nothing to do. A DDL is about
	// to be delivered via OnDDL, which hard-stops v1.
	return nil
}

// database halts the stream with an explicit remediation error (ruling seq
// 953 #2 — silent DDL skipping is forbidden). A DDL outside the target
// database (or with an empty schema) cannot affect the replicated tables:
// log it and keep streaming (MS-11e A — cross-database noise used to halt
// healthy chains). MS-11e pen7: the comparison is case-insensitive
// (EqualFold, fail-closed) — under lower_case_table_names=1 the binlog
// schema is always the folded storage name, so a byte-exact compare let a
// mixed-case cfg.Database silently IGNORE target-database DDL (red-line
// fail-open); EqualFold's worst case is a symmetric false halt, which is
// the safe direction. MS-11e pen8: before ignoring, the statement itself
// is coarsely scanned for a target-qualified object name (`tgt`.`t` /
// tgt.t) — a fully-qualified DDL executed with no default database (or
// defaulted elsewhere) carries schema=""/other in the binlog, and without
// this scan the empty-schema ignore branch silently skipped
// TARGET-database DDL (te black-box red #1). Coarse match errs toward
// halting — fail-closed, same direction as EqualFold.
func (h *canalHandler) OnDDL(header *replication.EventHeader, nextPos mysql.Position, queryEvent *replication.QueryEvent) error {
	schema := string(queryEvent.Schema)
	target := h.s.cfg.Database
	query := string(queryEvent.Query)
	if schema == "" || !strings.EqualFold(schema, target) {
		if ddlMentionsTargetDB(query, target) {
			err := ddlUnsupportedError(query)
			h.s.setFatal(err)
			return err
		}
		h.s.log.Info("binlog source: DDL outside target database ignored",
			zap.String("schema", schema),
			zap.String("target", target),
			zap.String("query", ddlQuerySummary(query)))
		return nil
	}
	err := ddlUnsupportedError(query)
	h.s.setFatal(err)
	return err
}

// ddlTableKind matches the table-object DDL verbs whose statements may
// carry qualified table references (pen8 ruling: the coarse scan is gated
// on this kind set so admin DDL like ALTER USER keeps the plain ignore).
var ddlTableKind = regexp.MustCompile(`(?i)\b(ALTER\s+TABLE|CREATE\s+(TABLE|UNIQUE\s+INDEX|INDEX)|DROP\s+(TABLE|INDEX)|TRUNCATE(\s+TABLE)?|RENAME\s+TABLE)\b`)

// ddlMentionsTargetDB reports whether a TABLE-kind DDL text carries a
// target-db-qualified object reference (tgt.t / `tgt`.`t`, anywhere in the
// statement — RENAME ... TO tgt.b and CREATE INDEX ... ON tgt.t included).
// Coarse by design: a false positive halts with remediation wording (safe),
// a false negative needs a statement that modifies the target db without
// ever naming it (impossible for table DDL).
func ddlMentionsTargetDB(query, target string) bool {
	if target == "" || query == "" {
		return false
	}
	if !ddlTableKind.MatchString(query) {
		return false
	}
	// Accept `tgt`. / tgt. (backticks optional on either side, gap-tolerant);
	// the leading boundary stops my_tgt/xdb substrings from matching.
	q := regexp.QuoteMeta(target)
	re := regexp.MustCompile("(?i)(^|[^0-9A-Za-z_$\\x{0080}-\\x{ffff}])`?" + q + "`?\\s*\\.")
	return re.MatchString(query)
}

// ddlQuerySummary truncates a DDL statement for logs (full text stays in
// the hard-stop error when it matters).
func ddlQuerySummary(query string) string {
	const max = 120
	if len(query) <= max {
		return query
	}
	return query[:max] + "...(truncated)"
}

func (h *canalHandler) OnXID(header *replication.EventHeader, nextPos mysql.Position) error {
	// Transaction boundary: the committed coordinate is a safe checkpoint
	// candidate. The one-behind delivered position advances here too.
	h.s.advance(nextPos.Name, nextPos.Pos)
	return nil
}

func (h *canalHandler) OnGTID(header *replication.EventHeader, gtidEvent mysql.BinlogGTIDEvent) error {
	return nil // v1 is file:pos; GTID is a v2 candidate
}

func (h *canalHandler) OnRowsQueryEvent(e *replication.RowsQueryEvent) error {
	return nil
}

func (h *canalHandler) OnPosSynced(header *replication.EventHeader, pos mysql.Position, set mysql.GTIDSet, force bool) error {
	return nil // canal-side persistence is disabled/not used (hard condition 1)
}

func (h *canalHandler) OnRow(e *canal.RowsEvent) error {
	events, err := rowsEventToCDCEvents(e, h.s.currentFile())
	if err != nil {
		h.s.setFatal(err)
		return err
	}
	for _, ev := range events {
		select {
		case h.s.events <- ev:
		case <-h.s.done:
			return fmt.Errorf("binlog source: streamer closed")
		}
	}
	// Row event end coordinate = one-behind delivered position.
	h.s.advance(h.s.currentFile(), e.Header.LogPos)
	return nil
}

func (s *canalStreamer) currentFile() string {
	s.curMu.Lock()
	defer s.curMu.Unlock()
	return s.curFile
}

// rowsEventToCDCEvents maps a canal RowsEvent onto source-neutral CDCEvents.
// Update rows come in [before, after] pairs; insert/delete one row each.
// Column values are canal-decoded (schema-aware); keys are flagged from the
// table's PK columns.
func rowsEventToCDCEvents(e *canal.RowsEvent, file string) ([]*CDCEvent, error) {
	if e.Table == nil {
		return nil, fmt.Errorf("binlog source: rows event without table meta (schema fetch failed)")
	}
	if e.Header == nil {
		return nil, fmt.Errorf("binlog source: rows event without header")
	}
	pos := &BinlogPosition{File: file, Pos: e.Header.LogPos}
	ts := time.Unix(int64(e.Header.Timestamp), 0)
	if e.Header.Timestamp == 0 {
		ts = time.Now()
	}

	keySet := make(map[int]bool, len(e.Table.PKColumns))
	for _, i := range e.Table.PKColumns {
		keySet[i] = true
	}
	col := func(i int, v interface{}) ColumnValue {
		name := ""
		if i < len(e.Table.Columns) {
			name = e.Table.Columns[i].Name
		}
		return ColumnValue{Name: name, Value: v, IsKey: keySet[i]}
	}
	cols := func(row []interface{}) []ColumnValue {
		out := make([]ColumnValue, len(row))
		for i, v := range row {
			out[i] = col(i, v)
		}
		return out
	}

	var out []*CDCEvent
	switch e.Action {
	case canal.InsertAction:
		for _, row := range e.Rows {
			out = append(out, &CDCEvent{
				Timestamp: ts, Kind: EventInsert, Schema: e.Table.Schema, Table: e.Table.Name,
				Columns: cols(row),
				Binlog:  pos,
			})
		}
	case canal.DeleteAction:
		for _, row := range e.Rows {
			out = append(out, &CDCEvent{
				Timestamp: ts, Kind: EventDelete, Schema: e.Table.Schema, Table: e.Table.Name,
				Columns: cols(row),
				Binlog:  pos,
			})
		}
	case canal.UpdateAction:
		if len(e.Rows)%2 != 0 {
			return nil, fmt.Errorf("binlog source: update rows event with odd row count (%d) — row-image v1/v2 required", len(e.Rows))
		}
		for i := 0; i < len(e.Rows); i += 2 {
			out = append(out, &CDCEvent{
				Timestamp: ts, Kind: EventUpdate, Schema: e.Table.Schema, Table: e.Table.Name,
				Columns:    cols(e.Rows[i+1]),
				OldColumns: cols(e.Rows[i]),
				Binlog:     pos,
			})
		}
	default:
		return nil, fmt.Errorf("binlog source: unknown rows action %q", e.Action)
	}
	return out, nil
}

// defaultBinlogSource wires the pen-1 seam to the canal adapter (pen 2).
var defaultBinlogSource = func(cfg BinlogSourceConfig) (binlogStreamer, error) {
	return newCanalStreamer(cfg)
}

func init() { newBinlogSource = defaultBinlogSource }
