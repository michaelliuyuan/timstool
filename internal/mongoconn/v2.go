package mongoconn

import (
	"context"
	"crypto/tls"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	v2mongo "go.mongodb.org/mongo-driver/v2/mongo"
	v2options "go.mongodb.org/mongo-driver/v2/mongo/options"
	v2readpref "go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// dialV2 connects through the v2 driver and verifies the handshake end to
// end: connect -> isMaster (proves the wire protocol and yields the server's
// maxWireVersion, logged for diagnostics) -> Ping. Any failure disconnects
// and returns the error for the Dial cascade.
func dialV2(ctx context.Context, cfg DialConfig) (Client, error) {
	client, err := v2mongo.Connect(buildV2Options(cfg))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := verifyV2(ctx, client); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return &v2Client{client: client}, nil
}

// WrapV2 adapts an already-connected v2 client (tests reuse fixtures; Dial
// itself goes through the same adapter).
func WrapV2(c *v2mongo.Client) Client { return &v2Client{client: c} }

// buildV2Options mirrors the M1 structured-credential convention (no URI
// string building): hosts, optional credential with authSource, optional
// TLS. A bare URI (Host empty) is passed through verbatim.
func buildV2Options(c DialConfig) *v2options.ClientOptions {
	if c.Host == "" && c.URI != "" {
		return v2options.Client().ApplyURI(c.URI)
	}
	opts := v2options.Client().SetHosts([]string{c.Addr()})
	if c.SelectionTimeout > 0 {
		opts.SetServerSelectionTimeout(c.SelectionTimeout)
	}
	if c.User != "" {
		cred := v2options.Credential{Username: c.User}
		if c.Password != "" {
			cred.Password = c.Password
		}
		if c.AuthDB != "" {
			cred.AuthSource = c.AuthDB
		}
		opts.SetAuth(cred)
	}
	if c.TLS {
		opts.SetTLSConfig(&tls.Config{}) // system CAs, verify server by default
	}
	return opts
}

// verifyV2 proves the connection answers commands: isMaster first (it is the
// handshake-equivalent every server 3.0+ speaks; also surfaces auth/wire
// failures the lazy connect defers), then Ping.
func verifyV2(ctx context.Context, client *v2mongo.Client) error {
	var hello struct {
		MaxWireVersion int32 `bson:"maxWireVersion"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "isMaster", Value: 1}}).Decode(&hello); err != nil {
		return fmt.Errorf("isMaster: %w", err)
	}
	if err := client.Ping(ctx, v2readpref.Primary()); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// browsableType reports whether a collection spec gets data-level detail
// (stats + indexes). Views don't; an empty type (older servers/drivers) is
// treated as a collection so those collections are never silently dropped
// from listings (the M1 guard, shared by both driver paths).
func browsableType(typ string) bool { return typ == "collection" || typ == "" }

// --- v2 Client ---

type v2Client struct{ client *v2mongo.Client }

func (c *v2Client) Driver() string { return "v2" }

func (c *v2Client) Ping(ctx context.Context) error {
	return c.client.Ping(ctx, v2readpref.Primary())
}

func (c *v2Client) Disconnect(ctx context.Context) error {
	return c.client.Disconnect(ctx)
}

func (c *v2Client) Version(ctx context.Context) (string, error) {
	var res struct {
		Version string `bson:"version"`
	}
	if err := c.client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&res); err != nil {
		return "", err
	}
	return "MongoDB " + res.Version, nil
}

func (c *v2Client) Collection(db, coll string) Coll {
	return &v2Coll{coll: c.client.Database(db).Collection(coll)}
}

func (c *v2Client) ListDatabases(ctx context.Context) ([]source.DatabaseInfo, error) {
	res, err := c.client.ListDatabases(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("listDatabases: %w", err)
	}
	out := make([]source.DatabaseInfo, 0, len(res.Databases))
	for _, d := range res.Databases {
		out = append(out, source.DatabaseInfo{Name: d.Name, SizeOnDisk: d.SizeOnDisk, Empty: d.Empty})
	}
	return out, nil
}

// ListCollections ports the M1 meta browser detail: type-aware specs, exact
// counts + sizes via collStats (estimated fallback), full index summaries.
func (c *v2Client) ListCollections(ctx context.Context, database string) ([]source.CollectionInfo, error) {
	cur, err := c.client.Database(database).ListCollections(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("listCollections(%s): %w", database, err)
	}
	var specs []v2mongo.CollectionSpecification
	if err := cur.All(ctx, &specs); err != nil {
		return nil, fmt.Errorf("decode collections: %w", err)
	}
	out := make([]source.CollectionInfo, 0, len(specs))
	for _, spec := range specs {
		info := source.CollectionInfo{
			Name:           spec.Name,
			Type:           spec.Type, // "collection" | "view" (timeseries reports "collection" + options)
			Count:          -1,
			StorageSize:    -1,
			TotalIndexSize: -1,
		}
		if browsableType(spec.Type) {
			c.fillStats(ctx, database, spec.Name, &info)
			if idxs, err := (&v2Coll{coll: c.client.Database(database).Collection(spec.Name)}).IndexSpecs(ctx); err == nil {
				info.Indexes = idxs
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func (c *v2Client) ListCollectionNames(ctx context.Context, database string) ([]string, error) {
	cur, err := c.client.Database(database).ListCollections(ctx, bson.D{}, v2options.ListCollections().SetNameOnly(true))
	if err != nil {
		return nil, fmt.Errorf("listCollections(%s): %w", database, err)
	}
	var specs []struct {
		Name string `bson:"name"`
	}
	if err := cur.All(ctx, &specs); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return names, nil
}

// ListCollectionNameTypes is the MC-9 A2 lightweight listing: nameOnly
// cursor decoding just name+type (same round trip as ListCollectionNames,
// with the type the engine's view filter needs).
func (c *v2Client) ListCollectionNameTypes(ctx context.Context, database string) ([]CollType, error) {
	cur, err := c.client.Database(database).ListCollections(ctx, bson.D{}, v2options.ListCollections().SetNameOnly(true))
	if err != nil {
		return nil, fmt.Errorf("listCollections(%s): %w", database, err)
	}
	var specs []struct {
		Name string `bson:"name"`
		Type string `bson:"type"`
	}
	if err := cur.All(ctx, &specs); err != nil {
		return nil, err
	}
	out := make([]CollType, 0, len(specs))
	for _, s := range specs {
		out = append(out, CollType{Name: s.Name, Type: s.Type})
	}
	return out, nil
}

// fillStats fills Count/StorageSize/TotalIndexSize via collStats; on failure
// it degrades to an estimated count with -1 sizes (browse detail, never
// fatal — the M1 contract).
func (c *v2Client) fillStats(ctx context.Context, database, coll string, info *source.CollectionInfo) {
	db := c.client.Database(database)
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

// --- v2 Coll ---

type v2Coll struct{ coll *v2mongo.Collection }

func (c *v2Coll) Find(ctx context.Context, query bsonD, opts FindOpts) (DocIter, error) {
	if opts.Batch <= 0 {
		opts.Batch = 1000
	}
	find := v2options.Find().
		SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetBatchSize(int32(opts.Batch))
	if opts.Limit > 0 {
		find.SetLimit(opts.Limit)
	}
	cur, err := c.coll.Find(ctx, query, find)
	if err != nil {
		return nil, err
	}
	return &v2Iter{cur: cur}, nil
}

// IDRange aggregates the _id min/max/count over the documents matching
// match (第9单 ④: the watermark predicate rides the initial $match so the
// bucket plan is clamped inside the window; an empty match is the legacy
// whole-collection shape, byte-identical).
func (c *v2Coll) IDRange(ctx context.Context, match bsonD) (RangeStats, error) {
	if match == nil {
		match = bson.D{}
	}
	cur, err := c.coll.Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: match}},
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

func (c *v2Coll) CountDocuments(ctx context.Context, filter bsonD) (int64, error) {
	if filter == nil {
		filter = bson.D{}
	}
	return c.coll.CountDocuments(ctx, filter)
}

func (c *v2Coll) IndexSpecs(ctx context.Context) ([]source.IndexInfo, error) {
	idxs, err := c.coll.Indexes().ListSpecifications(ctx)
	if err != nil {
		return nil, err
	}
	return v2IndexInfos(idxs), nil
}

// v2IndexInfos converts driver index specifications into the neutral
// source.IndexInfo (order-preserving keys, unique/sparse/clustered/TTL).
func v2IndexInfos(specs []v2mongo.IndexSpecification) []source.IndexInfo {
	out := make([]source.IndexInfo, 0, len(specs))
	for _, ix := range specs {
		out = append(out, v2IndexInfo(ix))
	}
	return out
}

func v2IndexInfo(ix v2mongo.IndexSpecification) source.IndexInfo {
	info := source.IndexInfo{Name: ix.Name, Keys: v2IndexKeys(ix.KeysDocument)}
	if ix.Unique != nil {
		info.Unique = *ix.Unique
	}
	if ix.ExpireAfterSeconds != nil {
		v := *ix.ExpireAfterSeconds
		info.ExpireAfterSeconds = &v
	}
	if ix.Sparse != nil {
		info.Sparse = *ix.Sparse
	}
	if ix.Clustered != nil {
		info.Clustered = *ix.Clustered
	}
	return info
}

func v2IndexKeys(raw bson.Raw) []source.IndexKey {
	var d bson.D
	if err := bson.Unmarshal(raw, &d); err != nil {
		return nil
	}
	keys := make([]source.IndexKey, 0, len(d))
	for _, kv := range d {
		keys = append(keys, source.IndexKey{Field: kv.Key, Order: normalizeIndexOrder(kv.Value)})
	}
	return keys
}

// --- v2 cursor adapter ---

type v2Iter struct {
	cur  *v2mongo.Cursor
	doc  bson.D
	done bool
	err  error
}

func (it *v2Iter) Next(ctx context.Context) bool {
	if it.done {
		return false
	}
	if !it.cur.Next(ctx) {
		it.done = true
		it.err = it.cur.Err()
		return false
	}
	var d bson.D
	if err := it.cur.Decode(&d); err != nil {
		it.done = true
		it.err = err
		return false
	}
	it.doc = d
	return true
}

func (it *v2Iter) Doc() bsonD { return it.doc }

func (it *v2Iter) Err() error { return it.err }

func (it *v2Iter) Close(ctx context.Context) error { return it.cur.Close(ctx) }
