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

	body  string
	delay time.Duration
	fail  map[string]error
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

	got := Charts(context.Background(), &counting{}, charts, 2, 4)

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
	got := Charts(context.Background(), &counting{}, []Chart{chart("home"), chart("lab")}, 2, 4)

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

	Charts(context.Background(), r, []Chart{chart("home"), chart("lab")}, 3, 4)

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

	got := Charts(context.Background(), r, []Chart{chart("home"), chart("lab"), chart("ops")}, 2, 4)

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

	Charts(context.Background(), r, charts, 3, 2)

	if r.maxActive > 2 {
		t.Errorf("%d renders ran at once, want at most 2", r.maxActive)
	}
}

func TestChartsRendersDifferentChartsConcurrently(t *testing.T) {
	m := newMeeting(3)

	got := Charts(context.Background(), m, []Chart{chart("a"), chart("b"), chart("c")}, 2, 8)

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

	got := Charts(context.Background(), m, []Chart{chart("only")}, 2, 8)

	if got[0].Err != nil {
		t.Errorf("rounds of a single chart did not overlap: %v", got[0].Err)
	}
}

func TestRoundsAreBoundedByTheJobLimitToo(t *testing.T) {
	// A single chart with many rounds must not ignore --jobs.
	r := &counting{delay: 5 * time.Millisecond}

	Charts(context.Background(), r, []Chart{chart("only")}, 8, 2)

	if r.maxActive > 2 {
		t.Errorf("%d renders ran at once, want at most 2", r.maxActive)
	}
}

func TestFewerThanTwoRoundsIsReportedPerChart(t *testing.T) {
	got := Charts(context.Background(), &counting{}, []Chart{chart("home")}, 1, 4)

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
		for _, r := range Charts(context.Background(), &counting{}, charts, 2, 8) {
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
