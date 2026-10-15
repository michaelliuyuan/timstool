package mongocompare

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
	"github.com/michaelliuyuan/timstool/internal/source"
)

// fakeClient is the zero-Docker R4 fixture: a scripted mongoconn.Client
// whose ListCollectionNameTypes answers drive compareDB's bidirectional
// existence logic (MC-10 B1). Only the listing surface carries data; the
// per-collection surface answers empty (no TTL, no docs — the schema-only
// Config never reaches data levels).
type fakeClient struct {
	nameTypes map[string][]mongoconn.CollType
	listCalls []string // databases whose name+type listing was requested
}

func (f *fakeClient) Ping(ctx context.Context) error { return nil }
func (f *fakeClient) Version(ctx context.Context) (string, error) {
	return "MongoDB fake", nil
}
func (f *fakeClient) ListDatabases(ctx context.Context) ([]source.DatabaseInfo, error) {
	return nil, nil
}
func (f *fakeClient) ListCollections(ctx context.Context, db string) ([]source.CollectionInfo, error) {
	return nil, nil
}
func (f *fakeClient) ListCollectionNames(ctx context.Context, db string) ([]string, error) {
	return nil, nil
}
func (f *fakeClient) ListCollectionNameTypes(ctx context.Context, db string) ([]mongoconn.CollType, error) {
	f.listCalls = append(f.listCalls, db)
	return f.nameTypes[db], nil
}
func (f *fakeClient) Collection(db, coll string) mongoconn.Coll { return fakeColl{} }
func (f *fakeClient) Driver() string                            { return "fake" }
func (f *fakeClient) Disconnect(ctx context.Context) error      { return nil }

// fakeColl: empty index set (no TTL probe hit), data surfaces unused under
// the schema-only Config.
type fakeColl struct{}

func (fakeColl) Find(ctx context.Context, q bson.D, opts mongoconn.FindOpts) (mongoconn.DocIter, error) {
	panic("fakeColl.Find: schema-only fixture must not stream data")
}
func (fakeColl) IDRange(ctx context.Context) (mongoconn.RangeStats, error) {
	return mongoconn.RangeStats{}, nil
}
func (fakeColl) CountDocuments(ctx context.Context) (int64, error) { return 0, nil }
func (fakeColl) IndexSpecs(ctx context.Context) ([]source.IndexInfo, error) {
	return nil, nil
}

func mc10Fixture() (src, dst *fakeClient) {
	src = &fakeClient{nameTypes: map[string][]mongoconn.CollType{
		"sdb": {
			{Name: "c1", Type: "collection"},
			{Name: "system.views", Type: "collection"}, // src system.* exclusion anchor
			{Name: "sv", Type: "view"},                 // src view → skipped B anchor
		},
	}}
	dst = &fakeClient{nameTypes: map[string][]mongoconn.CollType{
		"ddb": {
			{Name: "c1", Type: "collection"},
			{Name: "extra", Type: "collection"},          // dst-only collection ×1
			{Name: "dview", Type: "view"},                // dst-only view ×1 (unreported)
			{Name: "system.profile", Type: "collection"}, // dst system.* exclusion anchor
		},
	}}
	return src, dst
}

// TestMC10BidirectionalExistenceFake locks the R4 semantics offline (no
// Docker): dst-only collection surfaces as missing-in-source, dst-only view
// and system.* stay unreported, the src view lands in skipped_views only,
// the four-state counts shape is unchanged, and with the schema level off
// the dst side is never enumerated.
func TestMC10BidirectionalExistenceFake(t *testing.T) {
	src, dst := mc10Fixture()

	s, err := CompareDatabases(context.Background(), src, dst, "sdb", "ddb", Config{Levels: []Level{LevelSchema}})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}

	byColl := map[string]CollectionResult{}
	for _, r := range s.Collections {
		if _, dup := byColl[r.Coll]; dup {
			t.Fatalf("duplicate row for %q: %+v", r.Coll, s.Collections)
		}
		byColl[r.Coll] = r
	}

	// dst-only collection: synthetic missing-in-source row.
	extra, ok := byColl["extra"]
	if !ok {
		t.Fatalf("dst-only collection extra missing from rows: %+v", s.Collections)
	}
	if extra.Overall != OverallDiff {
		t.Fatalf("extra overall = %q, want diff", extra.Overall)
	}
	if extra.DB != "ddb" || extra.Coll != "extra" {
		t.Fatalf("extra row identity = %s.%s, want ddb.extra", extra.DB, extra.Coll)
	}
	if d, _ := extra.Checks.Schema["diff"].([]string); len(d) != 1 || d[0] != "missing-in-source" {
		t.Fatalf("extra schema diff = %#v, want [missing-in-source]", extra.Checks.Schema["diff"])
	}

	// dst-only view, system.* on either side, src system.*: unreported.
	for _, name := range []string{"dview", "system.profile", "system.views"} {
		if _, reported := byColl[name]; reported {
			t.Fatalf("%q must stay unreported (view/system exclusion)", name)
		}
	}

	// src view: skipped_views B anchor (dstNames filter does not disturb the
	// MC-9 A2 skip path).
	sk, _ := s.Stats["skipped_views"].([]string)
	if len(sk) != 1 || sk[0] != "sdb.sv" {
		t.Fatalf("skipped_views = %#v, want [sdb.sv]", s.Stats["skipped_views"])
	}

	// c1 (present both sides): schema ok, overall equal.
	c1, ok := byColl["c1"]
	if !ok || c1.Overall != OverallEqual {
		t.Fatalf("c1 row = %+v, want equal", byColl["c1"])
	}
	if ok, _ := c1.Checks.Schema["ok"].(bool); !ok {
		t.Fatalf("c1 schema = %#v, want ok:true", c1.Checks.Schema)
	}

	// Four-state shape: equal=1 (c1), diff=1 (extra), others 0.
	if s.Counts["equal"] != 1 || s.Counts["diff"] != 1 || s.Counts["error"] != 0 || s.Counts["ttl_skip"] != 0 {
		t.Fatalf("counts = %v, want equal=1 diff=1 error=0 ttl_skip=0", s.Counts)
	}
	if s.Overall != OverallDiff {
		t.Fatalf("overall = %q, want diff", s.Overall)
	}

	// schema-off anchor: dst listing never requested.
	src2, dst2 := mc10Fixture()
	s2, err := CompareDatabases(context.Background(), src2, dst2, "sdb", "ddb", Config{Levels: []Level{LevelCount}})
	if err != nil {
		t.Fatalf("schema-off compare: %v", err)
	}
	for _, db := range dst2.listCalls {
		if db == "ddb" {
			t.Fatalf("schema-off: dst side enumerated (%v)", dst2.listCalls)
		}
	}
	if len(s2.Collections) != 1 { // c1 only, data surfaces are fake-empty
		t.Fatalf("schema-off rows = %+v, want exactly the src collection c1", s2.Collections)
	}
	for _, r := range s2.Collections {
		if r.Coll == "extra" {
			t.Fatalf("schema-off: dst-only extra reported: %+v", s2.Collections)
		}
	}

	// dst-view masking-fix anchor: src has collection vc, dst only a
	// same-named VIEW — vc must be reported missing-in-target (the view name
	// must not satisfy dstNames).
	srcV := &fakeClient{nameTypes: map[string][]mongoconn.CollType{
		"sdb": {{Name: "vc", Type: "collection"}},
	}}
	dstV := &fakeClient{nameTypes: map[string][]mongoconn.CollType{
		"ddb": {{Name: "base", Type: "collection"}, {Name: "vc", Type: "view"}},
	}}
	sv, err := CompareDatabases(context.Background(), srcV, dstV, "sdb", "ddb", Config{Levels: []Level{LevelSchema}})
	if err != nil {
		t.Fatalf("dst-view compare: %v", err)
	}
	rows := map[string]CollectionResult{}
	for _, r := range sv.Collections {
		rows[r.Coll] = r
	}
	vc, ok := rows["vc"]
	if !ok || vc.Overall != OverallDiff {
		t.Fatalf("dst-view anchor: vc row = %+v, want diff (view must not mask missing-in-target)", rows)
	}
	if d, _ := vc.Checks.Schema["diff"].([]string); len(d) != 1 || d[0] != "missing-in-target" {
		t.Fatalf("dst-view anchor: vc schema diff = %#v, want [missing-in-target]", vc.Checks.Schema["diff"])
	}
	base, ok := rows["base"]
	if !ok || base.Overall != OverallDiff {
		t.Fatalf("dst-view anchor: base row = %+v, want diff missing-in-source", rows)
	}
}
