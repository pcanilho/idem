// Package check renders a chart repeatedly and reports what did not settle.
//
// This is cause 1 of the five in docs/design.md §8 - render-side
// non-determinism - and it is the only one establishable without a cluster:
// render the same input twice, compare structurally, and any difference is an
// observed fact rather than an inference.
package check

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/manifest"
)

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

	// Skipped counts objects excluded from the comparison because no engine
	// applies them. Counted rather than dropped in silence: a tool that reports
	// what it checked has to report what it did not.
	Skipped int
}

// Compare reports what differed across renders of one chart.
//
// Every round is compared against the first, not against its predecessor. A
// value drawn from a small space - randInt on a short range, a timestamp at
// second resolution - can repeat by chance in consecutive rounds, and pairwise
// comparison would then call a non-deterministic chart clean.
//
// It takes renders rather than producing them so that the caller owns
// scheduling: rounds are independent and worth running concurrently, and the
// comparison has no business knowing that.
func Compare(renders [][]manifest.Object) (Result, error) {
	if len(renders) < 2 {
		return Result{}, fmt.Errorf("got %d renders: at least 2 are needed, because a single render cannot be compared to anything", len(renders))
	}

	var skipped int
	kept := make([][]manifest.Object, 0, len(renders))
	for _, round := range renders {
		applied, dropped := applicable(round)
		skipped += dropped
		kept = append(kept, applied)
	}
	renders = kept

	first := renders[0]
	sources := sourceIndex(first)

	merged := make(map[string]Finding)
	for round, next := range renders[1:] {
		changes, err := diff.Compare(first, next)
		if err != nil {
			return Result{}, fmt.Errorf("comparing round 1 against round %d: %w", round+2, err)
		}
		for _, c := range changes {
			merge(merged, c, sources)
		}
	}

	return Result{
		Rounds:   len(renders),
		Findings: sortedFindings(merged),
		Skipped:  skipped,
	}, nil
}

// unappliedHooks are the helm.sh/hook values no engine reconciles.
//
// Verified against ArgoCD's Helm page: it maps crd-install, pre/post-install,
// pre/post-upgrade and pre/post-delete onto its own hook system, and of the
// rest says "Argo CD currently skips manifests that include hooks not supported
// by Argo CD, including Helm test hooks".
//
// Helm and Flux reach the same place by a different route: a test hook is
// created only by `helm test`, deleted afterwards, and never compared against
// the cluster. Rollback hooks fire only on a rollback. So churn in one of these
// is not churn under any of the three engines idem answers for.
//
// `test` is Helm 3's spelling; test-success and test-failure are the Helm 2
// names it still honours.
var unappliedHooks = []string{"test", "test-success", "test-failure", "pre-rollback", "post-rollback"}

// applicable drops the objects no engine applies, and counts them.
func applicable(objs []manifest.Object) ([]manifest.Object, int) {
	out := make([]manifest.Object, 0, len(objs))
	for _, o := range objs {
		if unapplied(o) {
			continue
		}
		out = append(out, o)
	}
	return out, len(objs) - len(out)
}

// unapplied reports whether an object carries only hooks nothing applies.
//
// The annotation is a comma-separated list, and ANY applied hook keeps the
// object: `helm.sh/hook: post-install,test` is installed by ArgoCD as a
// PostSync hook, so it is compared.
func unapplied(o manifest.Object) bool {
	meta, ok := o.Body["metadata"].(map[string]any)
	if !ok {
		return false
	}
	annotations, ok := meta["annotations"].(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := annotations["helm.sh/hook"].(string)
	if !ok || hooks == "" {
		return false
	}

	for hook := range strings.SplitSeq(hooks, ",") {
		if !slices.Contains(unappliedHooks, strings.TrimSpace(hook)) {
			return false
		}
	}
	return true
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

	// A round that disagreed about whether the object EXISTS beats one that
	// only disagreed about its fields, and the two must not be welded together.
	//
	// Keeping the first type while appending later rounds' paths produced
	// `only-in-left` carrying field paths, and both readers of that combination
	// branch on len(Paths) == 0: the report then described field churn instead
	// of saying the object sometimes does not render, and remediate emitted an
	// ignoreDifferences entry that cannot fix a disappearing object.
	if existing.Change.Type != diff.Differs {
		return
	}
	if c.Type != diff.Differs {
		into[key] = Finding{Source: existing.Source, Change: c}
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
