package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// --- fake ProgressReporter: records every call in order ---

type fakeRpt struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRpt) RegisterSchemaTables(names []string) error {
	return nil
}
func (f *fakeRpt) MarkSchemaTableCompleted(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "completed:"+name)
	return nil
}
func (f *fakeRpt) MarkSchemaTableFailed(name string, errStr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "failed:"+name)
	return nil
}
func (f *fakeRpt) SetSubPhase(phase, sub string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "sub:"+phase+"/"+sub)
	return nil
}
func (f *fakeRpt) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// --- fake executor: substitutes the TiDB connection in runDDL ---

type fakeExec struct {
	mu      sync.Mutex
	execLog []string
	// stmts containing a key here return the mapped error on EVERY attempt.
	failOn map[string]error
}

func (f *fakeExec) ExecContext(ctx context.Context, q string, args ...interface{}) (sql.Result, error) {
	f.mu.Lock()
	f.execLog = append(f.execLog, q)
	f.mu.Unlock()
	for frag, err := range f.failOn {
		if strings.Contains(q, frag) {
			return nil, err
		}
	}
	return stubResult{}, nil
}

type stubResult struct{}

func (stubResult) LastInsertId() (int64, error) { return 0, nil }
func (stubResult) RowsAffected() (int64, error) { return 1, nil }

func newFakeExec(fail map[string]error) *fakeExec {
	return &fakeExec{failOn: fail}
}

// TestAttributeStatementToTable pins the P0 attribution: CREATE/ALTER/DROP
// TABLE attribute directly; CREATE [UNIQUE] INDEX attributes via the ON
// clause (the index name itself is NOT the table); SET and comments do not
// attribute at all.
func TestAttributeStatementToTable(t *testing.T) {
	cases := []struct {
		name, stmt, want string
	}{
		{"create table", "CREATE TABLE `t1` (id INT)", "t1"},
		{"create if not exists", "CREATE TABLE IF NOT EXISTS t2 (id INT)", "t2"},
		{"alter table", "ALTER TABLE `t3` ADD COLUMN c INT", "t3"},
		{"drop table", "DROP TABLE IF EXISTS `t4`", "t4"},
		{"create index via ON", "CREATE INDEX `idx_a` ON `t5` (`col`)", "t5"},
		{"create unique index via ON", "CREATE UNIQUE INDEX `idx_b` ON t6 (col)", "t6"},
		{"create index if not exists via ON", "CREATE INDEX IF NOT EXISTS `idx_c` ON `t8` (`col`)", "t8"},
		{"create unique index if not exists via ON", "CREATE UNIQUE INDEX IF NOT EXISTS idx_d ON t9 (col)", "t9"},
		{"set is unattributed", "SET FOREIGN_KEY_CHECKS = 0", ""},
		{"comment is unattributed", "-- Table: t7 (SKIPPED: already exists in target)", ""},
	}
	for _, c := range cases {
		action := extractDDLAction(c.stmt)
		if got := attributeStatementToTable(action, c.stmt); got != c.want {
			t.Errorf("%s: attributeStatementToTable(%q) = %q, want %q", c.name, c.stmt, got, c.want)
		}
	}
	// extractObjectName keeps returning the INDEX name for labels — the
	// attribution fix lives in attributeStatementToTable, not here.
	if got := extractObjectName("CREATE INDEX `idx_a` ON `t5` (`col`)"); got != "idx_a" {
		t.Errorf("extractObjectName(create index) = %q, want idx_a (label semantics)", got)
	}
	// Cosmetic fix: IF NOT EXISTS must be skipped, not captured as "IF".
	if got := extractObjectName("CREATE INDEX IF NOT EXISTS `idx_c` ON `t8` (`col`)"); got != "idx_c" {
		t.Errorf("extractObjectName(create index if not exists) = %q, want idx_c", got)
	}
}

// TestRunDDLMarksExecutionProgress: schema tables_done must climb with
// REAL execution — completed marks fire per attributed statement (CREATE
// TABLE t1, then CREATE INDEX … ON t1 attributing t1 again, idempotent),
// SET/comments never mark.
func TestRunDDLMarksExecutionProgress(t *testing.T) {
	m := NewMigrator(config.Config{})
	rpt := &fakeRpt{}
	m.SetProgressReporter(rpt)

	stmts := []string{
		"SET FOREIGN_KEY_CHECKS = 0",
		"-- Table: t1",
		"CREATE TABLE `t1` (id INT PRIMARY KEY)",
		"CREATE INDEX `idx_a` ON `t1` (`id`)",
		"ALTER TABLE `t2` ADD COLUMN c INT",
	}
	if err := m.runDDL(context.Background(), newFakeExec(nil), stmts); err != nil {
		t.Fatalf("runDDL: %v", err)
	}
	got := rpt.recorded()
	want := []string{"completed:t1", "completed:t1", "completed:t2"}
	if len(got) != len(want) {
		t.Fatalf("mark sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mark sequence = %v, want %v", got, want)
		}
	}
}

// TestRunDDLDuplicateIndexCountsAsSuccess: 1061 duplicate key name is an
// idempotent re-run — the attributed table still reaches completed.
func TestRunDDLDuplicateIndexCountsAsSuccess(t *testing.T) {
	m := NewMigrator(config.Config{})
	rpt := &fakeRpt{}
	m.SetProgressReporter(rpt)

	fake := newFakeExec(map[string]error{
		"idx_dup": fmt.Errorf("Error 1061: Duplicate key name 'idx_dup'"),
	})
	err := m.runDDL(context.Background(), fake, []string{
		"CREATE INDEX `idx_dup` ON `t1` (`id`)",
	})
	if err != nil {
		t.Fatalf("runDDL: %v", err)
	}
	got := rpt.recorded()
	if len(got) != 1 || got[0] != "completed:t1" {
		t.Fatalf("marks = %v, want [completed:t1]", got)
	}
}

// TestRunDDLFailureMarksFailedAndAborts: retries exhausted + default
// OnError (abort) → the attributed table is marked failed and the error
// propagates.
func TestRunDDLFailureMarksFailedAndAborts(t *testing.T) {
	m := NewMigrator(config.Config{})
	rpt := &fakeRpt{}
	m.SetProgressReporter(rpt)

	fake := newFakeExec(map[string]error{
		"CREATE TABLE `bad`": errors.New("Error 1146: boom"),
	})
	err := m.runDDL(context.Background(), fake, []string{
		"CREATE TABLE `good` (id INT)",
		"CREATE TABLE `bad` (id INT)",
	})
	if err == nil {
		t.Fatal("runDDL must abort on exhausted retries (OnError != skip)")
	}
	got := rpt.recorded()
	if len(got) != 2 || got[0] != "completed:good" || !strings.HasPrefix(got[1], "failed:bad") {
		t.Fatalf("marks = %v, want [completed:good failed:bad:...]", got)
	}
}

// TestRunDDLFailureSkipStrategyMarksAndContinues: OnError=skip marks the
// table failed but the run continues (subsequent statements execute).
func TestRunDDLFailureSkipStrategyMarksAndContinues(t *testing.T) {
	cfg := config.Config{}
	cfg.Migration.OnError = "skip"
	m := NewMigrator(cfg)
	rpt := &fakeRpt{}
	m.SetProgressReporter(rpt)

	fake := newFakeExec(map[string]error{
		"CREATE TABLE `bad`": errors.New("Error 1146: boom"),
	})
	err := m.runDDL(context.Background(), fake, []string{
		"CREATE TABLE `bad` (id INT)",
		"CREATE TABLE `good` (id INT)",
	})
	if err != nil {
		t.Fatalf("runDDL with skip must not fail: %v", err)
	}
	got := rpt.recorded()
	if len(got) != 2 || !strings.HasPrefix(got[0], "failed:bad") || got[1] != "completed:good" {
		t.Fatalf("marks = %v, want [failed:bad:... completed:good]", got)
	}
}
