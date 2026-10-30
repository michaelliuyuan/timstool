package mongocompare

// coll_select_test.go locks the coll-select ① whitelist semantics offline
// (reusing the MC-10 zero-Docker fixture family): the whitelist gates both
// sides symmetrically, unselected views stay out of skipped_views, the
// selected/matched disclosure pair records post-creation drift, and a
// whitelist nothing of which exists fails loudly instead of completing as
// vacuous equality evidence.

import (
	"context"
	"strings"
	"testing"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

func collSelectFixture() (src, dst *fakeClient) {
	src = &fakeClient{nameTypes: map[string][]mongoconn.CollType{
		"sdb": {
			{Name: "c1", Type: "collection"},
			{Name: "c2", Type: "collection"},
			{Name: "sv", Type: "view"},
			{Name: "sv2", Type: "view"},
			{Name: "system.views", Type: "collection"},
		},
	}}
	dst = &fakeClient{nameTypes: map[string][]mongoconn.CollType{
		"ddb": {
			{Name: "c1", Type: "collection"},
			{Name: "extra", Type: "collection"},
		},
	}}
	return src, dst
}

func rowsByColl(s *Summary) map[string]CollectionResult {
	m := map[string]CollectionResult{}
	for _, r := range s.Collections {
		m[r.Coll] = r
	}
	return m
}

// TestCollSelectWhitelistGatesBothSides: only c1 is selected — c2 is not
// compared, the dst-only "extra" (unselected) surfaces NO noise row, and
// the disclosure pair records selected=1/matched=1.
func TestCollSelectWhitelistGatesBothSides(t *testing.T) {
	src, dst := collSelectFixture()
	s, err := CompareDatabases(context.Background(), src, dst, "sdb", "ddb", Config{
		Levels:          []Level{LevelSchema},
		OnlyCollections: []string{"c1"},
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	rows := rowsByColl(s)
	if len(rows) != 1 || rows["c1"].Overall != OverallEqual {
		t.Fatalf("rows = %+v, want exactly c1 equal", s.Collections)
	}
	if _, noise := rows["extra"]; noise {
		t.Fatalf("unselected dst-only extra must not surface: %+v", s.Collections)
	}
	if _, noise := rows["c2"]; noise {
		t.Fatalf("unselected src c2 must not be compared: %+v", s.Collections)
	}
	if s.Stats["selected_collections"] != 1 || s.Stats["matched_collections"] != 1 {
		t.Fatalf("disclosure = %v/%v, want 1/1", s.Stats["selected_collections"], s.Stats["matched_collections"])
	}
	// Unselected views stay out of skipped_views.
	if sk, _ := s.Stats["skipped_views"].([]string); len(sk) != 0 {
		t.Fatalf("skipped_views = %#v, want empty (no selected views)", sk)
	}
}

// TestCollSelectDriftDisclosure: one of two selected collections was
// dropped after task creation — the survivor is compared, the run does NOT
// fail, and selected=2/matched=1 records the drift for the report face.
func TestCollSelectDriftDisclosure(t *testing.T) {
	src, dst := collSelectFixture()
	s, err := CompareDatabases(context.Background(), src, dst, "sdb", "ddb", Config{
		Levels:          []Level{LevelSchema},
		OnlyCollections: []string{"c1", "ghost"},
	})
	if err != nil {
		t.Fatalf("partial drift must not fail the run: %v", err)
	}
	rows := rowsByColl(s)
	if len(rows) != 1 || rows["c1"].Overall != OverallEqual {
		t.Fatalf("rows = %+v, want exactly c1 equal", s.Collections)
	}
	if s.Stats["selected_collections"] != 2 || s.Stats["matched_collections"] != 1 {
		t.Fatalf("disclosure = %v/%v, want 2/1", s.Stats["selected_collections"], s.Stats["matched_collections"])
	}
}

// TestCollSelectNoneMatchedFailsLoudly: a whitelist selecting ONLY
// collections absent from the source (typo, or dropped post-creation) must
// error — a silent "completed, 0 compared" would read as equality
// evidence. The same holds when everything selected is a view (views never
// enter the comparable task set).
func TestCollSelectNoneMatchedFailsLoudly(t *testing.T) {
	for _, cols := range [][]string{{"ghost"}, {"sv"}, {"ghost", "sv2"}} {
		src, dst := collSelectFixture()
		_, err := CompareDatabases(context.Background(), src, dst, "sdb", "ddb", Config{
			Levels:          []Level{LevelSchema},
			OnlyCollections: cols,
		})
		if err == nil {
			t.Fatalf("whitelist %v with no comparable source collection must fail loudly", cols)
		}
		if !strings.Contains(err.Error(), "selected collections") {
			t.Fatalf("error = %q, want the none-of-the-selected diagnostic", err)
		}
	}
}

// TestCollSelectSelectedViewDisclosed: a SELECTED view lands in
// skipped_views (the user asked for it, the skip is disclosed) while the
// unselected sibling view stays silent.
func TestCollSelectSelectedViewDisclosed(t *testing.T) {
	src, dst := collSelectFixture()
	s, err := CompareDatabases(context.Background(), src, dst, "sdb", "ddb", Config{
		Levels:          []Level{LevelSchema},
		OnlyCollections: []string{"c1", "sv"},
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	sk, _ := s.Stats["skipped_views"].([]string)
	if len(sk) != 1 || sk[0] != "sdb.sv" {
		t.Fatalf("skipped_views = %#v, want [sdb.sv]", s.Stats["skipped_views"])
	}
	// matched counts comparable collections only (c1; the view is skip, not match).
	if s.Stats["matched_collections"] != 1 {
		t.Fatalf("matched = %v, want 1 (views are skips, not matches)", s.Stats["matched_collections"])
	}
}

// TestCollSelectLegacyNilEqualsFull: nil whitelist keeps the legacy
// enumerate-everything behavior — the MC-10 anchor fixture still produces
// its full row set through the new code path.
func TestCollSelectLegacyNilEqualsFull(t *testing.T) {
	src, dst := collSelectFixture()
	s, err := CompareDatabases(context.Background(), src, dst, "sdb", "ddb", Config{
		Levels: []Level{LevelSchema},
	})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	rows := rowsByColl(s)
	if len(rows) != 3 { // c1, c2, extra(dst-only)
		t.Fatalf("legacy rows = %+v, want c1+c2+extra", s.Collections)
	}
	if _, has := s.Stats["selected_collections"]; has {
		t.Fatalf("legacy run must not carry the disclosure pair: %v", s.Stats)
	}
}
