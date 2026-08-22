// Package scan renders many charts and compares each one's rounds.
//
// Parallelism spans charts AND the rounds within a chart, under one shared
// limit. Measured on a real estate, a single TrueCharts-idiom chart took 6.1s
// per render while the other fifteen together took ~5s: parallelising charts
// alone would leave that one chart serial and floor the whole run at 12.2s for
// two rounds. Its rounds are independent renders sharing no state, so running
// them together puts the floor at one render instead of two.
package scan

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/doctor"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/manifest"
)

// Renderer produces one render of a chart.
type Renderer interface {
	Render(ctx context.Context, spec engine.Spec) ([]manifest.Object, error)
}

// Chart is one chart to check.
type Chart struct {
	Name string
	Dir  string
	Spec engine.Spec

	// Server, when set, is a second render condition to measure alongside:
	// the same chart through the API server, where lookup resolves. That is
	// what turns the Flux and Helm verdicts from `unknown` into an
	// observation. Nil means no cluster was asked for.
	Server *engine.Spec
}

// Inspect examines a chart's source. Optional; nil skips it.
//
// Runs in the same pool as rendering rather than in a pass afterwards, so a
// chart's source scan overlaps other charts' renders instead of adding its own
// serial tail - and so that --jobs bounds all the work, not just some of it.
type Inspect func(Chart) ([]analyze.Use, error)

// Admission asks the cluster what it would change about a chart's rendered
// output on admission. Optional; nil skips it.
type Admission func(Chart, []manifest.Object) ([]doctor.Rewrite, error)

// Prepare readies a chart for rendering, returning the spec to render and a
// cleanup to run once its rounds are done.
//
// Runs before the rounds rather than alongside them: every round renders the
// same prepared chart, and letting each round resolve dependencies for itself
// would do the work N times and race on the same directory.
type Prepare func(Chart) (engine.Spec, func(), error)

// Hooks are the optional stages Charts runs alongside rendering. A zero Hooks
// renders and compares, which is the whole job without a repository or a
// cluster to ask about.
//
// A struct rather than positional arguments: every one of these is optional,
// and `Charts(ctx, h, charts, 2, 8, nil, nil, nil)` says nothing about which
// nil is which.
type Hooks struct {
	Inspect   Inspect
	Prepare   Prepare
	Admission Admission
}

// Result is one chart's outcome. Err set means the chart could not be
// rendered, which is exit 2 - never a silent skip.
type Result struct {
	Chart    Chart
	Findings []check.Finding
	Err      error

	// Uses is what Inspect found, and InspectErr why it could not look.
	// A failed inspection is an honest unknown for the engine verdicts, never
	// a reason to call the chart unrenderable.
	Uses       []analyze.Use
	InspectErr error

	// ServerRendered reports whether the API-server condition was measured,
	// ServerFindings what differed under it, and ServerErr why it could not be.
	// A cluster that cannot be reached degrades the verdict to unknown; it is
	// never a reason to fail the chart, which renders perfectly well without.
	ServerRendered bool
	ServerFindings []check.Finding
	ServerErr      error

	// Rendered is the first round's output, kept so the caller can ask the
	// API server what it would do with it without rendering again.
	Rendered []manifest.Object

	// Rewrites is what the API server said it would change on admission, and
	// RewriteErr why it could not be asked. A cluster that will not answer
	// leaves the question open; it never fails the chart, which renders
	// perfectly well without one.
	Rewrites   []doctor.Rewrite
	RewriteErr error
}

// Charts checks every chart, running at most jobs renders at once.
//
// Results come back in the order the charts were given, whatever order they
// finished in. A tool that reports non-determinism cannot exhibit it.
func Charts(ctx context.Context, r Renderer, charts []Chart, rounds, jobs int, hooks Hooks) []Result {
	// One semaphore for every render in the run, so the two levels of
	// parallelism cannot multiply into charts*rounds processes at once.
	gate := make(chan struct{}, resolveJobs(jobs))

	results := make([]Result, len(charts))
	var wg sync.WaitGroup
	for i, c := range charts {
		wg.Go(func() {
			results[i] = one(ctx, r, c, rounds, gate, hooks)
		})
	}
	wg.Wait()

	return results
}

// one renders a single chart's rounds concurrently and compares them.
//
// The chart's goroutine never holds the gate itself - only the renders do - so
// charts waiting on their rounds cannot starve the pool.
func one(ctx context.Context, r Renderer, c Chart, rounds int, gate chan struct{}, hooks Hooks) Result {
	if rounds < 2 {
		return Result{Chart: c, Err: fmt.Errorf("checking %s: rounds is %d, and at least 2 renders are needed", c.Name, rounds)}
	}

	spec := c.Spec
	if hooks.Prepare != nil {
		gate <- struct{}{}
		prepared, cleanup, err := hooks.Prepare(c)
		<-gate

		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return Result{Chart: c, Err: fmt.Errorf("preparing %s: %w", c.Name, err)}
		}
		spec = prepared
	}

	renders := make([][]manifest.Object, rounds)
	errs := make([]error, rounds)

	serverRenders := make([][]manifest.Object, rounds)
	serverErrs := make([]error, rounds)

	var uses []analyze.Use
	var inspectErr error

	var wg sync.WaitGroup
	for round := range rounds {
		wg.Go(func() {

			gate <- struct{}{}
			defer func() { <-gate }()

			renders[round], errs[round] = r.Render(ctx, spec)
		})
	}

	// The API-server condition is an independent measurement of the same
	// chart, so its rounds run alongside the client ones under the same limit.
	if c.Server != nil {
		serverSpec := *c.Server
		serverSpec.ChartRef = spec.ChartRef

		for round := range rounds {
			wg.Go(func() {
				gate <- struct{}{}
				defer func() { <-gate }()

				serverRenders[round], serverErrs[round] = r.Render(ctx, serverSpec)
			})
		}
	}

	// Reading the chart source does not depend on rendering it, so it runs
	// alongside the rounds, taking a slot like any other unit of work.
	if hooks.Inspect != nil {
		wg.Go(func() {
			gate <- struct{}{}
			defer func() { <-gate }()

			uses, inspectErr = hooks.Inspect(c)
		})
	}
	wg.Wait()

	// Report the earliest round that failed, so the reason does not depend on
	// which goroutine happened to finish first. The inspection is still
	// carried: a chart that would not render may still be worth warning about.
	for round, err := range errs {
		if err != nil {
			return Result{
				Chart: c, Uses: uses, InspectErr: inspectErr,
				Err: fmt.Errorf("rendering %s (round %d): %w", c.Name, round+1, err),
			}
		}
	}

	result, err := check.Compare(renders)
	if err != nil {
		return Result{
			Chart: c, Uses: uses, InspectErr: inspectErr,
			Err: fmt.Errorf("checking %s: %w", c.Name, err),
		}
	}

	out := Result{Chart: c, Findings: result.Findings, Uses: uses, InspectErr: inspectErr, Rendered: renders[0]}
	if c.Server != nil {
		out.ServerFindings, out.ServerRendered, out.ServerErr = compareServer(c, serverRenders, serverErrs)
	}

	// Asked here rather than in a pass over the finished results: it is a
	// cluster round trip, the slowest single thing idem does, and one chart's
	// query overlaps every other chart's renders from inside the pool. Only
	// after the rounds, because it needs something rendered to ask about.
	if hooks.Admission != nil && len(renders[0]) > 0 {
		gate <- struct{}{}
		out.Rewrites, out.RewriteErr = hooks.Admission(c, renders[0])
		<-gate
	}
	return out
}

// compareServer folds the API-server rounds into an answer.
//
// A cluster that cannot be reached, or that refuses the dry run, leaves the
// verdict unknown rather than failing the chart: the chart renders perfectly
// well without a cluster, and the client-side answer is unaffected.
func compareServer(c Chart, renders [][]manifest.Object, errs []error) ([]check.Finding, bool, error) {
	for round, err := range errs {
		if err != nil {
			return nil, false, fmt.Errorf("rendering %s against the cluster (round %d): %w", c.Name, round+1, err)
		}
	}

	result, err := check.Compare(renders)
	if err != nil {
		return nil, false, fmt.Errorf("comparing cluster renders of %s: %w", c.Name, err)
	}
	return result.Findings, true, nil
}

// resolveJobs turns the flag value into a worker count.
//
// Unset, zero and negative all mean "use the machine": a literal zero would be
// a pool that never runs anything.
func resolveJobs(jobs int) int {
	if jobs < 1 {
		return runtime.NumCPU()
	}
	return jobs
}
