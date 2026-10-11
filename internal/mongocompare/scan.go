package mongocompare

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/mongocompare/canon"
	"github.com/michaelliuyuan/timstool/internal/mongoconn"
)

// HashSide streams one bucket from one side through the canonical bucket
// hasher (sorted by _id, batched cursor; O(1) documents in memory — spec §9
// client-side normalization requirement). Works through the version-adaptive
// connection (M5): the v2 path decodes natively, the v1 bridge converts raw
// wire bytes at its boundary — the hasher sees v2 bson.D either way.
func HashSide(ctx context.Context, coll mongoconn.Coll, query bson.D, opts canon.Opts, batch int) (canon.BucketDigest, error) {
	iter, err := coll.Find(ctx, query, mongoconn.FindOpts{Batch: batch})
	if err != nil {
		return canon.BucketDigest{}, err
	}
	defer iter.Close(ctx)

	hasher := canon.NewBucketHasher(opts)
	for iter.Next(ctx) {
		hasher.Update(iter.Doc())
	}
	if err := iter.Err(); err != nil {
		return canon.BucketDigest{}, err
	}
	return hasher.Digest(), nil
}
