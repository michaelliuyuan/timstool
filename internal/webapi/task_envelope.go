package webapi

import "github.com/michaelliuyuan/timstool/internal/store"

// taskKindMigration is the default kind for every task whose ConfigJSON is
// not a compare envelope (legacy mysql/pg migration tasks, empty configs).
const taskKindMigration = "migration"

// taskEnvelope is the wire shape for task responses: store.Task plus an explicit
// server-derived kind discriminator. ConfigJSON stays hidden (json:"-") — it
// contains credentials; clients must discriminate on kind, never on config.
type taskEnvelope struct {
	*store.Task
	Kind string `json:"kind"`
}

// newTaskEnvelope derives the kind by sniffing the compare envelope out of the
// hidden ConfigJSON. Anything else — including empty or legacy configs —
// routes as a migration task.
func newTaskEnvelope(t *store.Task) *taskEnvelope {
	kind := taskKindMigration
	if _, ok := parseCompareTask(t.ConfigJSON); ok {
		kind = compareTaskKind
	}
	return &taskEnvelope{Task: t, Kind: kind}
}
