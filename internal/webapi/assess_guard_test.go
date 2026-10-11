package webapi

import "testing"

// MS-10b2 item 8, adversarial P3 follow-up: the assess system-schema
// guard is unit-anchored here (case-insensitivity, whitespace tolerance,
// db-as-schema normalization shape). The HTTP-level 400 (POST /assess
// with a MySQL system schema) is exercised in the black-box ticket —
// the handler test harness has no stubbed source connection layer.
func TestAssessSchemaRejected(t *testing.T) {
	for _, tc := range []struct{ driver, schema string }{
		{"mysql", "mysql"},
		{"mysql", "INFORMATION_SCHEMA"},
		{"mysql", "Sys"},
		{"mysql", "metrics_schema"},
		{"mysql", " mysql "}, // whitespace must not slip past
		{"mysql", "performance_schema"},
	} {
		if !assessSchemaRejected(tc.driver, tc.schema) {
			t.Errorf("assessSchemaRejected(%q, %q) = false, want true", tc.driver, tc.schema)
		}
	}
	for _, tc := range []struct{ driver, schema string }{
		{"mysql", "migration_test"},
		{"mysql", "public"}, // a user db literally named public is not a system schema
		{"pgx", "public"},
		{"pgx", "mysql"}, // PG path never guarded (schema name is free-form)
		{"", "mysql"},
	} {
		if assessSchemaRejected(tc.driver, tc.schema) {
			t.Errorf("assessSchemaRejected(%q, %q) = true, want false", tc.driver, tc.schema)
		}
	}
}
