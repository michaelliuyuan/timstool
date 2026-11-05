package validator

// dialect_keyset_key_test.go anchors the #t95 B1 dialect root-cause fix
// (single item 2, the M1-b fallback's underlying detection bug): PG
// DetectTableKey must reject PARTIAL unique indexes (WHERE predicate) and
// EXPRESSION indexes as table-level keys, so those tables fall into the
// no-PK path (hash_group + guardrail) instead of a keyset run that silently
// drops rows. Plain PKs and plain unique indexes keep chunking unchanged.

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
	"github.com/michaelliuyuan/timstool/internal/common/reporter"
)

// TestUsableTableUniqueIndex (①②③): the raw pg_get_indexdef classifier.
// Plain unique indexes (with or without sort options / quoted idents) are
// usable; partial indexes and expression indexes are not.
func TestUsableTableUniqueIndex(t *testing.T) {
	cases := []struct {
		def  string
		want bool
	}{
		{"CREATE UNIQUE INDEX uk_name ON t1 (name)", true},
		{"CREATE UNIQUE INDEX uk_col ON \"myTable\" (\"myCol\")", true},
		{"CREATE UNIQUE INDEX uk ON t1 (email ASC, name DESC NULLS LAST)", true},
		// partial indexes (WHERE predicate) — not table-level unique
		{"CREATE UNIQUE INDEX uk_email ON t1 USING btree (email) WHERE (status = 1)", false},
		{"CREATE UNIQUE INDEX uk_email ON t1 USING btree (email) WHERE (deleted_at IS NULL)", false},
		{"CREATE UNIQUE INDEX uk ON t1 (email) WHERE status = 1", false},
		// expression indexes — uniqueness on the expression, not raw columns
		{"CREATE UNIQUE INDEX uk_email ON t1 USING btree (lower(email))", false},
		{"CREATE UNIQUE INDEX uk_a ON t1 ((a-b))", false},
		{"CREATE UNIQUE INDEX uk_c ON t1 USING btree (upper(email))", false},
	}
	for _, c := range cases {
		if got := usableTableUniqueIndex(c.def); got != c.want {
			t.Errorf("usableTableUniqueIndex(%q) = %v, want %v", c.def, got, c.want)
		}
	}
}

// TestDetectTableKeyPGRejectsPartialAndExprIndexes: end-to-end through
// postgresDialect.DetectTableKey (scripted pg_indexes probe) — a partial or
// expression unique index is NOT reported as a table-level unique key, while
// a plain unique index still is (③ regression).
func TestDetectTableKeyPGRejectsPartialAndExprIndexes(t *testing.T) {
	pd := postgresDialect{}
	cases := []struct {
		name string
		uk   string
		want []string // nil = no unique key may be reported
	}{
		{"plain unique", "CREATE UNIQUE INDEX uk_name ON t1 (name)", []string{"name"}},
		{"partial predicate", "CREATE UNIQUE INDEX uk_email ON t1 USING btree (email) WHERE (status = 1)", nil},
		{"expression index", "CREATE UNIQUE INDEX uk_email ON t1 USING btree (lower(email))", nil},
	}
	for _, c := range cases {
		src := &sideScript{count: 5, uniqueIdxDef: c.uk}
		info, err := pd.DetectTableKey(context.Background(), src.open(), "db", "t1")
		if err != nil {
			t.Fatalf("%s: DetectTableKey: %v", c.name, err)
		}
		if c.want == nil {
			if info.HasUniqueIndex || len(info.UniqueColumns) != 0 {
				t.Fatalf("%s: unique key mis-detected, info=%+v (must fall to no-PK path)", c.name, info)
			}
			continue
		}
		if !info.HasUniqueIndex || len(info.UniqueColumns) != len(c.want) {
			t.Fatalf("%s: unique key not detected, info=%+v", c.name, info)
		}
		for i := range c.want {
			if info.UniqueColumns[i] != c.want[i] {
				t.Fatalf("%s: UniqueColumns = %v, want %v", c.name, info.UniqueColumns, c.want)
			}
		}
	}
}

// TestChecksumChunkPartialIndexSkipsKeysetChunks: a PG table whose ONLY
// unique index is partial (WHERE predicate) must NOT be chunked with a
// keyset cursor — DetectTableKey now rejects it, so the table goes straight
// to the hash_group full comparison (no "SELECT * ... LIMIT" window queries
// on either side) and a real diff still fails honestly.
func TestChecksumChunkPartialIndexSkipsKeysetChunks(t *testing.T) {
	uk := "CREATE UNIQUE INDEX uk_email ON t1 USING btree (email) WHERE (status = 1)"
	src := &sideScript{count: 4, uniqueIdxDef: uk, dataCols: []string{"id", "email", "status", "val"}, dataColTypes: []string{"int8", "text", "int8", "text"},
		data: [][]driver.Value{
			{int64(1), "a@x", int64(1), "x"}, {int64(2), "b@x", int64(1), "x"}, {int64(3), "c@x", int64(1), "x"}, {int64(4), "d@x", int64(1), "DIFF"},
		}}
	dst := &sideScript{count: 4, uniqueIdxDef: uk, dataCols: []string{"id", "email", "status", "val"}, dataColTypes: []string{"int8", "text", "int8", "text"},
		data: [][]driver.Value{
			{int64(1), "a@x", int64(1), "x"}, {int64(2), "b@x", int64(1), "x"}, {int64(3), "c@x", int64(1), "x"}, {int64(4), "d@x", int64(1), "x"},
		}}

	v := NewValidator(config.Config{Compare: config.CompareConfig{ChecksumChunkSize: 2}})
	tr := v.validateChecksumChunked(context.Background(), src.open(), dst.open(), "t1", make(concSem, 4))

	if tr.Status != reporter.StatusFail {
		t.Fatalf("diff in a partial-index table must FAIL via hash_group, got %s err=%q", tr.Status, tr.Error)
	}
	if !strings.Contains(tr.Error, "hash group") {
		t.Fatalf("verdict must be the hash_group full comparison, got %q", tr.Error)
	}
	// No keyset chunks: every chunk window query carries LIMIT; the direct
	// hash_group path issues full scans (no LIMIT). Assert zero window scans
	// on the source side.
	for _, q := range src.sawQueries("SELECT *") {
		if strings.Contains(q, "LIMIT") {
			t.Fatalf("partial-index table must not run keyset chunk windows, saw %q", q)
		}
	}
	if !src.sawFullScan() {
		t.Fatal("partial-index table must run the hash_group full scan (evidence)")
	}
}
