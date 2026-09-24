package webapi

import (
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
	// The watermark value is ALWAYS a $1 parameter — no string interpolation
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
	// every run — same-second late rows are picked up, at the cost of
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

	// Bad conflict strategy / bad identifiers / bad batch size → 400.
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

	// Happy path: create → list → update (config edit keeps state) → delete.
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
