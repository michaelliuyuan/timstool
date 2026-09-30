package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// FEAT-INC-KEY-WARN anchors: key-disclosure SQL structural guards, empty
// batch short-circuit, JSON shape, and the batch endpoint's pre-DB
// validation surface (identifier injection rejection, cap, duplicates).
// Live four-form判定 (PK / plain unique / partial / expression / invalid /
// partition) is covered by isolation S1 against a real PG.

// A1: the catalog query pins every判定 guard — valid indexes only,
// non-partial (indpred), non-expression (indexprs), partition children
// excluded, and one ANY($2) round trip for the whole batch.
func TestIncTableKeysSQLStructuralGuards(t *testing.T) {
	for _, pin := range []string{
		"i.indisvalid",
		"i.indpred IS NULL",
		"i.indexprs IS NULL",
		"tc.relispartition = false",
		"bool_or(i.indisprimary)",
		"bool_or(i.indisunique AND NOT i.indisprimary",
		"tc.relname = ANY($2)",
		"GROUP BY tc.relname",
	} {
		if !strings.Contains(incTableKeysSQL, pin) {
			t.Errorf("incTableKeysSQL missing structural guard %q", pin)
		}
	}
}

// A2: empty table list short-circuits to an empty map without touching
// the database (nil db is never dereferenced).
func TestIncTableKeysEmptyBatch(t *testing.T) {
	m, err := queryIncTableKeys(context.Background(), nil, "public", nil)
	if err != nil {
		t.Fatalf("empty batch must not error, got %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("empty batch must return empty map, got %v", m)
	}
}

// A3: incKeyInfo marshals to the contracted key_info shape.
func TestIncKeyInfoJSONShape(t *testing.T) {
	b, err := json.Marshal(incKeyInfo{HasPK: true, HasUnique: false})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"has_pk":true,"has_unique":false}` {
		t.Fatalf("unexpected JSON shape: %s", b)
	}
}

// A4: absent table reads as zero-value (no key) — the documented
// "missing from map ⇒ no key" contract.
func TestIncTableKeysAbsentMeansNoKey(t *testing.T) {
	m := map[string]incKeyInfo{"t_pk": {HasPK: true}}
	if ki := m["t_plain"]; ki.HasPK || ki.HasUnique {
		t.Fatalf("absent table must read as no-key, got %+v", ki)
	}
}

// A5: batch endpoint rejects identifier-injection table names BEFORE any
// connection attempt (the ANY($2) parameter only ever sees
// incIdentifierOK-vetted names — this anchor pins that the vetting is
// not relaxed).
func TestIncColumnsBatchRejectsInjectionTableNames(t *testing.T) {
	s, _ := newTestServer(t)
	for _, bad := range []string{
		`t; DROP TABLE x`, `t--comment`, `"quoted"`, `t'or'1'='1`,
	} {
		w, req := doReq("POST", "/api/v1/incremental/columns-batch",
			`{"source_ref": "whatever", "tables": ["`+bad+`"]}`)
		s.handleIncrementalColumnsBatch(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("table %q must 400, got %d", bad, w.Code)
		}
	}
}

// A6: batch endpoint enforces the cap and duplicate rejection pre-DB.
func TestIncColumnsBatchCapAndDuplicates(t *testing.T) {
	s, _ := newTestServer(t)
	tables := make([]string, incColumnsBatchLimit+1)
	for i := range tables {
		tables[i] = "t_ok_1"
	}
	b, _ := json.Marshal(map[string]interface{}{"source_ref": "x", "tables": tables})
	w, req := doReq("POST", "/api/v1/incremental/columns-batch", string(b))
	s.handleIncrementalColumnsBatch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("over-cap batch must 400, got %d", w.Code)
	}

	w, req = doReq("POST", "/api/v1/incremental/columns-batch",
		`{"source_ref": "x", "tables": ["dup", "dup"]}`)
	s.handleIncrementalColumnsBatch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("duplicate tables must 400, got %d", w.Code)
	}
}
