// Package diff structurally compares two sets of rendered manifests.
package diff

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/pcanilho/idem/internal/manifest"
	"github.com/pcanilho/idem/internal/objpath"
)

// ChangeType describes how an object differs between two renders.
type ChangeType int

const (
	// Invalid is the zero value. A zero Change must not read as a real verdict.
	Invalid ChangeType = iota
	// Differs means the object exists on both sides with different content.
	Differs
	// OnlyInLeft means the object was rendered only by the left-hand side.
	OnlyInLeft
	// OnlyInRight means the object was rendered only by the right-hand side.
	OnlyInRight
)

func (c ChangeType) String() string {
	switch c {
	case Differs:
		return "differs"
	case OnlyInLeft:
		return "only-in-left"
	case OnlyInRight:
		return "only-in-right"
	}
	return "invalid"
}

// MarshalJSON emits the name rather than the ordinal, so `-o json` stays
// readable and survives any future reordering of the constants.
func (c ChangeType) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// ObjectRef identifies an object structurally, so that consumers - notably the
// ignoreDifferences generator, which needs kind, name and namespace - never
// have to re-parse a formatted display string to recover them.
type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`

	// GenerateName carries hook Jobs and anything else the API server names at
	// apply time. Without it every such object in a render collapses to the
	// same Key and the same Display, so two findings cannot be told apart.
	GenerateName string `json:"generateName,omitempty"`
}

func refOf(o manifest.Object) ObjectRef {
	return ObjectRef{
		APIVersion:   o.APIVersion,
		Kind:         o.Kind,
		Namespace:    o.Namespace,
		Name:         o.Name,
		GenerateName: o.GenerateName,
	}
}

// object rebuilds the manifest.Object this ref came from.
//
// Key and Display delegate rather than reimplement: holding the identity rule
// in two places is exactly how ObjectRef came to disagree with the objects it
// describes, and a consumer joining findings back to a render by key would
// silently miss.
func (r ObjectRef) object() manifest.Object {
	return manifest.Object{
		APIVersion:   r.APIVersion,
		Kind:         r.Kind,
		Namespace:    r.Namespace,
		Name:         r.Name,
		GenerateName: r.GenerateName,
	}
}

// Key is the identity used to match objects across renders.
func (r ObjectRef) Key() string { return r.object().Key() }

// Display is the short human-facing form.
func (r ObjectRef) Display() string { return r.object().Display() }

// PathDiff is a single differing leaf within an object.
//
// HasLeft and HasRight distinguish "the key is absent on that side" from "the
// key is present with a null value". Without them the two are the same record,
// and a generated ignoreDifferences entry could target a field that does not
// exist.
type PathDiff struct {
	Path     objpath.Path
	Left     any
	Right    any
	HasLeft  bool
	HasRight bool

	// Reordered marks a list whose elements are unchanged and whose order is
	// not. Path addresses the list itself rather than a leaf inside it,
	// because the leaves did not change - their positions did.
	//
	// A bool rather than a named kind, and the zero value is deliberate: an
	// unset Reordered means "an ordinary differing leaf", which is the loud
	// direction. A consumer that ignores this field falls back to today's
	// behaviour rather than to a false pass.
	//
	// Per-path, not per-Change: one object can hold a reordered list AND a
	// regenerated password, and the two need different treatment inside the
	// same finding.
	Reordered bool
}

// MarshalJSON flattens the path's two renderings up to this level, so consumers
// reach them as `.paths[].pointer` rather than `.paths[].path.pointer`.
func (d PathDiff) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Path string `json:"path"`
		// Pointer addresses the list itself on a reordered entry, and it is
		// NOT a pointer to paste: no ArgoCD or Flux config can ignore ordering
		// alone, so suppressing it would suppress the contents too.
		Pointer   string `json:"pointer"`
		Reordered bool   `json:"reordered,omitempty"`
		Left      any    `json:"left,omitempty"`
		Right     any    `json:"right,omitempty"`
		HasLeft   bool   `json:"hasLeft"`
		HasRight  bool   `json:"hasRight"`
	}{
		Path:      d.Path.String(),
		Pointer:   d.Path.JSONPointer(),
		Reordered: d.Reordered,
		Left:      d.Left,
		Right:     d.Right,
		HasLeft:   d.HasLeft,
		HasRight:  d.HasRight,
	})
}

// Change is one object's worth of difference between two renders.
type Change struct {
	Object ObjectRef  `json:"object"`
	Type   ChangeType `json:"type"`
	Paths  []PathDiff `json:"paths,omitempty"`
}

// Compare matches objects by identity and reports how they differ.
//
// Matching is by identity, so document order is irrelevant: `helm template`
// makes no ordering guarantee across subchart resolution, and treating a
// reordered but identical render as a difference would make the tool useless.
//
// Output is sorted - objects by key, paths within an object by position - so
// that idem's own output is deterministic. A tool that reports non-determinism
// cannot afford to exhibit it.
func Compare(left, right []manifest.Object) ([]Change, error) {
	li, err := index(left)
	if err != nil {
		return nil, fmt.Errorf("left: %w", err)
	}
	ri, err := index(right)
	if err != nil {
		return nil, fmt.Errorf("right: %w", err)
	}

	seen := make(map[string]struct{}, len(li)+len(ri))
	for k := range li {
		seen[k] = struct{}{}
	}
	for k := range ri {
		seen[k] = struct{}{}
	}
	keys := slices.Sorted(maps.Keys(seen))

	var out []Change
	for _, k := range keys {
		l, inLeft := li[k]
		r, inRight := ri[k]

		switch {
		case inLeft && !inRight:
			out = append(out, Change{Object: refOf(l), Type: OnlyInLeft})
		case !inLeft && inRight:
			out = append(out, Change{Object: refOf(r), Type: OnlyInRight})
		default:
			var paths []PathDiff
			walk(nil, l.Body, r.Body, true, true, &paths)
			if len(paths) > 0 {
				out = append(out, Change{Object: refOf(l), Type: Differs, Paths: paths})
			}
		}
	}
	return out, nil
}

func index(objs []manifest.Object) (map[string]manifest.Object, error) {
	m := make(map[string]manifest.Object, len(objs))
	for _, o := range objs {
		k := o.Key()
		if prev, dup := m[k]; dup {
			return nil, fmt.Errorf("duplicate object %s (documents %d and %d); identities must be unique or one would be dropped before comparison",
				o.Display(), prev.DocIndex, o.DocIndex)
		}
		m[k] = o
	}
	return m, nil
}

// walk records every differing leaf reachable from path.
func walk(path objpath.Path, l, r any, hasL, hasR bool, out *[]PathDiff) {
	if !hasL || !hasR {
		*out = append(*out, PathDiff{Path: path, Left: l, Right: r, HasLeft: hasL, HasRight: hasR})
		return
	}

	lm, lIsMap := l.(map[string]any)
	rm, rIsMap := r.(map[string]any)
	if lIsMap && rIsMap {
		for _, k := range unionKeys(lm, rm) {
			lv, lHas := lm[k]
			rv, rHas := rm[k]
			walk(path.Append(objpath.Key(k)), lv, rv, lHas, rHas, out)
		}
		return
	}

	ls, lIsSeq := l.([]any)
	rs, rIsSeq := r.([]any)
	if lIsSeq && rIsSeq {
		// A permutation is one fact about the list, and descending would state
		// it as many false facts about its leaves: every element is identical
		// on both sides, so nothing at .env[0].value changed except which
		// element sits at index 0. remediate then turns each of those leaves
		// into a jsonPointer, and the block suppresses the list's CONTENTS to
		// hide its ORDER.
		if permuted(ls, rs) {
			*out = append(*out, PathDiff{Path: path, Left: ls, Right: rs, HasLeft: true, HasRight: true, Reordered: true})
			return
		}

		n := max(len(ls), len(rs))
		for i := range n {
			var lv, rv any
			lHas, rHas := i < len(ls), i < len(rs)
			if lHas {
				lv = ls[i]
			}
			if rHas {
				rv = rs[i]
			}
			walk(path.Append(objpath.Index(i)), lv, rv, lHas, rHas, out)
		}
		return
	}

	// Scalars, or a type mismatch between the two sides. DeepEqual rather than
	// "!=" because `any` holding an uncomparable type would panic.
	if !reflect.DeepEqual(l, r) {
		*out = append(*out, PathDiff{Path: path, Left: l, Right: r, HasLeft: true, HasRight: true})
	}
}

// permuted reports whether two sequences hold the same elements in a different
// order.
//
// Exact multiset equality, never a name key: nothing in idem matches list
// elements by name, and a heuristic here would call a list whose elements also
// CHANGED a clean permutation - dropping real churn out of the fix block. The
// failure direction is chosen deliberately: a miss leaves today's positional
// output, which is merely verbose, while a false hit would hide churn.
//
// Canonicalise once and sort, rather than pairwise deep equality: that is
// O(n log n) against O(n^2), and design.md §9 already carries one quadratic
// worth apologising for. manifest.stringKeys has rewritten every map[any]any as
// map[string]any before this point, so marshalling succeeds for anything YAML
// can produce - and an element that will not marshal returns false, which is
// the same safe direction as everything else here.
func permuted(a, b []any) bool {
	// Identical lists are not a difference at all and must fall through so the
	// walk emits nothing.
	//
	// len(a) < 2 IS a guard, and it earns its place on the empty pair: a nil
	// []any and an empty one are both length zero, DeepEqual says they differ,
	// and both canonicalise to nothing - so without it an empty list would be
	// reported as reordered. YAML cannot currently produce that pair, which is
	// exactly why it is pinned by a test rather than trusted.
	//
	// The length comparison is an early-out, NOT a guard: slices.Equal below
	// already refuses two different-sized multisets. It is here to avoid
	// marshalling two whole lists to learn what their lengths already say.
	if len(a) < 2 || len(a) != len(b) || reflect.DeepEqual(a, b) {
		return false
	}

	ca, ok := canonical(a)
	if !ok {
		return false
	}
	cb, ok := canonical(b)
	if !ok {
		return false
	}
	slices.Sort(ca)
	slices.Sort(cb)
	return slices.Equal(ca, cb)
}

// canonical renders each element to a stable string. json.Marshal sorts map
// keys, so two equal elements always produce equal bytes.
func canonical(s []any) ([]string, bool) {
	out := make([]string, 0, len(s))
	for _, e := range s {
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, false
		}
		out = append(out, string(raw))
	}
	return out, true
}

func unionKeys(a, b map[string]any) []string {
	keys := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return slices.Sorted(maps.Keys(keys))
}
