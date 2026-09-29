package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
)

// createPhaseTestTask provisions a datasource pair + task and returns the
// server, task id (cwd is switched to a temp dir by the caller).
func createPhaseTestTask(t *testing.T, name string) (*Server, string) {
	t.Helper()
	s, _ := newTestServer(t)

	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "pm-src", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "pm-tgt", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/tasks", fmt.Sprintf(`{"name": %q, "source_ref": %q, "target_ref": %q, "opts": {}}`, name, srcID, tgtID))
	s.handleCreateTask(w, req)
	return s, dsBody(t, w)["id"].(string)
}

type phaseAPI struct {
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	SubLabel string  `json:"sub_label"`
	Duration float64 `json:"duration"`
	Warn     bool    `json:"warn"`
	Error    string  `json:"error"`

	ImportedTables int `json:"imported_tables"`
}

func fetchPhases(t *testing.T, s *Server, taskID string) []phaseAPI {
	t.Helper()
	w, req := doReq("GET", "/api/v1/tasks/"+taskID+"/phases", "")
	req = withChiParam(req, "taskID", taskID)
	s.handleTaskPhases(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("phases: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Phases []phaseAPI `json:"phases"`
	}
	if err := json.Unmarshal([]byte(w.Body.String()), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Phases
}

// TestTaskPhases_MachineIsSourceOfTruth: with the phase state machine in
// the checkpoint, statuses/sub-labels/duration/warn come from the machine
// alone — including a visible skipped phase and a warn-completed phase —
// regardless of the task's coarse status.
func TestTaskPhases_MachineIsSourceOfTruth(t *testing.T) {
	s, taskID := createPhaseTestTask(t, "pm-machine")
	t.Chdir(t.TempDir())
	cpMgr, err := checkpoint.NewManager(filepath.Join(".checkpoint", taskID))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := cpMgr.InitPhases(map[string]bool{
		"precheck": false, "schema": false, "data": true, "validate": false,
	}); err != nil {
		t.Fatal(err)
	}
	_ = cpMgr.StartPhase("precheck")
	_ = cpMgr.FinishPhase("precheck", nil, false)
	_ = cpMgr.StartPhase("schema")
	_ = cpMgr.SetSubPhase("schema", "schema-execute")
	_ = cpMgr.FinishPhase("schema", nil, true)
	cpMgr.Flush()

	phases := fetchPhases(t, s, taskID)
	byName := map[string]phaseAPI{}
	for _, p := range phases {
		byName[p.Name] = p
	}
	if byName["precheck"].Status != "completed" {
		t.Errorf("precheck = %s, want completed", byName["precheck"].Status)
	}
	sch := byName["schema"]
	if sch.Status != "completed" || !sch.Warn {
		t.Errorf("schema = %+v, want completed+warn", sch)
	}
	if sch.SubLabel != "执行 DDL" {
		t.Errorf("schema sub_label = %q, want 执行 DDL", sch.SubLabel)
	}
	if sch.Duration <= 0 {
		t.Errorf("schema duration = %v, want > 0", sch.Duration)
	}
	if byName["data"].Status != "skipped" {
		t.Errorf("data = %s, want skipped (visible)", byName["data"].Status)
	}
	if byName["validate"].Status != "pending" {
		t.Errorf("validate = %s, want pending", byName["validate"].Status)
	}
}

// TestTaskPhases_MachineFailedCarriesError: a failed phase record surfaces
// its error text in the API payload.
func TestTaskPhases_MachineFailedCarriesError(t *testing.T) {
	s, taskID := createPhaseTestTask(t, "pm-failed")
	t.Chdir(t.TempDir())
	cpMgr, err := checkpoint.NewManager(filepath.Join(".checkpoint", taskID))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_ = cpMgr.InitPhases(map[string]bool{"precheck": false, "schema": false, "data": false, "validate": false})
	_ = cpMgr.StartPhase("data")
	_ = cpMgr.FinishPhase("data", fmt.Errorf("export failed"), false)
	cpMgr.Flush()

	phases := fetchPhases(t, s, taskID)
	for _, p := range phases {
		if p.Name == "data" {
			if p.Status != "failed" || p.Error != "export failed" {
				t.Errorf("data = %+v, want failed/export failed", p)
			}
			return
		}
	}
	t.Fatal("data phase missing")
}

// TestTaskPhases_LegacyFallbackOrdinalInference: a checkpoint WITHOUT the
// phases map (historical) keeps the exact pre-P1 ordinal-inference
// behavior — rolling-deploy old tasks render unchanged.
func TestTaskPhases_LegacyFallbackOrdinalInference(t *testing.T) {
	s, taskID := createPhaseTestTask(t, "pm-legacy")
	t.Chdir(t.TempDir())
	cpMgr, err := checkpoint.NewManager(filepath.Join(".checkpoint", taskID))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// No InitPhases — legacy checkpoint shape.
	_ = cpMgr.SetPhase("data")
	cpMgr.Flush()

	// task.Phase is not wired through the store here; the phases handler
	// only sees the checkpoint. The invariant we anchor: no machine → the
	// handler still answers 200 with four phases and never invents
	// skipped/failed statuses from the machine (there is none).
	phases := fetchPhases(t, s, taskID)
	if len(phases) != 4 {
		t.Fatalf("phases len = %d, want 4", len(phases))
	}
	for _, p := range phases {
		if p.Status == "skipped" {
			t.Errorf("phase %s must never be skipped on the fallback path", p.Name)
		}
	}
}

// TestTaskPhases_MachineSubLabelSoleSource anchors 🟡2: in machine mode the
// data sub-label comes ONLY from the machine SubPhase — the legacy coarse
// cpPhase switch must not override it. CIR shape: machine says
// data-import while coarse phase is still "data" (never "data-import")
// — the label must still render and ImportedTables must still fill.
func TestTaskPhases_MachineSubLabelSoleSource(t *testing.T) {
	s, taskID := createPhaseTestTask(t, "pm-cir-sublabel")
	t.Chdir(t.TempDir())
	cpMgr, err := checkpoint.NewManager(filepath.Join(".checkpoint", taskID))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_ = cpMgr.InitPhases(map[string]bool{"precheck": false, "schema": false, "data": false, "validate": false})
	_ = cpMgr.StartPhase("schema")
	_ = cpMgr.FinishPhase("schema", nil, false)
	_ = cpMgr.StartPhase("data")
	// CIR pipeline shape: SubPhase set, coarse phase lingers at "data".
	_ = cpMgr.SetSubPhase("data", "data-import")
	_ = cpMgr.SetPhase("data")
	_ = cpMgr.SetImportMode(checkpoint.ImportModeLightning)
	_ = cpMgr.SetImportedTables(3)
	for _, tbl := range []string{"orders", "customers", "products"} {
		cpMgr.GetOrCreateTable(tbl, 100)
		_ = cpMgr.MarkTableCompleted(tbl, 100)
	}
	cpMgr.Flush()

	phases := fetchPhases(t, s, taskID)
	for _, p := range phases {
		if p.Name != "data" {
			continue
		}
		if p.SubLabel != "数据导入" {
			t.Errorf("data sub_label = %q, want 数据导入 (machine SubPhase sole source)", p.SubLabel)
		}
		if p.ImportedTables != 3 {
			t.Errorf("data imported_tables = %d, want 3 (filled in machine mode)", p.ImportedTables)
		}
		return
	}
	t.Fatal("data phase missing")
}

// TestTaskPhases_LegacySubLabelSwitchStillWorks: on the legacy fallback
// path (no phases map) the coarse-phase switch remains the sub-label
// source — rolling-deploy behavior preserved.
func TestTaskPhases_LegacySubLabelSwitchStillWorks(t *testing.T) {
	s, taskID := createPhaseTestTask(t, "pm-legacy-sublabel")
	t.Chdir(t.TempDir())
	cpMgr, err := checkpoint.NewManager(filepath.Join(".checkpoint", taskID))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Legacy shape: no InitPhases; coarse phase only.
	_ = cpMgr.SetPhase("data-import")
	_ = cpMgr.SetImportMode(checkpoint.ImportModeLightning)
	_ = cpMgr.SetImportedTables(2)
	cpMgr.Flush()

	phases := fetchPhases(t, s, taskID)
	for _, p := range phases {
		if p.Name != "data" {
			continue
		}
		if p.SubLabel != "数据导入" {
			t.Errorf("data sub_label = %q, want 数据导入 (legacy switch)", p.SubLabel)
		}
		if p.ImportedTables != 2 {
			t.Errorf("data imported_tables = %d, want 2", p.ImportedTables)
		}
		return
	}
	t.Fatal("data phase missing")
}
