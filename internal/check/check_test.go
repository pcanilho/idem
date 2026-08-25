package check

import (
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/diff"
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

// ArgoCD never applies a Helm test hook, so churn in one is not churn.
//
// Verified against ArgoCD's own Helm page: it maps pre/post-install, pre/post
// -upgrade, pre/post-delete and crd-install onto its own hook system, and of
// the rest says "Argo CD currently skips manifests that include hooks not
// supported by Argo CD, including Helm test hooks" - i.e. test, test-success,
// test-failure, pre-rollback, post-rollback.
//
// This mattered because podinfo - Prodan's flagship demo chart, and the single
// most likely first run a Flux user makes - ships three test-hook Pods with
// generated names. idem reported `1 of 1 chart will churn under ArgoCD` on it,
// which is false for the engine it names.
//
// Nor is it churn under the other two: helm creates a test hook only during
// `helm test` and deletes it after, and it never takes part in drift detection.
// So these are dropped from the comparison for every engine - counted, never
// silently.
func TestObjectsArgoCDNeverAppliesAreNotCompared(t *testing.T) {
	render := func(token string) string {
		return `
apiVersion: v1
kind: Pod
metadata:
  name: chart-test-` + token + `
  annotations:
    helm.sh/hook: test
spec: {containers: [{name: c, image: busybox}]}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: real
data: {k: v}
`
	}

	got, err := Compare([][]manifest.Object{parse(t, render("aaaa")), parse(t, render("bbbb"))})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	if len(got.Findings) != 0 {
		t.Errorf("Findings = %+v, want none - the only difference is in a hook ArgoCD ignores", got.Findings)
	}
	if got.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 - both renders' test Pods, counted rather than dropped in silence", got.Skipped)
	}
}

// A hook ArgoCD DOES map onto its own system is applied, so churn in it is real.
func TestObjectsArgoCDDoesApplyAreStillCompared(t *testing.T) {
	render := func(token string) string {
		return `
apiVersion: v1
kind: Job
metadata:
  name: migrate
  annotations:
    helm.sh/hook: pre-upgrade
spec: {token: ` + token + `}
`
	}

	got, err := Compare([][]manifest.Object{parse(t, render("aaaa")), parse(t, render("bbbb"))})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	if len(got.Findings) != 1 {
		t.Errorf("Findings = %+v, want 1 - pre-upgrade maps to PreSync and is applied", got.Findings)
	}
	if got.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", got.Skipped)
	}
}

// An object whose PRESENCE varies keeps the presence verdict.
//
// merge kept the first round's change type and appended later rounds' paths to
// it, so an object that is absent in round 2 and merely different in round 3
// ended up typed only-in-left WITH field paths. Two things then read that
// combination wrongly, both by branching on len(Paths) == 0:
//
//   - report.writeFinding took the field branch, so the user was never told the
//     object sometimes does not render at all - the more serious fact.
//   - remediate.skip emitted an ignoreDifferences entry for it, which cannot
//     possibly fix an object that intermittently disappears.
//
// Presence beats fields: if any round disagrees about whether the object exists,
// that is the finding.
func TestAnObjectThatSometimesVanishesKeepsThePresenceVerdict(t *testing.T) {
	const present = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: keep
data: {k: one}
`
	const absent = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: other
data: {k: one}
`
	const differs = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: keep
data: {k: two}
`

	got, err := Compare([][]manifest.Object{parse(t, present), parse(t, absent), parse(t, differs)})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	var keep *Finding
	for i := range got.Findings {
		if got.Findings[i].Change.Object.Name == "keep" {
			keep = &got.Findings[i]
		}
	}
	if keep == nil {
		t.Fatalf("no finding for ConfigMap/keep: %+v", got.Findings)
	}

	if keep.Change.Type != diff.OnlyInLeft {
		t.Errorf("Type = %v, want only-in-left - round 2 did not render it at all", keep.Change.Type)
	}
	if len(keep.Change.Paths) != 0 {
		t.Errorf("Paths = %+v, want none: a field list makes the report and the fix block both treat this as ordinary field churn", keep.Change.Paths)
	}
}

// A round that permuted and a round that changed content both survive.
//
// merge dedups on the JSON pointer, and a reorder is recorded at the list
// (/spec/items) while a changed element is recorded at the leaf
// (/spec/items/2). They are different pointers, so both are kept - which is
// honest: round 2 moved the elements and round 3 replaced one, and neither
// fact implies the other.
//
// It matters because the two need different treatment downstream. remediate
// drops the reorder path and keeps the leaf, so this object still gets the fix
// for the half a fix can reach. Pinned so that is deliberate rather than a
// coincidence of pointer spelling.
func TestAReorderInOneRoundAndAChangedElementInAnotherAreBothKept(t *testing.T) {
	const round1 = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
spec: {items: ["a", "b", "c"]}
`
	const permuted = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
spec: {items: ["c", "a", "b"]}
`
	const changed = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
spec: {items: ["a", "b", "z"]}
`

	got, err := Compare([][]manifest.Object{parse(t, round1), parse(t, permuted), parse(t, changed)})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1: %+v", len(got.Findings), got.Findings)
	}

	byPointer := map[string]bool{}
	for _, p := range got.Findings[0].Change.Paths {
		byPointer[p.Path.JSONPointer()] = p.Reordered
	}

	reorder, ok := byPointer["/spec/items"]
	if !ok {
		t.Errorf("no path at /spec/items: round 2 permuted the list and that fact was lost: %v", byPointer)
	}
	if !reorder {
		t.Errorf("Reordered = false at /spec/items, want true")
	}

	leaf, ok := byPointer["/spec/items/2"]
	if !ok {
		t.Errorf("no path at /spec/items/2: round 3 replaced an element and that fact was lost: %v", byPointer)
	}
	if leaf {
		t.Errorf("Reordered = true at /spec/items/2, want false: the element's value changed")
	}
}
