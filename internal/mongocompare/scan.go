package mongocompare

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/michaelliuyuan/timstool/internal/mongocompare/canon"
)

// HashSide streams one bucket from one side through the canonical bucket
// hasher (sorted by _id, batched cursor; O(1) documents in memory — spec §9
// client-side normalization requirement).
func HashSide(ctx context.Context, coll *mongo.Collection, query bson.D, opts canon.Opts, batch int) (canon.BucketDigest, error) {
	if batch <= 0 {
		batch = 1000
	}
	cur, err := coll.Find(ctx, query, options.Find().
		SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetBatchSize(int32(batch)))
	if err != nil {
		return canon.BucketDigest{}, err
	}
	defer cur.Close(ctx)

	hasher := canon.NewBucketHasher(opts)
	for cur.Next(ctx) {
		var doc bson.D
		if err := cur.Decode(&doc); err != nil {
			return canon.BucketDigest{}, err
		}
		hasher.Update(doc)
	}
	if err := cur.Err(); err != nil {
		return canon.BucketDigest{}, err
	}
	return hasher.Digest(), nil
}

// RangeStats is the source-side _id min/max/count aggregate feeding the
// bucket plan.
type RangeStats struct {
	Min any
	Max any
	N   int64
}

// IDRange computes the _id min/max/count of a collection (empty collection
// reports N=0).
func IDRange(ctx context.Context, coll *mongo.Collection) (RangeStats, error) {
	cur, err := coll.Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "min", Value: bson.D{{Key: "$min", Value: "$_id"}}},
			{Key: "max", Value: bson.D{{Key: "$max", Value: "$_id"}}},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	})
	if err != nil {
		return RangeStats{}, err
	}
	defer cur.Close(ctx)

	var rows []struct {
		Min any   `bson:"min"`
		Max any   `bson:"max"`
		N   int64 `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return RangeStats{}, err
	}
	if len(rows) == 0 || rows[0].N == 0 {
		return RangeStats{N: 0}, nil
	}
	return RangeStats{Min: rows[0].Min, Max: rows[0].Max, N: rows[0].N}, nil
}
