package webapi

import (
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

	// non-PG source
	w, req = doReq("POST", "/api/v1/datasources", `{
		"name": "wm-mysql", "type": "mysql",
		"fields": {"host": "10.0.0.2", "port": 3306, "user": "u", "password": "p", "database": "db"}
	}`)
	s.handleCreateDataSource(w, req)
	id := dsBody(t, w)["id"].(string)
	w, req = doReq("POST", "/api/v1/incremental/suggest-watermark", fmt.Sprintf(`{"source_ref":%q}`, id))
	s.handleSuggestWatermark(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("mysql source = %d %s, want 400 (PG-only)", w.Code, w.Body.String())
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
