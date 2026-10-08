package cdc

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// openDDLLogForTest builds an in-memory pg2tidb_ddl_log mirror (only the
// columns FetchNewDDL reads) with the caller seeding rows.
func openDDLLogForTest(t *testing.T) *DDLTracker {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE pg2tidb_ddl_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ddl_time TEXT,
		schema_name TEXT,
		object_name TEXT,
		object_type TEXT,
		ddl_command TEXT)`); err != nil {
		t.Fatal(err)
	}
	return NewDDLTracker(db, nil)
}

// TestFetchNewDDLNullSchemaName (H, MS-11e): a NULL schema_name row must not
// fail the Scan — pre-fix the whole batch errored ("converting NULL to string
// is unsupported"), the ddl-poller WARN-spammed every 2s, and the lastID
// cursor never advanced so every later DDL row was permanently unreachable.
// Post-fix the row scans to an empty Schema, the batch continues to later
// rows, and no error returns.
func TestFetchNewDDLNullSchemaName(t *testing.T) {
	tr := openDDLLogForTest(t)
	db := tr.db
	// Row 1: NULL schema_name (the production shape: sql_drop-captured rows
	// and command-tag-only events carry no schema). Row 2: a normal row that
	// MUST still be fetched after the NULL one (cursor-unblock proof).
	ins := `INSERT INTO pg2tidb_ddl_log (ddl_time, schema_name, object_name, object_type, ddl_command) VALUES (?,?,?,?,?)`
	if _, err := db.Exec(ins, "2026-10-08", nil, "orders", "TABLE", "DROP TABLE orders"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ins, "2026-10-08", "public", "customers", "TABLE", "CREATE TABLE customers (id INT)"); err != nil {
		t.Fatal(err)
	}

	entries, err := tr.FetchNewDDL(context.Background(), 0)
	if err != nil {
		t.Fatalf("fetch with NULL schema_name row must not error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("both rows must be fetched (NULL row must not block the batch): %d", len(entries))
	}
	if entries[0].Schema != "" {
		t.Fatalf("NULL schema_name must scan to empty string, got %q", entries[0].Schema)
	}
	if entries[0].ObjectName != "orders" || entries[1].Schema != "public" || entries[1].ObjectName != "customers" {
		t.Fatalf("row fields mismatched: %+v %+v", entries[0], entries[1])
	}
	// Cursor advance: a re-fetch from the last seen id returns nothing new
	// (the poller's sinceID contract — the pre-fix error path never got here).
	again, err := tr.FetchNewDDL(context.Background(), entries[1].ID)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-fetch after cursor: %v %d", err, len(again))
	}
}
