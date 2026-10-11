package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ListDatabases implements source.MetaBrowser level 1: the databases visible
// to the connected user, with on-disk size and emptiness as reported by the
// listDatabases command.
func (s *Source) ListDatabases(ctx context.Context) ([]source.DatabaseInfo, error) {
	if s.client == nil {
		return nil, fmt.Errorf("mongodb source: not connected")
	}
	res, err := s.client.ListDatabases(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("mongodb source: listDatabases: %w", err)
	}
	out := make([]source.DatabaseInfo, 0, len(res.Databases))
	for _, d := range res.Databases {
		out = append(out, source.DatabaseInfo{
			Name:       d.Name,
			SizeOnDisk: d.SizeOnDisk,
			Empty:      d.Empty,
		})
	}
	return out, nil
}

// ListCollections implements source.MetaBrowser level 2: the collections of
// one database. Real collections carry an estimated document count and the
// full index summary (keys order-preserved, unique, TTL); views carry neither
// (Count stays -1, Indexes nil). Index/count fetch errors are non-fatal — the
// collection is still listed, just without that detail.
func (s *Source) ListCollections(ctx context.Context, database string) ([]source.CollectionInfo, error) {
	if s.client == nil {
		return nil, fmt.Errorf("mongodb source: not connected")
	}
	cur, err := s.client.Database(database).ListCollections(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("mongodb source: listCollections(%s): %w", database, err)
	}
	var specs []mongo.CollectionSpecification
	if err := cur.All(ctx, &specs); err != nil {
		return nil, fmt.Errorf("mongodb source: decode collections: %w", err)
	}

	out := make([]source.CollectionInfo, 0, len(specs))
	for _, c := range specs {
		info := source.CollectionInfo{
			Name: c.Name,
			Type: c.Type, // "collection" | "view" (timeseries reports "collection" + options)
			// unknown-by-default per the browse contract
			Count:          -1,
			StorageSize:    -1,
			TotalIndexSize: -1,
		}
		if isBrowsableCollection(c.Type) {
			s.fillCollectionStats(ctx, database, c.Name, &info)
			if idxs, err := s.client.Database(database).Collection(c.Name).Indexes().ListSpecifications(ctx); err == nil {
				info.Indexes = indexSummaries(idxs)
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// isBrowsableCollection reports whether a collection spec gets data-level
// detail (stats + indexes). Views don't; an empty type (older drivers) is
// treated as a collection so it is never silently dropped.
func isBrowsableCollection(typ string) bool { return typ == "collection" || typ == "" }

// fillCollectionStats fills Count/StorageSize/TotalIndexSize via the collStats
// command (exact count included); on any failure it degrades to an estimated
// count with -1 sizes — browse-only detail, never fatal.
func (s *Source) fillCollectionStats(ctx context.Context, database, coll string, info *source.CollectionInfo) {
	db := s.client.Database(database)
	var res struct {
		Count          int64 `bson:"count"`
		StorageSize    int64 `bson:"storageSize"`
		TotalIndexSize int64 `bson:"totalIndexSize"`
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "collStats", Value: coll}}).Decode(&res); err == nil {
		info.Count = res.Count
		info.StorageSize = res.StorageSize
		info.TotalIndexSize = res.TotalIndexSize
		return
	}
	if n, err := db.Collection(coll).EstimatedDocumentCount(ctx); err == nil {
		info.Count = n
	}
}

// indexSummaries converts driver index specifications into the strongly-typed
// source.IndexInfo (order-preserving keys, unique flag, TTL).
func indexSummaries(specs []mongo.IndexSpecification) []source.IndexInfo {
	out := make([]source.IndexInfo, 0, len(specs))
	for _, ix := range specs {
		info := source.IndexInfo{
			Name: ix.Name,
			Keys: indexKeys(ix.KeysDocument),
		}
		if ix.Unique != nil {
			info.Unique = *ix.Unique
		}
		// TTL as a first-class field: the M2 compare engine skips data
		// comparison on TTL collections (contents expire by design).
		info.ExpireAfterSeconds = ix.ExpireAfterSeconds
		out = append(out, info)
	}
	return out
}

// indexKeys decodes an index key document ({a:1, b:-1}) into order-preserved
// []source.IndexKey. Complex directions (text/2dsphere/…) arrive as their raw
// BSON value in Order.
func indexKeys(raw bson.Raw) []source.IndexKey {
	var d bson.D
	if err := bson.Unmarshal(raw, &d); err != nil {
		return nil
	}
	keys := make([]source.IndexKey, 0, len(d))
	for _, kv := range d {
		keys = append(keys, source.IndexKey{Field: kv.Key, Order: kv.Value})
	}
	return keys
}
