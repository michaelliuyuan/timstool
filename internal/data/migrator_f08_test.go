package data

import (
	"reflect"
	"testing"
)

// F-08 anchor: FK-aware import ordering. topoLevels must place every
// parent before its children, keep each level alphabetical (parallel-safe,
// deterministic), tolerate cycles and self-references, and preserve plain
// alphabetical order when there are no edges.
func TestTopoLevels(t *testing.T) {
	// 1. No FKs: single alphabetical level.
	got := topoLevels([]string{"b", "a", "c"}, nil)
	want := [][]string{{"a", "b", "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no-FK case: got %v want %v", got, want)
	}

	// 2. Parent chain: gp -> parent -> child.
	edges := map[string][]string{"parent": {"gp"}, "child": {"parent"}}
	got = topoLevels([]string{"child", "parent", "gp"}, edges)
	want = [][]string{{"gp"}, {"parent"}, {"child"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain case: got %v want %v", got, want)
	}

	// 3. Diamond: d depends on b and c, both depend on a; b/c share a level.
	edges = map[string][]string{"b": {"a"}, "c": {"a"}, "d": {"b", "c"}}
	got = topoLevels([]string{"d", "c", "b", "a"}, edges)
	want = [][]string{{"a"}, {"b", "c"}, {"d"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diamond case: got %v want %v", got, want)
	}

	// 4. Cycle: x<->y mutually reference; z independent. Cycle remainder
	// lands in the final level, z (and any satisfied tables) first.
	edges = map[string][]string{"x": {"y"}, "y": {"x"}}
	got = topoLevels([]string{"x", "y", "z"}, edges)
	want = [][]string{{"z"}, {"x", "y"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cycle case: got %v want %v", got, want)
	}

	// 5. Self-reference and out-of-set parents are ignored.
	edges = map[string][]string{"self": {"self"}, "child": {"parent", "ghost"}}
	got = topoLevels([]string{"child", "self", "parent"}, edges)
	want = [][]string{{"parent", "self"}, {"child"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("self/out-of-set case: got %v want %v", got, want)
	}
}
