package mongoconn

import (
	"context"
	"crypto/tls"
	"fmt"

	v1bson "go.mongodb.org/mongo-driver/bson"
	v1mongo "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/description"
	v1options "go.mongodb.org/mongo-driver/mongo/options"
	v1readpref "go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/bson"
	v1topology "go.mongodb.org/mongo-driver/x/mongo/driver/topology"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// The v1 bridge serves pre-4.4 servers (wire < 9). Single rule: v1 is a byte
// pipe. Commands go out as v1-built documents (or cross the byte bridge from
// v2 shapes), results come back as raw wire BSON and are decoded exclusively
// by the v2 bson package — bytes in, v2 types out. No v1 Go type ever
// reaches the engine, the canonicalizer, or the meta structures, so the
// single-decode-form arbitration (seq=50) holds on this path byte-for-byte.

// The driver's own floor (wire 6 / MongoDB 3.6 since v1.12, hardcoded in the
// unsupported-semver x/topology package) is relaxed to wire 4 (3.2): this
// tool's bridge target list includes standing 3.2-era instances, the v1 line
// still carries the pre-3.6 protocol paths (OP_QUERY handshake, SCRAM-SHA-1,
// cursor-based aggregate), and the compare workload is read-only. Support
// below 3.6 is best-effort by declaration (docs) and by test (the 3.x
// live-matrix gate runs the full pipeline against a real 3.2.4); the floor
// relaxation lives ONLY on the v1 side — the v2 path never connects to
// servers this old.
func init() {
	v1topology.SupportedWireVersions = description.NewVersionRange(4, 21)
}

// DialV1 forces the v1 bridge regardless of the server's wire version.
// Production code goes through Dial (auto-selection); this is the
// bridge-equivalence test surface (running the v1 bridge against a modern
// server must produce identical results to the v2 path) and a diagnostics
// escape hatch.
func DialV1(ctx context.Context, cfg DialConfig) (Client, error) {
	return dialV1(ctx, cfg)
}

// dialV1 connects through the v1 driver and verifies the handshake: connect
// (lazy) -> isMaster (protocol + auth proof, wire version observed for
// diagnostics) -> Ping. Errors return to the Dial cascade.
func dialV1(ctx context.Context, cfg DialConfig) (Client, error) {
	var opts *v1options.ClientOptions
	if cfg.Host == "" && cfg.URI != "" {
		opts = v1options.Client().ApplyURI(cfg.URI)
	} else {
		opts = v1options.Client().SetHosts([]string{cfg.Addr()})
		if cfg.SelectionTimeout > 0 {
			opts.SetServerSelectionTimeout(cfg.SelectionTimeout)
		}
		if cfg.User != "" {
			cred := v1options.Credential{Username: cfg.User}
			if cfg.Password != "" {
				cred.Password = cfg.Password
			}
			if cfg.AuthDB != "" {
				cred.AuthSource = cfg.AuthDB
			}
			opts.SetAuth(cred)
		}
		if cfg.TLS {
			opts.SetTLSConfig(&tls.Config{})
		}
	}
	client, err := v1mongo.Connect(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	c := &v1Client{client: client}
	if err := c.verify(ctx); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return c, nil
}

// --- byte bridge helpers (the only place the two driver type systems meet) ---

// v2ToV1 crosses an outbound v2 value into the v1 type system through the
// wire format itself: v2 marshal -> bytes -> v1 unmarshal. Lossless by
// construction (both sides speak BSON).
func v2ToV1(out any, v2val any) error {
	raw, err := bson.Marshal(v2val)
	if err != nil {
		return fmt.Errorf("bridge marshal: %w", err)
	}
	if err := v1bson.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("bridge unmarshal: %w", err)
	}
	return nil
}

// decodeV2 decodes raw wire bytes into a v2-typed target (results direction
// of the bridge).
func decodeV2(raw []byte, target any) error {
	if err := bson.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("bridge decode: %w", err)
	}
	return nil
}

// --- v1 Client ---

type v1Client struct{ client *v1mongo.Client }

func (c *v1Client) Driver() string { return "v1" }

func (c *v1Client) verify(ctx context.Context) error {
	var hello struct {
		MaxWireVersion int32 `bson:"maxWireVersion"`
	}
	if err := c.runDecode(ctx, "admin", v1bson.D{{Key: "isMaster", Value: 1}}, &hello); err != nil {
		return fmt.Errorf("isMaster: %w", err)
	}
	if err := c.client.Ping(ctx, v1readpref.Primary()); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// runDecode runs one command and decodes its raw reply with the v2 bson
// package (the results half of the bridge). DecodeBytes exists across the
// whole v1 line (the pinned v1.12 predates SingleResult.Raw).
func (c *v1Client) runDecode(ctx context.Context, db string, cmd v1bson.D, target any) error {
	res := c.client.Database(db).RunCommand(ctx, cmd)
	raw, err := res.DecodeBytes()
	if err != nil {
		return err
	}
	return decodeV2(raw, target)
}

func (c *v1Client) Ping(ctx context.Context) error {
	return c.client.Ping(ctx, v1readpref.Primary())
}

func (c *v1Client) Disconnect(ctx context.Context) error {
	return c.client.Disconnect(ctx)
}

func (c *v1Client) Version(ctx context.Context) (string, error) {
	var res struct {
		Version string `bson:"version"`
	}
	if err := c.runDecode(ctx, "admin", v1bson.D{{Key: "buildInfo", Value: 1}}, &res); err != nil {
		return "", err
	}
	return "MongoDB " + res.Version, nil
}

func (c *v1Client) Collection(db, coll string) Coll {
	return &v1Coll{coll: c.client.Database(db).Collection(coll)}
}

func (c *v1Client) ListDatabases(ctx context.Context) ([]source.DatabaseInfo, error) {
	var res struct {
		Databases []struct {
			Name       string `bson:"name"`
			SizeOnDisk int64  `bson:"sizeOnDisk"`
			Empty      bool   `bson:"empty"`
		} `bson:"databases"`
	}
	if err := c.runDecode(ctx, "admin", v1bson.D{{Key: "listDatabases", Value: 1}}, &res); err != nil {
		return nil, fmt.Errorf("listDatabases: %w", err)
	}
	out := make([]source.DatabaseInfo, 0, len(res.Databases))
	for _, d := range res.Databases {
		out = append(out, source.DatabaseInfo{Name: d.Name, SizeOnDisk: d.SizeOnDisk, Empty: d.Empty})
	}
	return out, nil
}

// ListCollections mirrors the M1 meta detail through raw commands:
// listCollections -> per-collection collStats (estimated-count fallback) +
// listIndexes. Views carry neither (Count -1, no indexes), same as the v2
// path.
func (c *v1Client) ListCollections(ctx context.Context, database string) ([]source.CollectionInfo, error) {
	var res struct {
		Cursor struct {
			FirstBatch []struct {
				Name string `bson:"name"`
				Type string `bson:"type"`
			} `bson:"firstBatch"`
		} `bson:"cursor"`
	}
	if err := c.runDecode(ctx, database, v1bson.D{{Key: "listCollections", Value: 1}}, &res); err != nil {
		return nil, fmt.Errorf("listCollections(%s): %w", database, err)
	}
	specs := res.Cursor.FirstBatch
	out := make([]source.CollectionInfo, 0, len(specs))
	for _, spec := range specs {
		info := source.CollectionInfo{
			Name:           spec.Name,
			Type:           spec.Type,
			Count:          -1,
			StorageSize:    -1,
			TotalIndexSize: -1,
		}
		if browsableType(spec.Type) {
			var stats struct {
				Count          int64 `bson:"count"`
				StorageSize    int64 `bson:"storageSize"`
				TotalIndexSize int64 `bson:"totalIndexSize"`
			}
			if err := c.runDecode(ctx, database, v1bson.D{{Key: "collStats", Value: spec.Name}}, &stats); err == nil {
				info.Count = stats.Count
				info.StorageSize = stats.StorageSize
				info.TotalIndexSize = stats.TotalIndexSize
			} else if n, err := c.client.Database(database).Collection(spec.Name).EstimatedDocumentCount(ctx); err == nil {
				info.Count = n
			}
			if idxs, err := (&v1Coll{coll: c.client.Database(database).Collection(spec.Name)}).IndexSpecs(ctx); err == nil {
				info.Indexes = idxs
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func (c *v1Client) ListCollectionNames(ctx context.Context, database string) ([]string, error) {
	var res struct {
		Cursor struct {
			FirstBatch []struct {
				Name string `bson:"name"`
			} `bson:"firstBatch"`
		} `bson:"cursor"`
	}
	cmd := v1bson.D{{Key: "listCollections", Value: 1}, {Key: "nameOnly", Value: true}}
	if err := c.runDecode(ctx, database, cmd, &res); err != nil {
		return nil, fmt.Errorf("listCollections(%s): %w", database, err)
	}
	names := make([]string, 0, len(res.Cursor.FirstBatch))
	for _, s := range res.Cursor.FirstBatch {
		names = append(names, s.Name)
	}
	return names, nil
}

// --- v1 Coll ---

type v1Coll struct{ coll *v1mongo.Collection }

func (c *v1Coll) Find(ctx context.Context, query bsonD, opts FindOpts) (DocIter, error) {
	if opts.Batch <= 0 {
		opts.Batch = 1000
	}
	if query == nil {
		query = bson.D{} // a nil filter is the empty document (v2 native parity)
	}
	var v1Query v1bson.D
	if err := v2ToV1(&v1Query, query); err != nil {
		return nil, err
	}
	find := v1options.Find().
		SetSort(v1bson.D{{Key: "_id", Value: 1}}).
		SetBatchSize(int32(opts.Batch))
	if opts.Limit > 0 {
		find.SetLimit(opts.Limit)
	}
	cur, err := c.coll.Find(ctx, v1Query, find)
	if err != nil {
		return nil, err
	}
	return &v1Iter{cur: cur}, nil
}

// v1IDRangePipeline is the outbound $match/$group min/max/count aggregate,
// built natively on the v1 side (a fixed shape; the byte bridge handles
// documents, and v2's Marshal does not accept a top-level array anyway).
var v1IDRangePipeline = v1bson.A{
	v1bson.D{{Key: "$match", Value: v1bson.D{}}},
	v1bson.D{{Key: "$group", Value: v1bson.D{
		{Key: "_id", Value: nil},
		{Key: "min", Value: v1bson.D{{Key: "$min", Value: "$_id"}}},
		{Key: "max", Value: v1bson.D{{Key: "$max", Value: "$_id"}}},
		{Key: "n", Value: v1bson.D{{Key: "$sum", Value: 1}}},
	}}},
}

func (c *v1Coll) IDRange(ctx context.Context) (RangeStats, error) {
	cur, err := c.coll.Aggregate(ctx, v1IDRangePipeline)
	if err != nil {
		return RangeStats{}, err
	}
	defer cur.Close(ctx)
	var rows []struct {
		Min any   `bson:"min"`
		Max any   `bson:"max"`
		N   int64 `bson:"n"`
	}
	for cur.Next(ctx) {
		var row struct {
			Min any   `bson:"min"`
			Max any   `bson:"max"`
			N   int64 `bson:"n"`
		}
		if err := decodeV2(cur.Current, &row); err != nil {
			return RangeStats{}, err
		}
		rows = append(rows, row)
	}
	if err := cur.Err(); err != nil {
		return RangeStats{}, err
	}
	if len(rows) == 0 || rows[0].N == 0 {
		return RangeStats{N: 0}, nil
	}
	return RangeStats{Min: rows[0].Min, Max: rows[0].Max, N: rows[0].N}, nil
}

func (c *v1Coll) CountDocuments(ctx context.Context) (int64, error) {
	return c.coll.CountDocuments(ctx, v1bson.D{})
}

func (c *v1Coll) IndexSpecs(ctx context.Context) ([]source.IndexInfo, error) {
	cur, err := c.coll.Indexes().List(ctx)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []source.IndexInfo
	for cur.Next(ctx) {
		var spec struct {
			Name               string `bson:"name"`
			Key                bson.D `bson:"key"`
			Unique             *bool  `bson:"unique"`
			Sparse             *bool  `bson:"sparse"`
			ExpireAfterSeconds *int32 `bson:"expireAfterSeconds"`
			Clustered          *bool  `bson:"clustered"`
		}
		if err := decodeV2(cur.Current, &spec); err != nil {
			return nil, err
		}
		info := source.IndexInfo{Name: spec.Name}
		for _, kv := range spec.Key {
			info.Keys = append(info.Keys, source.IndexKey{Field: kv.Key, Order: normalizeIndexOrder(kv.Value)})
		}
		if spec.Unique != nil {
			info.Unique = *spec.Unique
		}
		if spec.Sparse != nil {
			info.Sparse = *spec.Sparse
		}
		if spec.ExpireAfterSeconds != nil {
			info.ExpireAfterSeconds = spec.ExpireAfterSeconds
		}
		if spec.Clustered != nil {
			info.Clustered = *spec.Clustered
		}
		out = append(out, info)
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// --- v1 cursor adapter (raw bytes -> v2 decode, per document) ---

type v1Iter struct {
	cur  *v1mongo.Cursor
	doc  bsonD
	done bool
	err  error
}

func (it *v1Iter) Next(ctx context.Context) bool {
	if it.done {
		return false
	}
	if !it.cur.Next(ctx) {
		it.done = true
		it.err = it.cur.Err()
		return false
	}
	var d bsonD
	if err := decodeV2(it.cur.Current, &d); err != nil {
		it.done = true
		it.err = err
		return false
	}
	it.doc = d
	return true
}

func (it *v1Iter) Doc() bsonD { return it.doc }

func (it *v1Iter) Err() error { return it.err }

func (it *v1Iter) Close(ctx context.Context) error { return it.cur.Close(ctx) }
