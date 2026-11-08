package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// #97 anchors: the checksum_chunk_size floor (1000, rows per window) on all
// three webapi intake points — compare create, compare options PUT, and
// migration task create. 0/absent keeps the runtime default; the options
// profile normalizes present-and-0 to the default window size (clear-by-0,
// same semantics as concurrency).

func TestCompareOptions_ChunkSizeFloor(t *testing.T) {
	s, _ := newTestServer(t)

	put := func(body string) (*int, string) {
		w, req := doReq("PUT", "/api/v1/compare/options", body)
		s.handlePutCompareOptions(w, req)
		return &w.Code, w.Body.String()
	}

	// 1-999 and negatives are the unit-confusion typo → rejected, and the
	// message must name the floor (actionable, F-08-2 message shape).
	code, body := put(`{"checksum_chunk_size":999}`)
	if *code != http.StatusBadRequest {
		t.Fatalf("999 must 400, got %d body=%s", *code, body)
	}
	if !strings.Contains(body, "1000") {
		t.Fatalf("rejection must name the floor 1000: %s", body)
	}
	if code, _ := put(`{"checksum_chunk_size":-5}`); *code != http.StatusBadRequest {
		t.Fatalf("negative must 400, got %d", *code)
	}

	// Explicit in-range value persists.
	if code, body := put(`{"checksum_chunk_size":20000}`); *code != http.StatusOK {
		t.Fatalf("20000 must save, got %d body=%s", *code, body)
	}
	if got := s.loadCompareOptions().ChecksumChunkSize; got != 20000 {
		t.Fatalf("saved chunk size=%d want 20000", got)
	}

	// Absent key preserves the saved value (partial-merge contract).
	if code, body := put(`{"sample_ratio":0.5}`); *code != http.StatusOK {
		t.Fatalf("absent-key put failed: %d %s", *code, body)
	}
	if got := s.loadCompareOptions().ChecksumChunkSize; got != 20000 {
		t.Fatalf("absent key must preserve, got %d", got)
	}

	// Present-and-0 resets to the default window size (clear-by-0).
	if code, body := put(`{"checksum_chunk_size":0}`); *code != http.StatusOK {
		t.Fatalf("clear failed: %d %s", *code, body)
	}
	if got := s.loadCompareOptions().ChecksumChunkSize; got != 50000 {
		t.Fatalf("present-and-0 must reset to default 50000, got %d", got)
	}
}

func TestCompareCreate_ChunkSizeFloor(t *testing.T) {
	// 999 at create → 400 naming the floor.
	s, _ := newTestServer(t)
	w, req := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s"},"target":{"host":"t"},"mode":"checksum","checksum_chunk_size":999}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("999 at create must 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "1000") {
		t.Fatalf("rejection must name the floor 1000: %s", w.Body.String())
	}

	// 0 = default → created (single-running slot then held by the wind-down).
	s2, _ := newTestServer(t)
	w2, req2 := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s"},"target":{"host":"t"},"mode":"checksum","checksum_chunk_size":0}`)
	s2.handleCreateCompare(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("0 (= default) must create, got %d body=%s", w2.Code, w2.Body.String())
	}
}

func TestMigrationCreate_ChunkSizeFloor(t *testing.T) {
	s, _ := newTestServer(t)

	// Create source + target datasources (same shape as the task-ref tests).
	w, req := doReq("POST", "/api/v1/datasources", `{
		"name": "src97", "type": "postgres",
		"fields": {"host": "10.0.0.1", "port": 5432, "user": "pg", "password": "pw1", "database": "db1"}
	}`)
	s.handleCreateDataSource(w, req)
	srcID := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "tgt97", "type": "tidb",
		"fields": {"host": "10.0.0.9", "port": 4000, "user": "root", "password": "pw2", "database": "db2"}
	}`)
	s.handleCreateDataSource(w, req)
	tgtID := dsBody(t, w)["id"].(string)

	// 999 in the migration opts → 400 naming the floor.
	body := fmt.Sprintf(`{"name": "cs97", "source_ref": %q, "target_ref": %q,
		"opts": {"compare_mode": "checksum", "checksum_chunk_size": 999}}`, srcID, tgtID)
	w, req = doReq("POST", "/api/v1/tasks", body)
	s.handleCreateTask(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("999 at migration create must 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "1000") {
		t.Fatalf("rejection must name the floor 1000: %s", w.Body.String())
	}

	// 0/absent = default → created.
	body = fmt.Sprintf(`{"name": "cs97b", "source_ref": %q, "target_ref": %q,
		"opts": {"compare_mode": "checksum"}}`, srcID, tgtID)
	w, req = doReq("POST", "/api/v1/tasks", body)
	s.handleCreateTask(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("absent chunk size must create, got %d body=%s", w.Code, w.Body.String())
	}
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
