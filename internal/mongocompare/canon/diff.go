package canon

import (
	"sort"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Delta is one field-level difference between two documents. Type is one of
// "modified" (both present, canonical differs), "missing-in-a" (present only
// in b), "missing-in-b" (present only in a). Key is the dot-joined leaf path
// with array positions as numeric segments ("arr.2.c").
type Delta struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

// DiffResult is the diffDocs outcome (spec §11).
type DiffResult struct {
	Equal  bool    `json:"equal"`
	Deltas []Delta `json:"deltas,omitempty"`
}

// DiffDocs compares two documents field-by-field. missingAsNull and
// numericTolerance apply recursively at any depth (spec §missingAsNull);
// array positions are positional and never normalized ([1,null] vs [1] is a
// diff at leaf path "v.1: missing-in-b"). Beyond maxDepth the whole branch is
// compared canonically and reported as "modified" at that path.
func DiffDocs(a, b any, opts Opts) DiffResult {
	o := opts.withDefaults()
	d := &differ{opts: o}
	d.walk(a, b, "", 0)
	return DiffResult{Equal: len(d.deltas) == 0, Deltas: d.deltas}
}

type differ struct {
	opts   Opts
	deltas []Delta
}

func (d *differ) push(path, typ string) {
	d.deltas = append(d.deltas, Delta{Key: path, Type: typ})
}

func (d *differ) handleMissing(present any, path, typ string) {
	if d.opts.MissingAsNull && isNull(present) {
		return
	}
	d.push(path, typ)
}

func (d *differ) walk(va, vb any, prefix string, depth int) {
	if depth > d.opts.MaxDepth {
		if Canonical(va, d.opts) != Canonical(vb, d.opts) {
			p := prefix
			if p == "" {
				p = "<root>"
			}
			d.push(p, "modified")
		}
		return
	}
	if d.opts.NumericTolerance && IsNumeric(va) && IsNumeric(vb) {
		if NumericString(va) != NumericString(vb) {
			d.push(prefix, "modified")
		}
		return
	}

	ka, aIsDoc := asDoc(va)
	kb, bIsDoc := asDoc(vb)
	if aIsDoc && bIsDoc {
		d.walkObj(ka, kb, prefix, depth)
		return
	}
	aa, aIsArr := asArray(va)
	ab, bIsArr := asArray(vb)
	if aIsArr && bIsArr {
		d.walkArr(aa, ab, prefix, depth)
		return
	}
	if Canonical(va, d.opts) != Canonical(vb, d.opts) {
		d.push(prefix, "modified")
	}
}

func (d *differ) walkObj(a, b docView, prefix string, depth int) {
	union := make([]string, 0, len(a.keys())+len(b.keys()))
	seen := make(map[string]bool, len(union))
	for _, k := range append(a.keys(), b.keys()...) {
		if !seen[k] {
			seen[k] = true
			union = append(union, k)
		}
	}
	sort.Strings(union)
	for _, k := range union {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		va, inA := a.get(k)
		vb, inB := b.get(k)
		switch {
		case inA && inB:
			d.walk(va, vb, p, depth+1)
		case inA:
			d.handleMissing(va, p, "missing-in-b")
		default:
			d.handleMissing(vb, p, "missing-in-a")
		}
	}
}

func (d *differ) walkArr(a, b []any, prefix string, depth int) {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		p := prefix
		if p != "" {
			p += "."
		}
		p += strconv.Itoa(i)
		if i < len(a) && i < len(b) {
			d.walk(a[i], b[i], p, depth+1)
		} else if i < len(a) {
			d.push(p, "missing-in-b")
		} else {
			d.push(p, "missing-in-a")
		}
	}
}

// docView unifies the document shapes the driver can decode into (bson.D,
// bson.M, map[string]any) for order-insensitive walking.
type docView struct {
	keysOrdered []string
	getFunc     func(string) (any, bool)
}

func (d docView) keys() []string           { return d.keysOrdered }
func (d docView) get(k string) (any, bool) { return d.getFunc(k) }

func asDoc(v any) (docView, bool) {
	switch d := v.(type) {
	case bson.D:
		keys := make([]string, 0, len(d))
		m := make(map[string]any, len(d))
		for _, e := range d {
			keys = append(keys, e.Key)
			m[e.Key] = e.Value
		}
		return docView{keys, func(k string) (any, bool) { v, ok := m[k]; return v, ok }}, true
	case map[string]any:
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		return docView{keys, func(k string) (any, bool) { v, ok := d[k]; return v, ok }}, true
	}
	return docView{}, false
}

func asArray(v any) ([]any, bool) {
	switch a := v.(type) {
	case []any:
		return a, true
	case bson.A:
		return []any(a), true
	}
	return nil, false
}
