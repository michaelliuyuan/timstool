package source

import (
	"fmt"
	"sort"
	"sync"
)

// The adapter registry maps a source name (e.g. "postgres", "mysql") to a
// factory. Sources register themselves via Register (typically in an init() of
// the adapter package), and the CLI/Web opens one by name via Open
// (`--source=mysql`). Unimplemented sources (oracle/mssql/db2) register a stub
// factory that returns a friendly "not implemented" error.

var (
	registryMu sync.RWMutex
	registry   = map[string]func(cfg SourceConfig) (Source, error){}
)

// Register adds (or replaces) a source adapter factory under the given name.
// Called from each adapter package's init().
func Register(name string, factory func(cfg SourceConfig) (Source, error)) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// Open creates the named source adapter. Returns a friendly error if the name
// is unknown or the adapter is a stub.
func Open(name string, cfg SourceConfig) (Source, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("source: unknown source %q (registered: %v)", name, Registered())
	}
	cfg.Kind = name
	return factory(cfg)
}

// Registered returns the sorted list of registered source names.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// StubFactory returns a factory that always fails with a "not implemented"
// message, used to reserve the name for an unimplemented source (oracle/mssql/db2).
func StubFactory(name string) func(SourceConfig) (Source, error) {
	return func(SourceConfig) (Source, error) {
		return nil, fmt.Errorf("source %q is not implemented yet (stub); currently supported: %v", name, Registered())
	}
}
