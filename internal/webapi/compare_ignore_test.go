package webapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// #ignore compare exclusion list anchors: normalization (trim/dedup/case/
// identifier/limit), create-time validation, options persistence
// (set / clear-by-empty / preserve-when-absent), and run-param plumbing.

// B1: normalizeIgnoreColumns — nil/empty stays nil; trims, drops empties,
// collapses case-insensitive duplicates (first wins), rejects invalid
// identifiers and over-limit lists.
func TestNormalizeIgnoreColumns(t *testing.T) {
	if out, err := normalizeIgnoreColumns(nil); out != nil || err != nil {
		t.Fatalf("nil must stay nil, got %v err=%v", out, err)
	}
	if out, err := normalizeIgnoreColumns([]string{}); out != nil || err != nil {
		t.Fatalf("empty must stay nil, got %v err=%v", out, err)
	}
	if out, err := normalizeIgnoreColumns([]string{"", "  "}); out != nil || err != nil {
		t.Fatalf("all-whitespace must stay nil, got %v err=%v", out, err)
	}

	out, err := normalizeIgnoreColumns([]string{" created_at ", "Updated_At", "", "updated_at", "id"})
	if err != nil {
		t.Fatalf("valid list must pass: %v", err)
	}
	// trimmed, deduped case-insensitively, first occurrence order kept.
	if len(out) != 3 || out[0] != "created_at" || out[1] != "Updated_At" || out[2] != "id" {
		t.Fatalf("normalized wrong: %v", out)
	}

	for i, bad := range [][]string{
		{"bad col"},      // space → invalid identifier
		{"col; DROP"},    // injection
		{`x"y`},          // quote
		distinctList(65), // over the 64 cap
	} {
		if _, err := normalizeIgnoreColumns(bad); err == nil {
			t.Errorf("case %d: %v must be rejected", i, bad)
		}
	}
}

// B2: options round-trip — a saved exclusion list persists unchanged and
// returns through loadCompareOptions; present-and-empty clears; absent keeps.
func TestCompareOptionsIgnoreColumnsRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)

	w, req := doReq("PUT", "/api/v1/compare/options",
		`{"ignore_columns":["created_at"," updated_at ","id"]}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set ignore list: %d %s", w.Code, w.Body.String())
	}
	opts := s.loadCompareOptions()
	if len(opts.IgnoreColumns) != 3 || opts.IgnoreColumns[0] != "created_at" || opts.IgnoreColumns[1] != "updated_at" || opts.IgnoreColumns[2] != "id" {
		t.Fatalf("saved ignore list wrong: %v", opts.IgnoreColumns)
	}

	// Absent key keeps the saved list (partial-merge by presence).
	w, req = doReq("PUT", "/api/v1/compare/options", `{"parallel":3}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("absent-key put: %d", w.Code)
	}
	if len(s.loadCompareOptions().IgnoreColumns) != 3 {
		t.Fatalf("absent ignore_columns key must preserve saved list")
	}

	// Present-and-empty clears.
	w, req = doReq("PUT", "/api/v1/compare/options", `{"ignore_columns":[]}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("empty-clear put: %d", w.Code)
	}
	if len(s.loadCompareOptions().IgnoreColumns) != 0 {
		t.Fatalf("ignore_columns:[] must clear the saved list")
	}

	// Invalid identifier rejected without touching the (now empty) list.
	w, req = doReq("PUT", "/api/v1/compare/options", `{"ignore_columns":["bad col"]}`)
	s.handlePutCompareOptions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid identifier must 400, got %d", w.Code)
	}
}

// B3: create-time validation — invalid ignore_columns rejects the task;
// valid list is normalized into the persisted task.
func TestCompareTaskIgnoreColumnsValidation(t *testing.T) {
	s, _ := newTestServer(t)

	for i, body := range []string{
		`{"source":{"host":"s"},"target":{"host":"t"},"ignore_columns":["bad col"]}`,
		`{"source":{"host":"s"},"target":{"host":"t"},"ignore_columns":["a;DROP"]}`,
		`{"source":{"host":"s"},"target":{"host":"t"},"ignore_columns":` + listJSON(65) + `}`,
	} {
		w, req := doReq("POST", "/api/v1/compare/tasks", body)
		s.handleCreateCompare(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("case %d: expected 400, got %d body=%s", i, w.Code, w.Body.String())
		}
	}

	// Valid list accepted → persisted task carries normalized names.
	w, req := doReq("POST", "/api/v1/compare/tasks",
		`{"source":{"host":"s"},"target":{"host":"t"},"ignore_columns":[" id ","created_at","ID"]}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("valid task must create, got %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created task missing id")
	}
	loaded, err := s.loadCompareTask(created.ID)
	if err != nil || loaded == nil {
		t.Fatalf("load created task: %v", err)
	}
	if len(loaded.IgnoreColumns) != 2 || loaded.IgnoreColumns[0] != "id" || loaded.IgnoreColumns[1] != "created_at" {
		t.Fatalf("persisted ignore list wrong: %v", loaded.IgnoreColumns)
	}
	// Wind down the background run goroutine (unreachable host) so the
	// server's TempDir is free for teardown.
	if s.compare.cancel != nil {
		s.compare.cancel()
	}
}

// B4: run-param plumbing — the task's ignore list reaches the compare config.
func TestBuildCompareRunParamsIgnoreColumns(t *testing.T) {
	task := &CompareTask{IgnoreColumns: []string{"created_at", "updated_at"}}
	_, _, cfg := buildCompareRunParams(task, "")
	if len(cfg.IgnoreColumns) != 2 || cfg.IgnoreColumns[0] != "created_at" || cfg.IgnoreColumns[1] != "updated_at" {
		t.Fatalf("ignore list not plumbed into compareCfg: %v", cfg.IgnoreColumns)
	}
}

// distinctList builds n distinct identifier names (the over-limit case must
// survive dedup to actually trip the 64 cap).
func distinctList(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "c" + intToString(i)
	}
	return out
}

func listJSON(n int) string {
	names := distinctList(n)
	out := "["
	for i, v := range names {
		if i > 0 {
			out += ","
		}
		out += `"` + v + `"`
	}
	return out + "]"
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
