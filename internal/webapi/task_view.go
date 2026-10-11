package webapi

import "github.com/michaelliuyuan/timstool/internal/store"

// taskKindMigration is the default kind for every task whose ConfigJSON is
// not a compare envelope (legacy mysql/pg migration tasks, empty configs).
const taskKindMigration = "migration"

// taskView is the wire shape for task responses: store.Task plus an explicit
// server-derived kind discriminator. ConfigJSON stays hidden (json:"-") — it
// contains credentials; clients must discriminate on kind, never on config.
type taskView struct {
	*store.Task
	Kind string `json:"kind"`
}

// newTaskView derives the kind by sniffing the compare envelope out of the
// hidden ConfigJSON. Anything else — including empty or legacy configs —
// routes as a migration task.
func newTaskView(t *store.Task) *taskView {
	kind := taskKindMigration
	if _, ok := parseCompareTask(t.ConfigJSON); ok {
		kind = compareTaskKind
	}
	return &taskView{Task: t, Kind: kind}
}
