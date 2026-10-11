package mongocompare

import (
	"math/big"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// BucketPlan is one bucket of a collection's hash plan. The first and last
// buckets are OPEN-ENDED on purpose: bounds come from the source-side
// min/max, so target documents outside that range must still land in the
// first/last bucket instead of being silently dropped (the gap-regression
// case: [bounds[N-1], max) was once missed by an off-by-one).
type BucketPlan struct {
	Index int
	Query bson.D
	Lo    any
	Hi    any
}

// PlanBuckets splits the _id range [min, max] (source-side aggregate) into n
// buckets keyed by the min value's BSON type: ObjectId (96-bit BigInt
// interpolation), Date (ms), Int32/Int64 (BigInt), Double (float). Any other
// _id type (String, composite, ...) or degenerate range falls back to a
// single full-collection bucket.
func PlanBuckets(min, max any, n int) []BucketPlan {
	if n < 2 {
		return singleBucket()
	}
	var bounds []any
	switch mn := min.(type) {
	case bson.ObjectID:
		mx, ok := max.(bson.ObjectID)
		if ok {
			bounds = interpBig(bigFromOID(mn), bigFromOID(mx), n, func(v *big.Int) any {
				var oid bson.ObjectID
				b := v.Bytes()
				if len(b) > 12 {
					b = b[len(b)-12:]
				}
				copy(oid[12-len(b):], b)
				return oid
			})
		}
	case bson.DateTime:
		if mx, ok := max.(bson.DateTime); ok {
			bounds = interpInt(int64(mn), int64(mx), n, func(v int64) any { return bson.DateTime(v) })
		}
	case int32:
		if mx, ok := max.(int32); ok {
			bounds = interpInt(int64(mn), int64(mx), n, func(v int64) any { return int32(v) })
		}
	case int64:
		if mx, ok := max.(int64); ok {
			bounds = interpInt(mn, mx, n, func(v int64) any { return v })
		}
	case float64:
		if mx, ok := max.(float64); ok {
			bounds = interpFloat(mn, mx, n)
		}
	}
	if len(bounds) < 2 {
		return singleBucket()
	}

	plan := make([]BucketPlan, 0, n)
	for i := 0; i < n; i++ {
		var q bson.D
		switch {
		case i == 0:
			q = bson.D{{Key: "_id", Value: bson.D{{Key: "$lt", Value: bounds[1]}}}}
		case i == n-1:
			q = bson.D{{Key: "_id", Value: bson.D{{Key: "$gte", Value: bounds[n-1]}}}}
		default:
			q = bson.D{{Key: "_id", Value: bson.D{
				{Key: "$gte", Value: bounds[i]},
				{Key: "$lt", Value: bounds[i+1]},
			}}}
		}
		plan = append(plan, BucketPlan{Index: i, Query: q, Lo: bounds[i], Hi: bounds[i+1]})
	}
	return plan
}

func singleBucket() []BucketPlan {
	return []BucketPlan{{Index: 0, Query: bson.D{}}}
}

func bigFromOID(o bson.ObjectID) *big.Int {
	return new(big.Int).SetBytes(o[:])
}

// interpBig interpolates i=0..n over [lo, hi] with exact integer arithmetic.
func interpBig(lo, hi *big.Int, n int, mk func(*big.Int) any) []any {
	if hi.Cmp(lo) <= 0 {
		return nil
	}
	out := make([]any, n+1)
	span := new(big.Int).Sub(hi, lo)
	for i := 0; i <= n; i++ {
		v := new(big.Int).Mul(span, big.NewInt(int64(i)))
		v.Div(v, big.NewInt(int64(n)))
		v.Add(v, lo)
		out[i] = mk(v)
	}
	return out
}

func interpInt(lo, hi int64, n int, mk func(int64) any) []any {
	if hi <= lo {
		return nil
	}
	return interpBig(big.NewInt(lo), big.NewInt(hi), n, func(v *big.Int) any {
		return mk(v.Int64())
	})
}

func interpFloat(lo, hi float64, n int) []any {
	if !(hi > lo) {
		return nil
	}
	out := make([]any, n+1)
	for i := 0; i <= n; i++ {
		out[i] = lo + (hi-lo)*float64(i)/float64(n)
	}
	return out
}
