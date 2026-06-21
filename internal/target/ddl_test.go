package target

import (
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/source"
)

func TestRenderCreateTable(t *testing.T) {
	tbl := source.Table{
		Name: "t_users",
		Columns: []source.Column{
			{Name: "id", TiDBType: "BIGINT", Nullable: false, IsAutoIncr: true},
			{Name: "name", TiDBType: "VARCHAR(50)", Nullable: true, Comment: "用户名"},
			{Name: "created_at", TiDBType: "DATETIME", Nullable: true, Default: "CURRENT_TIMESTAMP"},
		},
		PK: []string{"id"},
		Indexes: []source.Index{
			{Name: "idx_name", Columns: []string{"name"}},
			{Name: "uk_email", Columns: []string{"email"}, Unique: true},
		},
	}
	got := RenderCreateTable(tbl)
	wants := []string{
		"CREATE TABLE IF NOT EXISTS `t_users`",
		"`id` BIGINT NOT NULL AUTO_INCREMENT",
		"`name` VARCHAR(50) COMMENT '用户名'",
		"`created_at` DATETIME DEFAULT CURRENT_TIMESTAMP",
		"PRIMARY KEY (`id`)",
		"INDEX `idx_name` (`name`)",
		"UNIQUE INDEX `uk_email` (`email`)",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("RenderCreateTable missing %q\ngot:\n%s", w, got)
		}
	}
}

func TestRenderCreateTable_NoPKNoIndex(t *testing.T) {
	// no-PK table (e.g. t_event_log) must not emit a PRIMARY KEY clause.
	got := RenderCreateTable(source.Table{
		Name: "t_event_log",
		Columns: []source.Column{
			{Name: "msg", TiDBType: "TEXT", Nullable: true},
		},
	})
	if strings.Contains(got, "PRIMARY KEY") {
		t.Errorf("no-PK table should not have PRIMARY KEY:\n%s", got)
	}
	if !strings.HasPrefix(got, "CREATE TABLE IF NOT EXISTS `t_event_log` (") {
		t.Errorf("unexpected DDL:\n%s", got)
	}
}

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"col":       "`col`",
		"o`db":      "`o``db`",
		"a'b":       "`a'b`",
		"with space": "`with space`",
	}
	for in, want := range cases {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
