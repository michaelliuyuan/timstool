package reporter

// reporter_duration_test.go anchors the FEAT-CMP-TBL-DURATION JSON compat
// surface: TableReport.Duration is a PRE-EXISTING omitempty field the
// compare path newly fills — report.json schema has grown by nothing
// (zero-add red line), old files decode unchanged, and FormatDuration (the
// single duration format across the report faces) is pinned for every form
// the per-table stamps can take.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTableReportDurationOmitEmptyCompat: an EMPTY Duration is omitted
// (old compare report.json files are byte-shaped exactly as before); a
// filled Duration appears — the migration report's own filling path is
// untouched and keeps working against the same field.
func TestTableReportDurationOmitEmptyCompat(t *testing.T) {
	empty, err := json.Marshal(TableReport{TableName: "t", Status: StatusPass})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "duration") {
		t.Errorf("empty duration must stay omitted (old-report byte compat): %s", empty)
	}

	filled, err := json.Marshal(TableReport{TableName: "t", Status: StatusPass, Duration: "1m2.500s"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(filled), `"duration":"1m2.500s"`) {
		t.Errorf("filled duration missing from JSON: %s", filled)
	}

	// An old report.json (no duration key) decodes with "" — the honest
	// blank both HTML faces render as「—」.
	var back TableReport
	if err := json.Unmarshal(empty, &back); err != nil {
		t.Fatal(err)
	}
	if back.Duration != "" {
		t.Errorf("old report decoded Duration=%q want empty", back.Duration)
	}
}

// TestFormatDurationForms pins the three forms the per-table stamps can
// take (sub-minute, sub-hour, hour+); the compare seam, the overall report
// Duration and the migration report all share this single formatter.
func TestFormatDurationForms(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{1500 * time.Millisecond, "1.500s"},
		{62*time.Second + 500*time.Millisecond, "1m2.500s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h2m3.000s"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.in); got != c.want {
			t.Errorf("FormatDuration(%v)=%q want %q", c.in, got, c.want)
		}
	}
}
