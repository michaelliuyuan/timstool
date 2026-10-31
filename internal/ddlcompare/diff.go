// diff.go — the pure comparison engine. Inputs are two Snapshots + one
// Options; the output names every object on both sides. Verdict policy
// (v1.4, user ruling 11:13「跨方言固有差异不设为对比错误，加小提示」):
//   - tables: one-sided table, PK difference → fail (structural, dialect
//     independent); charset/collation differences → fail/warn in
//     SAME-dialect runs, but in CROSS-dialect runs (PG source vs MySQL-wire
//     target) they downgrade to hint (提示级) — charset values and collation
//     vocabularies are dialect-inherent and carry no migration-correctness
//     signal. A both-sided table whose only cross-dialect deltas are hints
//     still counts as matched (一致) for the category count;
//   - views / procedures / functions / triggers / sequences: count + name
//     sets — any difference → fail (single-side existence is a real diff);
//     a PG source's sequences against a MySQL-wire target without any carry
//     the「目标端无序列对象」annotation (V2/A3);
//   - users: count + name sets — any difference → fail (V1: user existence
//     compares across dialects, both sides are name lists);
//   - privileges: SAME-dialect (both mysql-wire) → per-user global privilege
//     set difference, can fail, full lists; CROSS-dialect (any PG side) →
//     vocabulary disjoint, hint (提示级) + both sides' raw token lists
//     disclosed (V1; warn → hint per v1.4);
//   - overall: Worst over categories EXCLUDING hint — hint never turns the
//     overall conclusion away from pass.
package ddlcompare

import (
	"sort"
)

// Diff compares two snapshots under opts. src/tgt may be nil (collection
// failed on that side) — the caller turns that into a failed Result with
// the collection error; the engine itself expects both present.
func Diff(src, tgt *Snapshot, opts Options) *Result {
	res := &Result{Options: opts}
	crossDialect := src.Dialect != tgt.Dialect
	res.Categories = []Category{
		diffTables(src, tgt, opts, crossDialect),
		nameCategory("views", "视图", src.Views, tgt.Views, opts),
		nameCategory("procedures", "存储过程", src.Procedures, tgt.Procedures, opts),
		nameCategory("functions", "自定义函数", src.Functions, tgt.Functions, opts),
		nameCategory("triggers", "触发器", src.Triggers, tgt.Triggers, opts),
		diffSequences(src, tgt, opts),
		nameCategory("users", "用户", userNames(src.Users), userNames(tgt.Users), opts),
		diffPrivileges(src, tgt, opts),
	}
	res.Overall = VerdictPass
	for _, c := range res.Categories {
		if c.Verdict == VerdictHint {
			// v1.4: 提示级 never drives the overall conclusion.
			continue
		}
		res.Overall = Worst(res.Overall, c.Verdict)
	}
	return res
}

// userNames projects the user inventory down to the existence name list
// (User@Host on mysql-wire, rolname on PG).
func userNames(users []UserMeta) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Name)
	}
	return out
}

// tableIndex builds the normalized-name → TableMeta map.
func tableIndex(tables []TableMeta, opts Options) map[string]*TableMeta {
	m := make(map[string]*TableMeta, len(tables))
	for i := range tables {
		m[NormIdent(tables[i].Name, opts.IgnoreCase)] = &tables[i]
	}
	return m
}

func diffTables(src, tgt *Snapshot, opts Options, crossDialect bool) Category {
	cat := Category{Key: "tables", Label: "表"}
	si, ti := tableIndex(src.Tables, opts), tableIndex(tgt.Tables, opts)
	cat.SourceCount, cat.TargetCount = len(src.Tables), len(tgt.Tables)

	names := make([]string, 0, len(si)+len(ti))
	seen := map[string]bool{}
	for n := range si {
		names = append(names, n)
		seen[n] = true
	}
	for n := range ti {
		if !seen[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	for _, n := range names {
		s, inS := si[n]
		t, inT := ti[n]
		switch {
		case inS && !inT:
			cat.Tables = append(cat.Tables, TableDiff{
				Table: s.Name, SourceOnly: true, Verdict: VerdictFail,
			})
		case !inS && inT:
			cat.Tables = append(cat.Tables, TableDiff{
				Table: t.Name, TargetOnly: true, Verdict: VerdictFail,
			})
		default:
			td, matched := compareTable(s, t, opts, crossDialect)
			cat.Tables = append(cat.Tables, td)
			if matched {
				cat.Matched++
			}
		}
	}
	cat.Verdict = VerdictPass
	for _, td := range cat.Tables {
		cat.Verdict = Worst(cat.Verdict, td.Verdict)
	}
	return cat
}

// compareTable diffs two present tables under the v1.3 口径 (主键 +
// 字符集 + 排序规则; 字段级与索引级已取消). matched = practically equal:
// PK equal + charset equal + collation equal — EXCEPT that in cross-dialect
// runs (v1.4) a collation inequality that only reflects the disjoint
// dialect vocabularies does not break matched (the twins ARE consistent;
// the difference renders as a hint, never as 不一致).
func compareTable(s, t *TableMeta, opts Options, crossDialect bool) (TableDiff, bool) {
	td := TableDiff{Table: s.Name, Verdict: VerdictPass}

	// primary key (exact, key-ordered) — structural, dialect independent:
	// a PK difference is always a fail, cross-dialect or not.
	pkEqual := normJoin(s.PK, opts) == normJoin(t.PK, opts)
	td.PKDiff = &PKDiff{Source: s.PK, Target: t.PK, Equal: pkEqual}
	if !pkEqual {
		td.Verdict = VerdictFail
	}

	// charset (V4 direction-limited fold; v1.4: cross-dialect value diffs
	// downgrade to hint — charset values are dialect-inherent, not a
	// migration-correctness signal).
	charsetEqual := true
	if cv, ceq, cnote := CharsetVerdict(s.Charset, t.Charset); !ceq {
		charsetEqual = false
		if crossDialect {
			cnote = "跨方言字符集差异仅提示（不判错）：" + cnote
			td.Verdict = Worst(td.Verdict, VerdictHint)
		} else {
			td.Verdict = Worst(td.Verdict, cv)
		}
		td.CharsetDiff = &AttrDiff{Source: s.Charset, Target: t.Charset, Equal: false, Note: cnote}
	} else if s.Charset != "" {
		// B 口径 (2026-10-31 刘源拍板): verdict-equal pairs (identical
		// non-empty, or folded utf8→utf8mb4) are always emitted so the report
		// shows 取到且相等 rather than an ambiguous absence; double-empty
		// (equal-by-absence, MySQL-family rows without TABLE_COLLATION)
		// stays nil and renders '—'.
		td.CharsetDiff = &AttrDiff{Source: s.Charset, Target: t.Charset, Equal: true}
	}

	// collation (V4: same-strength different-impl and vocabulary-mismatch
	// both warn in same-dialect runs, never a silent fold; v1.4:
	// cross-dialect vocabulary mismatches downgrade to hint).
	collationEqual := true
	if ov, oeq, onote := CollationVerdict(s.Charset, s.Collation, t.Charset, t.Collation); !oeq {
		collationEqual = false
		if crossDialect {
			onote = "跨方言排序规则词汇表不相交，仅提示（不判错）"
			td.Verdict = Worst(td.Verdict, VerdictHint)
		} else {
			td.Verdict = Worst(td.Verdict, ov)
		}
		td.CollationDiff = &AttrDiff{Source: s.Collation, Target: t.Collation, Equal: false, Note: onote}
	} else if s.Collation != "" {
		// B 口径同守: identical non-empty collations are emitted as
		// Equal:true; double-empty stays nil → '—'.
		td.CollationDiff = &AttrDiff{Source: s.Collation, Target: t.Collation, Equal: true}
	}

	matched := pkEqual && charsetEqual && (collationEqual || crossDialect)
	return td, matched
}

// normJoin renders the PK sequence as a comparable "a,b" string.
func normJoin(cols []string, opts Options) string {
	out := ""
	for i, c := range cols {
		if i > 0 {
			out += ","
		}
		out += NormIdent(c, opts.IgnoreCase)
	}
	return out
}

// diffSequences compares the sequences class by existence (count + name
// set). The MySQL-wire collector inventories TiDB SEQUENCE objects via
// TABLE_TYPE='SEQUENCE', so a TiDB target compares for real; a target
// that carries none (vanilla MySQL instance, or a TiDB database without
// sequences) makes a PG source with sequences fail honestly with the
// 「目标端无序列对象」annotation carried on the category (rendered
// distinctly, not as a regular set-difference FAIL).
func diffSequences(src, tgt *Snapshot, opts Options) Category {
	cat := nameCategory("sequences", "序列", src.Sequences, tgt.Sequences, opts)
	if len(src.Sequences) > 0 && tgt.Dialect == "mysql" && len(tgt.Sequences) == 0 {
		cat.Note = "目标端无序列对象（TiDB 支持 SEQUENCE，本目标库未采集到；MySQL 实例则无此对象类型）：PG 源含序列时总体结论预期为 FAIL"
	}
	return cat
}

// nameCategory compares two name lists: count + set difference — any
// difference fails the class (design §4). Sets collapse duplicates for
// the detail panes, but the counts stay raw: a multiset difference (same
// name twice on one side) fails even when both sets look equal. Trigger
// lists arrive table-qualified from the collectors, which is what makes
// the set dimension sound for cross-table same-named triggers.
func nameCategory(key, label string, src, tgt []string, opts Options) Category {
	cat := Category{Key: key, Label: label, Names: &NameSetDiff{}}
	sset := normSet(src, opts.IgnoreCase)
	tset := normSet(tgt, opts.IgnoreCase)
	cat.SourceCount, cat.TargetCount = len(src), len(tgt)
	for n := range sset {
		if _, ok := tset[n]; ok {
			cat.Names.Common = append(cat.Names.Common, n)
		} else {
			cat.Names.SourceOnly = append(cat.Names.SourceOnly, n)
		}
	}
	for n := range tset {
		if _, ok := sset[n]; !ok {
			cat.Names.TargetOnly = append(cat.Names.TargetOnly, n)
		}
	}
	sort.Strings(cat.Names.Common)
	sort.Strings(cat.Names.SourceOnly)
	sort.Strings(cat.Names.TargetOnly)
	cat.Matched = len(cat.Names.Common)
	if len(cat.Names.SourceOnly) == 0 && len(cat.Names.TargetOnly) == 0 &&
		cat.SourceCount == cat.TargetCount {
		cat.Verdict = VerdictPass
	} else {
		cat.Verdict = VerdictFail
	}
	return cat
}

// diffPrivileges compares the per-user global privilege fingerprints (V1
// 分轨). Same-dialect (both mysql-wire, i.e. the source is mysql): the two
// vocabularies are the same mysql.user _priv columns, so the set
// difference is a real verdict (can fail; full lists). Cross-dialect (the
// source is PG): the vocabulary is disjoint (mysql _priv columns vs PG
// booleans + memberships), so no pass/fail conclusion is drawn — hint
// (提示级, v1.4; was warn) + both sides' raw token lists disclosed. Either
// side's users degraded to
// empty (no privilege to read mysql.user / pg_roles) is handled honestly:
// the lists simply show what each side could collect, and the collector
// warning explains the cause.
func diffPrivileges(src, tgt *Snapshot, opts Options) Category {
	cat := Category{Key: "privileges", Label: "权限"}
	comparable := src.Dialect == "mysql" && tgt.Dialect == "mysql"
	cat.Privileges = &PrivSetDiff{
		Comparable:   comparable,
		CrossDialect: !comparable,
	}
	if !comparable {
		cat.Note = "跨方言权限词汇表不相交，不做结论"
	}
	si, ti := privilegeIndex(src.Users, opts), privilegeIndex(tgt.Users, opts)
	cat.SourceCount, cat.TargetCount = len(src.Users), len(tgt.Users)

	names := make([]string, 0, len(si)+len(ti))
	seen := map[string]bool{}
	for n := range si {
		names = append(names, n)
		seen[n] = true
	}
	for n := range ti {
		if !seen[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	matched := 0
	verdict := VerdictPass
	for _, n := range names {
		s, inS := si[n]
		t, inT := ti[n]
		row := PrivUserDiff{User: n}
		sset, tset := map[string]bool{}, map[string]bool{}
		switch {
		case inS && !inT:
			row.SourceOnly = true
			row.Source = s.Privileges
			for _, p := range s.Privileges {
				sset[p] = true
			}
		case !inS && inT:
			row.TargetOnly = true
			row.Target = t.Privileges
			for _, p := range t.Privileges {
				tset[p] = true
			}
		default:
			row.Source, row.Target = s.Privileges, t.Privileges
			for _, p := range s.Privileges {
				sset[p] = true
			}
			for _, p := range t.Privileges {
				tset[p] = true
			}
		}
		row.Equal = setsEqual(sset, tset)
		cat.Privileges.Users = append(cat.Privileges.Users, row)
		if row.Equal {
			matched++
		} else if comparable {
			verdict = VerdictFail
		}
	}
	cat.Matched = matched
	if comparable {
		cat.Verdict = verdict
	} else if cat.SourceCount == 0 && cat.TargetCount == 0 {
		// nothing to compare on either side (both empty) → pass; the
		// collector warning explains any one-sided degradation.
		cat.Verdict = VerdictPass
	} else {
		// cross-dialect with at least one account: vocabulary disjoint,
		// no conclusion — hint (提示级, v1.4; was warn), never pass/fail.
		cat.Verdict = VerdictHint
	}
	return cat
}

// privilegeIndex builds the normalized-user → UserMeta map.
func privilegeIndex(users []UserMeta, opts Options) map[string]UserMeta {
	m := make(map[string]UserMeta, len(users))
	for _, u := range users {
		m[NormIdent(u.Name, opts.IgnoreCase)] = u
	}
	return m
}

// setsEqual compares two token sets.
func setsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// normSet deduplicates a name list into a normalized set.
func normSet(names []string, ignoreCase bool) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[NormIdent(n, ignoreCase)] = true
	}
	return m
}
