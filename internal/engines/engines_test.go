package engines

import (
	"errors"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/engine"
)

func byName(t *testing.T, name string) Target {
	t.Helper()
	for _, e := range All() {
		if e.Name() == name {
			return e
		}
	}
	t.Fatalf("no engine named %q", name)
	return Target{}
}

var oneUse = Evidence{Uses: []analyze.Use{{File: "charts/common/templates/_secrets.tpl", Line: 103}}}

var noUse = Evidence{}

func TestAllListsTheThreeEnginesInReadingOrder(t *testing.T) {
	var names []string
	for _, e := range All() {
		names = append(names, e.Name())
	}
	if got, want := strings.Join(names, ","), "argocd,flux,helm"; got != want {
		t.Errorf("All() = %q, want %q", got, want)
	}
}

func TestCapabilitiesMatchWhatEachEngineActuallyDoes(t *testing.T) {
	// Verified in docs/design.md: ArgoCD's repo-server runs `helm template`
	// with no cluster access and re-renders every reconcile; Flux's
	// helm-controller does a real install through the Helm SDK, so lookup
	// resolves and templates are not re-rendered on reconcile.
	for _, tc := range []struct {
		name           string
		lookupResolves bool
		rerenders      bool
	}{
		{"argocd", false, true},
		{"flux", true, false},
		{"helm", true, false},
	} {
		caps := byName(t, tc.name).Capabilities()
		if caps.LookupResolves != tc.lookupResolves {
			t.Errorf("%s LookupResolves = %v, want %v", tc.name, caps.LookupResolves, tc.lookupResolves)
		}
		if caps.RerendersOnReconcile != tc.rerenders {
			t.Errorf("%s RerendersOnReconcile = %v, want %v", tc.name, caps.RerendersOnReconcile, tc.rerenders)
		}
	}
}

func TestArgoCDChurnsAsAnObservedFact(t *testing.T) {
	// `helm template` resolves lookup to {} by construction, which IS the
	// condition ArgoCD's repo-server renders under. So a difference observed
	// between two `helm template` runs is not extrapolated to ArgoCD - it was
	// measured there.
	v := byName(t, "argocd").Verdict(oneUse)

	if v.Result != engine.Churns {
		t.Errorf("Result = %v, want churns", v.Result)
	}
	if !v.Observed {
		t.Error("Observed = false, want true - this verdict was measured, not inferred")
	}
}

func TestArgoCDChurnsEvenThoughTheChartUsesLookup(t *testing.T) {
	// A lookup cannot save ArgoCD: the repo-server has no cluster access, so
	// it resolves to {} however the chart is written.
	if got := byName(t, "argocd").Verdict(oneUse).Result; got != engine.Churns {
		t.Errorf("Result = %v, want churns regardless of lookup", got)
	}
}

func TestFluxAndHelmChurnSoundlyWhenTheChartHasNoLookup(t *testing.T) {
	// Nothing in the chart could stabilise the value, so this holds for any
	// engine - it is a chart defect rather than an ArgoCD limitation.
	for _, name := range []string{"flux", "helm"} {
		v := byName(t, name).Verdict(noUse)
		if v.Result != engine.Churns {
			t.Errorf("%s Result = %v, want churns", name, v.Result)
		}
		if v.Observed {
			t.Errorf("%s Observed = true, want false - this is sound reasoning, not a measurement", name)
		}
	}
}

func TestFluxAndHelmAreUnknownWhenTheChartUsesLookup(t *testing.T) {
	// idem does not trace whether that lookup guards this particular value.
	// The tracer is cut permanently, so the honest answer is unknown.
	for _, name := range []string{"flux", "helm"} {
		v := byName(t, name).Verdict(oneUse)
		if v.Result != engine.Unknown {
			t.Errorf("%s Result = %v, want unknown", name, v.Result)
		}
		if v.Observed {
			t.Errorf("%s Observed = true, want false", name)
		}
	}
}

func TestAnUnknownVerdictCitesTheLookupItFound(t *testing.T) {
	// "unknown" with no evidence is unfalsifiable. Naming the file and line
	// lets the reader check idem's reasoning themselves.
	v := byName(t, "flux").Verdict(oneUse)

	if !strings.Contains(v.Because, "_secrets.tpl") || !strings.Contains(v.Because, "103") {
		t.Errorf("Because = %q, want the lookup located", v.Because)
	}
}

func TestEveryVerdictExplainsItself(t *testing.T) {
	for _, e := range All() {
		for _, uses := range []Evidence{noUse, oneUse} {
			if v := e.Verdict(uses); strings.TrimSpace(v.Because) == "" {
				t.Errorf("%s: verdict %v has no reason", e.Name(), v.Result)
			}
		}
	}
}

func TestVerdictNamesItsOwnEngine(t *testing.T) {
	for _, e := range All() {
		if got := e.Verdict(noUse).Engine; got != e.Name() {
			t.Errorf("Verdict().Engine = %q, want %q", got, e.Name())
		}
	}
}

func TestSelectResolvesEachEngineByName(t *testing.T) {
	for _, name := range []string{"argocd", "flux", "helm"} {
		got, err := Select(name)
		if err != nil {
			t.Fatalf("Select(%q) error = %v", name, err)
		}
		if len(got) != 1 || got[0].Name() != name {
			t.Errorf("Select(%q) = %v, want just that engine", name, got)
		}
	}
}

func TestSelectAllReturnsEveryEngine(t *testing.T) {
	got, err := Select("all")
	if err != nil {
		t.Fatalf("Select(all) error = %v", err)
	}
	if len(got) != len(All()) {
		t.Errorf("Select(all) returned %d engines, want %d", len(got), len(All()))
	}
}

func TestSelectRejectsAnUnknownEngineAndSaysWhatIsValid(t *testing.T) {
	_, err := Select("fleet")
	if err == nil {
		t.Fatal("Select() error = nil, want an error")
	}
	for _, want := range []string{"fleet", "argocd", "flux", "helm", "all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestAFailedScanIsUnknownRatherThanClean(t *testing.T) {
	// A corrupt vendored archive means idem could not look. Treating that as
	// "no lookup anywhere" would turn a failed scan into a CHURNS verdict
	// that §5 states as sound - the one place a wrong answer is stated with
	// full confidence.
	ev := Evidence{Err: errors.New("scanning charts/common-2.0.0.tgz for lookup: unexpected EOF")}

	for _, name := range []string{"flux", "helm"} {
		v := byName(t, name).Verdict(ev)
		if v.Result != engine.Unknown {
			t.Errorf("%s Result = %v, want unknown", name, v.Result)
		}
		if !strings.Contains(v.Because, "common-2.0.0.tgz") {
			t.Errorf("%s Because = %q, want the unreadable archive named", name, v.Because)
		}
	}

	// ArgoCD's answer does not depend on the scan at all.
	if got := byName(t, "argocd").Verdict(ev).Result; got != engine.Churns {
		t.Errorf("argocd Result = %v, want churns - lookup is irrelevant there", got)
	}
}

func observed(stable bool) Evidence { return Evidence{Uses: oneUse.Uses, Cluster: &stable} }

func TestAMeasuredClusterRenderBeatsGuessing(t *testing.T) {
	// The whole point of --cluster: `unknown` becomes a fact. helm's server
	// dry run resolves lookup and uses the cluster's real capabilities, which
	// is what an engine doing a real install sees.
	for _, name := range []string{"flux", "helm"} {
		v := byName(t, name).Verdict(observed(true))

		if v.Result != engine.Stable {
			t.Errorf("%s Result = %v, want stable", name, v.Result)
		}
		if !v.Observed {
			t.Errorf("%s Observed = false, want true - this was measured", name)
		}
	}
}

func TestAChartThatChurnsEvenWithLookupResolvedIsAFact(t *testing.T) {
	// The lookup is present but does not guard this value. Without --cluster
	// idem could only say unknown; with it, this is settled.
	v := byName(t, "flux").Verdict(observed(false))

	if v.Result != engine.Churns {
		t.Errorf("Result = %v, want churns", v.Result)
	}
	if !v.Observed {
		t.Error("Observed = false, want true")
	}
	if !strings.Contains(v.Because, "observed") {
		t.Errorf("Because = %q, want it to say the answer was measured", v.Because)
	}
}

func TestArgoCDIsUnaffectedByWhatTheClusterSays(t *testing.T) {
	// ArgoCD's repo-server has no cluster access whatever idem can reach, so a
	// measurement taken WITH access says nothing about it.
	if got := byName(t, "argocd").Verdict(observed(true)).Result; got != engine.Churns {
		t.Errorf("argocd Result = %v, want churns regardless", got)
	}
}

func TestAMeasurementOutranksAFailedScan(t *testing.T) {
	// If idem watched the chart render identically with lookup resolving, not
	// having been able to read the source no longer matters.
	stable := true
	ev := Evidence{Err: errors.New("scanning charts/x.tgz: unexpected EOF"), Cluster: &stable}

	if got := byName(t, "flux").Verdict(ev).Result; got != engine.Stable {
		t.Errorf("Result = %v, want the observation to win", got)
	}
}
