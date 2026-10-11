// Package canon is the deterministic canonical-BSON serialization core of the
// MongoDB compare engine (M2), a Go port of mongo-dbcheck's js/canonical.js
// per docs/canonical-bson-spec.md v1 (the spec is the authority; js code is
// the behavioral reference). Where the Go runtime diverges from JS it is
// stronger: decoding BSON into `any` preserves concrete types (int32/int64/
// float64/bson.Decimal128/...), so the type-sensitivity requirement (spec §4)
// holds without promoteValues gymnastics.
//
// DECODE-FORM CONSTRAINT (M2 arbitration seq=50): the engine feeds canon with
// a single decode form — bson.D — everywhere (cursors decode into bson.D;
// bson.Raw boundaries go through bson.Unmarshal(&bson.D)). bson.M/map input
// shapes are still accepted for standalone use, but the engine MUST NOT
// produce them: duplicate-key bson.D and bson.M collapse differently, which
// would break canonical stability across decode paths.
//
// Canonical strings are consumed only by this engine (hashing + diffing both
// endpoints), so exact byte parity with the js implementation is NOT required;
// determinism per-process is. Documented divergences: double formatting uses
// Go's shortest-round-trip form (JS parity holds for ordinary magnitudes like
// "0.1"/"1"; extreme exponents may differ, which does not affect correctness).
package canon

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Opts mirrors spec §10. Zero value = defaults (missingAsNull=false,
// numericTolerance=false, maxDepth=100, sha256).
type Opts struct {
	MissingAsNull    bool
	NumericTolerance bool
	MaxDepth         int
}

// DefaultMaxDepth is the spec default nesting cap (spec §8).
const DefaultMaxDepth = 100

func (o Opts) withDefaults() Opts {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	return o
}

// Canonical serializes one BSON value/document to its canonical string.
// Accepts the Go shapes the driver decodes into `any`: bson.D / bson.M /
// map[string]any (documents), []any (arrays), nil (BSON null), and the bson
// primitives (ObjectID, Decimal128, Binary, Timestamp, Regex, DateTime,
// JavaScript, Symbol, DBPointer, CodeWithScope, Undefined, Null, MinKey,
// MaxKey) plus string/bool/int32/int64/float64.
func Canonical(x any, opts Opts) string {
	return serialize(x, opts.withDefaults(), 0)
}

// CanonicalHash is sha256 of Canonical (spec §9).
func CanonicalHash(x any, opts Opts) string {
	sum := sha256.Sum256([]byte(Canonical(x, opts)))
	return fmt.Sprintf("%x", sum)
}

func serialize(x any, opts Opts, depth int) string {
	if depth > opts.MaxDepth {
		return "u:undefined" // spec §8: over-deep truncation
	}

	switch v := x.(type) {
	case nil:
		return "z:null"
	case bson.Null:
		return "z:null"
	case bson.Undefined:
		return "u:undefined"
	case bool:
		return "l:" + strconv.FormatBool(v)
	case string:
		return "s:" + escapeStr(v)
	case float64:
		if opts.NumericTolerance {
			return "n:" + numericStringFloat(v)
		}
		return "d:" + doubleCanon(v)
	case int32:
		if opts.NumericTolerance {
			return "n:" + strconv.FormatInt(int64(v), 10)
		}
		return "c:" + strconv.FormatInt(int64(v), 10)
	case int64:
		if opts.NumericTolerance {
			return "n:" + strconv.FormatInt(v, 10)
		}
		return "g:" + strconv.FormatInt(v, 10)
	case bson.Decimal128:
		s := decimalNumericString(v)
		if opts.NumericTolerance {
			return "n:" + s
		}
		return "m:" + s
	case bson.ObjectID:
		return "i:" + v.Hex()
	case bson.DateTime:
		return "D:" + strconv.FormatInt(int64(v), 10)
	case bson.Regex:
		return "r:/" + v.Pattern + "/" + sortFlags(v.Options)
	case bson.Binary:
		return "b:" + strconv.Itoa(int(v.Subtype)) + ":" + base64.StdEncoding.EncodeToString(v.Data)
	case bson.Timestamp:
		return "t:" + strconv.FormatUint(uint64(v.T), 10) + ":" + strconv.FormatUint(uint64(v.I), 10)
	case bson.MinKey:
		// "n:MinKey" / "x:MaxKey" are a faithful copy of the js reference
		// alphabet (mongo-dbcheck js/canonical.js:152-153), not a typo — the
		// asymmetry and the "n:" double-duty with numericTolerance's "n:"
		// prefix exist there too (no collision today: MinKey is not a numeric
		// string). M4 must pick fresh letters for any new type.
		return "n:MinKey"
	case bson.MaxKey:
		return "x:MaxKey"
	case bson.JavaScript:
		return "j:" + string(v)
	case bson.CodeWithScope:
		// Spec §4 row J (code + recursively normalized scope). The js
		// reference left CodeWithScope to its unknown-type fallback; the
		// spec table is followed here.
		return "J:" + string(v.Code) + ":" + serialize(v.Scope, opts, depth+1)
	case bson.DBPointer:
		return "p:" + v.DB + `\0` + serialize(v.Pointer, opts, depth+1)
	case bson.Symbol:
		return "y:" + escapeStr(string(v))
	case []any:
		parts := make([]string, 0, len(v))
		for _, el := range v {
			parts = append(parts, serialize(el, opts, depth+1))
		}
		return "a:[" + strings.Join(parts, ",") + "]"
	case bson.A:
		parts := make([]string, 0, len(v))
		for _, el := range v {
			parts = append(parts, serialize(el, opts, depth+1))
		}
		return "a:[" + strings.Join(parts, ",") + "]"
	case bson.D:
		m := make(map[string]any, len(v))
		keys := make([]string, 0, len(v))
		for _, e := range v {
			keys = append(keys, e.Key)
			m[e.Key] = e.Value
		}
		return serializeDoc(keys, func(k string) any { return m[k] }, opts, depth)
	case bson.M:
		return serializeDoc(mapKeys(v), func(k string) any { return v[k] }, opts, depth)
	case map[string]any:
		return serializeDoc(mapKeys(v), func(k string) any { return v[k] }, opts, depth)
	default:
		// Unknown-type fallback: stable rendering, never a panic (mirrors
		// the js reference's "?:" branch).
		return "?:(" + fmt.Sprintf("%T", x) + "):" + escapeStr(fmt.Sprintf("%v", x))
	}
}

// serializeDoc emits `o:{k:c,...}` with keys in UTF-8 byte order (spec §2) and
// the missingAsNull object-field elision (spec §missingAsNull; arrays keep
// positional semantics and are never elided).
func serializeDoc(keys []string, get func(string) any, opts Opts, depth int) string {
	sort.Strings(keys) // Go string compare IS UTF-8 byte order
	var b strings.Builder
	b.WriteString("o:{")
	first := true
	for _, k := range keys {
		v := get(k)
		if opts.MissingAsNull && isNull(v) {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(escapeStr(k))
		b.WriteByte(':')
		b.WriteString(serialize(v, opts, depth+1))
	}
	b.WriteByte('}')
	return b.String()
}

func docKeys(d bson.D) []string {
	keys := make([]string, 0, len(d))
	for _, e := range d {
		keys = append(keys, e.Key)
	}
	return keys
}

func mapKeys[M ~map[string]any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func isNull(v any) bool {
	return v == nil || v == bson.Null{}
}

// escapeStr escapes the separator-relevant characters (spec §1: the output
// must be unambiguous: backslash, NUL, newline, carriage return, colon).
func escapeStr(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		"\x00", `\u0000`,
		"\n", `\n`,
		"\r", `\r`,
		`:`, `\:`,
	)
	return r.Replace(s)
}

// doubleCanon implements spec §6: NaN (with NaN==NaN by unique spelling),
// ±Infinity, -0 collapses to 0, otherwise Go's shortest round-trip decimal
// (same injectivity property the js String(double) relies on).
func doubleCanon(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	case v == 0:
		return "0" // covers -0
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

func numericStringFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	case v == 0:
		return "0"
	default:
		// 'f' (never scientific): cross-type tolerance equality must hold at
		// exponent-switch magnitudes — double(2^53) renders
		// "9007199254740992" matching int64, and 1e-7 renders "0.0000001"
		// matching decimal128 ('g' would emit "9.007...e+15"/"1e-07").
		// (The non-tolerance doubleCanon keeps 'g' — injectivity only.)
		return trimZeros(strconv.FormatFloat(v, 'f', -1, 64))
	}
}

// NumericString renders any numeric BSON value as the normalized decimal
// string used by numericTolerance (spec §10: "n:<normalized decimal>").
func NumericString(x any) string {
	switch v := x.(type) {
	case float64:
		return numericStringFloat(v)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case bson.Decimal128:
		return decimalNumericString(v)
	default:
		return ""
	}
}

// decimalNumericString renders a Decimal128 as a plain (never scientific)
// decimal string with trailing zeros trimmed. The Go driver's String()
// switches to "1E-7" notation for small exponents where the js toString
// expands — normalization keeps m:/n: stable and cross-type comparable.
func decimalNumericString(v bson.Decimal128) string {
	return trimZeros(expandScientific(v.String()))
}

// expandScientific converts "1E-7"/"1.5E+3" to "0.0000001"/"1500"; plain
// strings pass through unchanged.
func expandScientific(s string) string {
	i := strings.IndexAny(s, "eE")
	if i < 0 {
		return s
	}
	exp, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return s
	}
	mant := s[:i]
	neg := strings.HasPrefix(mant, "-")
	if neg {
		mant = mant[1:]
	}
	intPart, fracPart := mant, ""
	if dot := strings.IndexByte(mant, '.'); dot >= 0 {
		intPart, fracPart = mant[:dot], mant[dot+1:]
	}
	digits := intPart + fracPart
	point := len(intPart) + exp
	var out string
	switch {
	case point <= 0:
		out = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		out = digits + strings.Repeat("0", point-len(digits))
	default:
		out = digits[:point] + "." + digits[point:]
	}
	if neg {
		out = "-" + out
	}
	return out
}

// IsNumeric reports whether x is one of the four numeric BSON types.
func IsNumeric(x any) bool {
	switch x.(type) {
	case float64, int32, int64, bson.Decimal128:
		return true
	}
	return false
}

// trimZeros removes trailing fraction zeros (1.50→1.5, 1.00→1) and passes
// special values through (spec §5).
func trimZeros(s string) string {
	if s == "NaN" || s == "Infinity" || s == "-Infinity" {
		return s
	}
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s
	}
	frac := s[dot+1:]
	trimmed := strings.TrimRight(frac, "0")
	if trimmed == "" {
		return s[:dot]
	}
	return s[:dot+1] + trimmed
}

// sortFlags alphabetizes regex option letters (spec §4 row r).
func sortFlags(opts string) string {
	b := []byte(opts)
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	return string(b)
}
