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

// SourceMeta is the metadata exposed by GET /sources for the Web UI's source
// selector (#t67 WSC).
type SourceMeta struct {
	Name        string `json:"name"`    // "postgres" | "mysql" | …
	Display     string `json:"display"` // "PostgreSQL" | "MySQL" | …
	Status      string `json:"status"`  // "implemented" | "stub"
	Description string `json:"description"`
}

// RegisteredMeta returns metadata for all registered sources (for the Web
// selector). A source is "stub" if its factory is a StubFactory (Open fails
// with "not implemented"). The Web UI disables stub sources with "即将支持".
func RegisteredMeta() []SourceMeta {
	registryMu.RLock()
	defer registryMu.RUnlock()
	var metas []SourceMeta
	for name, factory := range registry {
		// Probe: stub factories return an error; implemented ones don't.
		_, err := factory(SourceConfig{})
		status := "implemented"
		if err != nil {
			status = "stub"
		}
		metas = append(metas, SourceMeta{
			Name:        name,
			Display:     sourceDisplayName(name),
			Status:      status,
			Description: sourceDescription(name),
		})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Name < metas[j].Name })
	return metas
}

// ConfigSchemaFor returns the ConfigField list for a source type (the Web UI
// renders the connection form from this). Returns nil if the source is unknown
// or a stub.
func ConfigSchemaFor(name string) []ConfigField {
	src, err := Open(name, SourceConfig{})
	if err != nil {
		return nil
	}
	return src.ConfigSchema()
}

func sourceDisplayName(name string) string {
	switch name {
	case "postgres":
		return "PostgreSQL"
	case "mysql":
		return "MySQL"
	case "oracle":
		return "Oracle"
	case "mssql":
		return "SQL Server"
	case "db2":
		return "DB2"
	default:
		return name
	}
}

func sourceDescription(name string) string {
	switch name {
	case "postgres":
		return "PostgreSQL database"
	case "mysql":
		return "MySQL / TiDB-compatible database"
	case "oracle":
		return "Oracle database"
	case "mssql":
		return "Microsoft SQL Server"
	case "db2":
		return "IBM DB2"
	default:
		return ""
	}
}

// StubFactory returns a factory that always fails with a "not implemented"
// message, used to reserve the name for an unimplemented source (oracle/mssql/db2).
func StubFactory(name string) func(SourceConfig) (Source, error) {
	return func(SourceConfig) (Source, error) {
		return nil, fmt.Errorf("source %q is not implemented yet (stub); currently supported: %v", name, Registered())
	}
}
