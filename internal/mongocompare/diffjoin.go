package mongocompare

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/michaelliuyuan/timstool/internal/mongocompare/canon"
)

// DocDelta is one document-level difference of the L6 drill-down: a missing
// document (one side only) or a modified one (both present, canonical
// differs; Fields carries the leaf-path deltas).
type DocDelta struct {
	ID     any           `json:"_id"`
	Type   string        `json:"type"` // modified | missing-in-target | missing-in-source
	Fields []canon.Delta `json:"fields,omitempty"`
}

// DiffBucket runs the L6 drill-down on one (mismatched) bucket: dual cursors
// sorted by _id merged in BSON canonical order; equal-rank _ids whose
// canonical strings match get a field-level diffDocs, canonical mismatches
// (same BSON rank, different type tag — e.g. int32 vs double of equal value)
// report as a cross missing pair. Composite _id types (Object/Array/Regex)
// degrade to a full-bucket map join.
func DiffBucket(ctx context.Context, sc, tc *mongo.Collection, query bson.D, opts canon.Opts, batch int) ([]DocDelta, error) {
	if batch <= 0 {
		batch = 1000
	}
	findOpts := func() *options.FindOptionsBuilder {
		return options.Find().
			SetSort(bson.D{{Key: "_id", Value: 1}}).
			SetBatchSize(int32(batch))
	}

	probe, err := sc.Find(ctx, query, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(1))
	if err != nil {
		return nil, err
	}
	var probeDocs []bson.D
	if err := probe.All(ctx, &probeDocs); err != nil {
		return nil, err
	}
	if len(probeDocs) > 0 {
		id, _ := idOf(probeDocs[0])
		if _, ok := canon.CompareIDs(id, id); !ok {
			return diffBucketMap(ctx, sc, tc, query, opts, findOpts)
		}
	}
	return diffBucketMerge(ctx, sc, tc, query, opts, findOpts)
}

type docIter struct {
	cur  *mongo.Cursor
	next bson.D
	done bool
}

func newDocIter(ctx context.Context, coll *mongo.Collection, query bson.D, opts *options.FindOptionsBuilder) (*docIter, error) {
	cur, err := coll.Find(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	it := &docIter{cur: cur}
	it.advance(ctx)
	return it, nil
}

func (it *docIter) advance(ctx context.Context) {
	if it.cur.Next(ctx) {
		var d bson.D
		if err := it.cur.Decode(&d); err == nil {
			it.next = d
			return
		}
	}
	it.done = true
}

func (it *docIter) close(ctx context.Context) { _ = it.cur.Close(ctx) }

func diffBucketMerge(ctx context.Context, sc, tc *mongo.Collection, query bson.D, opts canon.Opts, findOpts func() *options.FindOptionsBuilder) ([]DocDelta, error) {
	itA, err := newDocIter(ctx, sc, query, findOpts())
	if err != nil {
		return nil, err
	}
	defer itA.close(ctx)
	itB, err := newDocIter(ctx, tc, query, findOpts())
	if err != nil {
		return nil, err
	}
	defer itB.close(ctx)

	var deltas []DocDelta
	for !itA.done && !itB.done {
		idA, _ := idOf(itA.next)
		idB, _ := idOf(itB.next)
		c, ok := canon.CompareIDs(idA, idB)
		if !ok {
			// The probe already ruled exotic _id types out for this bucket;
			// hitting this means mixed _id types appeared mid-stream.
			return nil, fmt.Errorf("bucket %v: mixed _id types not comparable mid-stream", query)
		}
		switch {
		case c == 0:
			if canon.Canonical(idA, opts) == canon.Canonical(idB, opts) {
				r := canon.DiffDocs(itA.next, itB.next, opts)
				if !r.Equal {
					deltas = append(deltas, DocDelta{ID: idA, Type: "modified", Fields: r.Deltas})
				}
			} else {
				deltas = append(deltas, DocDelta{ID: idA, Type: "missing-in-target"})
				deltas = append(deltas, DocDelta{ID: idB, Type: "missing-in-source"})
			}
			itA.advance(ctx)
			itB.advance(ctx)
		case c < 0:
			deltas = append(deltas, DocDelta{ID: idA, Type: "missing-in-target"})
			itA.advance(ctx)
		default:
			deltas = append(deltas, DocDelta{ID: idB, Type: "missing-in-source"})
			itB.advance(ctx)
		}
	}
	for !itA.done {
		id, _ := idOf(itA.next)
		deltas = append(deltas, DocDelta{ID: id, Type: "missing-in-target"})
		itA.advance(ctx)
	}
	for !itB.done {
		id, _ := idOf(itB.next)
		deltas = append(deltas, DocDelta{ID: id, Type: "missing-in-source"})
		itB.advance(ctx)
	}
	return deltas, nil
}

func diffBucketMap(ctx context.Context, sc, tc *mongo.Collection, query bson.D, opts canon.Opts, findOpts func() *options.FindOptionsBuilder) ([]DocDelta, error) {
	m := make(map[string]bson.D)
	cur, err := sc.Find(ctx, query, findOpts())
	if err != nil {
		return nil, err
	}
	for cur.Next(ctx) {
		var d bson.D
		if err := cur.Decode(&d); err != nil {
			_ = cur.Close(ctx)
			return nil, err
		}
		id, _ := idOf(d)
		m[canon.Canonical(id, opts)] = d
	}
	if err := cur.Err(); err != nil {
		_ = cur.Close(ctx)
		return nil, err
	}
	_ = cur.Close(ctx)

	var deltas []DocDelta
	cur2, err := tc.Find(ctx, query, findOpts())
	if err != nil {
		return nil, err
	}
	for cur2.Next(ctx) {
		var d bson.D
		if err := cur2.Decode(&d); err != nil {
			_ = cur2.Close(ctx)
			return nil, err
		}
		id, _ := idOf(d)
		k := canon.Canonical(id, opts)
		src, ok := m[k]
		if !ok {
			deltas = append(deltas, DocDelta{ID: id, Type: "missing-in-source"})
			continue
		}
		delete(m, k)
		r := canon.DiffDocs(src, d, opts)
		if !r.Equal {
			deltas = append(deltas, DocDelta{ID: id, Type: "modified", Fields: r.Deltas})
		}
	}
	if err := cur2.Err(); err != nil {
		_ = cur2.Close(ctx)
		return nil, err
	}
	_ = cur2.Close(ctx)

	for _, d := range m {
		id, _ := idOf(d)
		deltas = append(deltas, DocDelta{ID: id, Type: "missing-in-target"})
	}
	return deltas, nil
}

func idOf(doc bson.D) (any, bool) {
	for _, e := range doc {
		if e.Key == "_id" {
			return e.Value, true
		}
	}
	return nil, false
}
