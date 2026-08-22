package scan

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/doctor"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/manifest"
)

func parse(t *testing.T, in string) []manifest.Object {
	t.Helper()
	objs, err := manifest.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return objs
}

func chart(name string) Chart {
	return Chart{Name: name, Dir: "./" + name, Spec: engine.Spec{ChartRef: "./" + name, Release: name}}
}

// counting renders a fixed object, recording how many renders overlap.
type counting struct {
	mu        sync.Mutex
	active    int
	maxActive int
	calls     map[string]int
	inspected int

	body        string
	delay       time.Duration
	fail        map[string]error
	failCluster error
}

// occupy takes a slot in the same accounting a render does, so work that is
// not a render can still be checked against the job limit.
func (c *counting) occupy() error {
	c.mu.Lock()
	c.active++
	c.maxActive = max(c.maxActive, c.active)
	c.mu.Unlock()

	time.Sleep(c.delay)

	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return nil
}

func (c *counting) Render(_ context.Context, spec engine.Spec) ([]manifest.Object, error) {
	c.mu.Lock()
	c.active++
	c.maxActive = max(c.maxActive, c.active)
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[spec.ChartRef]++
	nth := c.calls[spec.ChartRef]
	err := c.fail[spec.ChartRef]
	if err == nil && spec.Cluster {
		err = c.failCluster
	}
	c.mu.Unlock()

	time.Sleep(c.delay)

	c.mu.Lock()
	c.active--
	c.mu.Unlock()

	if err != nil {
		return nil, err
	}
	body := c.body
	if body == "" {
		// Vary by round so every chart produces a finding.
		body = fmt.Sprintf("value%d", nth)
	}
	return manifest.Parse(strings.NewReader(
		"apiVersion: v1\nkind: Secret\nmetadata: {name: " + spec.Release + "}\ndata: {k: " + body + "}\n"))
}

// meeting blocks every Render until `want` of them are in flight at once, so a
// test can prove real concurrency without timing assumptions. If the scheduler
// never gets that many going, each call times out and reports it as an error
// rather than hanging the suite.
type meeting struct {
	mu      sync.Mutex
	arrived int
	want    int
	reached chan struct{}
	once    sync.Once
}

func newMeeting(want int) *meeting {
	return &meeting{want: want, reached: make(chan struct{})}
}

func (m *meeting) Render(_ context.Context, spec engine.Spec) ([]manifest.Object, error) {
	m.mu.Lock()
	m.arrived++
	if m.arrived >= m.want {
		m.once.Do(func() { close(m.reached) })
	}
	m.mu.Unlock()

	select {
	case <-m.reached:
		return manifest.Parse(strings.NewReader(
			"apiVersion: v1\nkind: Secret\nmetadata: {name: " + spec.Release + "}\ndata: {k: v}\n"))
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("only %d renders were ever in flight, want %d concurrent", m.arrived, m.want)
	}
}

func TestChartsReturnsOneResultPerChartInInputOrder(t *testing.T) {
	// Completion order under concurrency is arbitrary; reported order must not
	// be. A tool that reports non-determinism cannot exhibit it.
	charts := []Chart{chart("zeta"), chart("alpha"), chart("mid")}

	got := Charts(context.Background(), &counting{}, charts, 2, 4, Hooks{})

	if len(got) != len(charts) {
		t.Fatalf("Charts() returned %d results, want %d", len(got), len(charts))
	}
	for i, want := range charts {
		if got[i].Chart.Name != want.Name {
			t.Errorf("result %d is %q, want %q", i, got[i].Chart.Name, want.Name)
		}
	}
}

func TestChartsReportsFindingsAgainstTheChartTheyCameFrom(t *testing.T) {
	got := Charts(context.Background(), &counting{}, []Chart{chart("home"), chart("lab")}, 2, 4, Hooks{})

	for _, r := range got {
		if r.Err != nil {
			t.Fatalf("%s: unexpected error %v", r.Chart.Name, r.Err)
		}
		if len(r.Findings) != 1 {
			t.Fatalf("%s: %d findings, want 1", r.Chart.Name, len(r.Findings))
		}
		if name := r.Findings[0].Change.Object.Name; name != r.Chart.Name {
			t.Errorf("%s: finding names object %q, want it attributed to its own chart", r.Chart.Name, name)
		}
	}
}

func TestChartsRendersEachChartOncePerRound(t *testing.T) {
	r := &counting{}

	Charts(context.Background(), r, []Chart{chart("home"), chart("lab")}, 3, 4, Hooks{})

	for _, ref := range []string{"./home", "./lab"} {
		if got := r.calls[ref]; got != 3 {
			t.Errorf("%s rendered %d times, want 3", ref, got)
		}
	}
}

func TestAChartThatFailsToRenderDoesNotStopTheOthers(t *testing.T) {
	// One unrenderable chart is exit 2, but the other charts still have
	// answers and the user still wants them.
	boom := errors.New("helm template: exit status 1")
	r := &counting{fail: map[string]error{"./lab": boom}}

	got := Charts(context.Background(), r, []Chart{chart("home"), chart("lab"), chart("ops")}, 2, 4, Hooks{})

	if got[0].Err != nil || got[2].Err != nil {
		t.Errorf("healthy charts carry errors: %v, %v", got[0].Err, got[2].Err)
	}
	if !errors.Is(got[1].Err, boom) {
		t.Errorf("lab error = %v, want it to wrap the render failure", got[1].Err)
	}
	if !strings.Contains(got[1].Err.Error(), "lab") {
		t.Errorf("lab error = %q, want it to name the chart", got[1].Err)
	}
}

func TestChartsNeverExceedsTheJobLimit(t *testing.T) {
	r := &counting{delay: 5 * time.Millisecond}
	charts := []Chart{chart("a"), chart("b"), chart("c"), chart("d"), chart("e")}

	Charts(context.Background(), r, charts, 3, 2, Hooks{})

	if r.maxActive > 2 {
		t.Errorf("%d renders ran at once, want at most 2", r.maxActive)
	}
}

func TestChartsRendersDifferentChartsConcurrently(t *testing.T) {
	m := newMeeting(3)

	got := Charts(context.Background(), m, []Chart{chart("a"), chart("b"), chart("c")}, 2, 8, Hooks{})

	for _, r := range got {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Chart.Name, r.Err)
		}
	}
}

func TestChartsRendersTheRoundsOfOneChartConcurrently(t *testing.T) {
	// This is the whole point of parallelising at (chart x round): one chart
	// that takes 6s to render dominated the estate run, and its rounds are
	// independent. Parallelising charts alone would leave that chart serial.
	m := newMeeting(2)

	got := Charts(context.Background(), m, []Chart{chart("only")}, 2, 8, Hooks{})

	if got[0].Err != nil {
		t.Errorf("rounds of a single chart did not overlap: %v", got[0].Err)
	}
}

func TestRoundsAreBoundedByTheJobLimitToo(t *testing.T) {
	// A single chart with many rounds must not ignore --jobs.
	r := &counting{delay: 5 * time.Millisecond}

	Charts(context.Background(), r, []Chart{chart("only")}, 8, 2, Hooks{})

	if r.maxActive > 2 {
		t.Errorf("%d renders ran at once, want at most 2", r.maxActive)
	}
}

func TestFewerThanTwoRoundsIsReportedPerChart(t *testing.T) {
	got := Charts(context.Background(), &counting{}, []Chart{chart("home")}, 1, 4, Hooks{})

	if got[0].Err == nil {
		t.Error("Err = nil, want a single round rejected")
	}
}

func TestResolveJobsDefaultsToNumCPU(t *testing.T) {
	// --jobs unset means "use the machine", and zero or negative would
	// otherwise mean "do nothing at all", which would hang or silently pass.
	for _, jobs := range []int{0, -1} {
		if got, want := resolveJobs(jobs), runtime.NumCPU(); got != want {
			t.Errorf("resolveJobs(%d) = %d, want %d", jobs, got, want)
		}
	}
	if got, want := resolveJobs(3), 3; got != want {
		t.Errorf("resolveJobs(3) = %d, want %d", got, want)
	}
}

func TestChartsIsDeterministicAcrossRuns(t *testing.T) {
	charts := []Chart{chart("zeta"), chart("alpha"), chart("mid")}

	var first []string
	for run := range 20 {
		var names []string
		for _, r := range Charts(context.Background(), &counting{}, charts, 2, 8, Hooks{}) {
			names = append(names, r.Chart.Name+":"+fmt.Sprint(len(r.Findings)))
		}
		if run == 0 {
			first = names
			continue
		}
		if strings.Join(names, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d gave %v, run 0 gave %v", run, names, first)
		}
	}
}

// --- inspection, run in the same pool as rendering ---

// inspector records its own concurrency into the shared counting renderer, so
// a test can assert the job limit covers both kinds of work.
func (c *counting) inspect(spec engine.Spec) ([]analyze.Use, error) {
	c.mu.Lock()
	c.active++
	c.maxActive = max(c.maxActive, c.active)
	c.mu.Unlock()

	time.Sleep(c.delay)

	c.mu.Lock()
	c.active--
	c.inspected++
	c.mu.Unlock()

	return []analyze.Use{{Function: "now", File: "t.yaml", Line: 1, Call: true}}, nil
}

func TestChartsInspectsEveryChart(t *testing.T) {
	r := &counting{}

	got := Charts(context.Background(), r, []Chart{chart("a"), chart("b")}, 2, 4,
		Hooks{Inspect: func(c Chart) ([]analyze.Use, error) { return r.inspect(c.Spec) }})

	if r.inspected != 2 {
		t.Errorf("inspected %d charts, want 2", r.inspected)
	}
	for _, res := range got {
		if len(res.Uses) != 1 {
			t.Errorf("%s: Uses = %+v, want the inspector's result", res.Chart.Name, res.Uses)
		}
	}
}

func TestInspectionObeysTheSameJobLimitAsRendering(t *testing.T) {
	// Scanning chart sources is real work competing for the same machine. If
	// it ran outside the limit, --jobs would stop meaning what it says.
	r := &counting{delay: 5 * time.Millisecond}
	charts := []Chart{chart("a"), chart("b"), chart("c"), chart("d")}

	Charts(context.Background(), r, charts, 3, 2,
		Hooks{Inspect: func(c Chart) ([]analyze.Use, error) { return r.inspect(c.Spec) }})

	if r.maxActive > 2 {
		t.Errorf("%d tasks ran at once, want at most 2", r.maxActive)
	}
}

func TestInspectionOverlapsWithOtherChartsRendering(t *testing.T) {
	// The whole point of moving it into the pool: a chart's source scan should
	// run while other charts are still rendering, not in a second pass after
	// every render has finished.
	//
	// This used the newMeeting(2) counter and was VACUOUS: one chart with
	// rounds: 2 puts two RENDERS in flight, which satisfies "two arrived" on
	// its own, so the meeting opened whether or not an inspection was ever
	// among them. Moving Inspect to a serial pass after wg.Wait() left it
	// green - the exact regression its comment names.
	//
	// Same shape as the admission test now: "slow" blocks in its render until
	// an inspection arrives, and only "fast" can send one. In a pass after the
	// pool drains, neither side meets the other and both say so.
	rv := newRendezvous()

	got := Charts(context.Background(), &blocking{r: rv, on: "slow"}, []Chart{chart("fast"), chart("slow")}, 2, 8,
		Hooks{Inspect: func(c Chart) ([]analyze.Use, error) {
			if c.Name != "fast" {
				return nil, nil
			}
			return nil, rv.admit()
		}})

	for _, res := range got {
		if res.Err != nil {
			t.Errorf("%s: %v", res.Chart.Name, res.Err)
		}
		if res.InspectErr != nil {
			t.Errorf("%s: %v", res.Chart.Name, res.InspectErr)
		}
	}
}

func TestAnInspectionFailureDoesNotFailTheChart(t *testing.T) {
	// Not being able to read the chart source is an honest unknown for the
	// verdicts; it is not a reason to call the chart unrenderable.
	boom := errors.New("scanning charts/common.tgz: unexpected EOF")
	r := &counting{}

	got := Charts(context.Background(), r, []Chart{chart("a")}, 2, 4,
		Hooks{Inspect: func(Chart) ([]analyze.Use, error) { return nil, boom }})

	if got[0].Err != nil {
		t.Errorf("Err = %v, want the chart still evaluated", got[0].Err)
	}
	if !errors.Is(got[0].InspectErr, boom) {
		t.Errorf("InspectErr = %v, want the scan failure kept", got[0].InspectErr)
	}
}

func TestChartsWithoutAnInspectorStillWorks(t *testing.T) {
	got := Charts(context.Background(), &counting{}, []Chart{chart("a")}, 2, 4, Hooks{})

	if len(got) != 1 || got[0].Err != nil {
		t.Errorf("Charts() = %+v, want the chart checked", got)
	}
}

func TestPreparedSpecIsWhatGetsRendered(t *testing.T) {
	// Dependency resolution can move a chart to a temp copy, and every round
	// must render that copy rather than the original.
	r := &counting{}

	Charts(context.Background(), r, []Chart{chart("home")}, 2, 4,
		Hooks{Prepare: func(Chart) (engine.Spec, func(), error) {
			return engine.Spec{ChartRef: "/tmp/copy/home", Release: "home"}, nil, nil
		}})

	if r.calls["/tmp/copy/home"] != 2 {
		t.Errorf("rendered %v, want both rounds against the prepared copy", r.calls)
	}
}

func TestPreparationRunsOncePerChartNotOncePerRound(t *testing.T) {
	// Resolving dependencies per round would do the work N times and race two
	// helm processes on the same directory.
	var prepared int
	var mu sync.Mutex

	Charts(context.Background(), &counting{}, []Chart{chart("home")}, 4, 4,
		Hooks{Prepare: func(c Chart) (engine.Spec, func(), error) {
			mu.Lock()
			prepared++
			mu.Unlock()
			return c.Spec, nil, nil
		}})

	if prepared != 1 {
		t.Errorf("prepared %d times, want once", prepared)
	}
}

func TestCleanupRunsAfterTheRoundsFinish(t *testing.T) {
	// Discarding the temp copy before the last render would break it.
	var cleaned, renders int
	var mu sync.Mutex

	r := &counting{}
	Charts(context.Background(), r, []Chart{chart("home")}, 3, 4,
		Hooks{Prepare: func(c Chart) (engine.Spec, func(), error) {
			return c.Spec, func() {
				mu.Lock()
				cleaned++
				renders = r.calls["./home"]
				mu.Unlock()
			}, nil
		}})

	if cleaned != 1 {
		t.Errorf("cleanup ran %d times, want once", cleaned)
	}
	if renders != 3 {
		t.Errorf("cleanup saw %d renders, want it to run after all 3", renders)
	}
}

func TestAChartThatCannotBePreparedIsUnevaluable(t *testing.T) {
	boom := errors.New("missing subcharts (common) and --no-deps was passed")

	got := Charts(context.Background(), &counting{}, []Chart{chart("home")}, 2, 4,
		Hooks{Prepare: func(Chart) (engine.Spec, func(), error) { return engine.Spec{}, nil, boom }})

	if !errors.Is(got[0].Err, boom) {
		t.Errorf("Err = %v, want the preparation failure", got[0].Err)
	}
}

func serverChart(name string) Chart {
	c := chart(name)
	server := c.Spec
	server.Cluster = true
	c.Server = &server
	return c
}

func TestTheClusterConditionIsMeasuredSeparately(t *testing.T) {
	// Two independent measurements of the same chart: without cluster access
	// (what ArgoCD's repo-server does) and with it (what Flux and Helm do).
	r := &counting{body: "fixed"}

	got := Charts(context.Background(), r, []Chart{serverChart("home")}, 2, 8, Hooks{})

	if !got[0].ServerRendered {
		t.Fatalf("ServerRendered = false, want the cluster condition measured (err: %v)", got[0].ServerErr)
	}
	if r.calls["./home"] != 4 {
		t.Errorf("rendered %d times, want 4 - two rounds under each condition", r.calls["./home"])
	}
}

func TestWithoutAServerSpecNothingExtraIsRendered(t *testing.T) {
	r := &counting{}

	got := Charts(context.Background(), r, []Chart{chart("home")}, 2, 8, Hooks{})

	if got[0].ServerRendered {
		t.Error("ServerRendered = true, want no cluster measurement")
	}
	if r.calls["./home"] != 2 {
		t.Errorf("rendered %d times, want 2", r.calls["./home"])
	}
}

func TestAnUnreachableClusterLeavesTheChartUsable(t *testing.T) {
	// The chart renders perfectly well without a cluster, and the client-side
	// answer does not depend on one. A refused dry run is an unknown verdict,
	// not a failed chart.
	boom := errors.New("Kubernetes cluster unreachable")
	r := &counting{failCluster: boom}

	got := Charts(context.Background(), r, []Chart{serverChart("home")}, 2, 8, Hooks{})

	if got[0].Err != nil {
		t.Errorf("Err = %v, want the chart still checked", got[0].Err)
	}
	if got[0].ServerRendered {
		t.Error("ServerRendered = true, want the cluster condition unmeasured")
	}
	if got[0].ServerErr == nil {
		t.Error("ServerErr = nil, want the reason kept")
	}
}

func TestTheClusterRoundsRenderThePreparedChart(t *testing.T) {
	// Dependency resolution can move the chart to a temp copy; both conditions
	// must measure the same thing.
	r := &counting{}

	Charts(context.Background(), r, []Chart{serverChart("home")}, 2, 8,
		Hooks{Prepare: func(c Chart) (engine.Spec, func(), error) {
			s := c.Spec
			s.ChartRef = "/tmp/copy/home"
			return s, nil, nil
		}})

	if r.calls["/tmp/copy/home"] != 4 {
		t.Errorf("rendered %v, want all four rounds against the prepared copy", r.calls)
	}
}

// The admission query is one round trip per chart, and it used to run in a
// second pass after the pool had drained - taking a 16-chart estate from ~10s
// to ~41s with --context. It is work like any other and belongs under --jobs.

func rewrite(name string) []doctor.Rewrite {
	return []doctor.Rewrite{{Object: diff.ObjectRef{APIVersion: "v1", Kind: "Service", Name: name}}}
}

// rendezvous makes two specific kinds of work wait for each other, so a test
// can prove they overlapped. A counter cannot: two rounds of the same chart
// satisfy "two in flight" on their own, which is how the first version of this
// test passed against an implementation that ran admission in a serial pass.
type rendezvous struct {
	rendered, admitted chan struct{}
	onceR, onceA       sync.Once
}

func newRendezvous() *rendezvous {
	return &rendezvous{rendered: make(chan struct{}), admitted: make(chan struct{})}
}

func (r *rendezvous) render() error {
	r.onceR.Do(func() { close(r.rendered) })
	select {
	case <-r.admitted:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("no admission query ran while this chart was still rendering")
	}
}

func (r *rendezvous) admit() error {
	r.onceA.Do(func() { close(r.admitted) })
	select {
	case <-r.rendered:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("no chart was still rendering when this admission query ran")
	}
}

// blocking renders one named chart through a rendezvous and everything else
// immediately, so a test can pin exactly which two things must overlap.
type blocking struct {
	r  *rendezvous
	on string
}

func (b *blocking) Render(_ context.Context, spec engine.Spec) ([]manifest.Object, error) {
	if spec.Release == b.on {
		if err := b.r.render(); err != nil {
			return nil, err
		}
	}
	return manifest.Parse(strings.NewReader(
		"apiVersion: v1\nkind: Secret\nmetadata: {name: " + spec.Release + "}\ndata: {k: v}\n"))
}

func TestAdmissionIsAskedAboutWhatTheChartRendered(t *testing.T) {
	var asked []manifest.Object

	got := Charts(context.Background(), &counting{body: "same"}, []Chart{chart("home")}, 2, 4,
		Hooks{Admission: func(c Chart, objects []manifest.Object) ([]doctor.Rewrite, error) {
			asked = objects
			return rewrite(c.Name), nil
		}})

	if len(asked) != 1 {
		t.Fatalf("asked about %d objects, want the rendered output", len(asked))
	}
	if len(got[0].Rewrites) != 1 || got[0].Rewrites[0].Object.Name != "home" {
		t.Errorf("Rewrites = %v, want the answer carried back", got[0].Rewrites)
	}
}

func TestAdmissionObeysTheSameJobLimitAsRendering(t *testing.T) {
	// A cluster round trip is the slowest thing idem does. Outside the limit,
	// --jobs 1 against a 16-chart estate would still open 16 connections.
	r := &counting{delay: 5 * time.Millisecond}
	charts := []Chart{chart("a"), chart("b"), chart("c"), chart("d")}

	Charts(context.Background(), r, charts, 2, 2,
		Hooks{Admission: func(_ Chart, _ []manifest.Object) ([]doctor.Rewrite, error) {
			return nil, r.occupy()
		}})

	if r.maxActive > 2 {
		t.Errorf("%d tasks ran at once, want at most 2", r.maxActive)
	}
}

func TestAdmissionOverlapsWithOtherChartsRendering(t *testing.T) {
	// The point of the move: one chart's dry run should run while other charts
	// are still rendering, not after every render in the run has finished.
	//
	// "slow" blocks in its render until an admission query arrives, and only
	// "fast" can send one. Run in a pass after the pool drains, neither side
	// ever meets the other and both say so.
	rv := newRendezvous()

	got := Charts(context.Background(), &blocking{r: rv, on: "slow"}, []Chart{chart("fast"), chart("slow")}, 2, 8,
		Hooks{Admission: func(c Chart, _ []manifest.Object) ([]doctor.Rewrite, error) {
			if c.Name != "fast" {
				return nil, nil
			}
			return nil, rv.admit()
		}})

	for _, res := range got {
		if res.Err != nil {
			t.Errorf("%s: %v", res.Chart.Name, res.Err)
		}
		if res.RewriteErr != nil {
			t.Errorf("%s: %v", res.Chart.Name, res.RewriteErr)
		}
	}
}

func TestAnAdmissionFailureDoesNotFailTheChart(t *testing.T) {
	// An unreachable cluster says nothing about whether the chart renders
	// consistently, which is the question idem was asked.
	boom := errors.New("dry-run apply: connection refused")

	got := Charts(context.Background(), &counting{}, []Chart{chart("home")}, 2, 4,
		Hooks{Admission: func(Chart, []manifest.Object) ([]doctor.Rewrite, error) { return nil, boom }})

	if got[0].Err != nil {
		t.Errorf("Err = %v, want the chart still usable", got[0].Err)
	}
	if !errors.Is(got[0].RewriteErr, boom) {
		t.Errorf("RewriteErr = %v, want the failure reported", got[0].RewriteErr)
	}
}

func TestAdmissionIsNotAskedAboutAChartThatCouldNotRender(t *testing.T) {
	// There is nothing to ask about, and asking would report a second failure
	// for one cause.
	r := &counting{fail: map[string]error{"./home": errors.New("no such chart")}}
	asked := false

	Charts(context.Background(), r, []Chart{chart("home")}, 2, 4,
		Hooks{Admission: func(Chart, []manifest.Object) ([]doctor.Rewrite, error) {
			asked = true
			return nil, nil
		}})

	if asked {
		t.Error("the cluster was asked about a chart that never rendered")
	}
}
