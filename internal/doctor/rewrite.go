package doctor

import (
	"slices"
	"strings"

	"github.com/pcanilho/idem/internal/cluster"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/manifest"
	"github.com/pcanilho/idem/internal/objpath"
)

// assigned are fields the cluster mints per object rather than defaulting to a
// constant. Worth distinguishing: a default is the same everywhere and reads
// as noise, while an assignment is genuinely unique to this object.
var assigned = []string{"clusterIP", "clusterIPs", "nodePort", "healthCheckNodePort"}

// unreliable are the fields idem refuses to compare across an apply.
//
// docs/design.md §9 and PLAN §6.1: the API server canonicalises resource
// quantities and IntOrString ports, so `cpu: 0.1` comes back `100m` and
// `targetPort: "80"` comes back `80`. Structural comparison calls those
// differences when nothing changed, and reporting them wrongly is worse than
// not reporting them - so they are dropped and counted, never shown.
var unreliable = []string{"resources", "targetPort", "port", "nodePort", "containerPort"}

// Change is one field the cluster wrote as an object was admitted.
type Change struct {
	Path objpath.Path

	// Value is what the cluster put there.
	Value any

	// Assigned marks a value minted for this object rather than a constant
	// default.
	Assigned bool
}

// Rewrite is an object the cluster changed on its way in.
type Rewrite struct {
	Object  diff.ObjectRef
	Changes []Change

	// Suppressed counts fields dropped as unreliable to compare, so the total
	// is never quietly short.
	Suppressed int
}

// Rewrites compares what was sent with what the API server said it would
// store.
//
// This is cause 3, and a dry run is the only thing that shows it: admission is
// synchronous, so the rewrite happens between sending an object and it being
// stored, where no amount of rendering can see it.
func Rewrites(sent, returned []manifest.Object) ([]Rewrite, error) {
	changes, err := diff.Compare(normalise(sent), normalise(returned))
	if err != nil {
		return nil, err
	}

	var out []Rewrite
	for _, c := range changes {
		rewrite := Rewrite{Object: c.Object}

		for _, p := range c.Paths {
			// Only what the cluster ADDED or altered. A field the render has
			// and the cluster dropped is a different story, and one the API
			// server tells far better than idem could.
			if !p.HasRight {
				continue
			}
			if suppressed(p.Path) {
				rewrite.Suppressed++
				continue
			}
			rewrite.Changes = append(rewrite.Changes, Change{
				Path:     p.Path,
				Value:    p.Right,
				Assigned: isAssigned(p.Path),
			})
		}

		if len(rewrite.Changes) == 0 && rewrite.Suppressed == 0 {
			continue
		}
		out = append(out, rewrite)
	}
	return out, nil
}

// normalise readies both sides of the comparison.
//
// The namespace is dropped as well as the usual server fields. A render
// usually carries no metadata.namespace at all - helm sets .Release.Namespace
// without writing the field - and kubectl fills it in from the context. Left
// in, the two sides have different identities and never pair up, so every
// object reads as added-and-removed and no rewrite is ever found.
func normalise(objects []manifest.Object) []manifest.Object {
	out := make([]manifest.Object, 0, len(objects))
	for _, o := range objects {
		normalised := cluster.Normalise(o)
		normalised.Namespace = ""
		if meta, ok := normalised.Body["metadata"].(map[string]any); ok {
			delete(meta, "namespace")
		}
		out = append(out, normalised)
	}
	return out
}

func suppressed(p objpath.Path) bool {
	return slices.ContainsFunc(unreliable, func(field string) bool {
		return strings.Contains(p.String(), "."+field)
	})
}

func isAssigned(p objpath.Path) bool {
	return slices.ContainsFunc(assigned, func(field string) bool {
		return strings.HasSuffix(p.String(), "."+field) || strings.Contains(p.String(), "."+field+"[")
	})
}
