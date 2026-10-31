package ddlcompare

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func optLoose() Options {
	return Options{IgnoreCase: true, LooseTypes: true, MaxObjects: DefaultMaxObjects}
}

func optStrict() Options {
	return Options{IgnoreCase: false, LooseTypes: true, MaxObjects: DefaultMaxObjects}
}

// ---- charset / collation directional fold (V4) ----

func TestCharsetVerdict(t *testing.T) {
	cases := []struct {
		name, src, tgt string
		wantV          Verdict
		wantEqual      bool
	}{
		{"same", "utf8", "utf8", VerdictPass, true},
		{"same case", "UTF8", "utf8", VerdictPass, true},
		{"mb3 alias", "utf8mb3", "utf8", VerdictPass, true},
		{"both empty", "", "", VerdictPass, true},
		{"fold target superset", "utf8", "utf8mb4", VerdictPass, true},
		{"PG UTF8 to utf8mb4", "UTF8", "utf8mb4", VerdictPass, true},
		{"reverse source superset", "utf8mb4", "utf8", VerdictWarn, false},
		{"unrelated", "latin1", "utf8mb4", VerdictFail, false},
	}
	for _, c := range cases {
		v, eq, _ := CharsetVerdict(c.src, c.tgt)
		if v != c.wantV || eq != c.wantEqual {
			t.Errorf("%s: CharsetVerdict(%q,%q) = %q/%v, want %q/%v",
				c.name, c.src, c.tgt, v, eq, c.wantV, c.wantEqual)
		}
	}
}

func TestCollationVerdict(t *testing.T) {
	cases := []struct {
		name                string
		srcCharset, srcColl string
		tgtCharset, tgtColl string
		wantV               Verdict
		wantEqual           bool
	}{
		{"identical", "utf8mb4", "utf8mb4_general_ci", "utf8mb4", "utf8mb4_general_ci", VerdictPass, true},
		{"both absent", "", "", "", "", VerdictPass, true},
		{"fold target superset same impl", "utf8", "utf8_general_ci", "utf8mb4", "utf8mb4_general_ci", VerdictPass, true},
		{"reverse source superset", "utf8mb4", "utf8mb4_general_ci", "utf8", "utf8_general_ci", VerdictWarn, false},
		{"general vs unicode same strength", "utf8mb4", "utf8mb4_general_ci", "utf8mb4", "utf8mb4_unicode_ci", VerdictWarn, false},
		{"ci vs bin strength differ", "utf8mb4", "utf8mb4_general_ci", "utf8mb4", "utf8mb4_bin", VerdictWarn, false},
		{"cross dialect PG locale", "utf8mb4", "en_US.UTF-8", "utf8mb4", "utf8mb4_bin", VerdictWarn, false},
		{"cross dialect C", "utf8mb4", "C", "utf8mb4", "utf8mb4_general_ci", VerdictWarn, false},
		{"different charset vocab", "utf8mb4", "utf8mb4_unicode_ci", "utf8", "utf8_general_ci", VerdictWarn, false},
	}
	for _, c := range cases {
		v, eq, _ := CollationVerdict(c.srcCharset, c.srcColl, c.tgtCharset, c.tgtColl)
		if v != c.wantV || eq != c.wantEqual {
			t.Errorf("%s: CollationVerdict = %q/%v, want %q/%v",
				c.name, v, eq, c.wantV, c.wantEqual)
		}
	}
}

func TestParseCollation(t *testing.T) {
	ok := func(raw, charset, impl, strength string) {
		t.Helper()
		c, fine := parseCollation(raw)
		if !fine || c.charset != charset || c.impl != impl || c.strength != strength {
			t.Errorf("parseCollation(%q) = %+v/%v, want %s/%s/%s", raw, c, fine, charset, impl, strength)
		}
	}
	ok("utf8mb4_general_ci", "utf8mb4", "general", "ci")
	ok("utf8mb4_bin", "utf8mb4", "", "bin")
	ok("utf8mb4_0900_ai_ci", "utf8mb4", "0900_ai", "ci")
	ok("latin1_swedish_ci", "latin1", "swedish", "ci")
	if _, fine := parseCollation("en_US.UTF-8"); fine {
		t.Error("PG locale must not parse as a MySQL collation")
	}
	if _, fine := parseCollation("C"); fine {
		t.Error("C collation must not parse")
	}
}

// ---- snapshots ----

func snapA() *Snapshot {
	return &Snapshot{
		Dialect: "postgres", Schema: "public",
		Tables: []TableMeta{
			{Name: "users", Charset: "UTF8", Collation: "en_US.UTF-8", PK: []string{"id"}},
			{Name: "orders", Charset: "UTF8", Collation: "en_US.UTF-8", PK: []string{"id"}},
		},
		Views:      []string{"v_users"},
		Procedures: []string{"sp_recalc"},
		Functions:  []string{"fn_tax", "fn_greet"},
		Triggers:   []string{"tg_audit"},
		Sequences:  []string{"seq_users", "seq_orders"},
		Users: []UserMeta{
			{Name: "appuser", Privileges: []string{"SUPER", "CREATEDB", "MEMBER:reader"}},
			{Name: "reporter", Privileges: []string{"MEMBER:reader"}},
		},
	}
}

func snapBEqual() *Snapshot {
	return &Snapshot{
		Dialect: "mysql", Schema: "migration_test",
		Tables: []TableMeta{
			{Name: "users", Charset: "utf8mb4", Collation: "utf8mb4_bin", PK: []string{"id"}},
			{Name: "orders", Charset: "utf8mb4", Collation: "utf8mb4_bin", PK: []string{"id"}},
		},
		Views:      []string{"v_users"},
		Procedures: []string{"sp_recalc"},
		Functions:  []string{"fn_tax", "fn_greet"},
		Triggers:   []string{"tg_audit"},
		Sequences:  nil,
		Users: []UserMeta{
			{Name: "appuser", Privileges: []string{"Select_priv", "Insert_priv"}},
			{Name: "reporter", Privileges: []string{"Select_priv"}},
		},
	}
}

func catByKey(t *testing.T, res *Result, key string) *Category {
	t.Helper()
	for i := range res.Categories {
		if res.Categories[i].Key == key {
			return &res.Categories[i]
		}
	}
	t.Fatalf("category %q missing", key)
	return nil
}

func TestDiffEqual(t *testing.T) {
	// PG→TiDB (cross-dialect, v1.4): charset folds (UTF8 → utf8mb4, target
	// superset); collation vocabularies differ → hint (提示级), and the
	// twins still count as matched (practical equality); sequences (PG has
	// them, target carries none) → fail (single-side existence); privileges
	// cross-dialect → hint. Overall fail (sequences).
	res := Diff(snapA(), snapBEqual(), optLoose())
	if res.Overall != VerdictFail {
		t.Fatalf("overall = %q, want fail; categories: %+v", res.Overall, res.Categories)
	}
	tb := catByKey(t, res, "tables")
	if tb.SourceCount != 2 || tb.TargetCount != 2 || tb.Matched != 2 {
		t.Errorf("tables counts = %d/%d/%d, want 2/2/2 (cross-dialect collation vocab does not break matched, v1.4)", tb.SourceCount, tb.TargetCount, tb.Matched)
	}
	if tb.Verdict != VerdictHint {
		t.Errorf("tables category verdict = %q, want hint (提示级)", tb.Verdict)
	}
	// users existence: both sides have appuser + reporter → pass.
	uc := catByKey(t, res, "users")
	if uc.Verdict != VerdictPass || uc.Matched != 2 {
		t.Errorf("users: verdict=%q matched=%d, want pass/2", uc.Verdict, uc.Matched)
	}
	// sequences: PG 源含序列 → 目标端无序列对象注解 + fail.
	sc := catByKey(t, res, "sequences")
	if sc.Verdict != VerdictFail || sc.Note == "" {
		t.Errorf("sequences: verdict=%q note=%q, want fail + annotation", sc.Verdict, sc.Note)
	}
	if !strings.Contains(sc.Note, "目标端无序列对象") {
		t.Errorf("sequences note must carry the target-no-sequence annotation: %q", sc.Note)
	}
	// privileges cross-dialect: hint (提示级, v1.4), not comparable.
	pc := catByKey(t, res, "privileges")
	if pc.Verdict != VerdictHint || pc.Privileges == nil || pc.Privileges.Comparable {
		t.Fatalf("privileges cross-dialect: verdict=%q comparable=%v, want hint/false", pc.Verdict, pc.Privileges != nil && pc.Privileges.Comparable)
	}
	if !strings.Contains(pc.Note, "跨方言权限词汇表不相交") {
		t.Errorf("privileges cross-dialect note must be carried on the category (E2/P3): %q", pc.Note)
	}
	if len(pc.Privileges.Users) != 2 {
		t.Errorf("privileges users = %d, want 2 (full disclosure)", len(pc.Privileges.Users))
	}
	// tables: charset folded equal, collation hint (vocab mismatch).
	for _, td := range tb.Tables {
		if td.CharsetDiff != nil && !td.CharsetDiff.Equal {
			t.Errorf("charset %s must fold equal (UTF8 → utf8mb4): %+v", td.Table, td.CharsetDiff)
		}
		if td.CollationDiff == nil || td.CollationDiff.Equal {
			t.Errorf("collation %s must disclose the PG-locale vs MySQL vocab delta: %+v", td.Table, td.CollationDiff)
		}
		if td.Verdict != VerdictHint {
			t.Errorf("table %s verdict = %q, want hint (collation vocab, v1.4)", td.Table, td.Verdict)
		}
	}
}

func TestDiffSameDialectPrivileges(t *testing.T) {
	// MySQL 源 → MySQL 目标 (同方言): privilege vocabulary comparable and
	// the set difference is the verdict.
	src := &Snapshot{Dialect: "mysql", Schema: "a", Users: []UserMeta{
		{Name: "app", Privileges: []string{"Select_priv", "Insert_priv"}},
	}}
	tgt := &Snapshot{Dialect: "mysql", Schema: "b", Users: []UserMeta{
		{Name: "app", Privileges: []string{"Select_priv"}},
	}}
	res := Diff(src, tgt, optLoose())
	pc := catByKey(t, res, "privileges")
	if !pc.Privileges.Comparable {
		t.Fatal("mysql→mysql privileges must be comparable")
	}
	if pc.Verdict != VerdictFail {
		t.Fatalf("same-dialect privilege diff verdict = %q, want fail", pc.Verdict)
	}
	u := pc.Privileges.Users[0]
	if u.Equal {
		t.Errorf("privilege set difference must be flagged unequal: %+v", u)
	}
	if !reflect.DeepEqual(u.Source, []string{"Select_priv", "Insert_priv"}) || !reflect.DeepEqual(u.Target, []string{"Select_priv"}) {
		t.Errorf("privilege lists must be disclosed fully: %+v", u)
	}
	if res.Overall != VerdictFail {
		t.Error("same-dialect privilege diff must fail overall")
	}

	// Equal sets → pass.
	tgt = &Snapshot{Dialect: "mysql", Schema: "b", Users: []UserMeta{
		{Name: "app", Privileges: []string{"Select_priv", "Insert_priv"}},
	}}
	res = Diff(src, tgt, optLoose())
	if pc := catByKey(t, res, "privileges"); pc.Verdict != VerdictPass || !pc.Privileges.Users[0].Equal {
		t.Errorf("equal privilege sets: verdict=%q, want pass", pc.Verdict)
	}
}

func TestDiffSameDialectTablesFold(t *testing.T) {
	// 同方言 MySQL→MySQL: utf8→utf8mb4 folds equal; utf8mb4→utf8 reverse
	// warns; general_ci vs unicode_ci warns.
	src := &Snapshot{Dialect: "mysql", Schema: "a", Tables: []TableMeta{
		{Name: "t1", Charset: "utf8", Collation: "utf8_general_ci", PK: []string{"id"}},
		{Name: "t2", Charset: "utf8mb4", Collation: "utf8mb4_general_ci", PK: []string{"id"}},
	}}
	tgt := &Snapshot{Dialect: "mysql", Schema: "b", Tables: []TableMeta{
		{Name: "t1", Charset: "utf8mb4", Collation: "utf8mb4_general_ci", PK: []string{"id"}},
		{Name: "t2", Charset: "utf8", Collation: "utf8_general_ci", PK: []string{"id"}},
	}}
	res := Diff(src, tgt, optLoose())
	tb := catByKey(t, res, "tables")
	byName := map[string]TableDiff{}
	for _, td := range tb.Tables {
		byName[td.Table] = td
	}
	// t1: fold target-superset → pass (both attributes equal-folded).
	if d := byName["t1"]; d.Verdict != VerdictPass || d.CharsetDiff == nil || !d.CharsetDiff.Equal {
		t.Errorf("t1 utf8→utf8mb4 must fold equal: %+v", d)
	}
	// t2: reverse utf8mb4→utf8 → warn.
	if d := byName["t2"]; d.Verdict != VerdictWarn || d.CharsetDiff == nil || d.CharsetDiff.Equal {
		t.Errorf("t2 utf8mb4→utf8 must warn: %+v", d)
	}
}

func TestDiffMissingAndExtraTable(t *testing.T) {
	tgt := snapBEqual()
	tgt.Tables = tgt.Tables[:1] // drop orders
	res := Diff(snapA(), tgt, optLoose())
	tb := catByKey(t, res, "tables")
	if tb.Verdict != VerdictFail || res.Overall != VerdictFail {
		t.Fatal("missing table must fail the tables category and overall")
	}
	found := false
	for _, td := range tb.Tables {
		if td.SourceOnly && td.Table == "orders" {
			found = true
		}
	}
	if !found {
		t.Error("orders must be listed as source-only with its name")
	}

	src := snapA()
	src.Tables = src.Tables[:1]
	tgt2 := snapBEqual()
	res = Diff(src, tgt2, optLoose())
	tb = catByKey(t, res, "tables")
	found = false
	for _, td := range tb.Tables {
		if td.TargetOnly && td.Table == "orders" {
			found = true
		}
	}
	if !found || tb.Verdict != VerdictFail {
		t.Error("extra target table must be listed as target-only and fail")
	}
}

func TestDiffPK(t *testing.T) {
	tgt := snapBEqual()
	tgt.Tables[0].PK = []string{"id", "name"}
	res := Diff(snapA(), tgt, optLoose())
	tb := catByKey(t, res, "tables")
	var row *TableDiff
	for i := range tb.Tables {
		if tb.Tables[i].Table == "users" {
			row = &tb.Tables[i]
		}
	}
	if row == nil || row.PKDiff.Equal {
		t.Fatal("PK difference must be flagged")
	}
	if !reflect.DeepEqual(row.PKDiff.Target, []string{"id", "name"}) {
		t.Errorf("PK target = %v", row.PKDiff.Target)
	}
	if row.Verdict != VerdictFail {
		t.Error("PK difference must fail the table")
	}
}

func TestDiffNameCategories(t *testing.T) {
	tgt := snapBEqual()
	tgt.Views = []string{"v_users", "v_orders"}
	tgt.Functions = []string{"fn_tax"}
	tgt.Triggers = nil
	res := Diff(snapA(), tgt, optLoose())
	v := catByKey(t, res, "views")
	if v.Verdict != VerdictFail || !reflect.DeepEqual(v.Names.TargetOnly, []string{"v_orders"}) {
		t.Errorf("views: verdict=%q targetOnly=%v", v.Verdict, v.Names.TargetOnly)
	}
	if v.SourceCount != 1 || v.TargetCount != 2 || v.Matched != 1 {
		t.Errorf("views counts = %d/%d/%d, want 1/2/1", v.SourceCount, v.TargetCount, v.Matched)
	}
	f := catByKey(t, res, "functions")
	if !reflect.DeepEqual(f.Names.SourceOnly, []string{"fn_greet"}) {
		t.Errorf("functions sourceOnly = %v, want [fn_greet]", f.Names.SourceOnly)
	}
	if !reflect.DeepEqual(f.Names.Common, []string{"fn_tax"}) {
		t.Errorf("functions common = %v, want [fn_tax]", f.Names.Common)
	}
	tr := catByKey(t, res, "triggers")
	if tr.Verdict != VerdictFail || !reflect.DeepEqual(tr.Names.SourceOnly, []string{"tg_audit"}) {
		t.Errorf("triggers: verdict=%q sourceOnly=%v", tr.Verdict, tr.Names.SourceOnly)
	}
	if res.Overall != VerdictFail {
		t.Error("overall must fail when a name category fails")
	}
}

func TestDiffIgnoreCase(t *testing.T) {
	src := snapA()
	src.Tables[0].Name = "Users"
	src.Views = []string{"V_USERS"}
	tgt := snapBEqual()
	// case-insensitive (default): tables exist on both sides.
	res := Diff(src, tgt, optLoose())
	tb := catByKey(t, res, "tables")
	for _, td := range tb.Tables {
		if td.SourceOnly || td.TargetOnly {
			t.Errorf("case-insensitive fold must not create one-sided table: %+v", td)
		}
	}
	// case-sensitive: Users vs users → one-sided fail.
	if res := Diff(src, tgt, optStrict()); res.Overall == VerdictPass {
		t.Error("case-sensitive compare must flag Users vs users")
	}
}

func TestWorst(t *testing.T) {
	if Worst(VerdictWarn, VerdictFail) != VerdictFail || Worst(VerdictFail, VerdictWarn) != VerdictFail {
		t.Error("fail must dominate")
	}
	if Worst(VerdictPass, VerdictWarn) != VerdictWarn {
		t.Error("warn must dominate pass")
	}
}

func TestSnapshotObjectsAndEndpointLabel(t *testing.T) {
	s := snapA()
	// v1.3 top-level口径: 2 tables + 1 view + 1 proc + 2 fn + 1 tg + 2 seq
	// = 9; users are not schema objects and are excluded.
	if got := s.Objects(); got != 9 {
		t.Errorf("Objects = %d, want 9", got)
	}
	e := Endpoint{Host: "h", Port: 4000, Schema: "db"}
	if e.Label() != "h:4000/db" {
		t.Errorf("Label = %q", e.Label())
	}
	e2 := Endpoint{Schema: "db"}
	if e2.Label() != "db" {
		t.Errorf("Label = %q", e2.Label())
	}
}

func TestDiffTriggersQualifiedAndCounted(t *testing.T) {
	// D1: trigger names are unique per TABLE (PG tgname semantics), so the
	// collectors emit table-qualified keys and the engine judges count +
	// set. Ten same-named "audit" triggers on ten source tables vs one on
	// the target must FAIL and name the nine missing qualified triggers.
	src := snapA()
	src.Triggers = nil
	for i := 1; i <= 10; i++ {
		src.Triggers = append(src.Triggers, fmt.Sprintf("t%d.audit", i))
	}
	tgt := snapBEqual()
	tgt.Triggers = []string{"t1.audit"}
	res := Diff(src, tgt, optLoose())
	tr := catByKey(t, res, "triggers")
	if tr.Verdict != VerdictFail || res.Overall != VerdictFail {
		t.Fatalf("cross-table same-named triggers: verdict=%q overall=%q, want fail/fail", tr.Verdict, res.Overall)
	}
	if tr.SourceCount != 10 || tr.TargetCount != 1 || tr.Matched != 1 {
		t.Errorf("counts = %d/%d/%d, want 10/1/1", tr.SourceCount, tr.TargetCount, tr.Matched)
	}
	if len(tr.Names.SourceOnly) != 9 {
		t.Errorf("sourceOnly = %v, want 9 qualified names", tr.Names.SourceOnly)
	}
	for _, want := range []string{"t2.audit", "t10.audit"} {
		if !containsStr(tr.Names.SourceOnly, want) {
			t.Errorf("sourceOnly missing %q (got %v)", want, tr.Names.SourceOnly)
		}
	}
	// Equal qualified sets on both sides → pass.
	tgtAll := snapBEqual()
	tgtAll.Triggers = append([]string(nil), src.Triggers...)
	if tr := catByKey(t, Diff(src, tgtAll, optLoose()), "triggers"); tr.Verdict != VerdictPass {
		t.Errorf("equal qualified trigger sets: %q, want pass", tr.Verdict)
	}
	// Count guard (design §4 个数差即 fail): identical SET with a
	// duplicate on one side must still fail.
	srcDup := snapA()
	srcDup.Triggers = []string{"t1.audit", "t1.audit"}
	tgtOne := snapBEqual()
	tgtOne.Triggers = []string{"t1.audit"}
	if tr := catByKey(t, Diff(srcDup, tgtOne, optLoose()), "triggers"); tr.Verdict != VerdictFail {
		t.Errorf("duplicate-name count diff: %q, want fail", tr.Verdict)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestUsersExistenceDiff(t *testing.T) {
	src := &Snapshot{Dialect: "postgres", Schema: "a", Users: []UserMeta{
		{Name: "alice", Privileges: []string{"SUPER"}},
		{Name: "bob", Privileges: []string{"CREATEDB"}},
	}}
	tgt := &Snapshot{Dialect: "mysql", Schema: "b", Users: []UserMeta{
		{Name: "alice", Privileges: []string{"Select_priv"}},
		{Name: "carol", Privileges: []string{"Select_priv"}},
	}}
	res := Diff(src, tgt, optLoose())
	uc := catByKey(t, res, "users")
	if uc.Verdict != VerdictFail {
		t.Fatalf("users existence diff verdict = %q, want fail", uc.Verdict)
	}
	if !reflect.DeepEqual(uc.Names.SourceOnly, []string{"bob"}) || !reflect.DeepEqual(uc.Names.TargetOnly, []string{"carol"}) {
		t.Errorf("users sets = %+v", uc.Names)
	}
	// Cross-dialect user existence is FAIL-able (V1 ruling: user lists
	// compare like name lists).
	if res.Overall != VerdictFail {
		t.Error("user existence difference must fail overall")
	}
}

func TestSequencesMySQLToTiDB(t *testing.T) {
	// Both sides MySQL-wire: no SEQUENCE type on either → pass, no note.
	src := &Snapshot{Dialect: "mysql", Schema: "a"}
	tgt := &Snapshot{Dialect: "mysql", Schema: "b"}
	res := Diff(src, tgt, optLoose())
	sc := catByKey(t, res, "sequences")
	if sc.Verdict != VerdictPass || sc.Note != "" {
		t.Errorf("mysql→mysql sequences: verdict=%q note=%q, want pass/empty", sc.Verdict, sc.Note)
	}
}

func TestSequencesPGToTiDBRealDiff(t *testing.T) {
	// v1.3.1: the MySQL-wire collector inventories TiDB SEQUENCE objects,
	// so a TiDB target compares for real — common sequences pass, real
	// set differences fail, and the「目标端无序列对象」annotation must
	// NOT appear while the target carries sequences.
	src := &Snapshot{Dialect: "postgres", Schema: "public",
		Sequences: []string{"seq_users", "seq_orders", "seq_extra"}}
	tgt := &Snapshot{Dialect: "mysql", Schema: "migration_test",
		Sequences: []string{"seq_users", "seq_orders"}}
	res := Diff(src, tgt, optLoose())
	sc := catByKey(t, res, "sequences")
	if sc.Verdict != VerdictFail {
		t.Errorf("verdict = %q, want fail (seq_extra missing on target)", sc.Verdict)
	}
	if sc.Note != "" {
		t.Errorf("note = %q, want empty while target carries sequences", sc.Note)
	}
	if !reflect.DeepEqual(sc.Names.Common, []string{"seq_orders", "seq_users"}) {
		t.Errorf("common = %v, want [seq_orders seq_users]", sc.Names.Common)
	}
	if !reflect.DeepEqual(sc.Names.SourceOnly, []string{"seq_extra"}) || len(sc.Names.TargetOnly) != 0 {
		t.Errorf("source_only = %v target_only = %v, want [seq_extra]/[]", sc.Names.SourceOnly, sc.Names.TargetOnly)
	}
	// Equal sequence sets across dialects → pass, no annotation.
	tgt2 := &Snapshot{Dialect: "mysql", Schema: "migration_test",
		Sequences: []string{"seq_users", "seq_orders", "seq_extra"}}
	sc2 := catByKey(t, Diff(src, tgt2, optLoose()), "sequences")
	if sc2.Verdict != VerdictPass || sc2.Note != "" {
		t.Errorf("equal sequences: verdict=%q note=%q, want pass/empty", sc2.Verdict, sc2.Note)
	}
}

func TestCollectMySQLSequencesFromTableType(t *testing.T) {
	// v1.3.1 contract: sequences on the MySQL-wire side are read from
	// information_schema.TABLES TABLE_TYPE='SEQUENCE' (present on TiDB,
	// empty on vanilla MySQL — one portable query for both).
	s := &capScript{rules: []capRule{
		{contains: "TABLE_TYPE = 'SEQUENCE'", cols: []string{"TABLE_NAME"},
			rows: [][]driver.Value{{"mock_seq_diff"}, {"mock_seq_shared"}}},
	}}
	db := newCapDB(t, s)
	snap, _, err := CollectMySQL(context.Background(), db, "db")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.Sequences, []string{"mock_seq_diff", "mock_seq_shared"}) {
		t.Errorf("sequences = %v, want [mock_seq_diff mock_seq_shared]", snap.Sequences)
	}
	if q := s.hits["TABLE_TYPE = 'SEQUENCE'"]; !strings.Contains(q, "information_schema.TABLES") {
		t.Errorf("sequence query must read information_schema.TABLES: %s", q)
	}
}

func TestPrivilegesBothEmptyPass(t *testing.T) {
	// Cross-dialect with no accounts on either side → pass (nothing to
	// compare); a one-sided degradation shows as the empty side + the
	// collector warning.
	src := &Snapshot{Dialect: "postgres", Schema: "a"}
	tgt := &Snapshot{Dialect: "mysql", Schema: "b"}
	res := Diff(src, tgt, optLoose())
	pc := catByKey(t, res, "privileges")
	if pc.Verdict != VerdictPass {
		t.Errorf("cross-dialect both-empty privileges: verdict=%q, want pass", pc.Verdict)
	}

	// One-sided (PG users but mysql side degraded/empty) → hint disclosure
	// (提示级, v1.4; was warn).
	src.Users = []UserMeta{{Name: "app", Privileges: []string{"SUPER"}}}
	res = Diff(src, tgt, optLoose())
	pc = catByKey(t, res, "privileges")
	if pc.Verdict != VerdictHint {
		t.Errorf("cross-dialect one-sided privileges: verdict=%q, want hint", pc.Verdict)
	}
}

// TestCrossDialectHintDowngrade pins the v1.4 提示级 policy (user ruling
// 11:13「跨方言固有差异不设为对比错误，加小提示」): in cross-dialect runs
// charset/collation value differences and the privileges vocabulary gap
// downgrade to hint and never drive the overall conclusion; PK differences
// and single-side existence stay fail; same-dialect runs keep the strict
// v1.3 verdicts.
func TestCrossDialectHintDowngrade(t *testing.T) {
	pgSide := func() *Snapshot {
		return &Snapshot{
			Dialect: "postgres", Schema: "public",
			Tables: []TableMeta{
				{Name: "t_twin", Charset: "UTF8", Collation: "en_US.UTF-8", PK: []string{"id"}},
				{Name: "t_cs", Charset: "UTF8", Collation: "en_US.UTF-8", PK: []string{"id"}},
				{Name: "t_pk", Charset: "UTF8", Collation: "en_US.UTF-8", PK: []string{"id"}},
			},
			Sequences: []string{"s1"},
			Users:     []UserMeta{{Name: "u1", Privileges: []string{"CREATEDB"}}},
		}
	}
	tidbSide := func() *Snapshot {
		return &Snapshot{
			Dialect: "mysql", Schema: "migration_test",
			Tables: []TableMeta{
				{Name: "t_twin", Charset: "utf8mb4", Collation: "utf8mb4_bin", PK: []string{"id"}},
				{Name: "t_cs", Charset: "latin1", Collation: "latin1_bin", PK: []string{"id"}},
				{Name: "t_pk", Charset: "utf8mb4", Collation: "utf8mb4_bin", PK: []string{"id", "ext"}},
			},
			Sequences: []string{"s1"},
			Users:     []UserMeta{{Name: "u1", Privileges: []string{"Select_priv"}}},
		}
	}

	res := Diff(pgSide(), tidbSide(), optLoose())
	tb := catByKey(t, res, "tables")
	byName := map[string]TableDiff{}
	for _, td := range tb.Tables {
		byName[td.Table] = td
	}
	// twins: collation vocab disjoint → hint, still matched (practical
	// equality, the noise the ruling targets).
	if td := byName["t_twin"]; td.Verdict != VerdictHint {
		t.Errorf("t_twin verdict = %q, want hint (cross-dialect collation vocab)", td.Verdict)
	}
	if tb.Matched != 1 {
		t.Errorf("tables matched = %d, want 1 (t_twin; hint twins count as 一致)", tb.Matched)
	}
	// charset value diff (UTF8 vs latin1) → hint, disclosed, NOT matched.
	if td := byName["t_cs"]; td.Verdict != VerdictHint || td.CharsetDiff == nil || td.CharsetDiff.Equal {
		t.Errorf("t_cs verdict = %q charset=%+v, want hint + disclosed unequal charset", td.Verdict, td.CharsetDiff)
	}
	if td := byName["t_cs"]; td.CharsetDiff != nil && !strings.Contains(td.CharsetDiff.Note, "仅提示") {
		t.Errorf("t_cs charset note must say 仅提示: %q", td.CharsetDiff.Note)
	}
	// PK diff → fail regardless of dialect.
	if td := byName["t_pk"]; td.Verdict != VerdictFail {
		t.Errorf("t_pk verdict = %q, want fail (PK diff stays structural)", td.Verdict)
	}
	// privileges cross-dialect with accounts → hint.
	if pc := catByKey(t, res, "privileges"); pc.Verdict != VerdictHint {
		t.Errorf("privileges verdict = %q, want hint", pc.Verdict)
	}
	// overall: sequences match, users match, tables fail only via t_pk →
	// fail; and WITHOUT the PK diff the run must be overall pass despite
	// the hints.
	pgClean, tidbClean := pgSide(), tidbSide()
	pgClean.Tables = pgClean.Tables[:1]
	tidbClean.Tables = tidbClean.Tables[:1]
	if res2 := Diff(pgClean, tidbClean, optLoose()); res2.Overall != VerdictPass {
		t.Errorf("overall = %q, want pass (hints must not fail the run)", res2.Overall)
	}

	// Same-dialect guard: mysql→mysql latin1 diff stays fail (the
	// downgrade is exclusively cross-dialect).
	srcM := &Snapshot{Dialect: "mysql", Schema: "a", Tables: []TableMeta{
		{Name: "t", Charset: "utf8mb4", Collation: "utf8mb4_bin", PK: []string{"id"}}}}
	tgtM := &Snapshot{Dialect: "mysql", Schema: "b", Tables: []TableMeta{
		{Name: "t", Charset: "latin1", Collation: "latin1_bin", PK: []string{"id"}}}}
	if res3 := Diff(srcM, tgtM, optLoose()); res3.Overall != VerdictFail {
		t.Errorf("same-dialect charset diff overall = %q, want fail (no downgrade)", res3.Overall)
	}
}

// --- dependency-free collector SQL contract guards ---
//
// The DB-side semantics (join filtering, key-part slicing) cannot be
// exercised without a live server — ④ 103 covers the real path. These
// capture-driver assertions run the REAL collector functions against a
// dispatching fake and pin the exact SQL clauses.

// capRule answers queries containing `contains` with canned rows.
type capRule struct {
	contains string
	cols     []string
	rows     [][]driver.Value
}

// capScript records the captured SQL per rule and answers each query via
// its first matching rule (unmatched queries get an empty result).
type capScript struct {
	rules []capRule
	hits  map[string]string
	args  []driver.Value
}

func newCapDB(t *testing.T, s *capScript) *sql.DB {
	t.Helper()
	s.hits = map[string]string{}
	name := "capdriver-" + fmt.Sprintf("%d", len(t.Name())) + "-" + strings.ToLower(strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, t.Name()))
	sql.Register(name, capDriver{s})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

type capDriver struct{ s *capScript }

func (d capDriver) Open(string) (driver.Conn, error) { return capConn{d.s}, nil }

type capConn struct{ s *capScript }

func (capConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("prepare unsupported") }
func (capConn) Close() error                        { return nil }
func (capConn) Begin() (driver.Tx, error)           { return nil, errors.New("begin unsupported") }

func (c capConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	for _, a := range args {
		c.s.args = append(c.s.args, a.Value)
	}
	for _, r := range c.s.rules {
		if r.contains == "" || strings.Contains(query, r.contains) {
			c.s.hits[r.contains] = query
			return &capRows{cols: r.cols, rows: r.rows}, nil
		}
	}
	return &capRows{}, nil
}

type capRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *capRows) Columns() []string { return r.cols }
func (r *capRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}
func (r *capRows) Close() error { return nil }

func TestPGPrimaryKeyJoinContract(t *testing.T) {
	// D6: the table_constraints↔key_column_usage join must carry ALL
	// three identity keys — a hand-named identical PK constraint on two
	// tables of one schema must not cross-join the columns.
	s := &capScript{rules: []capRule{{
		cols: []string{"table_name", "column_name"},
		rows: [][]driver.Value{{"a", "id"}, {"b", "id"}},
	}}}
	db := newCapDB(t, s)
	pk, err := pgPrimaryKey(context.Background(), db, "public")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"kcu.constraint_name = tc.constraint_name",
		"kcu.table_schema = tc.table_schema",
		"kcu.table_name = tc.table_name",
	} {
		if !strings.Contains(s.hits[""], key) {
			t.Errorf("PK join missing identity key %q\nquery: %s", key, s.hits[""])
		}
	}
	if !reflect.DeepEqual(pk["a"], []string{"id"}) || !reflect.DeepEqual(pk["b"], []string{"id"}) {
		t.Errorf("PK grouping = %v, want per-table isolation", pk)
	}
}

func TestCollectMySQLTriggerQualifiedContract(t *testing.T) {
	// D1 MySQL twin: CollectMySQL must emit trigger names TABLE-QUALIFIED
	// (CONCAT(EVENT_OBJECT_TABLE, '.', TRIGGER_NAME)) so the engine's
	// count + set judgment is sound across dialects. The dispatching
	// script drives the full walk with an empty catalog except triggers
	// and a working mysql.user privilege-column probe.
	s := &capScript{rules: []capRule{
		{contains: "COLUMN_NAME FROM information_schema.COLUMNS", cols: []string{"COLUMN_NAME"}, rows: [][]driver.Value{{"Select_priv"}}},
		{contains: "FROM mysql.user", cols: []string{"User", "Host", "Select_priv"}, rows: [][]driver.Value{}},
		{contains: "TRIGGERS", cols: []string{"x"}, rows: [][]driver.Value{{"orders.audit"}}},
	}}
	db := newCapDB(t, s)
	snap, warns, err := CollectMySQL(context.Background(), db, "db")
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if !reflect.DeepEqual(snap.Triggers, []string{"orders.audit"}) {
		t.Errorf("qualified triggers = %v, want [orders.audit]", snap.Triggers)
	}
	if q := s.hits["TRIGGERS"]; !strings.Contains(q, "CONCAT(EVENT_OBJECT_TABLE, '.', TRIGGER_NAME)") {
		t.Errorf("trigger query not table-qualified: %s", q)
	}
}

func TestCollectMySQLCharsetFromCollation(t *testing.T) {
	// MySQL tables carry TABLE_COLLATION; the charset derives from the
	// collation prefix (utf8mb4_general_ci → utf8mb4). Empty collation →
	// empty charset (both sides then compare equal-by-absence).
	s := &capScript{rules: []capRule{
		{
			contains: "TABLE_COLLATION",
			cols:     []string{"TABLE_NAME", "TABLE_COLLATION"},
			rows:     [][]driver.Value{{"users", "utf8mb4_general_ci"}, {"raw", nil}},
		},
	}}
	db := newCapDB(t, s)
	snap, _, err := CollectMySQL(context.Background(), db, "db")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tables) != 2 {
		t.Fatalf("tables = %d, want 2", len(snap.Tables))
	}
	if snap.Tables[0].Charset != "utf8mb4" || snap.Tables[0].Collation != "utf8mb4_general_ci" {
		t.Errorf("charset/collation = %q/%q, want utf8mb4/utf8mb4_general_ci",
			snap.Tables[0].Charset, snap.Tables[0].Collation)
	}
	if snap.Tables[1].Charset != "" || snap.Tables[1].Collation != "" {
		t.Errorf("nil collation must yield empty charset/collation: %+v", snap.Tables[1])
	}
}

func TestCollectMySQLTableMetaPKBeyondInitialCap(t *testing.T) {
	// F1: mysqlTableMeta used to address rows through *TableMeta pointers
	// captured before append reallocations — past the initial cap of 16
	// the backing array moves and the stale pointers' PK writes landed in
	// the abandoned array, so tables 1..N-before-last-growth silently
	// compared with an empty target PK (false FAIL on >16-table schemas).
	// Index-based addressing must keep every PK regardless of table count.
	const n = 20 // 16 initial cap + 4 more → at least one realloc
	tabRows := make([][]driver.Value, 0, n)
	pkRows := make([][]driver.Value, 0, 2*n)
	want := map[string][]string{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%02d", i)
		tabRows = append(tabRows, []driver.Value{name, "utf8mb4_bin"})
		pk := []string{"id"}
		if i%3 == 0 {
			pk = []string{"id", "ext"} // composite PKs mix in
		}
		for _, c := range pk {
			pkRows = append(pkRows, []driver.Value{name, c})
		}
		want[name] = pk
	}
	s := &capScript{rules: []capRule{
		{contains: "TABLE_COLLATION", cols: []string{"TABLE_NAME", "TABLE_COLLATION"}, rows: tabRows},
		{contains: "COLUMN_KEY = 'PRI'", cols: []string{"TABLE_NAME", "COLUMN_NAME"}, rows: pkRows},
	}}
	db := newCapDB(t, s)
	snap, _, err := CollectMySQL(context.Background(), db, "db")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tables) != n {
		t.Fatalf("tables = %d, want %d", len(snap.Tables), n)
	}
	for _, tm := range snap.Tables {
		if !reflect.DeepEqual(tm.PK, want[tm.Name]) {
			t.Errorf("table %s PK = %v, want %v (F1 pointer-aliasing regression)", tm.Name, tm.PK, want[tm.Name])
		}
	}
}

// --- V3 mysql.user read-only red lines ---

func TestCollectMySQLUsersWhitelistNoHash(t *testing.T) {
	// V3 ①/④: the mysql.user SELECT must be an EXPLICIT privilege-column
	// whitelist — no SELECT *, no authentication_string, no password
	// columns — and the collected rows must carry only whitelist tokens.
	s := &capScript{rules: []capRule{
		{
			contains: "COLUMN_NAME FROM information_schema.COLUMNS",
			cols:     []string{"COLUMN_NAME"},
			rows:     [][]driver.Value{{"Select_priv"}, {"Insert_priv"}, {"Super_priv"}},
		},
		{
			contains: "FROM mysql.user",
			cols:     []string{"User", "Host", "Select_priv", "Insert_priv", "Super_priv"},
			rows: [][]driver.Value{
				{"root", "%", "Y", "Y", "N"},
				{"app", "10.0.0.1", "N", "Y", "Y"},
				{"mysql.sys", "localhost", "Y", "Y", "Y"}, // filtered system account
			},
		},
	}}
	db := newCapDB(t, s)
	snap, warns, err := CollectMySQL(context.Background(), db, "db")
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	q := s.hits["FROM mysql.user"]
	if strings.Contains(strings.ToLower(q), "*") {
		t.Errorf("mysql.user query must never SELECT *: %s", q)
	}
	lower := strings.ToLower(q)
	for _, bad := range []string{"authentication_string", "password_lifetime", "password", "global_priv"} {
		if strings.Contains(lower, bad) {
			t.Errorf("mysql.user query leaked sensitive column %q: %s", bad, q)
		}
	}
	joined := mustJSON(t, snap)
	if strings.Contains(strings.ToLower(joined), "authentication_string") ||
		strings.Contains(strings.ToLower(joined), "password") {
		t.Errorf("collected snapshot leaked hash/sensitive text: %s", joined)
	}
	if len(snap.Users) != 2 {
		t.Fatalf("users = %d, want 2 (system account filtered)", len(snap.Users))
	}
	byName := map[string]UserMeta{}
	for _, u := range snap.Users {
		byName[u.Name] = u
	}
	if !reflect.DeepEqual(byName["root@%"].Privileges, []string{"Select_priv", "Insert_priv"}) {
		t.Errorf("root privileges = %v", byName["root@%"].Privileges)
	}
	if !reflect.DeepEqual(byName["app@10.0.0.1"].Privileges, []string{"Insert_priv", "Super_priv"}) {
		t.Errorf("app privileges = %v", byName["app@10.0.0.1"].Privileges)
	}
}

func TestCollectMySQLUsersDegradesOnNoPrivilege(t *testing.T) {
	// V3 ③: no permission to read mysql.user → the users/privileges classes
	// degrade to a warning + empty set, never failing the whole run. The
	// zero-column intersection yields no usable whitelist → degrade.
	s := &capScript{rules: []capRule{{
		contains: "COLUMN_NAME FROM information_schema.COLUMNS",
		cols:     []string{"COLUMN_NAME"},
		rows:     [][]driver.Value{},
	}}}
	db := newCapDB(t, s)
	snap, warns, err := CollectMySQL(context.Background(), db, "db")
	if err != nil {
		t.Fatalf("no-privilege users must NOT fail the run: %v", err)
	}
	if len(snap.Users) != 0 {
		t.Errorf("degraded users = %v, want empty", snap.Users)
	}
	hasWarn := false
	for _, w := range warns {
		if strings.Contains(w, "用户/权限") {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Errorf("degraded users must surface a warning: %v", warns)
	}
}

func TestCollectPGUsersMembership(t *testing.T) {
	// PG side: pg_roles booleans + pg_auth_members member names (V3 ④);
	// system roles filtered; no password column exists on pg_roles.
	s := &capScript{rules: []capRule{
		{
			contains: "current_database()",
			cols:     []string{"pg_encoding_to_char", "datcollate"},
			rows:     [][]driver.Value{{"UTF8", "en_US.UTF-8"}},
		},
		{
			contains: "FROM pg_roles",
			cols:     []string{"rolname", "rolsuper", "rolcreatedb", "rolcreaterole", "rolreplication"},
			rows: [][]driver.Value{
				{"appuser", true, true, false, false},
				{"reporter", false, false, false, false},
			},
		},
		{
			contains: "FROM pg_auth_members",
			cols:     []string{"member", "role"},
			rows:     [][]driver.Value{{"appuser", "reader"}},
		},
	}}
	db := newCapDB(t, s)
	snap, warns, err := CollectPG(context.Background(), db, "public")
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if len(snap.Users) != 2 {
		t.Fatalf("users = %d, want 2", len(snap.Users))
	}
	byName := map[string]UserMeta{}
	for _, u := range snap.Users {
		byName[u.Name] = u
	}
	if !reflect.DeepEqual(byName["appuser"].Privileges, []string{"SUPER", "CREATEDB", "MEMBER:reader"}) {
		t.Errorf("appuser privileges = %v", byName["appuser"].Privileges)
	}
	if len(byName["reporter"].Privileges) != 0 {
		t.Errorf("reporter privileges = %v, want empty", byName["reporter"].Privileges)
	}
	joined := mustJSON(t, snap)
	if strings.Contains(strings.ToLower(joined), "password") {
		t.Errorf("PG snapshot leaked password text: %s", joined)
	}
	// System built-in roles are excluded by the pg_roles SQL predicate
	// (rolname NOT LIKE 'pg\_%'); pin the clause.
	if q := s.hits["FROM pg_roles"]; !strings.Contains(q, `NOT LIKE 'pg\_%'`) {
		t.Errorf("pg_roles query must filter system roles: %s", q)
	}
	// E1/P1: pg_auth_members join must use the real PG16 column am.member
	// (not memberid, which does not exist); pin both the join column and the
	// member-name projection so a regression can't silently drop membership.
	if q := s.hits["FROM pg_auth_members"]; !strings.Contains(q, "ON m.oid = am.member") || strings.Contains(q, "memberid") {
		t.Errorf("pg_auth_members query must join on am.member (PG16 column), got: %s", q)
	}
}

func TestCollectPGSequences(t *testing.T) {
	s := &capScript{rules: []capRule{
		{
			contains: "current_database()",
			cols:     []string{"pg_encoding_to_char", "datcollate"},
			rows:     [][]driver.Value{{"UTF8", "en_US.UTF-8"}},
		},
		{
			contains: "FROM information_schema.sequences",
			cols:     []string{"sequence_name"},
			rows:     [][]driver.Value{{"seq_users"}, {"seq_orders"}},
		},
	}}
	db := newCapDB(t, s)
	snap, warns, err := CollectPG(context.Background(), db, "public")
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if !reflect.DeepEqual(snap.Sequences, []string{"seq_users", "seq_orders"}) {
		t.Errorf("sequences = %v", snap.Sequences)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
