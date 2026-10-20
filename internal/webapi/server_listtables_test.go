package webapi

import (
	"strings"
	"testing"
)

// TestListTablesSQLPartitionGuards pins the /config/list-tables universe
// alignment (wmCatalogSQL 口径): partition children are excluded via the
// pg_class/pg_namespace relkind+relispartition guards, the row_estimate
// stays a correlated pg_class subquery, and ordering stays table_name. The
// compare page, /sources/tables (CollectTables) and the suggest catalog
// must share ONE table universe — drift here resurfaces the "12 vs 10
// tables" class of divergence (analysis seq ec2b035 §3).
func TestListTablesSQLPartitionGuards(t *testing.T) {
	for _, guard := range []string{
		"relkind IN ('r', 'p')",
		"NOT t.relispartition",
		"FROM pg_class t",
		"JOIN pg_namespace n",
		"AS row_estimate",
		"ORDER BY t.relname",
	} {
		if !strings.Contains(listTablesSQL, guard) {
			t.Errorf("listTablesSQL missing guard %q", guard)
		}
	}
	// The legacy information_schema shape must be gone — partition children
	// hide inside its BASE TABLE filter.
	if strings.Contains(listTablesSQL, "information_schema.tables") {
		t.Error("listTablesSQL still reads information_schema.tables (partition children leak)")
	}
}
