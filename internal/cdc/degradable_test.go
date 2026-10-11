package cdc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func myErr(number uint16, msg string) error {
	return &mysql.MySQLError{Number: number, Message: msg}
}

// F-09 group 1 (+ adversarial numeric ruling): the degradable-skip list is
// deliberately narrow — object-state mismatches only, matched by the
// driver's NUMERIC error code, never by substring (a table named
// 'myError_1064_x' inside an unrelated message must not match).
// Transient (retry-owned) and access/conn/1054 errors must NEVER match:
// skipping those drops live data.
func TestIsDegradableError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
		note string
	}{
		{myErr(1064, "You have an error in your SQL syntax"), true, "untranslatable DDL"},
		{myErr(1146, "Table 'test.Users' doesn't exist"), true, "missing object"},
		{myErr(1050, "Table 'test.users' already exists"), true, "idempotent replay"},
		{myErr(1061, "Duplicate index name"), true, "idempotent replay"},
		{myErr(1091, "Index 'idx_foo' doesn't exist"), true, "idempotent replay"},
		{myErr(1205, "Lock wait timeout exceeded"), false, "transient: retry, never skip"},
		{myErr(1213, "Deadlock found when trying to get lock"), false, "transient: retry, never skip"},
		{myErr(1054, "Unknown column 'foo' in 'field list'"), false, "column drift: fail-hard (not object-state)"},
		{myErr(1142, "SELECT command denied for table 'myError_1064_x'"), false, "substring trap: 1142 access, must not match 1064"},
		{myErr(1142, "command denied to user"), false, "fail-hard"},
		{errors.New("connection refused"), false, "non-driver error"},
		{errors.New("i/o timeout"), false, "fail-hard"},
	}
	for _, tt := range tests {
		if got := isDegradableError(tt.err); got != tt.want {
			t.Errorf("isDegradableError(%v) = %v, want %v (%s)", tt.err, got, tt.want, tt.note)
		}
	}
	if isDegradableError(nil) {
		t.Error("isDegradableError(nil) must be false")
	}
}

// A wrapped *mysql.MySQLError must still be recognized (errors.As chain) —
// the applier wraps apply errors with %w.
func TestIsDegradableErrorWrapped(t *testing.T) {
	err := fmt.Errorf("apply: %w", myErr(1064, "syntax error"))
	if !isDegradableError(err) {
		t.Error("wrapped 1064 must be degradable")
	}
	err = fmt.Errorf("apply: %w", myErr(1205, "lock wait"))
	if isDegradableError(err) {
		t.Error("wrapped 1205 must not be degradable")
	}
}

// The degradable list and the fatal list must stay disjoint where they
// overlap on error codes: 1064 is BOTH fatal-for-DML-retry (no point
// retrying a syntax error) and degradable-for-DDL-replay (skip the DDL,
// keep the chain). The applier keeps isFatalError precedence, so a 1064 DML
// still fails fast; only the DDL poller consults isDegradableError. This
// test pins that 1146 is NOT fatal (schema path owns it).
func Test1146IsSchemaNotFatal(t *testing.T) {
	err := myErr(1146, "Table 'test.users' doesn't exist")
	if isFatalError(err) {
		t.Error("1146 must stay non-fatal (schema-mismatch path)")
	}
	if !isSchemaError(err) {
		t.Error("1146 must stay schema-classified")
	}
}
