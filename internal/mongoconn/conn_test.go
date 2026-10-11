package mongoconn

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	v2mongo "go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/michaelliuyuan/timstool/internal/source"
)

// TestBrowsableType: the empty type (older servers/drivers) must be treated
// as a collection so those collections are never silently dropped from
// listings (the M1 guard, now shared by both driver paths).
func TestBrowsableType(t *testing.T) {
	for _, typ := range []string{"collection", ""} {
		if !browsableType(typ) {
			t.Errorf("browsableType(%q) = false, want true", typ)
		}
	}
	if browsableType("view") {
		t.Error(`browsableType("view") = true, want false`)
	}
}

// TestDialConfigAddr: probe errors must name the exact target — defaults
// applied for host/port, bare URI shown verbatim.
func TestDialConfigAddr(t *testing.T) {
	if got := (DialConfig{}).Addr(); got != "localhost:27017" {
		t.Errorf("Addr() = %q, want localhost:27017", got)
	}
	if got := (DialConfig{Host: "mdb1", Port: 28018}).Addr(); got != "mdb1:28018" {
		t.Errorf("Addr() = %q, want mdb1:28018", got)
	}
	if got := (DialConfig{URI: "mongodb://x/y"}).Addr(); got != "mongodb://x/y" {
		t.Errorf("Addr() = %q, want the bare URI", got)
	}
	// Discrete fields win over URI in the address rendering too.
	if got := (DialConfig{Host: "h", Port: 2, URI: "mongodb://x"}).Addr(); got != "h:2" {
		t.Errorf("Addr() = %q, want h:2 (discrete wins)", got)
	}
}

// TestDialProbeFailure is the third state of the probe matrix (M5 gate ④):
// an unreachable target must fail with the hard ProbeError that carries the
// address and BOTH drivers' errors — never a silent wrong-path fallback.
// The unreachable address is a local refused port; SelectionTimeout keeps
// each driver's selection wait short.
func TestDialProbeFailure(t *testing.T) {
	cfg := DialConfig{Host: "127.0.0.1", Port: 1, SelectionTimeout: 500 * time.Millisecond}
	start := time.Now()
	_, err := Dial(context.Background(), cfg)
	if err == nil {
		t.Fatal("Dial to refused port succeeded, want ProbeError")
	}
	var pe *ProbeError
	if !errors.As(err, &pe) {
		t.Fatalf("Dial err = %T (%v), want *ProbeError", err, err)
	}
	msg := pe.Error()
	for _, want := range []string{"probe", "127.0.0.1:1", "v2 driver", "v1 driver"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ProbeError missing %q: %s", want, msg)
		}
	}
	if pe.V2 == nil || pe.V1 == nil {
		t.Error("ProbeError must carry both drivers' errors", pe.V2, pe.V1)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("probe took %v, SelectionTimeout not honored", elapsed)
	}
}

// TestV2IndexInfos covers the driver-spec → source.IndexInfo conversion:
// order-preserved keys, unique flag, TTL passthrough (M2 hard requirement:
// the engine skips TTL collections via ExpireAfterSeconds), plus the M5
// sparse/clustered passthrough.
func TestV2IndexInfos(t *testing.T) {
	ttl := int32(3600)
	unique := true
	sparse := true
	compound := mustBSON(t, bson.D{{Key: "a", Value: 1}, {Key: "b", Value: -1}})
	single := mustBSON(t, bson.D{{Key: "createdAt", Value: 1}})
	textish := mustBSON(t, bson.D{{Key: "title", Value: "text"}})

	specs := []v2mongo.IndexSpecification{
		{Name: "compound_idx", KeysDocument: compound},
		{Name: "ttl_idx", KeysDocument: single, Unique: &unique, ExpireAfterSeconds: &ttl},
		{Name: "text_idx", KeysDocument: textish, Sparse: &sparse},
	}

	out := v2IndexInfos(specs)
	if len(out) != 3 {
		t.Fatalf("v2IndexInfos len = %d, want 3", len(out))
	}

	// Order-preserved compound keys.
	if k := out[0].Keys; len(k) != 2 || k[0].Field != "a" || k[1].Field != "b" {
		t.Errorf("compound keys = %+v, want [a b] in order", k)
	}
	if v, ok := out[0].Keys[0].Order.(int32); !ok || v != 1 {
		t.Errorf("keys[0].Order = %#v, want int32(1)", out[0].Keys[0].Order)
	}

	// Unique + TTL passthrough.
	if !out[1].Unique || out[1].ExpireAfterSeconds == nil || *out[1].ExpireAfterSeconds != 3600 {
		t.Errorf("ttl_idx = %+v, want Unique + ExpireAfterSeconds=3600", out[1])
	}
	if out[0].ExpireAfterSeconds != nil {
		t.Errorf("non-TTL index ExpireAfterSeconds = %v, want nil", out[0].ExpireAfterSeconds)
	}

	// Sparse passthrough (M5).
	if !out[2].Sparse {
		t.Errorf("text_idx Sparse = false, want true")
	}

	// Non-numeric direction (text) survives as-is.
	if v, ok := out[2].Keys[0].Order.(string); !ok || v != "text" {
		t.Errorf("text direction = %#v, want \"text\"", out[2].Keys[0].Order)
	}

	// The neutral type must keep the JSON shape stable (wire contract for
	// /sources/meta consumers).
	if out[1].Clustered {
		t.Error("Clustered set without driver value, want false")
	}
}

// TestIndexInfoNeutralShape pins the neutral IndexInfo JSON tags — the v1
// bridge and the v2 path both produce this shape, and the engine's canonical
// index form derives from it (M5: driver-agnostic index comparison).
func TestIndexInfoNeutralShape(t *testing.T) {
	ttl := int32(60)
	ix := source.IndexInfo{
		Name:               "i",
		Keys:               []source.IndexKey{{Field: "a", Order: int32(1)}},
		Unique:             true,
		Sparse:             true,
		ExpireAfterSeconds: &ttl,
	}
	if ix.Name != "i" || len(ix.Keys) != 1 || !ix.Unique || !ix.Sparse || *ix.ExpireAfterSeconds != 60 {
		t.Errorf("IndexInfo round-trip mismatch: %+v", ix)
	}
}

func mustBSON(t *testing.T, doc bson.D) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(doc)
	if err != nil {
		t.Fatalf("bson.Marshal: %v", err)
	}
	return bson.Raw(b)
}

// TestNormalizeIndexOrder pins the seq=176 ruling's narrow fold: the integer
// forms of +1/-1 (int32/int64/double) collapse to int32 — the 3.x-vs-modern
// serialization split of index key directions (double 1.0 vs int32 1, same
// semantics) must not leak into the canonical index form. Everything else
// passes through untouched: string directions ("2d"/"2dsphere"/"text"/
// "hashed"), nested wildcard key documents, and any numeric value beyond
// ±1 — a same-semantics fold, not a general numeric taming.
//
// Same-class scan across IndexInfo's numeric surface (seq=176 refinement 2):
// ExpireAfterSeconds is the only other numeric field and is already strongly
// typed *int32 on BOTH decode paths (v1 struct tag, v2 driver spec), so its
// canonical form is always the int32 tag — no same-class exposure found,
// recorded here per the ruling.
func TestNormalizeIndexOrder(t *testing.T) {
	fold := []struct {
		in  any
		out int32
	}{
		{int32(1), 1}, {int64(1), 1}, {float64(1), 1},
		{int32(-1), -1}, {int64(-1), -1}, {float64(-1), -1},
	}
	for _, c := range fold {
		got := normalizeIndexOrder(c.in)
		v, ok := got.(int32)
		if !ok || v != c.out {
			t.Errorf("normalizeIndexOrder(%#v) = %#v, want int32(%d)", c.in, got, c.out)
		}
	}
	wild := mustBSON(t, bson.D{{Key: "$**", Value: 1}})
	var wildcard bson.D
	if err := bson.Unmarshal(wild, &wildcard); err != nil {
		t.Fatalf("unmarshal wildcard: %v", err)
	}
	pass := []any{"2d", "2dsphere", "text", "hashed", int32(2), int64(2), float64(2), float64(1.5), wildcard, nil}
	for _, v := range pass {
		if got := normalizeIndexOrder(v); !reflect.DeepEqual(got, v) {
			t.Errorf("normalizeIndexOrder(%#v) = %#v, want untouched passthrough", v, got)
		}
	}
}
