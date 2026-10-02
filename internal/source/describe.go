package source

import (
	"fmt"
	"sort"
	"sync"
)

// FieldSpec describes one connection-form field for the Web UI. The frontend
// renders the dynamic connection form from a source's []FieldSpec; the backend
// builds SourceConfig from the submitted values. This is the single source of
// truth for "which fields a source needs" — adding an adapter auto-enables its
// form with zero frontend changes (doc multi-source-web-form-design §4).
type FieldSpec struct {
	Key         string   `json:"key"`   // "host"|"port"|"user"|"password"|"database"|"schema"|"sslmode"|...
	Label       string   `json:"label"` // display label
	Type        string   `json:"type"`  // text|number|password|select|switch
	Required    bool     `json:"required"`
	Default     any      `json:"default,omitempty"` // default value (port/charset/...)
	Placeholder string   `json:"placeholder,omitempty"`
	Options     []Option `json:"options,omitempty"` // candidates for type=select
	Help        string   `json:"help,omitempty"`
	Group       string   `json:"group"` // "common"|"source" (advanced reserved, added in a later batch)
}

// Option is one candidate for a select-type FieldSpec.
type Option struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Capabilities declares what a source adapter can do. MS-01 extends the
// original {schema, data, cdc} trio with function-level bits (compare /
// watermark / assess / ddl_export) so the UI and the API 400-guards can read
// ONE truth instead of scattering `type != "postgres"` branches (five-pin
// architecture v1.0: capability bit is the single truth; the source registry
// is the only place allowed to branch on type).
type Capabilities struct {
	Schema    bool `json:"schema"`     // can read schema
	Data      bool `json:"data"`       // can full-export data
	CDC       bool `json:"cdc"`        // can incremental (PG=true; MySQL CDC deferred)
	Compare   bool `json:"compare"`    // usable as the compare feature's source side
	Watermark bool `json:"watermark"`  // watermark backfill / incremental sync
	Assess    bool `json:"assess"`     // compatibility assessment ruleset
	DDLExport bool `json:"ddl_export"` // DDL export
}

// Capability is the typed name of one capability bit (single truth: UI
// greying, backend 400 guards and capability queries all go through Capable).
type Capability string

const (
	CapSchema    Capability = "schema"
	CapData      Capability = "data"
	CapCDC       Capability = "cdc"
	CapCompare   Capability = "compare"
	CapWatermark Capability = "watermark"
	CapAssess    Capability = "assess"
	CapDDLExport Capability = "ddl_export"
)

// NormalizeKind resolves a source kind with the single legacy default:
// ""/missing means "postgres" (zero-regression: existing datasource profiles
// and task configs without a type stay PG). A non-empty unknown kind is an
// error — never silently downgraded.
func NormalizeKind(kind string) (string, error) {
	if kind == "" {
		return "postgres", nil
	}
	metaMu.RLock()
	_, ok := metaRegistry[kind]
	metaMu.RUnlock()
	if !ok {
		return "", fmt.Errorf("source: unknown source %q (described: %v)", kind, Described())
	}
	return kind, nil
}

// Capable reports whether the (normalized) source kind has the capability bit
// set. It is the single truth for UI greying and backend 400 double-guarding;
// callers must not branch on the raw type string themselves.
func Capable(kind string, c Capability) (bool, error) {
	k, err := NormalizeKind(kind)
	if err != nil {
		return false, err
	}
	metaMu.RLock()
	m, ok := metaRegistry[k]
	metaMu.RUnlock()
	if !ok {
		return false, fmt.Errorf("source: unknown source %q", k)
	}
	switch c {
	case CapSchema:
		return m.Capabilities.Schema, nil
	case CapData:
		return m.Capabilities.Data, nil
	case CapCDC:
		return m.Capabilities.CDC, nil
	case CapCompare:
		return m.Capabilities.Compare, nil
	case CapWatermark:
		return m.Capabilities.Watermark, nil
	case CapAssess:
		return m.Capabilities.Assess, nil
	case CapDDLExport:
		return m.Capabilities.DDLExport, nil
	default:
		return false, fmt.Errorf("source: unknown capability %q", c)
	}
}

// SourceMeta is a source's complete connection description. It drives both the
// source selector and the schema-driven connection form. Crucially it does NOT
// require opening a connection, so stub sources can still describe themselves
// (Implemented=false) for the UI (doc §4).
type SourceMeta struct {
	Name         string       `json:"name"`        // "postgres"
	DisplayName  string       `json:"displayName"` // "PostgreSQL"
	Implemented  bool         `json:"implemented"` // stub=false
	DefaultPort  int          `json:"defaultPort"` // 5432
	Fields       []FieldSpec  `json:"fields"`
	Capabilities Capabilities `json:"capabilities"`
	NotImplMsg   string       `json:"notImplMsg,omitempty"` // stub hint shown in the disabled form
}

var (
	metaMu       sync.RWMutex
	metaRegistry = map[string]SourceMeta{}
)

// RegisterMeta registers a source's connection metadata. Called from each
// adapter's init() (including stubs, with Implemented=false). This is separate
// from the Open-factory Register so a stub can describe its form without a
// working Open.
func RegisterMeta(name string, meta SourceMeta) {
	metaMu.Lock()
	defer metaMu.Unlock()
	meta.Name = name
	metaRegistry[name] = meta
}

// Describe returns the metadata for one source without opening a connection.
// Works for stub sources (Implemented=false).
func Describe(name string) (SourceMeta, error) {
	metaMu.RLock()
	defer metaMu.RUnlock()
	m, ok := metaRegistry[name]
	if !ok {
		return SourceMeta{}, fmt.Errorf("source: unknown source %q (described: %v)", name, Described())
	}
	return m, nil
}

// DescribeAll returns metadata for all registered sources, for the Web selector
// + schema-driven form. Sorted by name.
func DescribeAll() []SourceMeta {
	metaMu.RLock()
	defer metaMu.RUnlock()
	out := make([]SourceMeta, 0, len(metaRegistry))
	for _, m := range metaRegistry {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Described returns the sorted list of source names that have registered meta.
func Described() []string {
	metaMu.RLock()
	defer metaMu.RUnlock()
	names := make([]string, 0, len(metaRegistry))
	for k := range metaRegistry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
