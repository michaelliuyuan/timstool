package canon

import (
	"math"
	"math/big"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// CompareIDs is the cross-type total-order comparator for _id values used by
// the L6 merge join (port of js compareIds). It returns (cmp, ok): ok=false
// means the heterogeneous pair cannot be precisely ordered (Object/Array/
// Regex _ids or unknown types) and the caller must fall back to a map join.
//
// Order (BSON canonical rank): MinKey < Null < Undefined < numbers(mixed by
// value) < String < Object < Array < Binary < ObjectId < Boolean < Date <
// Timestamp < Regex < MaxKey.
func CompareIDs(a, b any) (int, bool) {
	ra, oka := bsonRank(a)
	rb, okb := bsonRank(b)
	if !oka || !okb {
		return 0, false
	}
	if ra != rb {
		if ra < rb {
			return -1, true
		}
		return 1, true
	}
	switch ra {
	case rankNumeric:
		return compareNumericIDs(a, b), true
	case rankString:
		return cmpBytes(string(a.(string)), string(b.(string))), true
	case rankBinary:
		ba, bb := a.(bson.Binary), b.(bson.Binary)
		if ba.Subtype != bb.Subtype {
			if ba.Subtype < bb.Subtype {
				return -1, true
			}
			return 1, true
		}
		return cmpBytes(string(ba.Data), string(bb.Data)), true
	case rankObjectID:
		oa, ob := a.(bson.ObjectID), b.(bson.ObjectID)
		return cmpBytes(string(oa[:]), string(ob[:])), true
	case rankBoolean:
		ab, bb := boolVal(a), boolVal(b)
		switch {
		case ab == bb:
			return 0, true
		case !ab:
			return -1, true
		default:
			return 1, true
		}
	case rankDate:
		da, db := int64(a.(bson.DateTime)), int64(b.(bson.DateTime))
		return cmpInt(da, db), true
	case rankTimestamp:
		ta, tb := a.(bson.Timestamp), b.(bson.Timestamp)
		if ta.T != tb.T {
			return cmpUint(uint64(ta.T), uint64(tb.T)), true
		}
		return cmpUint(uint64(ta.I), uint64(tb.I)), true
	case rankObject, rankArray, rankRegex:
		// Composite _ids: ordering too complex → caller falls back to a map
		// join (mirrors the js null return).
		return 0, false
	default:
		// MinKey/Null/Undefined/MaxKey: same rank ⇒ equal.
		return 0, true
	}
}

const (
	rankMinKey = iota
	rankNull
	rankUndefined
	rankNumeric
	rankString
	rankObject
	rankArray
	rankBinary
	rankObjectID
	rankBoolean
	rankDate
	rankTimestamp
	rankRegex
	rankMaxKey
)

func bsonRank(v any) (int, bool) {
	switch v.(type) {
	case bson.MinKey:
		return rankMinKey, true
	case nil, bson.Null:
		return rankNull, true
	case bson.Undefined:
		return rankUndefined, true
	case float64, int32, int64, bson.Decimal128:
		return rankNumeric, true
	case string:
		return rankString, true
	case bson.D, bson.M, map[string]any:
		return rankObject, true // order too complex → not comparable
	case []any, bson.A:
		return rankArray, true
	case bson.Binary:
		return rankBinary, true
	case bson.ObjectID:
		return rankObjectID, true
	case bool:
		return rankBoolean, true
	case bson.DateTime:
		return rankDate, true
	case bson.Timestamp:
		return rankTimestamp, true
	case bson.Regex:
		return rankRegex, true // not comparable within rank
	case bson.MaxKey:
		return rankMaxKey, true
	default:
		return 0, false
	}
}

func boolVal(v any) bool {
	b, _ := v.(bool)
	return b
}

func compareNumericIDs(a, b any) int {
	na, nb := numRep(a), numRep(b)
	if na.big != nil && nb.big != nil {
		return na.big.Cmp(nb.big)
	}
	// Mixed exact/integral vs fractional: compare by value as float64.
	// (The js reference falls into a NaN comparison here and returns 0 for
	// e.g. int64(9) vs double(1.5); the Go port fixes this — the merge-join
	// order must be a strict total order across numeric types.)
	return cmpFloat(repFloat(na), repFloat(nb))
}

func repFloat(r numRepT) float64 {
	if r.big != nil {
		f, _ := new(big.Float).SetInt(r.big).Float64()
		return f
	}
	return r.float
}

type numRepT struct {
	big   *big.Int
	float float64
}

// numRep prefers exact big.Int (int32/int64, integral doubles within safe
// range, decimal128 with integral string); otherwise float64.
func numRep(x any) numRepT {
	switch v := x.(type) {
	case int32:
		return numRepT{big: big.NewInt(int64(v))}
	case int64:
		return numRepT{big: big.NewInt(v)}
	case bson.Decimal128:
		s := v.String()
		if isPlainInt(s) {
			n := new(big.Int)
			n.SetString(s, 10)
			return numRepT{big: n}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return numRepT{float: f}
	default:
		f, _ := x.(float64)
		if n, ok := exactInt(f); ok {
			return numRepT{big: n}
		}
		return numRepT{float: f}
	}
}

func isPlainInt(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i = 1
	}
	if i >= len(s) {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

const safeInt = 1 << 53

func exactInt(f float64) (*big.Int, bool) {
	if f != math.Trunc(f) || f < -safeInt || f > safeInt {
		return nil, false
	}
	return new(big.Int).SetInt64(int64(f)), true
}

func cmpBytes(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
