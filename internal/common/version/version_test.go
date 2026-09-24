package version

import "testing"

// S1-UI-07 anchors: TiDB blob → "TiDB v7.1.9", PG long line →
// "PostgreSQL 16.15", garbage → first-line fallback.

const tidbBlob = `Release Version: v7.1.9
Edition: Community
Git Commit Hash: 2c9984e9e6e3a619f1f2f5b8f75ac2e48a1e6a17
Git Branch: heads/refs/tags/v7.1.9
UTC Build Time: 2024-05-21 03:26:13
GoVersion: go1.21.10
Race Enabled: false
Check Table Before Drop: false
Store: unistore
TiKV Rust Version: 6.0.0-alpha`

const pgLong = "PostgreSQL 16.15 (Debian 16.15-1.pgdg120+1) on x86_64-pc-linux-gnu, compiled by gcc (Debian 12.2.0-14) 12.2.0, 64-bit"

func TestShortTiDB(t *testing.T) {
	if got := ShortTiDB(tidbBlob); got != "TiDB v7.1.9" {
		t.Fatalf("ShortTiDB = %q, want TiDB v7.1.9", got)
	}
	// Pre-release tail is preserved up to the comma/newline.
	if got := ShortTiDB("Release Version: v8.0.0-alpha.1\nEdition: Community"); got != "TiDB v8.0.0-alpha.1" {
		t.Fatalf("ShortTiDB pre-release = %q", got)
	}
	// Garbage: first line, trimmed.
	if got := ShortTiDB("\n  not a blob \nsecond line"); got != "not a blob" {
		t.Fatalf("ShortTiDB fallback = %q", got)
	}
	if got := ShortTiDB(""); got != "" {
		t.Fatalf("ShortTiDB empty = %q", got)
	}
}

func TestShortPostgreSQL(t *testing.T) {
	if got := ShortPostgreSQL(pgLong); got != "PostgreSQL 16.15" {
		t.Fatalf("ShortPostgreSQL = %q, want PostgreSQL 16.15", got)
	}
	// Two-segment versions (e.g. PG 10 on old Debian) keep their shape.
	if got := ShortPostgreSQL("PostgreSQL 10.23 (Debian 10.23-1.pgdg90+1) x86_64"); got != "PostgreSQL 10.23" {
		t.Fatalf("ShortPostgreSQL two-segment = %q", got)
	}
	// Garbage: first line, trimmed.
	if got := ShortPostgreSQL("  weird output\nmore"); got != "weird output" {
		t.Fatalf("ShortPostgreSQL fallback = %q", got)
	}
}
