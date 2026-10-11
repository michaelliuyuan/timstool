package canon

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"sort"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// docViewShape is the minimal shape BucketHash needs from a row: the _id
// lookup. Rows arrive as bson.D / map[string]any from the respective cursor
// decode paths.
func rowID(row any) (any, bool) {
	switch d := row.(type) {
	case bson.D:
		for _, e := range d {
			if e.Key == "_id" {
				return e.Value, true
			}
		}
	case map[string]any:
		v, ok := d["_id"]
		return v, ok
	}
	return nil, false
}

// DocHash = sha256( canonical(_id) + "\n" + canonical(doc) + "\n" ) (spec §9
// v1 revision: per-document hash feeding the bucket sum).
func DocHash(doc any, opts Opts) string {
	o := opts.withDefaults()
	id, ok := rowID(doc)
	h := sha256.New()
	if ok {
		h.Write([]byte(Canonical(id, o)))
	}
	h.Write([]byte("\n"))
	h.Write([]byte(Canonical(doc, o)))
	h.Write([]byte("\n"))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// BucketHash is the sorted-concat reference implementation (js bucketHash):
// rows sorted by canonical(_id) ascending, then sha256 of the concatenated
// per-document blocks. Kept as the cross-check baseline; the engine uses the
// streaming BucketHasher.
func BucketHash(rows []any, opts Opts) string {
	o := opts.withDefaults()
	type keyed struct {
		idCanon string
		doc     any
	}
	ks := make([]keyed, 0, len(rows))
	for _, r := range rows {
		id, _ := rowID(r)
		ks = append(ks, keyed{Canonical(id, o), r})
	}
	// Sort by canonical _id string (bytewise). Duplicate canonical _ids
	// (different types sharing a string cannot happen — the tag prefixes
	// differ) keep a stable order; identical docs hash identically anyway.
	sort.SliceStable(ks, func(i, j int) bool { return ks[i].idCanon < ks[j].idCanon })
	h := sha256.New()
	for _, k := range ks {
		h.Write([]byte(k.idCanon))
		h.Write([]byte("\n"))
		h.Write([]byte(Canonical(k.doc, o)))
		h.Write([]byte("\n"))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// BucketHasher is the streaming bucket accumulator (spec §9 v1 revision):
// bucket hash = ( Σ sha256-blocks ) mod 2^256, order-independent, with the
// per-doc block identical to DocHash. O(1) documents in memory; equal iff the
// sorted-concat form is equal (multiset equality), cross-checked in tests.
type BucketHasher struct {
	opts Opts
	sum  big.Int
	n    int64
}

func NewBucketHasher(opts Opts) *BucketHasher {
	return &BucketHasher{opts: opts.withDefaults()}
}

func (b *BucketHasher) Update(doc any) {
	var v big.Int
	d := DocHash(doc, b.opts)
	v.SetString(d, 16)
	b.sum.Add(&b.sum, &v)
	b.sum.And(&b.sum, mask256)
	b.n++
}

// BucketDigest is the accumulated result: document count + 64-hex bucket hash.
type BucketDigest struct {
	Count int64  `json:"count"`
	Hash  string `json:"hash"`
}

func (b *BucketHasher) Digest() BucketDigest {
	return BucketDigest{Count: b.n, Hash: fmt.Sprintf("%064x", &b.sum)}
}

var mask256 = func() *big.Int {
	m := new(big.Int)
	m.Lsh(big.NewInt(1), 256)
	m.Sub(m, big.NewInt(1))
	return m
}()
