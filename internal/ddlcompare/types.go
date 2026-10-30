// Package ddlcompare implements the DDL comparison feature (DDL 对比校验):
// it inventories both sides of a heterogeneous pair (source: MySQL or
// PostgreSQL; target: TiDB over the MySQL protocol) into a normalized
// CatalogSnapshot per side, then diffs the snapshots across eight object
// classes — tables (primary key + charset + collation), views, stored
// procedures, functions, triggers, sequences, users and user privileges —
// producing a structured Result that names every object on both sides
// (design doc: docs/DDL-COMPARE-DESIGN.md v1.3).
//
// The package is deliberately connection-agnostic: collectors take an
// already-open *sql.DB (the webapi layer owns datasource resolution), the
// diff engine is pure and table-testable, and per-class catalog queries
// degrade to a warning + empty set instead of failing the whole run (a
// TiDB build without information_schema.ROUTINES must not kill the table
// comparison; a scan account without mysql.user SELECT privilege degrades
// the users/privileges classes the same way).
package ddlcompare

import (
	"strconv"
	"time"
)

// Verdict is one category's (or the overall) conclusion.
type Verdict string

const (
	VerdictPass Verdict = "pass"
	VerdictWarn Verdict = "warn"
	VerdictFail Verdict = "fail"
)

// Worst returns the harsher of two verdicts (fail > warn > pass).
func Worst(a, b Verdict) Verdict {
	rank := map[Verdict]int{VerdictPass: 0, VerdictWarn: 1, VerdictFail: 2}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

// Options control what "一致" means for one run. Defaults are decided by
// the webapi layer; the pure engine just obeys.
type Options struct {
	// IgnoreCase folds identifier case (lowercase compare). MySQL/TiDB
	// identifiers are case-sensitive-by-platform, PG folds to lowercase —
	// default true keeps "Users" and "users" comparable.
	IgnoreCase bool `json:"ignore_case"`
	// LooseTypes is retained for request/JSON compat with v1.2 payloads.
	// Since v1.3 字段级对比取消 (A1), table rows compare PK + charset +
	// collation only, so the type-family switch no longer influences any
	// verdict; it is accepted and ignored.
	LooseTypes bool `json:"loose_types"`
	// MaxObjects caps the per-side object count (0 = DefaultMaxObjects).
	MaxObjects int `json:"max_objects"`
}

// DefaultMaxObjects mirrors ddlexport's defaultMaxObjects (20000): a
// runaway compare over a monster catalog fails fast with an actionable
// message instead of exhausting memory.
const DefaultMaxObjects = 20000

// TableMeta is one table's structural inventory under v1.3 口径: primary
// key + charset + collation (字段级对比已取消, A1; 索引不比较).
type TableMeta struct {
	Name string `json:"name"`
	// Charset is the table character set (MySQL/TiDB) or the database
	// encoding (PG tables carry no per-table charset; the database-level
	// value stands in, v1.3 口径).
	Charset string `json:"charset,omitempty"`
	// Collation is the table collation (MySQL/TiDB) or the database
	// lc_collate value (PG stand-in).
	Collation string `json:"collation,omitempty"`
	// PK is the primary-key column sequence in key order (empty = no PK).
	PK []string `json:"pk,omitempty"`
}

// UserMeta is one account's global-privilege fingerprint. Privileges is
// the dialect vocabulary: MySQL/TiDB = mysql.user global _priv column
// names; PG = rolsuper/rolcreatedb/rolcreaterole/rolreplication booleans
// plus granted role memberships (pg_auth_members member names only).
type UserMeta struct {
	Name       string   `json:"name"`
	Privileges []string `json:"privileges"`
}

// Snapshot is one side's inventory of one schema/database.
type Snapshot struct {
	// Dialect: "mysql" (source or TiDB target) or "postgres".
	Dialect string `json:"dialect"`
	// Schema is the compared schema (PG) or database (MySQL/TiDB) name.
	Schema     string      `json:"schema"`
	Tables     []TableMeta `json:"tables"`
	Views      []string    `json:"views"`
	Procedures []string    `json:"procedures"`
	Functions  []string    `json:"functions"`
	Triggers   []string    `json:"triggers"`
	Sequences  []string    `json:"sequences"`
	Users      []UserMeta  `json:"users"`
}

// Objects counts the snapshot's top-level objects against the MaxObjects
// cap — the ddlexport exporter's top-level口径 (tables, views, procedures,
// functions, triggers) plus the v1.3-added sequences class. User accounts
// are not schema objects and are excluded (a MySQL instance's account
// rows are not a catalog of exportable objects; documented in the design
// doc).
func (s *Snapshot) Objects() int {
	return len(s.Tables) + len(s.Views) + len(s.Procedures) + len(s.Functions) + len(s.Triggers) + len(s.Sequences)
}

// TableDiff is one table's comparison outcome. SourceOnly/TargetOnly mark
// one-sided tables; both-sided tables carry the PK/charset/collation deltas.
type TableDiff struct {
	Table string `json:"table"`
	// One-sided markers (mutually exclusive).
	SourceOnly bool    `json:"source_only,omitempty"`
	TargetOnly bool    `json:"target_only,omitempty"`
	Verdict    Verdict `json:"verdict"`
	// PK equality: nil PKDiff = both sides have no PK (equal).
	PKDiff *PKDiff `json:"pk_diff,omitempty"`
	// CharsetDiff/CollationDiff carry the per-attribute comparison (v1.3;
	// nil = equal / both absent).
	CharsetDiff   *AttrDiff `json:"charset_diff,omitempty"`
	CollationDiff *AttrDiff `json:"collation_diff,omitempty"`
}

// PKDiff is the primary-key comparison for one table.
type PKDiff struct {
	Source []string `json:"source"`
	Target []string `json:"target"`
	Equal  bool     `json:"equal"`
}

// AttrDiff is one table attribute's (charset/collation) comparison. Note
// carries the V4 direction reason when the attribute folds to warn
// (e.g. 源 utf8mb4 → 目标 utf8: 4 字节字符可能丢失).
type AttrDiff struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Equal  bool   `json:"equal"`
	Note   string `json:"note,omitempty"`
}

// NameSetDiff is the set comparison for name-listed classes (views,
// procedures, functions, triggers, sequences, users-existence).
type NameSetDiff struct {
	SourceOnly []string `json:"source_only,omitempty"`
	TargetOnly []string `json:"target_only,omitempty"`
	Common     []string `json:"common,omitempty"`
}

// PrivUserDiff is one account's privilege comparison (both sides' full
// token lists, honest disclosure). Equal is meaningful only when the
// category is Comparable.
type PrivUserDiff struct {
	User       string   `json:"user"`
	Source     []string `json:"source,omitempty"`
	Target     []string `json:"target,omitempty"`
	Equal      bool     `json:"equal"`
	SourceOnly bool     `json:"source_only,omitempty"`
	TargetOnly bool     `json:"target_only,omitempty"`
}

// PrivSetDiff is the privileges category payload (V1 分轨):
//   - same-dialect (both MySQL-wire): Comparable=true, the per-user global
//     privilege set diff IS the verdict basis (can fail), full lists.
//   - cross-dialect (any PG side): vocabularies are disjoint, so no
//     pass/fail conclusion is drawn — Comparable=false, the category warns
//     and both sides' raw token lists are disclosed verbatim.
type PrivSetDiff struct {
	Comparable   bool           `json:"comparable"`
	CrossDialect bool           `json:"cross_dialect,omitempty"`
	Users        []PrivUserDiff `json:"users"`
}

// Category is one compared object class.
type Category struct {
	// Key: tables | views | procedures | functions | triggers | sequences
	// | users | privileges.
	Key   string `json:"key"`
	Label string `json:"label"`
	// SourceCount/TargetCount are raw per-side object counts (list classes
	// keep the multiset counts; sets collapse for detail).
	SourceCount int `json:"source_count"`
	TargetCount int `json:"target_count"`
	// Matched: tables fully equal / names present on both sides.
	Matched int     `json:"matched"`
	Verdict Verdict `json:"verdict"`
	// Tables carries per-table detail (tables category).
	Tables []TableDiff `json:"tables,omitempty"`
	// Names carries the set diff for name-listed categories.
	Names *NameSetDiff `json:"names,omitempty"`
	// Privileges carries the per-user privilege diff (privileges category).
	Privileges *PrivSetDiff `json:"privileges,omitempty"`
	// Note is a category-level annotation rendered distinctly (V2):
	// e.g. sequences「目标端无此对象类型（TiDB/MySQL 无 SEQUENCE）」.
	Note string `json:"note,omitempty"`
}

// Endpoint labels one side in the result (credentials never included).
type Endpoint struct {
	// Kind: source | target.
	Kind   string `json:"kind"`
	Type   string `json:"type"` // postgres | mysql | tidb
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Schema string `json:"schema"`
}

// Label renders host:port/schema (credential-free, report banner use).
func (e Endpoint) Label() string {
	label := e.Schema
	if e.Host != "" {
		if e.Port != 0 {
			label = e.Host + ":" + strconv.Itoa(e.Port) + "/" + e.Schema
		} else {
			label = e.Host + "/" + e.Schema
		}
	}
	return label
}

// Result is the full comparison outcome for one run.
type Result struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Duration    string     `json:"duration"`
	Options     Options    `json:"options"`
	Source      Endpoint   `json:"source"`
	Target      Endpoint   `json:"target"`
	Categories  []Category `json:"categories"`
	Overall     Verdict    `json:"overall"`
	// Warnings carry degraded-class notes (e.g. a TiDB build without
	// information_schema.ROUTINES, or a scan account without mysql.user
	// SELECT privilege — the users/privileges classes degrade to empty).
	Warnings []string `json:"warnings,omitempty"`
}
