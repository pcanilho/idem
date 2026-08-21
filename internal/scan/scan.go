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

	"github.com/pcanilho/idem/internal/check"
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
}

// Result is one chart's outcome. Err set means the chart could not be
// rendered, which is exit 2 - never a silent skip.
type Result struct {
	Chart    Chart
	Findings []check.Finding
	Err      error
}

// Charts checks every chart, running at most jobs renders at once.
//
// Results come back in the order the charts were given, whatever order they
// finished in. A tool that reports non-determinism cannot exhibit it.
func Charts(ctx context.Context, r Renderer, charts []Chart, rounds, jobs int) []Result {
	// One semaphore for every render in the run, so the two levels of
	// parallelism cannot multiply into charts*rounds processes at once.
	gate := make(chan struct{}, resolveJobs(jobs))

	results := make([]Result, len(charts))
	var wg sync.WaitGroup
	for i, c := range charts {
		wg.Go(func() {
			results[i] = one(ctx, r, c, rounds, gate)
		})
	}
	wg.Wait()

	return results
}

// one renders a single chart's rounds concurrently and compares them.
//
// The chart's goroutine never holds the gate itself - only the renders do - so
// charts waiting on their rounds cannot starve the pool.
func one(ctx context.Context, r Renderer, c Chart, rounds int, gate chan struct{}) Result {
	if rounds < 2 {
		return Result{Chart: c, Err: fmt.Errorf("checking %s: rounds is %d, and at least 2 renders are needed", c.Name, rounds)}
	}

	renders := make([][]manifest.Object, rounds)
	errs := make([]error, rounds)

	var wg sync.WaitGroup
	for round := range rounds {
		wg.Go(func() {

			gate <- struct{}{}
			defer func() { <-gate }()

			renders[round], errs[round] = r.Render(ctx, c.Spec)
		})
	}
	wg.Wait()

	// Report the earliest round that failed, so the reason does not depend on
	// which goroutine happened to finish first.
	for round, err := range errs {
		if err != nil {
			return Result{Chart: c, Err: fmt.Errorf("rendering %s (round %d): %w", c.Name, round+1, err)}
		}
	}

	result, err := check.Compare(renders)
	if err != nil {
		return Result{Chart: c, Err: fmt.Errorf("checking %s: %w", c.Name, err)}
	}
	return Result{Chart: c, Findings: result.Findings}
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
