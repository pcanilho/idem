// Package check renders a chart repeatedly and reports what did not settle.
//
// This is cause 1 of the five in docs/design.md §8 - render-side
// non-determinism - and it is the only one establishable without a cluster:
// render the same input twice, compare structurally, and any difference is an
// observed fact rather than an inference.
package check

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/manifest"
)

// Renderer produces one render of a chart.
//
// Declared here rather than taken from engine so that check depends on the one
// method it uses. engine.Renderer satisfies it.
type Renderer interface {
	Render(ctx context.Context, spec engine.Spec) ([]manifest.Object, error)
}

// Finding is one object that did not render the same way every time.
type Finding struct {
	// Source is the template that produced the object, from helm's
	// "# Source:" comment. Empty means the render carried none - reported as
	// unknown, never guessed at.
	Source string

	Change diff.Change
}

// Result is what one chart's rounds established.
type Result struct {
	Rounds   int
	Findings []Finding
}

// Run renders spec `rounds` times and reports what differed.
//
// Every round is compared against the first, not against its predecessor. A
// value drawn from a small space - randInt on a short range, a timestamp at
// second resolution - can repeat by chance in consecutive rounds, and pairwise
// comparison would then call a non-deterministic chart clean.
func Run(ctx context.Context, r Renderer, spec engine.Spec, rounds int) (Result, error) {
	if rounds < 2 {
		return Result{}, fmt.Errorf("rounds is %d: at least 2 renders are needed, because a single render cannot be compared to anything", rounds)
	}

	first, err := r.Render(ctx, spec)
	if err != nil {
		return Result{}, fmt.Errorf("rendering %s: %w", spec.ChartRef, err)
	}
	sources := sourceIndex(first)

	merged := make(map[string]Finding)
	for round := 2; round <= rounds; round++ {
		next, err := r.Render(ctx, spec)
		if err != nil {
			return Result{}, fmt.Errorf("rendering %s (round %d): %w", spec.ChartRef, round, err)
		}

		changes, err := diff.Compare(first, next)
		if err != nil {
			return Result{}, fmt.Errorf("comparing renders of %s (round %d): %w", spec.ChartRef, round, err)
		}
		for _, c := range changes {
			merge(merged, c, sources)
		}
	}

	return Result{
		Rounds:   rounds,
		Findings: sortedFindings(merged),
	}, nil
}

// sourceIndex maps object identity to the template that produced it.
func sourceIndex(objs []manifest.Object) map[string]string {
	idx := make(map[string]string, len(objs))
	for _, o := range objs {
		idx[o.Key()] = o.Source
	}
	return idx
}

// merge folds one round's change into the accumulated findings.
//
// The same field differing in every round is one finding, not one per round:
// the user fixes a field once. A change type recorded by an earlier round is
// kept, because the first way an object failed to settle is the one to report.
func merge(into map[string]Finding, c diff.Change, sources map[string]string) {
	key := c.Object.Key()
	existing, seen := into[key]
	if !seen {
		into[key] = Finding{Source: sources[key], Change: c}
		return
	}

	seenPaths := make(map[string]struct{}, len(existing.Change.Paths))
	for _, p := range existing.Change.Paths {
		seenPaths[p.Path.JSONPointer()] = struct{}{}
	}
	for _, p := range c.Paths {
		if _, dup := seenPaths[p.Path.JSONPointer()]; dup {
			continue
		}
		existing.Change.Paths = append(existing.Change.Paths, p)
	}
	into[key] = existing
}

// sortedFindings orders findings by object key, and paths within each finding
// by pointer, so that idem's own output never varies between runs.
func sortedFindings(merged map[string]Finding) []Finding {
	var out []Finding
	for _, key := range slices.Sorted(maps.Keys(merged)) {
		f := merged[key]
		slices.SortFunc(f.Change.Paths, func(a, b diff.PathDiff) int {
			return strings.Compare(a.Path.JSONPointer(), b.Path.JSONPointer())
		})
		out = append(out, f)
	}
	return out
}
