package check

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/manifest"
)

// scripted renders a pre-set sequence of rounds, so a test can describe
// exactly what non-determinism looks like without needing helm or a chart.
type scripted struct {
	rounds [][]manifest.Object
	err    error
	calls  int
}

func (s *scripted) Render(context.Context, engine.Spec) ([]manifest.Object, error) {
	if s.err != nil {
		return nil, s.err
	}
	round := s.rounds[min(s.calls, len(s.rounds)-1)]
	s.calls++
	return round, nil
}

func parse(t *testing.T, in string) []manifest.Object {
	t.Helper()
	objs, err := manifest.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return objs
}

func secret(t *testing.T, password string) []manifest.Object {
	t.Helper()
	return parse(t, `
# Source: home/templates/secrets.yaml
apiVersion: v1
kind: Secret
metadata: {name: home-creds}
data: {password: `+password+`}
`)
}

func run(t *testing.T, r Renderer, rounds int) Result {
	t.Helper()
	got, err := Run(context.Background(), r, engine.Spec{Release: "home", ChartRef: "./home"}, rounds)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return got
}

func TestRunReportsNothingWhenEveryRoundIsIdentical(t *testing.T) {
	got := run(t, &scripted{rounds: [][]manifest.Object{secret(t, "aaa"), secret(t, "aaa")}}, 2)

	if len(got.Findings) != 0 {
		t.Errorf("Run() found %d findings, want 0: %+v", len(got.Findings), got.Findings)
	}
}

func TestRunReportsTheFieldThatDiffers(t *testing.T) {
	got := run(t, &scripted{rounds: [][]manifest.Object{secret(t, "aaa"), secret(t, "bbb")}}, 2)

	if len(got.Findings) != 1 {
		t.Fatalf("Run() found %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
	paths := got.Findings[0].Change.Paths
	if len(paths) != 1 {
		t.Fatalf("finding has %d paths, want 1: %+v", len(paths), paths)
	}
	if want := ".data.password"; paths[0].Path.String() != want {
		t.Errorf("path = %q, want %q", paths[0].Path.String(), want)
	}
}

func TestRunAttributesAFindingToTheTemplateThatProducedIt(t *testing.T) {
	// Findings are grouped by template in the output, and the only source of
	// that attribution is helm's "# Source:" comment on the render.
	got := run(t, &scripted{rounds: [][]manifest.Object{secret(t, "aaa"), secret(t, "bbb")}}, 2)

	if len(got.Findings) != 1 {
		t.Fatalf("Run() found %d findings, want 1", len(got.Findings))
	}
	if want := "home/templates/secrets.yaml"; got.Findings[0].Source != want {
		t.Errorf("Source = %q, want %q", got.Findings[0].Source, want)
	}
}

func TestRunLeavesSourceEmptyWhenTheRenderCarriesNone(t *testing.T) {
	// `argocd app manifests` output has lost the comment. Absent is reported
	// as absent; the formatter says so rather than guessing a template.
	noSource := func(password string) []manifest.Object {
		return parse(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: home-creds}\ndata: {password: "+password+"}\n")
	}
	got := run(t, &scripted{rounds: [][]manifest.Object{noSource("aaa"), noSource("bbb")}}, 2)

	if len(got.Findings) != 1 {
		t.Fatalf("Run() found %d findings, want 1", len(got.Findings))
	}
	if got.Findings[0].Source != "" {
		t.Errorf("Source = %q, want empty", got.Findings[0].Source)
	}
}

func TestRunComparesEveryRoundAgainstTheFirst(t *testing.T) {
	// A value that happens to repeat in rounds 1 and 2 but changes in round 3
	// is still non-deterministic. Comparing only consecutive pairs would find
	// it; comparing only the first pair would not.
	got := run(t, &scripted{rounds: [][]manifest.Object{
		secret(t, "aaa"), secret(t, "aaa"), secret(t, "ccc"),
	}}, 3)

	if len(got.Findings) != 1 {
		t.Fatalf("Run() found %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
}

func TestRunReportsAPathOnceWhenItDiffersInSeveralRounds(t *testing.T) {
	got := run(t, &scripted{rounds: [][]manifest.Object{
		secret(t, "aaa"), secret(t, "bbb"), secret(t, "ccc"),
	}}, 3)

	if len(got.Findings) != 1 {
		t.Fatalf("Run() found %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
	if n := len(got.Findings[0].Change.Paths); n != 1 {
		t.Errorf("finding has %d paths, want 1 - the same field must not be reported per round", n)
	}
}

func TestRunRendersExactlyTheRequestedNumberOfRounds(t *testing.T) {
	r := &scripted{rounds: [][]manifest.Object{secret(t, "aaa")}}
	got := run(t, r, 4)

	if r.calls != 4 {
		t.Errorf("Render called %d times, want 4", r.calls)
	}
	if got.Rounds != 4 {
		t.Errorf("Result.Rounds = %d, want 4", got.Rounds)
	}
}

func TestRunRejectsFewerThanTwoRounds(t *testing.T) {
	// One render cannot be compared to anything, and reporting "no findings"
	// from a single render would be a pass the user cannot trust.
	for _, rounds := range []int{0, 1} {
		_, err := Run(context.Background(), &scripted{rounds: [][]manifest.Object{secret(t, "a")}},
			engine.Spec{ChartRef: "./home"}, rounds)
		if err == nil {
			t.Errorf("Run(rounds=%d) error = nil, want an error", rounds)
		}
	}
}

func TestRunPropagatesARenderFailure(t *testing.T) {
	// A chart that cannot be rendered is exit 2, never a silent skip - that is
	// the allow_skip lesson, and the same class of bug idem exists to catch.
	boom := errors.New("helm template: exit status 1")
	_, err := Run(context.Background(), &scripted{err: boom},
		engine.Spec{ChartRef: "./home"}, 2)

	if !errors.Is(err, boom) {
		t.Errorf("Run() error = %v, want it to wrap the render failure", err)
	}
}

func TestRunReportsAnObjectRenderedInOnlyOneRound(t *testing.T) {
	// An object that appears or disappears between renders never converges
	// either, and it has no differing field to point at.
	both := parse(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: a}
data: {x: "1"}
---
apiVersion: v1
kind: Secret
metadata: {name: b}
data: {y: "2"}
`)
	got := run(t, &scripted{rounds: [][]manifest.Object{both, both[:1]}}, 2)

	if len(got.Findings) != 1 {
		t.Fatalf("Run() found %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
	if got.Findings[0].Change.Object.Name != "b" {
		t.Errorf("finding names %q, want the object that vanished", got.Findings[0].Change.Object.Name)
	}
}

func TestRunOrdersFindingsDeterministically(t *testing.T) {
	objs := func(a, b string) []manifest.Object {
		return parse(t, `
apiVersion: v1
kind: Secret
metadata: {name: zeta}
data: {k: `+a+`}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: alpha}
data: {k: `+b+`}
`)
	}
	got := run(t, &scripted{rounds: [][]manifest.Object{objs("1", "1"), objs("2", "2")}}, 2)

	if len(got.Findings) != 2 {
		t.Fatalf("Run() found %d findings, want 2", len(got.Findings))
	}
	// Sorted by object key: "v1|ConfigMap||alpha" before "v1|Secret||zeta".
	if got.Findings[0].Change.Object.Name != "alpha" {
		t.Errorf("first finding is %q, want alpha", got.Findings[0].Change.Object.Name)
	}
}
