package webapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// #t4 anchors: unified concurrency budget — request folding into the task,
// persisted shape, and options persistence (set / clear-by-0 / preserve).

// A1: create folds the deprecated knobs into Concurrency (explicit wins,
// legacy max, default 4, clamp at 8) and persists the effective value.
func TestCompareTaskConcurrencyFold(t *testing.T) {
	lastConc := 0
	cases := []struct {
		body    string
		wantCon int
	}{
		{`{"source":{"host":"s"},"target":{"host":"t"},"concurrency":6}`, 6},
		{`{"source":{"host":"s"},"target":{"host":"t"},"parallel":2,"checksum_parallel":5}`, 5},
		{`{"source":{"host":"s"},"target":{"host":"t"},"parallel":3}`, 3},
		{`{"source":{"host":"s"},"target":{"host":"t"},"parallel":0,"checksum_parallel":0}`, 4}, // 0/absent = unset → default 4 (appendix ①)
		{`{"source":{"host":"s"},"target":{"host":"t"}}`, 4},
		{`{"source":{"host":"s"},"target":{"host":"t"},"concurrency":32}`, 8},
		{`{"source":{"host":"s"},"target":{"host":"t"},"parallel":64}`, 8},
	}
	for i, c := range cases {
		// One server per case: a created task claims the single running
		// slot while its (unreachable) run winds down.
		s, _ := newTestServer(t)
		w, req := doReq("POST", "/api/v1/compare/tasks", c.body)
		s.handleCreateCompare(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("case %d: create failed %d body=%s", i, w.Code, w.Body.String())
		}
		var resp CompareTask
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("case %d: decode response: %v", i, err)
		}
		if resp.Concurrency != c.wantCon {
			t.Errorf("case %d: task.Concurrency=%d want %d", i, resp.Concurrency, c.wantCon)
		}
		lastConc = resp.Concurrency
	}

	// buildCompareRunParams must plumb the budget into the validator config.
	_, _, cfg := buildCompareRunParams(&CompareTask{Concurrency: lastConc}, "")
	if cfg.Concurrency != lastConc {
		t.Fatalf("run params must carry the folded budget, got %d", cfg.Concurrency)
	}
	if cfg.Concurrency != 8 {
		t.Fatalf("last case must be the clamped 8, got %d", cfg.Concurrency)
	}

	// Negative concurrency is rejected at create too (⚠️1: same surface as PUT).
	s, _ := newTestServer(t)
	w, req := doReq("POST", "/api/v1/compare/tasks", `{"source":{"host":"s"},"target":{"host":"t"},"concurrency":-1}`)
	s.handleCreateCompare(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("negative concurrency at create must 400, got %d", w.Code)
	}
}

// A2: options persistence — set / clear-by-0 / preserve-when-absent, plus
// negative rejection and over-cap clamping.
func TestCompareOptionsConcurrencyMerge(t *testing.T) {
	s, _ := newTestServer(t)

	put := func(body string) int {
		w, req := doReq("PUT", "/api/v1/compare/options", body)
		s.handlePutCompareOptions(w, req)
		return w.Code
	}

	if put(`{"concurrency":6}`) != http.StatusOK {
		t.Fatal("set failed")
	}
	if got := s.loadCompareOptions().Concurrency; got != 6 {
		t.Fatalf("saved concurrency=%d want 6", got)
	}

	// Absent key preserves.
	if put(`{"sample_ratio":0.5}`) != http.StatusOK {
		t.Fatal("absent-key put failed")
	}
	if got := s.loadCompareOptions().Concurrency; got != 6 {
		t.Fatalf("absent key must preserve, got %d", got)
	}

	// Present-and-0 clears to auto.
	if put(`{"concurrency":0}`) != http.StatusOK {
		t.Fatal("clear failed")
	}
	if got := s.loadCompareOptions().Concurrency; got != 0 {
		t.Fatalf("concurrency:0 must clear, got %d", got)
	}

	// Negative rejected.
	if put(`{"concurrency":-1}`) != http.StatusBadRequest {
		t.Fatal("negative concurrency must 400")
	}
	// Over cap clamps, not rejects.
	if put(`{"concurrency":99}`) != http.StatusOK {
		t.Fatal("over-cap concurrency must clamp, not 400")
	}
	if got := s.loadCompareOptions().Concurrency; got != 8 {
		t.Fatalf("over-cap must clamp to 8, got %d", got)
	}
}
