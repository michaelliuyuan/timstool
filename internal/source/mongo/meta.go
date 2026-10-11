package mongo

import (
	"context"
	"fmt"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// ListDatabases implements source.MetaBrowser level 1: the databases visible
// to the connected user, with on-disk size and emptiness as reported by the
// listDatabases command. Detail computation lives in internal/mongoconn
// (M5: identical metadata on the v2 and v1-bridge paths).
func (s *Source) ListDatabases(ctx context.Context) ([]source.DatabaseInfo, error) {
	if s.client == nil {
		return nil, fmt.Errorf("mongodb source: not connected")
	}
	return s.client.ListDatabases(ctx)
}

// ListCollections implements source.MetaBrowser level 2: the collections of
// one database. Real collections carry an exact document count and the full
// index summary (keys order-preserved, unique, TTL); views carry neither
// (Count stays -1, Indexes nil). Index/count fetch errors are non-fatal —
// the collection is still listed, just without that detail.
func (s *Source) ListCollections(ctx context.Context, database string) ([]source.CollectionInfo, error) {
	if s.client == nil {
		return nil, fmt.Errorf("mongodb source: not connected")
	}
	return s.client.ListCollections(ctx, database)
}
