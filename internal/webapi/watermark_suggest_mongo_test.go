// watermark_suggest_mongo_test.go anchors the mongo watermark candidate
// catalog (mongo 水位列自动发现单) offline: type mapping, index-prefix
// detection, sample histogram, the $exists probe-back, view/system
// exclusion, and the shared-scorer integration (utime 更新系 after the
// dictionary addition). Fakes implement exactly the mongoconn surface the
// catalog consumes — no server, no mongod.
package webapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/mongoconn"
	"github.com/michaelliuyuan/timstool/internal/source"
)

// --- fakes: the catalog touches ListCollectionNameTypes, Collection,
// IndexSpecs and Find only. ---

type wmFakeIter struct {
	docs []bson.D
	i    int
	err  error
}

func (it *wmFakeIter) Next(ctx context.Context) bool {
	if it.err != nil || it.i >= len(it.docs) {
		return false
	}
	it.i++
	return true
}
func (it *wmFakeIter) Doc() bson.D                     { return it.docs[it.i-1] }
func (it *wmFakeIter) Err() error                      { return it.err }
func (it *wmFakeIter) Close(ctx context.Context) error { return nil }

type wmFakeColl struct {
	docs  []bson.D // the sample universe (Find {})
	specs []source.IndexInfo
	// probeDocs, when set, answers $exists probes independently of docs —
	// the "field exists beyond the 50-doc sample" case.
	probeDocs []bson.D
	findErr   error
}

func (c *wmFakeColl) Find(ctx context.Context, query bson.D, opts mongoconn.FindOpts) (mongoconn.DocIter, error) {
	if c.findErr != nil {
		return nil, c.findErr
	}
	// Empty query = bounded sample (catalog uses Batch/Limit 50; honor the
	// limit here so the sample bound is real in tests).
	if len(query) == 0 {
		n := int64(len(c.docs))
		if opts.Limit > 0 && opts.Limit < n {
			n = opts.Limit
		}
		return &wmFakeIter{docs: c.docs[:n]}, nil
	}
	// {field: {$exists: true}} probe = first doc carrying the field. A
	// $-prefixed probe field is rejected the way a real server rejects
	// unknown top-level operators — pinning 对抗席 a1 (a regression that
	// probes "$**" must fail loud HERE, not 500 a whole catalog silently).
	if len(query) == 1 {
		if inner, ok := query[0].Value.(bson.D); ok && len(inner) == 1 && inner[0].Key == "$exists" {
			field := query[0].Key
			if strings.HasPrefix(field, "$") {
				return nil, fmt.Errorf("wmFakeColl: server rejects unknown operator %q", field)
			}
			pool := c.probeDocs
			if pool == nil {
				pool = c.docs
			}
			for _, d := range pool {
				if _, ok := wmMongoFieldOf(d, field); ok {
					return &wmFakeIter{docs: []bson.D{d}}, nil
				}
			}
			return &wmFakeIter{}, nil
		}
	}
	return nil, fmt.Errorf("wmFakeColl: unsupported query %#v", query)
}
func (c *wmFakeColl) IDRange(ctx context.Context, match bson.D) (mongoconn.RangeStats, error) {
	return mongoconn.RangeStats{}, nil
}
func (c *wmFakeColl) CountDocuments(ctx context.Context, filter bson.D) (int64, error) {
	return int64(len(c.docs)), nil
}
func (c *wmFakeColl) IndexSpecs(ctx context.Context) ([]source.IndexInfo, error) {
	return c.specs, nil
}

type wmFakeClient struct {
	types map[string]string // name -> listCollections type
	colls map[string]mongoconn.Coll
}

func (f *wmFakeClient) Ping(ctx context.Context) error { return nil }
func (f *wmFakeClient) Version(ctx context.Context) (string, error) {
	return "MongoDB fake", nil
}
func (f *wmFakeClient) ListDatabases(ctx context.Context) ([]source.DatabaseInfo, error) {
	return nil, nil
}
func (f *wmFakeClient) ListCollections(ctx context.Context, database string) ([]source.CollectionInfo, error) {
	return nil, nil
}
func (f *wmFakeClient) ListCollectionNames(ctx context.Context, database string) ([]string, error) {
	return nil, nil
}
func (f *wmFakeClient) ListCollectionNameTypes(ctx context.Context, database string) ([]mongoconn.CollType, error) {
	var out []mongoconn.CollType
	for name, typ := range f.types {
		out = append(out, mongoconn.CollType{Name: name, Type: typ})
	}
	return out, nil
}
func (f *wmFakeClient) Collection(database, coll string) mongoconn.Coll { return f.colls[coll] }
func (f *wmFakeClient) Driver() string                                  { return "fake" }
func (f *wmFakeClient) Disconnect(ctx context.Context) error            { return nil }

// doc builds one bson.D from field/value pairs (keeps nested literals sane).
func doc(kv ...any) bson.D {
	var d bson.D
	for i := 0; i+1 < len(kv); i += 2 {
		d = append(d, bson.E{Key: kv[i].(string), Value: kv[i+1]})
	}
	return d
}

// --- type mapping ---

func TestWmMongoDataTypeEngineWhitelist(t *testing.T) {
	for v, want := range map[any]string{
		bson.DateTime(1234): "datetime",
		int64(7):            "bigint",
		int32(7):            "integer",
	} {
		if got, ok := wmMongoDataType(v); !ok || got != want {
			t.Errorf("wmMongoDataType(%T) = %q,%v; want %q,true", v, got, ok, want)
		}
	}
	// Outside the engine's custom-column whitelist: never suggestible.
	for _, v := range []any{"s", 1.5, bson.ObjectID{}, bson.A{}, true} {
		if _, ok := wmMongoDataType(v); ok {
			t.Errorf("wmMongoDataType(%T) = ok; want rejected", v)
		}
	}
}

func TestWmMongoRepTypeMajorityAndTimeTiebreak(t *testing.T) {
	if got := wmMongoRepType(map[string]int{"integer": 5, "datetime": 3}); got != "integer" {
		t.Errorf("majority lost: rep = %q, want integer", got)
	}
	if got := wmMongoRepType(map[string]int{"datetime": 3, "integer": 3}); got != "datetime" {
		t.Errorf("tie must prefer time semantics: rep = %q, want datetime", got)
	}
	if got := wmMongoRepType(map[string]int{}); got != "" {
		t.Errorf("empty histogram rep = %q, want empty", got)
	}
}

// --- index prefixes ---

func TestWmMongoIndexedFieldsPrefixNumericOnly(t *testing.T) {
	specs := []source.IndexInfo{
		{Name: "ut", Keys: []source.IndexKey{{Field: "utime", Order: int32(1)}}},                                     // single
		{Name: "cmp", Keys: []source.IndexKey{{Field: "utime", Order: int32(-1)}, {Field: "user", Order: int32(1)}}}, // compound prefix
		{Name: "txt", Keys: []source.IndexKey{{Field: "body", Order: "text"}}},                                       // text never ranges
		{Name: "hsh", Keys: []source.IndexKey{{Field: "uid", Order: "hashed"}}},                                      // hashed never ranges
		{Name: "sec", Keys: []source.IndexKey{{Field: "x", Order: int32(1)}, {Field: "user", Order: int32(1)}}},      // user only SECOND key: not a prefix
	}
	got := wmMongoIndexedFields(specs)
	want := map[string]bool{"utime": true, "x": true} // x = sec's FIRST key; user (second key only) stays out
	if !reflect.DeepEqual(got, want) {
		t.Errorf("indexed = %v; want %v", got, want)
	}
}

// --- per-collection catalog ---

func TestWmMongoCollCatalogHistogramAndProbes(t *testing.T) {
	ctx := context.Background()
	dt := bson.DateTime(1700000000000)

	t.Run("sample histogram picks majority, drops non-comparable, stamps index flag", func(t *testing.T) {
		c := &wmFakeColl{
			docs: []bson.D{
				doc("_id", bson.ObjectID{}, "utime", dt, "name", "x"),
				doc("_id", bson.ObjectID{}, "utime", dt),
				doc("_id", bson.ObjectID{}, "utime", int64(9)),
			},
			specs: []source.IndexInfo{{Keys: []source.IndexKey{{Field: "utime", Order: int32(1)}}}},
		}
		rows, err := wmMongoCollCatalog(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		want := []wmCatalogColumn{
			{Column: "utime", DataType: "datetime", Indexed: true}, // 2×datetime beats 1×bigint; ObjectId _id never maps
		}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("rows = %+v; want %+v", rows, want)
		}
	})

	t.Run("sample-unseen index field probes back once", func(t *testing.T) {
		c := &wmFakeColl{
			docs:      []bson.D{doc("_id", bson.ObjectID{})}, // sample: _id only
			probeDocs: []bson.D{doc("_id", bson.ObjectID{}, "utime", dt)},
			specs:     []source.IndexInfo{{Keys: []source.IndexKey{{Field: "utime", Order: int32(1)}}}},
		}
		rows, err := wmMongoCollCatalog(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		want := []wmCatalogColumn{{Column: "utime", DataType: "datetime", Indexed: true}}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("rows = %+v; want %+v", rows, want)
		}
	})

	t.Run("sample-shown non-comparable index field is dropped without probe", func(t *testing.T) {
		c := &wmFakeColl{
			docs:  []bson.D{doc("status", "active")},
			specs: []source.IndexInfo{{Keys: []source.IndexKey{{Field: "status", Order: int32(1)}}}},
		}
		rows, err := wmMongoCollCatalog(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("rows = %+v; want none (string field is not a candidate)", rows)
		}
	})

	t.Run("int _id surfaces, ObjectId custom field does not", func(t *testing.T) {
		c := &wmFakeColl{
			docs: []bson.D{doc("_id", int32(5), "ref", bson.ObjectID{})},
			specs: []source.IndexInfo{
				{Keys: []source.IndexKey{{Field: "_id", Order: int32(1)}}},
				{Keys: []source.IndexKey{{Field: "ref", Order: int32(1)}}},
			},
		}
		rows, err := wmMongoCollCatalog(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		want := []wmCatalogColumn{{Column: "_id", DataType: "integer", Indexed: true}}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("rows = %+v; want %+v", rows, want)
		}
	})

	t.Run("wildcard index and non-identifier fields never probe (a1/a2)", func(t *testing.T) {
		c := &wmFakeColl{
			docs: []bson.D{doc("更新时间", dt, "a.b", dt, "$x", int64(1), "utime", dt)},
			specs: []source.IndexInfo{
				{Keys: []source.IndexKey{{Field: "$**", Order: int32(1)}}},  // wildcard: must be skipped, never probed
				{Keys: []source.IndexKey{{Field: "更新时间", Order: int32(1)}}}, // non-ASCII key: outside the identifier gate
			},
		}
		rows, err := wmMongoCollCatalog(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		// Only the identifier-clean comparable field survives; the fake
		// fails loud on any $-prefixed probe, so a regression that probes
		// "$**" surfaces as an error above.
		want := []wmCatalogColumn{{Column: "utime", DataType: "datetime", Indexed: false}}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("rows = %+v; want %+v", rows, want)
		}
	})

	t.Run("find failure fails loud", func(t *testing.T) {
		c := &wmFakeColl{findErr: fmt.Errorf("boom")}
		if _, err := wmMongoCollCatalog(ctx, c); err == nil {
			t.Fatal("catalog must fail loud on scan error")
		}
	})
}

// --- whole-database catalog ---

func TestWmMongoCatalogExcludesViewsAndSystem(t *testing.T) {
	ctx := context.Background()
	dt := bson.DateTime(1700000000000)
	fc := &wmFakeClient{
		types: map[string]string{
			"orders":    "collection",
			"aView":     "view",
			"system.js": "collection",
		},
		colls: map[string]mongoconn.Coll{
			"orders":    &wmFakeColl{docs: []bson.D{doc("utime", dt)}, specs: []source.IndexInfo{{Keys: []source.IndexKey{{Field: "utime", Order: int32(1)}}}}},
			"aView":     &wmFakeColl{docs: []bson.D{doc("utime", dt)}},
			"system.js": &wmFakeColl{docs: []bson.D{doc("x", int32(1))}},
		},
	}
	rows, err := wmMongoCatalog(ctx, fc, "db")
	if err != nil {
		t.Fatal(err)
	}
	want := []wmCatalogColumn{{Table: "orders", Column: "utime", DataType: "datetime", Indexed: true}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v; want %+v", rows, want)
	}
}

func TestWmMongoCatalogFailLoudAndEmpty(t *testing.T) {
	ctx := context.Background()
	fc := &wmFakeClient{
		types: map[string]string{"ok": "collection", "bad": "collection"},
		colls: map[string]mongoconn.Coll{
			"ok":  &wmFakeColl{docs: []bson.D{doc("n", int64(1))}},
			"bad": &wmFakeColl{findErr: fmt.Errorf("cursor died")},
		},
	}
	if _, err := wmMongoCatalog(ctx, fc, "db"); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v; want loud failure naming the failing collection", err)
	}
	empty := &wmFakeClient{types: map[string]string{"v": "view"}, colls: map[string]mongoconn.Coll{}}
	rows, err := wmMongoCatalog(ctx, empty, "db")
	if err != nil || rows != nil {
		t.Fatalf("view-only db: rows=%v err=%v; want nil,nil", rows, err)
	}
}

// --- shared-scorer integration: the approved utime/ctime dictionary ---

func TestWmMongoScorerUtimeDictionaryAndRanking(t *testing.T) {
	dt := bson.DateTime(1700000000000)
	fc := &wmFakeClient{
		types: map[string]string{
			"orders": "collection", "events": "collection", "logs": "collection",
		},
		colls: map[string]mongoconn.Coll{
			"orders": &wmFakeColl{docs: []bson.D{doc("utime", dt, "ctime", dt)}, specs: []source.IndexInfo{{Keys: []source.IndexKey{{Field: "utime", Order: int32(1)}}}}},
			"events": &wmFakeColl{docs: []bson.D{doc("utime", dt, "ctime", dt)}, specs: []source.IndexInfo{{Keys: []source.IndexKey{{Field: "utime", Order: int32(1)}}}}},
			"logs":   &wmFakeColl{docs: []bson.D{doc("seq", int64(1))}, specs: []source.IndexInfo{{Keys: []source.IndexKey{{Field: "seq", Order: int32(1)}}}}},
		},
	}
	rows, err := wmMongoCatalog(context.Background(), fc, "db")
	if err != nil {
		t.Fatal(err)
	}
	total, cands := scoreWatermarkCandidates(rows)
	if total != 3 {
		t.Fatalf("total = %d; want 3", total)
	}
	if len(cands) == 0 || cands[0].Column != "utime" {
		t.Fatalf("top candidate = %+v; want utime first", cands)
	}
	ut := cands[0]
	if ut.Coverage != 2.0/3.0 {
		t.Errorf("utime coverage = %v; want 2/3", ut.Coverage)
	}
	if ut.IndexedRatio != 1 {
		t.Errorf("indexed ratio = %v; want 1 (both matched collections indexed)", ut.IndexedRatio)
	}
	foundReason := false
	for _, r := range ut.Reasons {
		if strings.Contains(r, "更新系命名") {
			foundReason = true
		}
	}
	if !foundReason {
		t.Errorf("utime must carry the 更新系命名 reason after the dictionary addition: %v", ut.Reasons)
	}
	ct := findCand(t, cands, "ctime")
	if !hasWarnContaining(ct, "创建系列仅在 INSERT 时前进") {
		t.Errorf("ctime must carry the created-series warning: %v", ct.Warnings)
	}
}

// --- handler validation branches (fast rejects; dial path is te103's) ---

func postMongoSuggest(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mongocompare/suggest-watermark", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSuggestMongoWatermark(rec, req)
	return rec
}

func TestHandleSuggestMongoWatermarkValidation(t *testing.T) {
	s, _ := newTestServer(t)

	rec := postMongoSuggest(t, s, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: code = %d; want 400", rec.Code)
	}
	rec = postMongoSuggest(t, s, `{}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "host or uri") {
		t.Errorf("empty source: code = %d body = %s; want 400 endpoint error (create-handler parity)", rec.Code, rec.Body.String())
	}
	rec = postMongoSuggest(t, s, `{"source":{"host":"h"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "source.database is required") {
		t.Errorf("no database: code = %d body = %s; want 400 database required", rec.Code, rec.Body.String())
	}
	rec = postMongoSuggest(t, s, `{"source":{"datasource_id":"no-such-ref","database":"db"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown datasource ref: code = %d; want 400", rec.Code)
	}
	rec = postMongoSuggest(t, s, `{"source":{"database":"db"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "host or uri") {
		t.Errorf("no host: code = %d body = %s; want 400 endpoint error", rec.Code, rec.Body.String())
	}
}
