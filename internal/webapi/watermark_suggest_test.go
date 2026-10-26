package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// wmCols is a small catalog builder for scorer tests.
func wmCols(rows ...[5]string) []wmCatalogColumn {
	var out []wmCatalogColumn
	for _, r := range rows {
		out = append(out, wmCatalogColumn{
			Table: r[0], Column: r[1], DataType: r[2],
			Indexed: r[3] == "idx", DefaultNow: r[4] == "now",
		})
	}
	return out
}

func findCand(t *testing.T, cands []wmSuggestCandidate, col string) wmSuggestCandidate {
	t.Helper()
	for _, c := range cands {
		if c.Column == col {
			return c
		}
	}
	t.Fatalf("candidate %s missing in %v", col, cands)
	return wmSuggestCandidate{}
}

func hasWarnContaining(c wmSuggestCandidate, frag string) bool {
	for _, w := range c.Warnings {
		if containsStr(w, frag) {
			return true
		}
	}
	return false
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestWMScoreDictionaryHit: the update-series strong name beats a plain
// non-dictionary column with identical coverage/type/index — and created/
// id names rank below it with warnings attached.
func TestWMScoreDictionaryHit(t *testing.T) {
	cols := wmCols(
		[5]string{"a", "updated_at", "timestamp with time zone", "idx", "now"},
		[5]string{"b", "updated_at", "timestamp with time zone", "idx", "now"},
		[5]string{"a", "sync_ts", "timestamp with time zone", "idx", ""},
		[5]string{"b", "sync_ts", "timestamp with time zone", "idx", ""},
	)
	_, cands := scoreWatermarkCandidates(cols)
	if len(cands) < 2 {
		t.Fatalf("cands = %v", cands)
	}
	if cands[0].Column != "updated_at" {
		t.Errorf("top = %s (%.1f), want updated_at", cands[0].Column, cands[0].Score)
	}
	sync := findCand(t, cands, "sync_ts")
	if sync.Score >= cands[0].Score {
		t.Errorf("non-dict sync_ts (%.1f) must rank below updated_at (%.1f)", sync.Score, cands[0].Score)
	}
}

func TestWMScoreCreatedDemoted(t *testing.T) {
	cols := wmCols(
		[5]string{"a", "updated_at", "timestamp with time zone", "idx", "now"},
		[5]string{"a", "created_at", "timestamp with time zone", "idx", "now"},
	)
	_, cands := scoreWatermarkCandidates(cols)
	created := findCand(t, cands, "created_at")
	if !hasWarnContaining(created, "INSERT") {
		t.Errorf("created_at warnings = %v, want INSERT-only warning", created.Warnings)
	}
	if created.Score >= findCand(t, cands, "updated_at").Score {
		t.Errorf("created_at must rank below updated_at")
	}
}

func TestWMScoreIDHardWarning(t *testing.T) {
	cols := wmCols(
		[5]string{"a", "id", "bigint", "idx", ""},
		[5]string{"a", "updated_at", "timestamp with time zone", "idx", "now"},
	)
	_, cands := scoreWatermarkCandidates(cols)
	idc := findCand(t, cands, "id")
	if !hasWarnContaining(idc, "UPDATE") {
		t.Errorf("id warnings = %v, want UPDATE-insensitive warning", idc.Warnings)
	}
	if idc.Score >= findCand(t, cands, "updated_at").Score {
		t.Errorf("id must rank last")
	}
}

// TestWMScoreUtimeCtimeDictionary (mongo 水位列自动发现单 seq=39 ③): the
// shared dictionaries gained utime (update series) and ctime (created
// series) — wmNameNorm folds the spellings, so u_time/UTIME hit the same
// entry. Anchored on the shared scorer: pg/mysql and mongo candidates rank
// identically through it.
func TestWMScoreUtimeCtimeDictionary(t *testing.T) {
	cols := wmCols(
		[5]string{"a", "utime", "datetime", "idx", ""},
		[5]string{"a", "u_time", "datetime", "idx", ""},
		[5]string{"a", "ctime", "datetime", "idx", ""},
	)
	_, cands := scoreWatermarkCandidates(cols)
	ut := findCand(t, cands, "utime")
	if len(ut.Reasons) == 0 {
		t.Fatalf("utime reasons empty")
	}
	found := false
	for _, r := range ut.Reasons {
		if strings.HasPrefix(r, "更新系命名") {
			found = true
		}
	}
	if !found {
		t.Errorf("utime reasons = %v; want 更新系命名 (dictionary hit)", ut.Reasons)
	}
	if ut.Score < 60 {
		t.Errorf("indexed full-coverage utime score = %.1f; want >= 60", ut.Score)
	}
	ct := findCand(t, cands, "ctime")
	if !hasWarnContaining(ct, "INSERT") {
		t.Errorf("ctime warnings = %v; want created-series INSERT-only warning", ct.Warnings)
	}
	// The folded twin scores identically (same normalized dictionary key).
	twin := findCand(t, cands, "u_time")
	if twin.Score != ut.Score {
		t.Errorf("u_time (%.1f) must equal utime (%.1f) after name folding", twin.Score, ut.Score)
	}
}

func TestWMScoreCoveragePenalty(t *testing.T) {
	// updated_at on 1/2 tables vs sync_ts on 2/2 — dictionary weight must
	// NOT fully override a coverage hole.
	cols := wmCols(
		[5]string{"a", "updated_at", "timestamp with time zone", "idx", "now"},
		[5]string{"a", "sync_ts", "timestamp with time zone", "idx", "now"},
		[5]string{"b", "sync_ts", "timestamp with time zone", "idx", "now"},
	)
	_, cands := scoreWatermarkCandidates(cols)
	partial := findCand(t, cands, "updated_at")
	if partial.Coverage != 0.5 {
		t.Errorf("updated_at coverage = %v, want 0.5", partial.Coverage)
	}
	if len(partial.UnmatchedTables) != 1 || partial.UnmatchedTables[0] != "b" {
		t.Errorf("unmatched = %v, want [b]", partial.UnmatchedTables)
	}
	full := findCand(t, cands, "sync_ts")
	if full.Coverage != 1.0 || len(full.MatchedTables) != 2 {
		t.Errorf("sync_ts coverage/matched = %v/%v, want 1.0/2", full.Coverage, full.MatchedTables)
	}
	// NB: partial (dictionary name, cov 0.5) may still outrank full
	// (non-dictionary, cov 1.0) — the 0.25 name weight legitimately
	// outweighs a 0.175 coverage delta. The dead ordering assertion that
	// used to sit here was removed (leader c-fix).
}

func TestWMScoreTypeGradingAndDateWarning(t *testing.T) {
	cols := wmCols(
		[5]string{"a", "updated_at", "timestamp with time zone", "idx", ""},
		[5]string{"b", "updated_at", "date", "idx", ""},
	)
	_, cands := scoreWatermarkCandidates(cols)
	c := findCand(t, cands, "updated_at")
	if c.TypeHistogram["timestamptz"] != 1 || c.TypeHistogram["date"] != 1 {
		t.Errorf("histogram = %v, want timestamptz:1 date:1", c.TypeHistogram)
	}
	if !hasWarnContaining(c, "天粒度") {
		t.Errorf("warnings = %v, want day-granularity warning", c.Warnings)
	}
}

func TestWMScoreIndexAndDefaultBonus(t *testing.T) {
	_, withBonus := scoreWatermarkCandidates(wmCols(
		[5]string{"a", "updated_at", "timestamp with time zone", "idx", "now"},
	))
	_, bare := scoreWatermarkCandidates(wmCols(
		[5]string{"a", "updated_at", "timestamp with time zone", "", ""},
	))
	base, better := withBonus[0].Score, bare[0].Score
	if base <= better {
		t.Errorf("indexed+default_now (%.1f) must outrank bare (%.1f)", base, better)
	}
	if base != 100 {
		t.Errorf("perfect candidate score = %.1f, want 100", base)
	}
	// Isolation B1: a zero-warning (perfect) candidate must still carry a
	// NON-nil Warnings slice — nil marshals to JSON null and crashed the
	// UI's warnings.length access on auto-apply.
	if withBonus[0].Warnings == nil {
		t.Error("perfect candidate Warnings = nil, want empty non-nil slice (JSON null crash)")
	}
	if withBonus[0].Reasons == nil {
		t.Error("perfect candidate Reasons = nil, want non-nil slice")
	}
}

// TestWMZeroWarningCandidateMarshalsEmptyArrays pins the null-crash CLASS
// (isolation B1) at the marshal level: a zero-warning candidate must
// serialize `"warnings":[]` — never `"warnings":null` — and the same for
// reasons, so the UI's .length access is safe for every candidate shape.
func TestWMZeroWarningCandidateMarshalsEmptyArrays(t *testing.T) {
	_, cands := scoreWatermarkCandidates(wmCols(
		[5]string{"a", "updated_at", "timestamp with time zone", "idx", "now"},
	))
	if len(cands) != 1 || cands[0].Warnings == nil {
		t.Fatalf("setup: want one zero-warning candidate, got %+v", cands)
	}
	b, err := json.Marshal(cands[0])
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"warnings":[]`) || strings.Contains(s, `"warnings":null`) {
		t.Errorf("marshal = %s, want \"warnings\":[] (never null)", s)
	}
	if !strings.Contains(s, `"reasons":[`) || strings.Contains(s, `"reasons":null`) {
		t.Errorf("marshal = %s, want non-null reasons array", s)
	}
}

func TestWMScoreNonComparableExcluded(t *testing.T) {
	cols := wmCols(
		[5]string{"a", "updated_at", "text", "idx", "now"},
		[5]string{"a", "payload", "jsonb", "", ""},
	)
	total, cands := scoreWatermarkCandidates(cols)
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(cands) != 0 {
		t.Errorf("cands = %v, want none (no comparable column)", cands)
	}
}

func TestWMScoreTop5Cap(t *testing.T) {
	var cols []wmCatalogColumn
	for i := 0; i < 8; i++ {
		cols = append(cols, wmCatalogColumn{
			Table: "t", Column: fmt.Sprintf("col%d", i),
			DataType: "timestamp with time zone", Indexed: true,
		})
	}
	_, cands := scoreWatermarkCandidates(cols)
	if len(cands) != 5 {
		t.Errorf("cands len = %d, want 5", len(cands))
	}
}

// TestSuggestWatermarkEndpointValidation: request validation paths that
// need no database — missing ref, unknown ref, non-PG source.
func TestSuggestWatermarkEndpointValidation(t *testing.T) {
	s, _ := newTestServer(t)

	// missing source_ref
	w, req := doReq("POST", "/api/v1/incremental/suggest-watermark", `{}`)
	s.handleSuggestWatermark(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body = %d %s, want 400", w.Code, w.Body.String())
	}

	// unknown ref
	w, req = doReq("POST", "/api/v1/incremental/suggest-watermark", `{"source_ref":"nope"}`)
	s.handleSuggestWatermark(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown ref = %d %s, want 400", w.Code, w.Body.String())
	}

	// Watermark-incapable source (MS-10a: mysql is capable now; tidb is the
	// target-only negative kind).
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "wm-tidb", "type": "tidb",
		"fields": {"host": "10.0.0.8", "port": 4000, "user": "root", "password": "p", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	id := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/incremental/suggest-watermark", fmt.Sprintf(`{"source_ref":%q}`, id))
	s.handleSuggestWatermark(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("tidb source = %d %s, want 400 (watermark-incapable)", w.Code, w.Body.String())
	}
}

// TestWMFilterCatalogSubset pins the subset narrowing contract: kept rows
// belong to the selection only, absent tables come back sorted, and an
// empty selection is a verbatim passthrough (whole-schema behavior).
func TestWMFilterCatalogSubset(t *testing.T) {
	cols := wmCols(
		[5]string{"a", "updated_at", "timestamp with time zone", "idx", "now"},
		[5]string{"b", "updated_at", "timestamp with time zone", "idx", "now"},
		[5]string{"c", "id", "bigint", "idx", ""},
	)
	kept, missing := wmFilterCatalogSubset(cols, []string{"a", "zz", "b"})
	if len(kept) != 2 {
		t.Fatalf("kept = %v, want a+b rows only", kept)
	}
	for _, c := range kept {
		if c.Table == "c" {
			t.Errorf("table c leaked into subset: %+v", c)
		}
	}
	if len(missing) != 1 || missing[0] != "zz" {
		t.Errorf("missing = %v, want [zz]", missing)
	}

	// Sorted regardless of request order.
	_, missing = wmFilterCatalogSubset(cols, []string{"zz2", "zz1"})
	if len(missing) != 2 || missing[0] != "zz1" || missing[1] != "zz2" {
		t.Errorf("missing must be sorted, got %v", missing)
	}

	// All-missing: kept empty, missing = the whole selection.
	kept, missing = wmFilterCatalogSubset(cols, []string{"nope"})
	if len(kept) != 0 || len(missing) != 1 || missing[0] != "nope" {
		t.Errorf("all-missing: kept=%v missing=%v", kept, missing)
	}

	// Passthrough: no tables slice ⇒ same slice, no missing.
	kept, missing = wmFilterCatalogSubset(cols, nil)
	if len(kept) != len(cols) || missing != nil {
		t.Errorf("passthrough broken: kept=%d missing=%v", len(kept), missing)
	}
}

// TestWMSuggestSubsetScoringDenominator: after subset narrowing the scorer's
// coverage/unmatched lists take the SELECTED set as denominator — a column
// covering the whole selection must read coverage 1.0 even though the wider
// schema lacks it.
func TestWMSuggestSubsetScoringDenominator(t *testing.T) {
	cols := wmCols(
		[5]string{"a", "wm_ts", "timestamp with time zone", "idx", "now"},
		[5]string{"b", "wm_ts", "timestamp with time zone", "idx", "now"},
		[5]string{"c", "id", "bigint", "idx", ""},
	)
	// Whole schema: wm_ts covers 2/3.
	total, cands := scoreWatermarkCandidates(cols)
	wm := findCand(t, cands, "wm_ts")
	if total != 3 || wm.Coverage != 2.0/3.0 {
		t.Fatalf("whole-schema: total=%d cov=%v, want 3 and 2/3", total, wm.Coverage)
	}
	// Subset {a,b}: wm_ts covers 2/2 and unmatched shrinks to none.
	kept, _ := wmFilterCatalogSubset(cols, []string{"a", "b"})
	total, cands = scoreWatermarkCandidates(kept)
	wm = findCand(t, cands, "wm_ts")
	if total != 2 || wm.Coverage != 1.0 || len(wm.UnmatchedTables) != 0 {
		t.Fatalf("subset: total=%d cov=%v unmatched=%v, want 2/1.0/none", total, wm.Coverage, wm.UnmatchedTables)
	}
	// id must not appear at all once its only table is filtered out.
	for _, c := range cands {
		if c.Column == "id" {
			t.Errorf("id candidate leaked into {a,b} subset scoring")
		}
	}
}

// TestSuggestWatermarkEndpointTablesValidation: the optional tables subset
// is vetted (identifier allow-list, duplicates, batch cap) BEFORE any
// datasource/DB work — same rules as columns-batch, no DB needed.
func TestSuggestWatermarkEndpointTablesValidation(t *testing.T) {
	s, _ := newTestServer(t)

	cases := []struct {
		name     string
		body     string
		wantFrag string
	}{
		{"bad identifier", `{"source_ref":"nope","tables":["ok","bad-name!"]}`, "invalid table name"},
		{"duplicate", `{"source_ref":"nope","tables":["t1","t1"]}`, "表重复"},
	}
	for _, tc := range cases {
		w, req := doReq("POST", "/api/v1/incremental/suggest-watermark", tc.body)
		s.handleSuggestWatermark(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.wantFrag) {
			t.Errorf("%s: got %d %s, want 400 containing %q", tc.name, w.Code, w.Body.String(), tc.wantFrag)
		}
	}

	tooMany := make([]string, incColumnsBatchLimit+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("t%d", i)
	}
	b, _ := json.Marshal(map[string]interface{}{"source_ref": "nope", "tables": tooMany})
	w, req := doReq("POST", "/api/v1/incremental/suggest-watermark", string(b))
	s.handleSuggestWatermark(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "上限") {
		t.Errorf("over-limit: got %d %s, want 400 上限", w.Code, w.Body.String())
	}
}

// TestWMNameClassIDFamily anchors the adversarial 🟡4 fix: the id-family
// check runs on the RAW name with a required separator — user_id still
// classifies as auto-increment-ish, but guid/valid/grid/userid (normalized
// form ends in "id") must NOT.
func TestWMNameClassIDFamily(t *testing.T) {
	for _, hit := range []string{"id", "user_id", "order-id", "ID", "User_ID"} {
		if w, label := wmNameClass(hit); label != "自增系命名" || w != 0.1 {
			t.Errorf("wmNameClass(%q) = %v/%q, want 0.1/自增系命名", hit, w, label)
		}
	}
	for _, miss := range []string{"guid", "valid", "grid", "userid", "updated_at"} {
		if _, label := wmNameClass(miss); label == "自增系命名" {
			t.Errorf("wmNameClass(%q) must not classify as id-family", miss)
		}
	}
}

// TestWMDefaultNowRegexVariants anchors the 🟡7 fix: auto-maintenance
// defaults in all supported spellings match; a quoted string LITERAL
// 'now()'::text (a constant default, not auto-maintained) must not.
func TestWMDefaultNowRegexVariants(t *testing.T) {
	hits := []string{
		"now()", "CURRENT_TIMESTAMP", "LOCALTIMESTAMP",
		"transaction_timestamp()::timestamp", "now()::timestamptz",
	}
	for _, h := range hits {
		if !wmDefaultNowRe.MatchString(h) {
			t.Errorf("wmDefaultNowRe must match %q", h)
		}
	}
	misses := []string{
		`'now()'::text`, `'current_timestamp'::character varying`,
		"'today'::date", "nextval('seq')",
	}
	for _, m := range misses {
		if wmDefaultNowRe.MatchString(m) {
			t.Errorf("wmDefaultNowRe must NOT match literal %q", m)
		}
	}
}

// TestWMTypeWeightSingleSource pins the single-source decision: every type
// in incWatermarkTypes must carry a weight here, and nothing outside it
// may — the two sets can never drift apart again.
func TestWMTypeWeightSingleSource(t *testing.T) {
	for dt := range incWatermarkTypes {
		if w, _ := wmTypeWeight(dt); w <= 0 {
			t.Errorf("incWatermarkTypes member %q has no wmTypeWeight — sets drifted", dt)
		}
	}
	if w, _ := wmTypeWeight("text"); w != 0 {
		t.Errorf("text must stay incomparable, got %v", w)
	}
}

// TestWMCatalogSQLGuards pins the structural guards of the catalog query
// (adversarial 🟡1/🟡8): partition children are excluded from the table
// universe and only VALID indexes set the indexed flag. String-level
// anchor — the SQL's live semantics are covered by isolation scenarios.
func TestWMCatalogSQLGuards(t *testing.T) {
	for _, guard := range []string{
		"NOT t.relispartition", "i.indisvalid",
		"t.relkind IN ('r', 'p')",
	} {
		if !strings.Contains(wmCatalogSQL, guard) {
			t.Errorf("wmCatalogSQL missing guard %q", guard)
		}
	}
}

// TestWMSuggestDialectDualTrack pins the system-schema dual-track equality
// in BOTH directions (MS-05, ruling seq 310): every wmSystemSchemas key
// must appear quoted in wmCatalogSQL's inline NOT IN, and the NOT IN
// literal set must equal the map key set exactly (no half-drift in either
// direction). The map is read through the dialect method (SystemSchemas),
// never directly.
func TestWMSuggestDialectDualTrack(t *testing.T) {
	sys := incSourceDialect.SystemSchemas()
	// Forward: every map key appears as a quoted literal in the SQL.
	for k := range sys {
		quoted := "'" + k + "'"
		if !strings.Contains(wmCatalogSQL, quoted) {
			t.Errorf("wmCatalogSQL missing quoted system schema %q (dual-track drift)", k)
		}
	}
	// Reverse: the NOT IN literal set equals the map key set exactly.
	line := ""
	for _, l := range strings.Split(wmCatalogSQL, "\n") {
		if strings.Contains(l, "NOT IN (") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatal("wmCatalogSQL has no NOT IN system-schema line")
	}
	inner := line[strings.Index(line, "(")+1 : strings.LastIndex(line, ")")]
	got := map[string]bool{}
	for _, lit := range strings.Split(inner, ",") {
		lit = strings.TrimSpace(lit)
		lit = strings.Trim(lit, "'")
		if lit != "" {
			got[lit] = true
		}
	}
	if len(got) != len(sys) {
		t.Errorf("NOT IN literal count %d != SystemSchemas count %d (half-drift)", len(got), len(sys))
	}
	for k := range got {
		if !sys[k] {
			t.Errorf("NOT IN literal %q not in SystemSchemas (dual-track drift)", k)
		}
	}
}
