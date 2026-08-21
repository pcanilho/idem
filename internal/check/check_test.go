package check

import (
	"strings"
	"testing"

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

func compare(t *testing.T, renders ...[]manifest.Object) Result {
	t.Helper()
	got, err := Compare(renders)
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	return got
}

func TestCompareReportsNothingWhenEveryRoundIsIdentical(t *testing.T) {
	got := compare(t, secret(t, "aaa"), secret(t, "aaa"))

	if len(got.Findings) != 0 {
		t.Errorf("Compare() found %d findings, want 0: %+v", len(got.Findings), got.Findings)
	}
}

func TestCompareReportsTheFieldThatDiffers(t *testing.T) {
	got := compare(t, secret(t, "aaa"), secret(t, "bbb"))

	if len(got.Findings) != 1 {
		t.Fatalf("Compare() found %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
	paths := got.Findings[0].Change.Paths
	if len(paths) != 1 {
		t.Fatalf("finding has %d paths, want 1: %+v", len(paths), paths)
	}
	if want := ".data.password"; paths[0].Path.String() != want {
		t.Errorf("path = %q, want %q", paths[0].Path.String(), want)
	}
}

func TestCompareAttributesAFindingToTheTemplateThatProducedIt(t *testing.T) {
	// Findings are grouped by template in the output, and the only source of
	// that attribution is helm's "# Source:" comment on the render.
	got := compare(t, secret(t, "aaa"), secret(t, "bbb"))

	if len(got.Findings) != 1 {
		t.Fatalf("Compare() found %d findings, want 1", len(got.Findings))
	}
	if want := "home/templates/secrets.yaml"; got.Findings[0].Source != want {
		t.Errorf("Source = %q, want %q", got.Findings[0].Source, want)
	}
}

func TestCompareLeavesSourceEmptyWhenTheRenderCarriesNone(t *testing.T) {
	// `argocd app manifests` output has lost the comment. Absent is reported
	// as absent; the formatter says so rather than guessing a template.
	noSource := func(password string) []manifest.Object {
		return parse(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: home-creds}\ndata: {password: "+password+"}\n")
	}
	got := compare(t, noSource("aaa"), noSource("bbb"))

	if len(got.Findings) != 1 {
		t.Fatalf("Compare() found %d findings, want 1", len(got.Findings))
	}
	if got.Findings[0].Source != "" {
		t.Errorf("Source = %q, want empty", got.Findings[0].Source)
	}
}

func TestCompareChecksEveryRoundAgainstTheFirst(t *testing.T) {
	// A value that happens to repeat in rounds 1 and 2 but changes in round 3
	// is still non-deterministic. Comparing only consecutive pairs would find
	// it; comparing only the first pair would not.
	got := compare(t, secret(t, "aaa"), secret(t, "aaa"), secret(t, "ccc"))

	if len(got.Findings) != 1 {
		t.Fatalf("Compare() found %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
}

func TestCompareReportsAPathOnceWhenItDiffersInSeveralRounds(t *testing.T) {
	got := compare(t, secret(t, "aaa"), secret(t, "bbb"), secret(t, "ccc"))

	if len(got.Findings) != 1 {
		t.Fatalf("Compare() found %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
	if n := len(got.Findings[0].Change.Paths); n != 1 {
		t.Errorf("finding has %d paths, want 1 - the same field must not be reported per round", n)
	}
}

func TestCompareReportsHowManyRoundsItSaw(t *testing.T) {
	got := compare(t, secret(t, "a"), secret(t, "a"), secret(t, "a"), secret(t, "a"))

	if got.Rounds != 4 {
		t.Errorf("Result.Rounds = %d, want 4", got.Rounds)
	}
}

func TestCompareRejectsFewerThanTwoRenders(t *testing.T) {
	// One render cannot be compared to anything, and reporting "no findings"
	// from a single render would be a pass the user cannot trust.
	for _, renders := range [][][]manifest.Object{
		nil,
		{secret(t, "a")},
	} {
		if _, err := Compare(renders); err == nil {
			t.Errorf("Compare(%d renders) error = nil, want an error", len(renders))
		}
	}
}

func TestCompareReportsAnObjectRenderedInOnlyOneRound(t *testing.T) {
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
	got := compare(t, both, both[:1])

	if len(got.Findings) != 1 {
		t.Fatalf("Compare() found %d findings, want 1: %+v", len(got.Findings), got.Findings)
	}
	if got.Findings[0].Change.Object.Name != "b" {
		t.Errorf("finding names %q, want the object that vanished", got.Findings[0].Change.Object.Name)
	}
}

func TestCompareOrdersFindingsDeterministically(t *testing.T) {
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
	got := compare(t, objs("1", "1"), objs("2", "2"))

	if len(got.Findings) != 2 {
		t.Fatalf("Compare() found %d findings, want 2", len(got.Findings))
	}
	// Sorted by object key: "v1|ConfigMap||alpha" before "v1|Secret||zeta".
	if got.Findings[0].Change.Object.Name != "alpha" {
		t.Errorf("first finding is %q, want alpha", got.Findings[0].Change.Object.Name)
	}
}

func TestCompareRejectsDuplicateObjectIdentitiesInARender(t *testing.T) {
	dup := parse(t, `
apiVersion: v1
kind: Secret
metadata: {name: a}
data: {k: "1"}
---
apiVersion: v1
kind: Secret
metadata: {name: a}
data: {k: "2"}
`)
	if _, err := Compare([][]manifest.Object{dup, dup}); err == nil {
		t.Error("Compare() error = nil, want the duplicate reported rather than one object silently dropped")
	}
}
