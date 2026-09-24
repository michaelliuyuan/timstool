package webapi

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
)

// F-04 anchors: watermark SQL construction, state-advance semantics, the
// three conflict strategies, and API validation (D4 source restriction +
// identifier allow-list).

func TestIncSelectSQL(t *testing.T) {
	got := incBuildSelectSQL("public", "users", []string{"id", "update_time", "name"}, "update_time", false)
	want := `SELECT "id", "update_time", "name" FROM "public"."users" WHERE "update_time" >= $1 ORDER BY "update_time" LIMIT $2`
	if got != want {
		t.Fatalf("non-strict select =\n%s\nwant\n%s", got, want)
	}
	// Strict mode (opt-in, may lose same-second late rows) must use ">".
	got = incBuildSelectSQL("public", "users", []string{"id"}, "wm", true)
	if !strings.Contains(got, `"wm" > $1`) {
		t.Fatalf("strict select missing > : %s", got)
	}
	// Identifiers containing quotes must be escaped, never inlined raw.
	got = incBuildSelectSQL(`s"x`, `t"y`, []string{`c"z`}, `w"m`, false)
	if strings.Contains(got, `"s"x"`) || !strings.Contains(got, `"s""x"`) {
		t.Fatalf("quote escaping failed: %s", got)
	}
	// The watermark value is ALWAYS a $1 parameter 鈥?no string interpolation
	// of values anywhere in the scan.
	if !strings.Contains(want, "$1") || strings.Count(want, "$1") != 1 {
		t.Fatalf("watermark must be parameterized exactly once: %s", want)
	}
}

func TestIncInsertSQLStrategies(t *testing.T) {
	replace := incBuildInsertSQL("db", "users", []string{"id", "name"}, 2, "replace")
	if !strings.HasPrefix(replace, "REPLACE INTO `db`.`users` (`id`, `name`) VALUES (?, ?), (?, ?)") {
		t.Fatalf("replace SQL = %s", replace)
	}
	ignore := incBuildInsertSQL("db", "users", []string{"id"}, 1, "ignore")
	if !strings.HasPrefix(ignore, "INSERT IGNORE INTO `db`.`users` (`id`) VALUES (?)") {
		t.Fatalf("ignore SQL = %s", ignore)
	}
	plain := incBuildInsertSQL("db", "users", []string{"id"}, 1, "error")
	if !strings.HasPrefix(plain, "INSERT INTO `db`.`users` (`id`) VALUES (?)") {
		t.Fatalf("error-strategy SQL = %s", plain)
	}
}

func TestIncAdvanceWatermark(t *testing.T) {
	// Zero rows: never move the watermark (a MAX query would race ahead of
	// in-flight writes).
	if got := incAdvanceWatermark("2026-01-01", "2026-06-01", 0); got != "2026-01-01" {
		t.Fatalf("zero rows must not advance: %s", got)
	}
	// Rows synced: the stream's MAX (last row of the ordered scan) wins.
	if got := incAdvanceWatermark("2026-01-01", "2026-06-01", 42); got != "2026-06-01" {
		t.Fatalf("rows synced must advance to stream max: %s", got)
	}
	// Boundary re-read: default >= semantics re-scan the boundary watermark
	// every run 鈥?same-second late rows are picked up, at the cost of
	// re-reading rows exactly at the watermark (anchored by the >= in the
	// select test above).
}

func TestIncIdentifierAllowList(t *testing.T) {
	for _, ok := range []string{"users", "t1", "_x", "A_b_9"} {
		if !incIdentifierOK(ok) {
			t.Errorf("identifier %q must be accepted", ok)
		}
	}
	for _, bad := range []string{"users; DROP TABLE x", `a"b`, "a b", "1abc", "a-b", "", "a.b"} {
		if incIdentifierOK(bad) {
			t.Errorf("identifier %q must be rejected", bad)
		}
	}
}

func TestIncWatermarkTypeWhitelist(t *testing.T) {
	for _, ok := range []string{"timestamp with time zone", "timestamp without time zone", "date", "integer", "bigint"} {
		if !incWatermarkTypes[ok] {
			t.Errorf("type %q must be whitelisted", ok)
		}
	}
	for _, bad := range []string{"text", "numeric", "jsonb", "boolean", "uuid"} {
		if incWatermarkTypes[bad] {
			t.Errorf("type %q must NOT be whitelisted", bad)
		}
	}
}

// TestIncrementalJobAPIValidation covers the D4 source restriction, target
// type check, identifier/strategy/batch validation, and the create/update/
// delete lifecycle. No live DB needed: validation happens before any connect.
func TestIncrementalJobAPIValidation(t *testing.T) {
	s, _ := newTestServer(t)

	// One postgres source + one tidb target registered.
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "inc-src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "inc-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)
	// And a tidb "source" to prove the D4 rejection.
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "inc-bad", "type": "mysql",
		"fields": {"host": "10.0.0.2", "port": 3306, "user": "u", "password": "pw", "database": "db3"}
	}`)
	s.handleCreateDataSource(w, req)
	mysqlID := dsBody(t, w)["id"].(string)

	validBody := func(src, strategy, table, wmCol string) string {
		return `{"name": "j1", "source_ref": "` + src + `", "target_ref": "` + tgtID +
			`", "batch_size": 500, "conflict_strategy": "` + strategy +
			`", "tables": [{"table": "` + table + `", "watermark_column": "` + wmCol + `", "initial_watermark": ""}]}`
	}

	// D4: non-postgres source rejected with 400.
	w, req = doReq("POST", "/api/v1/incremental/jobs", validBody(mysqlID, "replace", "users", "update_time"))
	s.handleCreateIncrementalJob(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "PostgreSQL") {
		t.Fatalf("mysql source must be rejected 400: %d %s", w.Code, w.Body.String())
	}

	// Bad conflict strategy / bad identifiers / bad batch size 鈫?400.
	for _, body := range []string{
		validBody(srcID, "truncate", "users", "update_time"),
		validBody(srcID, "replace", "users; DROP TABLE x", "update_time"),
		validBody(srcID, "replace", "users", "col-from-(select)"),
	} {
		w, req = doReq("POST", "/api/v1/incremental/jobs", body)
		s.handleCreateIncrementalJob(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid job accepted: %d %s (%s)", w.Code, w.Body.String(), body)
		}
	}
	w, req = doReq("POST", "/api/v1/incremental/jobs",
		`{"name": "j2", "source_ref": "`+srcID+`", "target_ref": "`+tgtID+`", "batch_size": 0, "conflict_strategy": "replace", "tables": []}`)
	s.handleCreateIncrementalJob(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty tables / bad batch must be 400, got %d", w.Code)
	}

	// Happy path: create 鈫?list 鈫?update (config edit keeps state) 鈫?delete.
	w, req = doReq("POST", "/api/v1/incremental/jobs", validBody(srcID, "replace", "users", "update_time"))
	s.handleCreateIncrementalJob(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := dsBody(t, w)["id"].(string)

	// Simulate runtime state, then update the config: the state must survive.
	incMu.Lock()
	list := s.loadIncrementalJobs()
	for i := range list {
		if list[i].ID == id {
			list[i].States["users"] = &incTableState{LastWatermark: "2026-01-01", TotalRows: 7}
		}
	}
	if err := s.saveIncrementalJobs(list); err != nil {
		t.Fatal(err)
	}
	incMu.Unlock()

	// D4 gate on UPDATE too (v2): PUT must not swap refs past the type checks.
	w, req = doReq("PUT", "/api/v1/incremental/jobs/"+id, validBody(mysqlID, "replace", "users", "update_time"))
	req = withChiParam(req, "id", id)
	s.handleUpdateIncrementalJob(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "PostgreSQL") {
		t.Fatalf("PUT with mysql source must be rejected 400: %d %s", w.Code, w.Body.String())
	}
	w, req = doReq("PUT", "/api/v1/incremental/jobs/"+id, `{"name": "j1", "source_ref": "`+srcID+
		`", "target_ref": "`+mysqlID+`", "batch_size": 500, "conflict_strategy": "replace", "tables": [{"table": "users", "watermark_column": "update_time"}]}`)
	req = withChiParam(req, "id", id)
	s.handleUpdateIncrementalJob(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "TiDB") {
		t.Fatalf("PUT with mysql target must be rejected 400: %d %s", w.Code, w.Body.String())
	}

	w, req = doReq("PUT", "/api/v1/incremental/jobs/"+id, validBody(srcID, "ignore", "users", "other_col"))
	req = withChiParam(req, "id", id)
	s.handleUpdateIncrementalJob(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	incMu.Lock()
	list = s.loadIncrementalJobs()
	incMu.Unlock()
	var found *incJob
	for i := range list {
		if list[i].ID == id {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatal("job vanished after update")
	}
	if st := found.States["users"]; st == nil || st.LastWatermark != "2026-01-01" || st.TotalRows != 7 {
		t.Fatalf("config edit must not reset table state: %+v", found.States)
	}

	w, req = doReq("DELETE", "/api/v1/incremental/jobs/"+id, "")
	req = withChiParam(req, "id", id)
	s.handleDeleteIncrementalJob(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	incMu.Lock()
	list = s.loadIncrementalJobs()
	incMu.Unlock()
	for _, e := range list {
		if e.ID == id {
			t.Fatal("job still present after delete")
		}
	}
}

// --- F-04 v2: same-value saturation (Blocker A) anchors ---

func TestIncDrainAndJumpSQL(t *testing.T) {
	got := incBuildDrainSQL("public", "users", []string{"id", "update_time"}, "update_time")
	want := `SELECT "id", "update_time" FROM "public"."users" WHERE "update_time" = $1`
	if got != want {
		t.Fatalf("drain SQL =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "LIMIT") {
		t.Fatalf("drain scan must be unbounded (streamed): %s", got)
	}
	jump := incBuildNextWatermarkSQL("public", "users", "update_time")
	wantJump := `SELECT MIN("update_time") FROM "public"."users" WHERE "update_time" > $1`
	if jump != wantJump {
		t.Fatalf("jump SQL =\n%s\nwant\n%s", jump, wantJump)
	}
}

func TestIncCursorStep(t *testing.T) {
	// Empty batch: keep the cursor, done.
	if wm, sat, done := incCursorStep("w0", "", 0, 100); wm != "w0" || sat || !done {
		t.Fatalf("empty batch: %q %v %v", wm, sat, done)
	}
	// Partial batch: advance to stream max, done.
	if wm, sat, done := incCursorStep("w0", "w5", 40, 100); wm != "w5" || sat || !done {
		t.Fatalf("partial batch: %q %v %v", wm, sat, done)
	}
	// Full batch, max moved: advance, continue keyset.
	if wm, sat, done := incCursorStep("w0", "w9", 100, 100); wm != "w9" || sat || done {
		t.Fatalf("advancing full batch: %q %v %v", wm, sat, done)
	}
	// Full batch ENTIRELY at the entry watermark: saturation 鈥?the livelock
	// case (same-second bulk INSERT) that MUST go to drain, not a retry.
	if wm, sat, done := incCursorStep("w0", "w0", 100, 100); wm != "w0" || !sat || done {
		t.Fatalf("saturation: %q %v %v", wm, sat, done)
	}
}

func TestIncJumpAfterDrain(t *testing.T) {
	if wm, done := incJumpAfterDrain(sql.NullString{Valid: false}); !done || wm != "" {
		t.Fatalf("NULL next 鈬?done: %q %v", wm, done)
	}
	if wm, done := incJumpAfterDrain(sql.NullString{String: ""}); !done || wm != "" {
		t.Fatalf("empty next 鈬?done: %q %v", wm, done)
	}
	if wm, done := incJumpAfterDrain(sql.NullString{String: "w7", Valid: true}); done || wm != "w7" {
		t.Fatalf("value next 鈬?continue at w7: %q %v", wm, done)
	}
}

// fakeWMTable models a source table as a sorted list of watermark values; the
// fetch/min probes mirror the exact SQL semantics the engine issues.
type fakeWMTable struct {
	rows []string // sorted ascending
}

func (f *fakeWMTable) fetch(wm string, limit int, strict bool) []string {
	op := func(v string) bool {
		if strict {
			return v > wm
		}
		return v >= wm
	}
	out := []string{}
	for _, v := range f.rows {
		if op(v) && len(out) < limit {
			out = append(out, v)
		}
	}
	return out
}

func (f *fakeWMTable) fetchEq(wm string) []string {
	out := []string{}
	for _, v := range f.rows {
		if v == wm {
			out = append(out, v)
		}
	}
	return out
}

func (f *fakeWMTable) minAbove(wm string) sql.NullString {
	for _, v := range f.rows {
		if v > wm {
			return sql.NullString{String: v, Valid: true}
		}
	}
	return sql.NullString{}
}

// runLoopModel replays syncOneTable's cursor driver over a fake table using
// the PRODUCTION decision helpers (incCursorStep / incJumpAfterDrain) 鈥?the
// loop skeleton mirrors the engine's, so a livelock here means one there.
func runLoopModel(t *testing.T, tbl *fakeWMTable, startWM string, batchSize int, strict bool) (written int, finalWM string) {
	t.Helper()
	const maxIters = 10000
	// Mirrors the engine: an empty starting watermark derives the cursor from
	// MIN(col) — strict jobs then get a >= first scan (adversarial ①, v2);
	// a >= scan also follows every drain jump (the jumped-to value's rows are
	// unconsumed — a strict > would skip them all).
	minDerived := startWM == ""
	geScan := minDerived
	wm, lastWM, written := startWM, startWM, 0
	for i := 0; ; i++ {
		if i > maxIters {
			t.Fatal("loop model livelocked (did not terminate)")
		}
		entryWM := wm
		if entryWM == "" {
			// MIN(col) probe on the fake table.
			if len(tbl.rows) == 0 {
				break
			}
			entryWM = tbl.rows[0]
			wm = entryWM
		}
		batch := tbl.fetch(entryWM, batchSize, strict && !geScan)
		if len(batch) == 0 {
			break
		}
		written += len(batch)
		lastWM = batch[len(batch)-1]
		next, saturated, done := incCursorStep(entryWM, lastWM, len(batch), batchSize)
		if saturated {
			// Drain: rewrite ALL rows at this value in chunks, then jump.
			// (REPLACE/IGNORE idempotent; the engine's error strategy would
			// surface a duplicate-key here 鈥?deterministic, not a livelock.)
			drain := tbl.fetchEq(lastWM)
			written += len(drain)
			jump, tableDone := incJumpAfterDrain(tbl.minAbove(lastWM))
			if tableDone {
				return written, lastWM
			}
			wm = jump
			geScan = true
			continue
		}
		geScan = false
		wm = next
		if done {
			break
		}
	}
	return written, lastWM
}

func TestIncLoopSameValue2xBatch(t *testing.T) {
	// Same value with 2脳BatchSize rows, plus one later value: pre-fix this
	// livelocked forever; now it completes, writes every row (via drain) and
	// the watermark jumps past the saturated value.
	rows := []string{}
	for i := 0; i < 20; i++ {
		rows = append(rows, "w1")
	}
	rows = append(rows, "w2", "w2")
	tbl := &fakeWMTable{rows: rows}
	written, wm := runLoopModel(t, tbl, "w0", 10, false)
	if wm != "w2" {
		t.Fatalf("watermark must jump to w2, got %q", wm)
	}
	// b1 (>=w0, 10×w1) + b2 (>=w1 re-reads the same 10 — the livelock batch,
	// now detected as saturation) + drain rewrites all 20 at w1 + w2 tail.
	if written != 10+10+20+2 {
		t.Fatalf("written = %d, want 42 (two keyset batches + full drain + tail)", written)
	}
}

func TestIncLoopWholeTableSameValue(t *testing.T) {
	rows := []string{}
	for i := 0; i < 35; i++ {
		rows = append(rows, "w9")
	}
	written, wm := runLoopModel(t, &fakeWMTable{rows: rows}, "", 10, false)
	if wm != "w9" || written != 10+35 {
		t.Fatalf("whole-table same value: wm=%q written=%d (want w9, 45)", wm, written)
	}
}

func TestIncLoopStrictMINBackfill(t *testing.T) {
	// Strict mode + empty initial watermark: the MIN-derived first scan must
	// be >= (all MIN rows arrive via the saturation drain), and the
	// post-jump scan must also be >= (the w2 row is unconsumed — a strict >
	// there would drop it entirely).
	rows := []string{}
	for i := 0; i < 25; i++ {
		rows = append(rows, "w1")
	}
	rows = append(rows, "w2")
	written, wm := runLoopModel(t, &fakeWMTable{rows: rows}, "", 10, true)
	if wm != "w2" || written != 10+25+1 {
		t.Fatalf("strict MIN backfill: wm=%q written=%d (want w2, 36 — zero rows lost)", wm, written)
	}
}

func TestIncLoopSaturationAtLastValue(t *testing.T) {
	// The saturated value is the table's maximum: drain finds no value above
	// 鈬?table complete, watermark = the saturated value, no extra query loop.
	rows := []string{}
	for i := 0; i < 15; i++ {
		rows = append(rows, "w1")
	}
	rows = append(rows, "w5")
	for i := 0; i < 12; i++ {
		rows = append(rows, "w7") // 12 > batchSize 鈬?saturation at max value
	}
	written, wm := runLoopModel(t, &fakeWMTable{rows: rows}, "w0", 10, false)
	if wm != "w7" || written != 10+10+15+10+10+12 {
		t.Fatalf("saturation at last value: wm=%q written=%d (want w7, 67)", wm, written)
	}
}

func TestIncLoopMixedRegular(t *testing.T) {
	// Ordinary mixed values, no saturation: identical outcome to the pre-fix
	// engine (regression anchor 鈥?the fix must not change normal behavior).
	tbl := &fakeWMTable{rows: []string{"w1", "w2", "w2", "w3", "w4", "w4", "w4"}}
	written, wm := runLoopModel(t, tbl, "w0", 3, false)
	// >= boundary re-reads make every value's first row appear twice; the
	// final w4×3 exactly fills a batch ⇒ one drain. Same shape as pre-fix,
	// minus the livelock.
	if wm != "w4" || written != 3*4+3 {
		t.Fatalf("mixed: wm=%q written=%d (want w4, 15)", wm, written)
	}
}

func TestIncLoopStrictUnchanged(t *testing.T) {
	// Strict mode (">") can never saturate: every fetched row is strictly
	// above the cursor, so lastWM != entryWM by construction.
	rows := []string{}
	for i := 0; i < 25; i++ {
		rows = append(rows, "w1")
	}
	rows = append(rows, "w2")
	written, wm := runLoopModel(t, &fakeWMTable{rows: rows}, "w0", 10, true)
	// Strict tradeoff (documented, opt-in): after advancing past "w1" the
	// remaining same-value rows are skipped 鈥?10 (first batch) + "w2" only.
	if wm != "w2" || written != 11 {
		t.Fatalf("strict: wm=%q written=%d (want w2, 11)", wm, written)
	}
}
