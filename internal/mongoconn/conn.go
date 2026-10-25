// Package mongoconn is the version-adaptive MongoDB connection layer (M5):
// one Client interface over two drivers. The compare engine, the meta
// browser (/sources/meta) and the connection test (M6 datasource test) all
// consume this interface; which driver answers is decided per connection at
// Dial time.
//
// Driver policy (M5 arbitration): mongo-driver v2 requires wire >= 9
// (MongoDB 4.4+); older servers (3.x, wire 4-8) are served through a v1
// driver bridge. Selection is per-connection (no global caching), probed by
// the isMaster handshake (maxWireVersion — supported across 3.0..7.x, unlike
// `hello` which postdates 3.x), and a failed probe is a hard error that
// carries both drivers' errors — never a silent wrong-path fallback.
//
// The v1 bridge keeps a single rule: v1 is a byte pipe. Cursors and command
// results are converted as raw BSON bytes and decoded exclusively by the v2
// bson package (bytes in, v2 types out — the "single-point bridge"); the
// canonicalizer never sees a v1 Go type, so seq=50's single-decode-form
// arbitration holds on both paths. Outbound queries cross the same bridge in
// reverse (v2 marshal -> bytes -> v1 unmarshal).
package mongoconn

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// bsonD aliases the v2 document type: every bson.D flowing through this
// package's interfaces is the v2 one (single-decode-form, seq=50).
type bsonD = bson.D

// MinWireV2 is the wire floor of the v2 driver path (MongoDB 4.4+).
const MinWireV2 = 9

// CollType is one collection's name and listCollections type ("collection",
// "view", ...). An empty Type means the server/driver did not report one and
// — per the M1 guard — must be treated as a collection by consumers.
type CollType struct {
	Name string
	Type string
}

// IsCollection reports whether the entry carries comparable data: views
// ("view", timeseries-buckets companions, ...) do not. An empty Type is a
// collection per the M1 guard (servers/drivers that omit the field never
// lose their collections).
func (ct CollType) IsCollection() bool { return browsableType(ct.Type) }

// Client is the unified connection surface: compare engine (Collection and
// the listing primitives), meta browser (ListDatabases/ListCollections with
// full detail) and connection test (Ping/Version) all sit on it.
type Client interface {
	// Ping verifies the connection is alive.
	Ping(ctx context.Context) error
	// Version returns the server version (buildInfo), prefixed "MongoDB ".
	Version(ctx context.Context) (string, error)
	// ListDatabases lists the databases visible to the connected user.
	ListDatabases(ctx context.Context) ([]source.DatabaseInfo, error)
	// ListCollections lists one database's collections/views with browse
	// detail (count/sizes/indexes where available).
	ListCollections(ctx context.Context, database string) ([]source.CollectionInfo, error)
	// ListCollectionNames lists just the names (engine's existence checks;
	// cheaper than the full browse).
	ListCollectionNames(ctx context.Context, database string) ([]string, error)
	// ListCollectionNameTypes lists name+type pairs via the nameOnly
	// listing (engine's view filter + per-db existence checks — MC-9 A2:
	// one round trip per database instead of one per collection).
	ListCollectionNameTypes(ctx context.Context, database string) ([]CollType, error)
	// Collection returns the ops handle for one collection.
	Collection(database, coll string) Coll
	// Driver reports which path is serving: "v2" or "v1" (diagnostics/logs —
	// makes the probe decision observable).
	Driver() string
	// Disconnect closes the connection (idempotent).
	Disconnect(ctx context.Context) error
}

// Coll is the engine-facing per-collection surface. All BSON types are v2.
type Coll interface {
	// Find streams documents matching query sorted by _id ascending (the
	// engine's only sort), batched per opts; limit 0 means unbounded.
	Find(ctx context.Context, query bsonD, opts FindOpts) (DocIter, error)
	// IDRange returns the _id min/max/count over the documents matching
	// match (empty match = whole collection; empty result: N=0). The
	// watermark window rides the initial $match so bucket planning is
	// clamped inside the window (第9单 ④).
	IDRange(ctx context.Context, match bsonD) (RangeStats, error)
	// CountDocuments is the exact count of documents matching filter
	// (empty filter = whole collection — byte-identical legacy behavior).
	CountDocuments(ctx context.Context, filter bsonD) (int64, error)
	// IndexSpecs lists the collection's indexes (engine: TTL probe + index
	// level; sparse/clustered ride along for the canonical index form).
	IndexSpecs(ctx context.Context) ([]source.IndexInfo, error)
}

// DocIter streams decoded v2 bson.D documents.
type DocIter interface {
	// Next advances; true when a document is available (then Doc is valid).
	Next(ctx context.Context) bool
	// Doc returns the document decoded from the last Next (v2 types only).
	Doc() bsonD
	// Err reports the terminal iteration error, if any.
	Err() error
	// Close releases the cursor.
	Close(ctx context.Context) error
}

// FindOpts tunes Find (batch<=0 defaults 1000; limit 0 unbounded).
type FindOpts struct {
	Batch int
	Limit int64
}

// RangeStats is the _id min/max/count aggregate feeding the bucket plan.
// Min/Max carry v2 BSON types (ObjectID, string, ...) on both driver paths.
type RangeStats struct {
	Min any
	Max any
	N   int64
}

// DialConfig is one endpoint's connection parameters (structured — no URI
// string building, the M1 convention). A bare URI is the fallback when Host
// is empty (discrete fields win when both are given, the compareEndpoint
// convention); URI escaping is then the caller's responsibility.
type DialConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	AuthDB   string
	TLS      bool
	URI      string
	// SelectionTimeout caps the per-driver server-selection wait during the
	// probe (0 = driver default). The connection test surface (M6) and the
	// probe-failure tests use it to fail fast on unreachable targets.
	SelectionTimeout time.Duration
}

// Addr renders the dial target with the M1 defaults applied (empty host ->
// localhost, zero port -> 27017), or the bare URI — used in probe errors so
// they name the exact target the user typed.
func (c DialConfig) Addr() string {
	if c.Host == "" && c.URI != "" {
		return c.URI
	}
	host := c.Host
	if host == "" {
		host = "localhost"
	}
	port := c.Port
	if port == 0 {
		port = 27017
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// ProbeError is the hard failure of the driver-selection handshake: both
// candidate connections failed. It reports the target address and both
// drivers' errors — the anti-silent-fallback contract (a wrong driver pick
// would surface later as cryptic wire errors instead).
type ProbeError struct {
	Addr string
	V2   error
	V1   error
}

func (e *ProbeError) Error() string {
	return fmt.Sprintf("mongodb probe %s failed: v2 driver: %v; v1 driver: %v", e.Addr, e.V2, e.V1)
}

// Dial connects to the endpoint and returns the serving Client. Selection:
// the v2 path is tried first (modern servers, one round trip); when v2 cannot
// even complete its handshake the v1 bridge is tried, and if that connects,
// the server's real maxWireVersion decides — <MinWireV2 stays on v1 (old
// server), >= MinWireV2 also stays on v1 (v2-declared floor disagreed with
// reality; the bridge speaks the protocol, the connection works — Driver()
// makes the anomaly observable). Both failing is a ProbeError.
func Dial(ctx context.Context, cfg DialConfig) (Client, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	v2c, v2err := dialV2(probeCtx, cfg)
	if v2c != nil {
		return v2c, nil // modern path, handshake verified
	}
	v1c, v1err := dialV1(probeCtx, cfg)
	if v1c != nil {
		return v1c, nil // legacy bridge (old server, or v2/v1 floor mismatch)
	}
	return nil, &ProbeError{Addr: cfg.Addr(), V2: v2err, V1: v1err}
}

// normalizeIndexOrder folds the cross-version serialization split of index
// key directions at the DECODE point (seq=176 ruling): 3.x servers report
// key values as double 1.0/-1.0 while modern servers report int32 — same
// semantics, different canonical type tag, so a 3.x↔7.x compare would flag
// every numeric index key as a diff (the seq=175 cross-instance false
// positive). The fold is deliberately narrow per the ruling: only the
// integer forms of +1/-1 collapse to int32; string directions ("2d",
// "2dsphere", "text", "hashed"), nested wildcard keys and any other numeric
// value pass through untouched — this is a same-semantics fold, not a
// general numeric taming, and canon itself stays untouched (red line).
func normalizeIndexOrder(v any) any {
	switch n := v.(type) {
	case int32:
		if n == 1 || n == -1 {
			return int32(n)
		}
	case int64:
		if n == 1 || n == -1 {
			return int32(n)
		}
	case float64:
		if n == 1 || n == -1 {
			return int32(n)
		}
	}
	return v
}
