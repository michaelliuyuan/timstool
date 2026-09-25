package cdc

import (
	"testing"
)

// F-09 group 1: the degradable-skip list is deliberately narrow —
// object-state mismatches only. Transient (retry-owned) and access/conn
// errors must NEVER match: skipping those drops live data.
func TestIsDegradableError(t *testing.T) {
	tests := []struct {
		errMsg string
		want   bool
		note   string
	}{
		{"Error 1064: You have an error in your SQL syntax", true, "untranslatable DDL"},
		{"Error 1146: Table 'test.Users' doesn't exist", true, "missing object"},
		{"Error 1050: Table 'test.users' already exists", true, "idempotent replay"},
		{"Error 1061: Duplicate index name", true, "idempotent replay"},
		{"Error 1091: Index 'idx_foo' doesn't exist", true, "idempotent replay"},
		{"Error 1205: Lock wait timeout exceeded", false, "transient: retry, never skip"},
		{"Error 1213: Deadlock found when trying to get lock", false, "transient: retry, never skip"},
		{"access denied for user 'root'", false, "fail-hard"},
		{"connection refused", false, "fail-hard"},
		{"i/o timeout", false, "fail-hard"},
		{"Error 1054: Unknown column 'foo' in 'field list'", false, "column drift: fail-hard (not object-state)"},
	}
	for _, tt := range tests {
		err := &testError{msg: tt.errMsg}
		if got := isDegradableError(err); got != tt.want {
			t.Errorf("isDegradableError(%q) = %v, want %v (%s)", tt.errMsg, got, tt.want, tt.note)
		}
	}
	if isDegradableError(nil) {
		t.Error("isDegradableError(nil) must be false")
	}
}

// The degradable list and the fatal list must stay disjoint where they
// overlap on error codes: 1064 is BOTH fatal-for-DML-retry (no point
// retrying a syntax error) and degradable-for-DDL-replay (skip the DDL,
// keep the chain). The applier keeps isFatalError precedence, so a 1064 DML
// still fails fast; only the DDL poller consults isDegradableError. This
// test pins that 1146 is NOT fatal (schema path owns it).
func Test1146IsSchemaNotFatal(t *testing.T) {
	err := &testError{msg: "Error 1146: Table 'test.users' doesn't exist"}
	if isFatalError(err) {
		t.Error("1146 must stay non-fatal (schema-mismatch path)")
	}
	if !isSchemaError(err) {
		t.Error("1146 must stay schema-classified")
	}
}
