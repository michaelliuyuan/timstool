package data

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/common/config"
)

// fakeExec records every statement issued through ExecContext.
type fakeExec struct {
	queries []string
	failOn  string // substring; matching statements return an error
}

func (f *fakeExec) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	f.queries = append(f.queries, query)
	if f.failOn != "" && strings.Contains(query, f.failOn) {
		return nil, fmt.Errorf("boom: %s", query)
	}
	return result{}, nil
}

type result struct{}

func (result) LastInsertId() (int64, error) { return 0, nil }
func (result) RowsAffected() (int64, error) { return 0, nil }

// F-10 anchor (leader ruling seq 189): applyTargetPolicy is now invoked on
// BOTH import paths (Lightning branch and the streaming importViaSQL entry),
// so a resumed streaming task (use_lightning=false — the production shape)
// clears its own table set before re-inserting, closing the 1062 duplicate
// debt. This test pins the shared policy executor itself:
//   - insert/default policy: no statements at all (pure append mode);
//   - truncate policy: FK checks disabled, every task table truncated with
//     fully-qualified quoted names, FK checks re-enabled afterwards;
//   - a failing TRUNCATE surfaces as an error (streaming must not continue
//     into uncleaned tables).
func TestApplyTargetPolicyStreamingAnchor(t *testing.T) {
	newMigrator := func(policy string) *Migrator {
		cfg := config.Config{}
		cfg.Migration.TargetPolicy = policy
		cfg.Target.Database = "tgt"
		return NewMigrator(cfg)
	}

	t.Run("insert-policy-is-no-op", func(t *testing.T) {
		for _, policy := range []string{"", "insert"} {
			fe := &fakeExec{}
			if err := newMigrator(policy).applyTargetPolicy(context.Background(), fe, []string{"a", "b"}); err != nil {
				t.Fatalf("policy %q: %v", policy, err)
			}
			if len(fe.queries) != 0 {
				t.Fatalf("policy %q issued statements: %v", policy, fe.queries)
			}
		}
	})

	t.Run("truncate-clears-task-tables-with-fk-guard", func(t *testing.T) {
		fe := &fakeExec{}
		tables := []string{"orders", "order_items"}
		if err := newMigrator("truncate").applyTargetPolicy(context.Background(), fe, tables); err != nil {
			t.Fatalf("truncate policy: %v", err)
		}
		want := []string{
			"SET SESSION FOREIGN_KEY_CHECKS = 0",
			"TRUNCATE TABLE `tgt`.`orders`",
			"TRUNCATE TABLE `tgt`.`order_items`",
			"SET SESSION FOREIGN_KEY_CHECKS = 1",
		}
		if len(fe.queries) != len(want) {
			t.Fatalf("statements = %v, want %v", fe.queries, want)
		}
		for i := range want {
			if fe.queries[i] != want[i] {
				t.Fatalf("statement[%d] = %q, want %q", i, fe.queries[i], want[i])
			}
		}
	})

	t.Run("truncate-failure-surfaces-error", func(t *testing.T) {
		fe := &fakeExec{failOn: "TRUNCATE TABLE `tgt`.`orders`"}
		err := newMigrator("truncate").applyTargetPolicy(context.Background(), fe, []string{"orders"})
		if err == nil || !strings.Contains(err.Error(), "truncate orders") {
			t.Fatalf("truncate failure must surface, got %v", err)
		}
	})
}
