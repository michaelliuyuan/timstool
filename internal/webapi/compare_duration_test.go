package webapi

// compare_duration_test.go anchors FEAT-CMP-TBL-DURATION C3: the OnTableDone
// WS broadcast payload (tableDoneProgress) now carries the finished table's
// status + duration so the FE running panel lights each table's 耗时 as it
// completes — while every legacy field (#t95 chunk progress, possibly_stuck
// heartbeat, tables counters) stays byte-identical (heartbeat red line: the
// broadcast rhythm is untouched, only enriched).

import (
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

func TestTableDoneWSProgressPayload(t *testing.T) {
	task := &CompareTask{Status: CompareStatusRunning, CurrentChunk: 3, TotalChunks: 16}
	tr := reporter.TableReport{TableName: "public.orders", Status: reporter.StatusPass, Duration: "12.500s"}

	got := tableDoneProgress(task, 2, 5, tr)

	wantKeys := []string{
		"type", "status", "tables_done", "tables_total", "current_table",
		"current_chunk", "total_chunks", "possibly_stuck",
		"table_status", "table_duration",
	}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("payload missing key %q", k)
		}
	}
	if len(got) != len(wantKeys) {
		t.Errorf("payload has %d keys, want exactly %d (no schema drift)", len(got), len(wantKeys))
	}
	// Legacy fields byte-identical to the pre-feature broadcast.
	if got["type"] != "compare" || got["status"] != "running" ||
		got["tables_done"] != 2 || got["tables_total"] != 5 ||
		got["current_table"] != "public.orders" ||
		got["current_chunk"] != 3 || got["total_chunks"] != 16 {
		t.Errorf("legacy payload fields drifted: %v", got)
	}
	// New per-table fields.
	if got["table_status"] != "pass" {
		t.Errorf("table_status=%v want pass", got["table_status"])
	}
	if got["table_duration"] != "12.500s" {
		t.Errorf("table_duration=%v want 12.500s", got["table_duration"])
	}

	// Old-task tolerance: a tr without duration (pre-feature engine) keeps
	// the field present-but-empty — the FE renders「—」, never breaks.
	trOld := reporter.TableReport{TableName: "public.legacy", Status: reporter.StatusFail}
	gotOld := tableDoneProgress(task, 1, 5, trOld)
	if d, ok := gotOld["table_duration"].(string); !ok || d != "" {
		t.Errorf("legacy table_duration=%v want empty string", gotOld["table_duration"])
	}
	if gotOld["table_status"] != "fail" {
		t.Errorf("legacy table_status=%v want fail", gotOld["table_status"])
	}
}
