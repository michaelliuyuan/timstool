package cdc

import (
	"testing"
	"time"
)

// F-10 group 2 item 3: the 1146 short-circuit set.
func TestMissingTablesShortCircuit(t *testing.T) {
	a := NewApplier(nil, DefaultBatchConfig(), NewTransformer(DefaultTransformerConfig()))

	tk := "public.orders"
	if a.isMissing(tk) {
		t.Fatal("fresh applier must not short-circuit anything")
	}

	// Confirmed missing → short-circuited.
	a.markMissing(tk)
	if !a.isMissing(tk) {
		t.Fatal("marked table must be short-circuited")
	}

	// DDL poller callback clears the whole set.
	a.ResetMissingTables()
	if a.isMissing(tk) {
		t.Fatal("ResetMissingTables (post-DDL callback) must clear the set")
	}

	// TTL expiry: an old entry is re-probed (manual compensation path).
	a.missingMu.Lock()
	a.missingTables[tk] = time.Now().Add(-2 * missingTTL)
	a.missingMu.Unlock()
	if a.isMissing(tk) {
		t.Fatal("expired entry must be re-probed, not short-circuited")
	}
	a.missingMu.Lock()
	if _, ok := a.missingTables[tk]; ok {
		t.Error("expired entry must be evicted on read")
	}
	a.missingMu.Unlock()
}
