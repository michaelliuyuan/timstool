package data

import (
	"path/filepath"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/checkpoint"
)

func TestPgQuoteLiteral(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"public", "'public'"},
		{"users", "'users'"},
		{"o'brien", "'o''brien'"},
		{"a''b", "'a''''b'"},
		{"", "''"},
	}
	for _, c := range cases {
		if got := pgQuoteLiteral(c.in); got != c.want {
			t.Errorf("pgQuoteLiteral(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func newTestMigrator(t *testing.T) *Migrator {
	t.Helper()
	mgr, err := checkpoint.NewManager(filepath.Join(t.TempDir(), "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	return &Migrator{cpMgr: mgr}
}

func TestSetExactRowsTotal_OverridesEstimate(t *testing.T) {
	m := newTestMigrator(t)
	m.cpMgr.GetOrCreateTable("t1", 1000)

	m.setExactRowsTotal("t1", 1234)

	tc, _ := m.cpMgr.GetTable("t1")
	if tc.RowsTotal != 1234 {
		t.Errorf("expected exact 1234, got %d", tc.RowsTotal)
	}
}

func TestSetExactRowsTotal_FailureKeepsEstimate(t *testing.T) {
	// COUNT failure means setExactRowsTotal is never called; the estimate
	// registered at pre-registration time must survive untouched.
	m := newTestMigrator(t)
	m.cpMgr.GetOrCreateTable("t1", 900)

	// no call — simulate dispatch-time COUNT failure

	tc, _ := m.cpMgr.GetTable("t1")
	if tc.RowsTotal != 900 {
		t.Errorf("expected estimate 900 preserved, got %d", tc.RowsTotal)
	}
}

func TestSetExactRowsTotal_NotBelowRowsDone(t *testing.T) {
	m := newTestMigrator(t)
	m.cpMgr.GetOrCreateTable("t1", 1000)
	m.cpMgr.UpdateTableProgress("t1", 500, 0)

	// exact COUNT turns out lower than rows already exported: clamp up.
	m.setExactRowsTotal("t1", 400)

	tc, _ := m.cpMgr.GetTable("t1")
	if tc.RowsTotal != 500 {
		t.Errorf("expected denominator clamped to RowsDone 500, got %d", tc.RowsTotal)
	}
	if tc.RowsDone != 500 {
		t.Errorf("expected RowsDone unchanged 500, got %d", tc.RowsDone)
	}
}
