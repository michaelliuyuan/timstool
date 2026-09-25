package logger

import (
	"testing"

	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// F-09 item 3: the extra-core registry must support multiple concurrent
// task cores and remove exactly the one that finished. The old single-slot
// Tee teardown let one task's exit tear down every other task's core.
func TestExtraCoreMultiSlot(t *testing.T) {
	coreA, logsA := observer.New(zapcore.InfoLevel)
	coreB, logsB := observer.New(zapcore.InfoLevel)

	RegisterExtraCore(coreA)
	RegisterExtraCore(coreB)

	extraMu.Lock()
	n := len(extraCores)
	extraMu.Unlock()
	if n != 2 {
		t.Fatalf("after two registers, want 2 live cores, got %d", n)
	}

	// Task A finishes: only A's core must go; B survives.
	UnregisterExtraCore(coreA)
	extraMu.Lock()
	n = len(extraCores)
	extraMu.Unlock()
	if n != 1 {
		t.Fatalf("after unregister A, want 1 live core, got %d", n)
	}

	// Unregistering an unknown/already-removed core is a no-op.
	UnregisterExtraCore(coreA)
	extraMu.Lock()
	n = len(extraCores)
	extraMu.Unlock()
	if n != 1 {
		t.Fatalf("after double unregister A, want still 1 live core, got %d", n)
	}

	// Task B finishes: registry empties.
	UnregisterExtraCore(coreB)
	extraMu.Lock()
	n = len(extraCores)
	extraMu.Unlock()
	if n != 0 {
		t.Fatalf("after unregister B, want 0 live cores, got %d", n)
	}
	_, _ = logsA, logsB
}

func TestRegisterExtraCoreNilIsNoop(t *testing.T) {
	before := len(extraCores)
	RegisterExtraCore(nil)
	if len(extraCores) != before {
		t.Fatalf("nil register changed registry size: %d -> %d", before, len(extraCores))
	}
}
