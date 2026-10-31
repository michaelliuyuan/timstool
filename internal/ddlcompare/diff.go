// diff.go — the pure comparison engine. Inputs are two Snapshots + one
// Options; the output names every object on both sides. Verdict policy
// (v1.3):
//   - tables: one-sided table, PK difference, charset difference (unfolded
//     direction) → fail; charset reverse-superset / collation semantic
//     differences → warn (V4 direction-limited fold);
//   - views / procedures / functions / triggers / sequences: count + name
//     sets — any difference → fail; a PG source's sequences against a
//     MySQL-wire target (which has no SEQUENCE type) carry the「目标端无此
//     对象类型」annotation (V2/A3);
//   - users: count + name sets — any difference → fail (V1: user
//     existence compares across dialects, both sides are name lists);
//   - privileges: SAME-dialect (both mysql-wire) → per-user global
//     privilege set difference, can fail, full lists; CROSS-dialect (any
//     PG side) → vocabulary disjoint, no pass/fail conclusion: warn +
//     both sides' raw token lists disclosed (V1).
package ddlcompare

import (
	"sort"
)

// Diff compares two snapshots under opts. src/tgt may be nil (collection
// failed on that side) — the caller turns that into a failed Result with
// the collection error; the engine itself expects both present.
func Diff(src, tgt *Snapshot, opts Options) *Result {
	res := &Result{Options: opts}
	res.Categories = []Category{
		diffTables(src, tgt, opts),
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

func diffTables(src, tgt *Snapshot, opts Options) Category {
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
			td, matched := compareTable(s, t, opts)
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
// 字符集 + 排序规则; 字段级与索引级已取消). matched = fully equal (verdict
// pass).
func compareTable(s, t *TableMeta, opts Options) (TableDiff, bool) {
	td := TableDiff{Table: s.Name, Verdict: VerdictPass}

	// primary key (exact, key-ordered).
	pkEqual := normJoin(s.PK, opts) == normJoin(t.PK, opts)
	td.PKDiff = &PKDiff{Source: s.PK, Target: t.PK, Equal: pkEqual}
	if !pkEqual {
		td.Verdict = VerdictFail
	}

	// charset (V4 direction-limited fold).
	if cv, ceq, cnote := CharsetVerdict(s.Charset, t.Charset); !ceq {
		td.CharsetDiff = &AttrDiff{Source: s.Charset, Target: t.Charset, Equal: false, Note: cnote}
		td.Verdict = Worst(td.Verdict, cv)
	} else if s.Charset != t.Charset {
		// folded-equal (utf8 → utf8mb4): record the pair as equal so the
		// report shows the fold, not a difference.
		td.CharsetDiff = &AttrDiff{Source: s.Charset, Target: t.Charset, Equal: true}
	}

	// collation (V4: same-strength different-impl and vocabulary-mismatch
	// both warn; never a silent fold).
	if ov, oeq, onote := CollationVerdict(s.Charset, s.Collation, t.Charset, t.Collation); !oeq {
		td.CollationDiff = &AttrDiff{Source: s.Collation, Target: t.Collation, Equal: false, Note: onote}
		td.Verdict = Worst(td.Verdict, ov)
	} else if s.Collation != t.Collation {
		td.CollationDiff = &AttrDiff{Source: s.Collation, Target: t.Collation, Equal: true}
	}

	return td, td.Verdict == VerdictPass
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
// set). Per V2/A3, the MySQL-wire collector does not inventory SEQUENCE
// objects (TiDB v7.1.9 itself supports them), so a PG source with
// sequences fails honestly with the「目标端无此对象类型」annotation
// carried on the category (rendered distinctly, not as a regular
// set-difference FAIL).
func diffSequences(src, tgt *Snapshot, opts Options) Category {
	cat := nameCategory("sequences", "序列", src.Sequences, tgt.Sequences, opts)
	if len(src.Sequences) > 0 && tgt.Dialect == "mysql" {
		cat.Note = "目标端无此对象类型（采集器未实现 MySQL 侧 SEQUENCE 采集；TiDB v7.1.9 实际支持 SEQUENCE）：PG 源含序列时总体结论预期为 FAIL"
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
// booleans + memberships), so no pass/fail conclusion is drawn — warn +
// both sides' raw token lists disclosed. Either side's users degraded to
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
		// no conclusion — warn, never pass/fail.
		cat.Verdict = VerdictWarn
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
