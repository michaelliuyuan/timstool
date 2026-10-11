package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
)

// TestTaskPhases_SchemaPhaseReporting anchors the schema-phase progress fix:
// with per-table SchemaState marks in the checkpoint, the schema phase
// reports its own tables_total/tables_done + per-table list, while the data
// phase stays empty until the schema phase is over (no 0/N preview from
// schema-registered entries).
// TestCreateTaskRejectsTablesExcludeIntersection anchors BUG-0930 commit 2:
// a table present in BOTH tables and exclude_tables is a contradictory
// config (data-side include would win while schema DDL is excluded →
// mid-run failure) and must be rejected 400 at creation.
func TestCreateTaskRejectsTablesExcludeIntersection(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "ix-src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "ix-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)

	newTask := func(opts string) *httptest.ResponseRecorder {
		w, req := doReq("POST", "/api/v1/tasks", fmt.Sprintf(`{"name": "ix", "source_ref": %q, "target_ref": %q, "opts": %s}`, srcID, tgtID, opts))
		s.handleCreateTask(w, req)
		return w
	}

	// Overlap → 400.
	if w := newTask(`{"tables": ["a", "b"], "exclude_tables": ["b", "c"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("overlap config = %d %s, want 400", w.Code, w.Body.String())
	}
	// Disjoint sets still create fine.
	if w := newTask(`{"tables": ["a"], "exclude_tables": ["b"]}`); w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("disjoint config = %d %s, want 2xx", w.Code, w.Body.String())
	}
}

func TestTaskPhases_SchemaPhaseReporting(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "sch-src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "sch-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/tasks", fmt.Sprintf(`{"name": "sch", "source_ref": %q, "target_ref": %q, "opts": {}}`, srcID, tgtID))
	s.handleCreateTask(w, req)
	taskID := dsBody(t, w)["id"].(string)

	t.Chdir(t.TempDir())
	cpMgr, err := checkpoint.NewManager(filepath.Join(".checkpoint", taskID))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Mid-schema state: 3 registered, 2 applied, 1 still pending; plus one
	// stale data entry from a PREVIOUS run (completed, 5000 rows) that must
	// not leak into the schema counters.
	if err := cpMgr.RegisterSchemaTables([]string{"t1", "t2", "t3"}); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"t1", "t2"} {
		if err := cpMgr.MarkSchemaTableCompleted(tbl); err != nil {
			t.Fatal(err)
		}
	}
	if tc := cpMgr.GetOrCreateTable("stale", 5000); tc == nil {
		t.Fatal("GetOrCreateTable stale")
	}
	if err := cpMgr.MarkTableCompleted("stale", 5000); err != nil {
		t.Fatal(err)
	}
	if err := cpMgr.SetPhase("schema"); err != nil {
		t.Fatal(err)
	}
	cpMgr.Flush()

	w, req = doReq("GET", "/api/v1/tasks/"+taskID+"/phases", "")
	req = withChiParam(req, "taskID", taskID)
	s.handleTaskPhases(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("phases: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Phases []struct {
			Name       string                   `json:"name"`
			TableCount int                      `json:"table_count"`
			TablesDone int                      `json:"tables_done"`
			RowsTotal  int64                    `json:"rows_total"`
			Tables     []map[string]interface{} `json:"tables"`
		} `json:"phases"`
	}
	if err := json.Unmarshal([]byte(w.Body.String()), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var schemaPhase, dataPhase *struct {
		Name       string                   `json:"name"`
		TableCount int                      `json:"table_count"`
		TablesDone int                      `json:"tables_done"`
		RowsTotal  int64                    `json:"rows_total"`
		Tables     []map[string]interface{} `json:"tables"`
	}
	for i := range body.Phases {
		switch body.Phases[i].Name {
		case "schema":
			schemaPhase = &body.Phases[i]
		case "data":
			dataPhase = &body.Phases[i]
		}
	}
	if schemaPhase == nil || dataPhase == nil {
		t.Fatalf("phases missing: %+v", body.Phases)
	}
	if schemaPhase.TableCount != 3 || schemaPhase.TablesDone != 2 {
		t.Errorf("schema phase = %d/%d, want 2/3", schemaPhase.TablesDone, schemaPhase.TableCount)
	}
	if len(schemaPhase.Tables) != 3 {
		t.Errorf("schema tables list len = %d, want 3", len(schemaPhase.Tables))
	}
	if dataPhase.TableCount != 0 {
		t.Errorf("data phase table_count = %d during schema phase, want 0 (no preview)", dataPhase.TableCount)
	}
}
