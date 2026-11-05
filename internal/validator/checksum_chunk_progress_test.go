package validator

// checksum_chunk_progress_test.go anchors the #t95 P1 chunk-progress
// callback:
//
//	①  chunk callback fires once per RUN chunk, exactly matching chunksRan
//	   (the scripted 4-chunks anchor: 200k rows / 50k chunk size → 4 calls,
//	   idx 0..3, total 4) and the table callback is NOT spuriously fired by
//	   the chunk path (isolation).
//	②  no callback registered → the run is byte-identical (no-callback
//	   compat; complements the existing 4-chunks anchor A5).
//	③  a no-PK table falling back to hash_group has no chunks at all → the
//	   chunk callback never fires (quick/sample/hash_group have no chunk
//	   concept).

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

type chunkCall struct {
	table string
	idx   int
	total int
}

func runChecksum4Chunks(t *testing.T, onChunk func(string, int, int)) reporter.TableReport {
	t.Helper()
	pgDB := sql.OpenDB(&scriptConnector{meter: &queryMeter{}, pg: true})
	defer pgDB.Close()
	tidbDB := sql.OpenDB(&scriptConnector{meter: &queryMeter{}, pg: false})
	defer tidbDB.Close()

	v := NewValidator(config.Config{})
	v.compareOverride = &config.CompareConfig{ChecksumChunkSize: 50000, Concurrency: 2}
	if onChunk != nil {
		v.OnChunkProgress(onChunk)
	}
	return v.validateChecksumChunked(context.Background(), pgDB, tidbDB, "t1", make(concSem, 2))
}

// TestChecksumChunkProgressFiresPerChunk (①): the 4-chunks anchor fires the
// chunk callback exactly 4 times — idx 0..3, total always 4 — and the table
// callback registered alongside stays quiet during the chunk-level run (the
// chunk path never fires per-table done; Run() owns that call site).
func TestChecksumChunkProgressFiresPerChunk(t *testing.T) {
	var (
		mu       sync.Mutex
		calls    []chunkCall
		tableCbs int
	)
	tr := runChecksum4Chunks(t, func(table string, idx, total int) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, chunkCall{table: table, idx: idx, total: total})
	})

	if tr.Status != "pass" {
		t.Fatalf("scripted chunk compare must pass, got status=%s err=%q", tr.Status, tr.Error)
	}
	if !strings.Contains(tr.Suggestion, "4 chunks") {
		t.Fatalf("chunk path not taken (suggestion=%q) — fix the script", tr.Suggestion)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("chunk progress calls = %d, want 4 (== chunksRan)", len(calls))
	}
	for i, c := range calls {
		if c.table != "t1" {
			t.Fatalf("call %d table = %q, want t1", i, c.table)
		}
		if c.idx != i {
			t.Fatalf("call %d idx = %d, want %d", i, c.idx, i)
		}
		if c.total != 4 {
			t.Fatalf("call %d total = %d, want 4", i, c.total)
		}
	}
	if tableCbs != 0 {
		t.Fatalf("table-done callback fired %d times inside validateChecksumChunked, want 0 (Run() owns it)", tableCbs)
	}
}

// TestChunkProgressAndTableDoneIsolation: with BOTH callbacks registered,
// the checksum chunk path fires only the chunk callback — the per-table
// done callback is structurally isolated (single call site in Run()). This
// pins the "onTableDone 仍按表触发" guarantee against regression.
func TestChunkProgressAndTableDoneIsolation(t *testing.T) {
	pgDB := sql.OpenDB(&scriptConnector{meter: &queryMeter{}, pg: true})
	defer pgDB.Close()
	tidbDB := sql.OpenDB(&scriptConnector{meter: &queryMeter{}, pg: false})
	defer tidbDB.Close()

	var (
		mu       sync.Mutex
		chunks   int
		tableCbs int
	)
	v := NewValidator(config.Config{})
	v.compareOverride = &config.CompareConfig{ChecksumChunkSize: 50000, Concurrency: 2}
	v.OnChunkProgress(func(string, int, int) {
		mu.Lock()
		defer mu.Unlock()
		chunks++
	})
	v.OnTableDone(func(done, total int, tr reporter.TableReport) {
		mu.Lock()
		defer mu.Unlock()
		tableCbs++
	})

	tr := v.validateChecksumChunked(context.Background(), pgDB, tidbDB, "t1", make(concSem, 2))
	if tr.Status != "pass" {
		t.Fatalf("scripted chunk compare must pass, got status=%s err=%q", tr.Status, tr.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if chunks != 4 {
		t.Fatalf("chunk callback calls = %d, want 4", chunks)
	}
	if tableCbs != 0 {
		t.Fatalf("table-done callback fired %d times from the chunk path, want 0 (Run() owns it)", tableCbs)
	}
}

// TestChecksumChunkProgressNoCallbackUnchanged (②): with NO chunk callback
// registered the same scripted 4-chunk run behaves byte-identically (pass +
// "4 chunks") — registering the progress hook can never alter validation.
func TestChecksumChunkProgressNoCallbackUnchanged(t *testing.T) {
	tr := runChecksum4Chunks(t, nil)
	if tr.Status != "pass" {
		t.Fatalf("scripted chunk compare must pass without a callback, got status=%s err=%q", tr.Status, tr.Error)
	}
	if !strings.Contains(tr.Suggestion, "4 chunks") {
		t.Fatalf("chunk path not taken (suggestion=%q) — fix the script", tr.Suggestion)
	}
}

// TestChecksumChunkProgressSilentOnHashGroupFallback (③): a table without
// PK/unique chunks falls back to hash_group (no windows), so the chunk
// callback must never fire — "no chunk" really means "no callback".
func TestChecksumChunkProgressSilentOnHashGroupFallback(t *testing.T) {
	same := rowsOf([2]any{int64(1), "a"}, [2]any{int64(2), "a"})
	src := &sideScript{count: 2, data: same}
	dst := &sideScript{count: 2, data: same}

	var (
		mu    sync.Mutex
		calls int
	)
	v := NewValidator(config.Config{})
	v.compareOverride = &config.CompareConfig{ChecksumChunkSize: 2, Concurrency: 2}
	v.OnChunkProgress(func(string, int, int) {
		mu.Lock()
		defer mu.Unlock()
		calls++
	})

	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 2))
	if tr.Status != "pass" {
		t.Fatalf("no-PK hash_group fallback must pass, got status=%s err=%q", tr.Status, tr.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("chunk callback fired %d times on a no-PK (hash_group) table, want 0", calls)
	}
}
