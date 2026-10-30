// normalize.go — the "what counts as equal" layer. Identifier case folding
// and the charset/collation directional-folding rules (V4) live here so the
// diff engine and the tests have a single truth (design doc §归一化规则).
//
// v1.3 对比面收窄后 the column type-family tables are gone (字段级对比取消,
// A1; 索引不比较): normalize.go now owns exactly what the tables category
// still compares — 主键 (via NormIdent) + 字符集/排序规则 (charset/collation
// directional fold, V4).
package ddlcompare

import "strings"

// NormIdent folds an identifier per the ignore-case option. Folding is
// lowercase (PG-native); case-sensitive compare keeps the raw spelling.
func NormIdent(s string, ignoreCase bool) string {
	if ignoreCase {
		return strings.ToLower(s)
	}
	return s
}

// ---- charset / collation directional fold (V4) ----

// charsetCanon canonicalizes a charset name: trims/lowercases and folds the
// MySQL "utf8" ≡ "utf8mb3" aliases onto one bucket ("utf8"). PG reports its
// database encoding as "UTF8" — the lowercased form "utf8" collides with
// the MySQL bucket, which is exactly what makes PG↔TiDB charset compare
// sane when the target speaks utf8mb4.
func charsetCanon(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "utf8mb3":
		return "utf8"
	}
	return s
}

// charsetSuperset reports whether a is a superset of b under the V4 fold
// table. The only declared superset relation is utf8mb4 ⊃ utf8 (mb3 = utf8
// after charsetCanon): folding is DIRECTION-LIMITED — a pair folds to equal
// only when the TARGET is the superset; the reverse direction warns (4-byte
// characters cannot survive in the smaller charset).
func charsetSuperset(a, b string) bool {
	return charsetCanon(a) == "utf8mb4" && charsetCanon(b) == "utf8"
}

// CharsetVerdict classifies a charset pair (V4): equal → pass; target
// superset (utf8 → utf8mb4) → pass (fold); source superset (utf8mb4 →
// utf8) → warn (4-byte loss risk); anything else (latin1 vs utf8mb4) → fail
// (a real character-set difference the fold table cannot reconcile).
func CharsetVerdict(srcCharset, tgtCharset string) (Verdict, bool, string) {
	s, t := charsetCanon(srcCharset), charsetCanon(tgtCharset)
	if s == "" && t == "" {
		return VerdictPass, true, ""
	}
	if s == t {
		return VerdictPass, true, ""
	}
	if charsetSuperset(t, s) {
		return VerdictPass, true, ""
	}
	if charsetSuperset(s, t) {
		return VerdictWarn, false, "源端字符集为超集：4 字节字符（如 emoji）在目标端可能丢失（utf8mb4→utf8）"
	}
	return VerdictFail, false, "字符集不同：源端 " + srcCharset + " ≠ 目标端 " + tgtCharset
}

// mysqlCollation is the parsed form of a MySQL/TiDB collation name
// "<charset>_<implementation>_<strength>", e.g. utf8mb4_general_ci →
// {utf8mb4, general, ci}; utf8mb4_bin → {utf8mb4, "", bin};
// utf8mb4_0900_ai_ci → {utf8mb4, 0900_ai, ci}.
type mysqlCollation struct {
	charset  string
	impl     string // implementation segment(s), "" for a bare <charset>_<bin>
	strength string // ci | cs | bin ("" = unrecognized form)
}

// parseCollation splits a collation name; ok=false marks a name that does
// not follow the MySQL <charset>_..._<ci|cs|bin> shape (PG locale
// collations like "en_US.UTF-8", "C", "default", or MySQL "binary").
func parseCollation(raw string) (c mysqlCollation, ok bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(raw)), "_")
	if len(parts) < 2 {
		return c, false
	}
	last := parts[len(parts)-1]
	switch last {
	case "ci", "cs", "bin":
	default:
		return c, false
	}
	c.charset = parts[0]
	c.strength = last
	if len(parts) > 2 {
		c.impl = strings.Join(parts[1:len(parts)-1], "_")
	}
	return c, true
}

// CollationVerdict classifies a collation pair under V4. Verdicts:
//   - identical names → pass; both absent → pass;
//   - both MySQL-style AND same implementation + strength: folds by the
//     same direction rule as the charset (target-superset → pass; source
//     superset → warn);
//   - same strength but DIFFERENT implementation (utf8mb4_general_ci vs
//     utf8mb4_unicode_ci) → warn (排序/大小写语义不同 — never folded);
//   - different strength (_ci vs _bin) → warn (大小写语义不同);
//   - one side not MySQL-style (PG locale / cross-dialect vocabulary) →
//     warn "不做结论" (mirror of V1's privilege vocabulary rule).
func CollationVerdict(srcCharset, srcCollation, tgtCharset, tgtCollation string) (Verdict, bool, string) {
	sc, tc := strings.TrimSpace(srcCollation), strings.TrimSpace(tgtCollation)
	if sc == "" && tc == "" {
		return VerdictPass, true, ""
	}
	if sc == tc {
		return VerdictPass, true, ""
	}
	sp, sok := parseCollation(sc)
	tp, tok := parseCollation(tc)
	if sok && tok {
		if sp.impl == tp.impl && sp.strength == tp.strength {
			if cs, eq, note := CharsetVerdict(srcCharset, tgtCharset); cs == VerdictFail {
				return cs, eq, note
			}
			// same implementation/strength: direction-limited fold applies.
			if charsetSuperset(tp.charset, sp.charset) || sp.charset == tp.charset {
				return VerdictPass, true, ""
			}
			if charsetSuperset(sp.charset, tp.charset) {
				return VerdictWarn, false, "排序规则对应字符集反向（源为超集）：4 字节字符可能丢失"
			}
			return VerdictWarn, false, "排序规则字符集不同：源端 " + srcCollation + " ≠ 目标端 " + tgtCollation
		}
		if sp.strength == tp.strength {
			return VerdictWarn, false, "排序规则同强度但实现不同：源端 " + srcCollation + " ≠ 目标端 " + tgtCollation + "（如 _general vs _unicode）"
		}
		return VerdictWarn, false, "排序规则强度不同（大小写语义不同）：源端 " + srcCollation + " ≠ 目标端 " + tgtCollation
	}
	return VerdictWarn, false, "排序规则词汇表跨方言不相交（PG locale 与 MySQL 命名不互认），不做结论：源端 " + srcCollation + " ≠ 目标端 " + tgtCollation
}
