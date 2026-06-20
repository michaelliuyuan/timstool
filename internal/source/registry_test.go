package source

import (
	"strings"
	"testing"
)

func TestRegistry_OpenUnknown(t *testing.T) {
	_, err := Open("definitely-not-a-source", SourceConfig{})
	if err == nil || !strings.Contains(err.Error(), "unknown source") {
		t.Fatalf("Open(unknown) err = %v, want an unknown-source error", err)
	}
}

func TestRegistry_StubFactory(t *testing.T) {
	Register("teststub-only", StubFactory("teststub-only"))
	defer unregister("teststub-only")

	if _, err := Open("teststub-only", SourceConfig{}); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("Open(stub) err = %v, want a not-implemented error", err)
	}
	if !contains(Registered(), "teststub-only") {
		t.Errorf("Registered() = %v, want teststub-only present", Registered())
	}
}

func TestRegistry_OpenSetsKind(t *testing.T) {
	Register("kindcheck-only", func(cfg SourceConfig) (Source, error) {
		if cfg.Kind != "kindcheck-only" {
			t.Errorf("factory received cfg.Kind = %q, want kindcheck-only (Open must set it)", cfg.Kind)
		}
		return nil, nil
	})
	defer unregister("kindcheck-only")

	if _, err := Open("kindcheck-only", SourceConfig{Host: "h"}); err != nil {
		t.Fatalf("Open(kindcheck) err = %v, want nil", err)
	}
}

func unregister(name string) {
	registryMu.Lock()
	delete(registry, name)
	registryMu.Unlock()
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
