package report

import (
	"errors"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/delivery"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/objpath"
)

// path turns ".data.password" into the segments the real pipeline would build.
func path(dotted string) objpath.Path {
	var p objpath.Path
	for seg := range strings.SplitSeq(strings.TrimPrefix(dotted, "."), ".") {
		p = p.Append(objpath.Key(seg))
	}
	return p
}

func finding(source, name string, fields ...string) check.Finding {
	var paths []diff.PathDiff
	for _, f := range fields {
		paths = append(paths, diff.PathDiff{
			Path: path(f), Left: "a", Right: "b", HasLeft: true, HasRight: true,
		})
	}
	return check.Finding{
		Source: source,
		Change: diff.Change{
			Object: diff.ObjectRef{APIVersion: "v1", Kind: "Secret", Name: name},
			Type:   diff.Differs,
			Paths:  paths,
		},
	}
}

func text(t *testing.T, r Report) string {
	t.Helper()
	var b strings.Builder
	if err := r.Text(&b); err != nil {
		t.Fatalf("Text() error = %v", err)
	}
	return b.String()
}

func clean(name string) Chart { return Chart{Name: name, Dir: "./" + name} }

func TestTextReportsACleanRunWithAVerdictSentence(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{clean("home"), clean("lab")},
		Helm:   "4.2.4",
		Rounds: 2,
	})

	if !strings.Contains(got, "All 2 charts render consistently under ArgoCD") {
		t.Errorf("Text() = %q, want a verdict sentence naming the engine", got)
	}
}

func TestTextUsesTheChartNameForASingleCleanChart(t *testing.T) {
	got := text(t, Report{Charts: []Chart{clean("home")}, Helm: "4.2.4", Rounds: 2})

	if !strings.Contains(got, "home renders consistently under ArgoCD") {
		t.Errorf("Text() = %q, want the chart named rather than a count", got)
	}
}

func TestTextAlwaysNamesTheHelmBinaryAndRoundCount(t *testing.T) {
	// A pass that does not say what it checked is a pass you cannot trust -
	// ArgoCD 3.5 swapped Helm 3.19 for 4.2 underneath everybody.
	for _, r := range []Report{
		{Charts: []Chart{clean("home")}, Helm: "4.2.4", Rounds: 2},
		{Charts: []Chart{{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "home-creds", ".data.password")}}}, Helm: "4.2.4", Rounds: 3},
	} {
		got := text(t, r)
		if !strings.Contains(got, "helm 4.2.4") {
			t.Errorf("Text() = %q, want the helm version", got)
		}
		if !strings.Contains(got, "rounds") {
			t.Errorf("Text() = %q, want the round count", got)
		}
	}
}

func TestTextGroupsFindingsByTheTemplateThatProducedThem(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{
			Name: "lab",
			Findings: []check.Finding{
				finding("lab/templates/database.yaml", "lab-harbor-postgres", ".data.password"),
				finding("lab/templates/database.yaml", "lab-harbor-postgres2", ".data.registry-token"),
				finding("lab/templates/secrets.yaml", "lab-other", ".data.key"),
			},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Count(got, "lab/templates/database.yaml") != 1 {
		t.Errorf("Text() = %q, want the template named once as a group header", got)
	}
	db := strings.Index(got, "lab/templates/database.yaml")
	secrets := strings.Index(got, "lab/templates/secrets.yaml")
	if db < 0 || secrets < 0 || db > secrets {
		t.Errorf("Text() = %q, want groups in sorted order", got)
	}
}

func TestTextGroupsFindingsWithNoSourceUnderAnExplicitUnknown(t *testing.T) {
	// Input that has lost helm's "# Source:" comment must say so. A guessed
	// template is worse than an admitted gap.
	got := text(t, Report{
		Charts: []Chart{{Name: "x", Findings: []check.Finding{finding("", "thing", ".data.k")}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "(source unknown)") {
		t.Errorf("Text() = %q, want an explicit unknown group", got)
	}
}

func TestTextNamesTheObjectAndFieldOfEachFinding(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{
			finding("home/templates/secrets.yaml", "home-ollama-secrets", ".data.WEBUI_SECRET_KEY"),
		}}},
		Helm: "4.2.4", Rounds: 2,
	})

	for _, want := range []string{"Secret/home-ollama-secrets", ".data.WEBUI_SECRET_KEY"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, want it to contain %q", got, want)
		}
	}
}

func TestTextCountsHowManyChartsWillChurn(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{
			{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "c", ".data.k")}},
			clean("lab"),
			clean("ops"),
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "1 of 3 charts will churn under ArgoCD") {
		t.Errorf("Text() = %q, want a count of churning charts out of the total", got)
	}
}

func TestTextReportsChartsThatCouldNotBeRendered(t *testing.T) {
	// Exit 2 is always fatal; a chart that silently fails to render and is
	// then skipped is the same class of bug idem exists to catch.
	got := text(t, Report{
		Charts: []Chart{clean("home"), {Name: "lab", Err: errors.New("helm template: exit status 1: missing dependency")}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "could not be rendered") {
		t.Errorf("Text() = %q, want unevaluable charts reported", got)
	}
	if !strings.Contains(got, "missing dependency") {
		t.Errorf("Text() = %q, want helm's own reason shown", got)
	}
}

func TestTextCapsTheFieldsListedForOneObject(t *testing.T) {
	// A Secret whose whole .data regenerates produces one line per key. Left
	// unbounded that is hundreds of lines straight to a terminal.
	var fields []string
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		fields = append(fields, ".data."+k)
	}
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{
			finding("home/templates/s.yaml", "big", fields...),
		}}},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, ".data.h") {
		t.Errorf("Text() = %q, want the field list capped", got)
	}
	if !strings.Contains(got, "more") {
		t.Errorf("Text() = %q, want the elided fields counted", got)
	}
}

func TestChurningAndUnevaluableCountCharts(t *testing.T) {
	r := Report{Charts: []Chart{
		{Name: "a", Findings: []check.Finding{finding("t.yaml", "o", ".x")}},
		{Name: "b", Err: errors.New("boom")},
		clean("c"),
	}}

	if got, want := r.Churning(), 1; got != want {
		t.Errorf("Churning() = %d, want %d", got, want)
	}
	if got, want := r.Unevaluable(), 1; got != want {
		t.Errorf("Unevaluable() = %d, want %d", got, want)
	}
}

func TestTextContainsNoRawTabs(t *testing.T) {
	// Columns are aligned with spaces. A raw tab renders at whatever width the
	// reader's terminal decides, which is exactly the misalignment that ruled
	// out box drawing and emoji badges in the first place.
	got := text(t, Report{
		Charts: []Chart{
			{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "creds", ".data.password")}},
			{Name: "lab", Err: errors.New("helm template: exit status 1")},
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "\t") {
		t.Errorf("Text() = %q, want no raw tab characters", got)
	}
}

func TestTextIndentsEveryLineOfAMultiLineRenderError(t *testing.T) {
	// helm's stderr is several lines with a blank line in the middle. Dumped
	// verbatim it breaks out of the indentation and reads as a crash.
	err := errors.New("helm template: exit status 1: Error: execution error\n\nUse --debug flag to render out invalid YAML")
	got := text(t, Report{
		Charts: []Chart{{Name: "lab", Err: err}},
		Helm:   "4.2.4", Rounds: 2,
	})

	for line := range strings.SplitSeq(strings.TrimRight(got, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "  ") {
			t.Errorf("line %q is not indented; Text() = %q", line, got)
		}
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("Text() = %q, want no blank runs from helm's stderr", got)
	}
}

func TestVerdictWhenEveryChartFailedToRender(t *testing.T) {
	// "0 charts render consistently under ArgoCD" is true but useless, and
	// reads as a verdict about charts that were never actually checked.
	got := text(t, Report{
		Charts: []Chart{{Name: "lab", Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "render consistently") {
		t.Errorf("Text() = %q, want no consistency claim when nothing rendered", got)
	}
	if !strings.Contains(got, "1 chart could not be rendered") {
		t.Errorf("Text() = %q, want the count of unevaluable charts", got)
	}
}

func verdicts(specs ...[3]string) []engine.Verdict {
	var out []engine.Verdict
	for _, s := range specs {
		var r engine.Result
		switch s[1] {
		case "churns":
			r = engine.Churns
		case "stable":
			r = engine.Stable
		}
		out = append(out, engine.Verdict{Engine: s[0], Result: r, Because: s[2], Observed: s[0] == "argocd"})
	}
	return out
}

func chartWithVerdicts(vs []engine.Verdict) Report {
	return Report{
		Charts: []Chart{{
			Name:     "home",
			Findings: []check.Finding{finding("home/templates/secrets.yaml", "creds", ".data.password")},
			Verdicts: vs,
		}},
		Helm: "4.2.4", Rounds: 2,
	}
}

func TestTextShowsOneVerdictRowPerEngine(t *testing.T) {
	got := text(t, chartWithVerdicts(verdicts(
		[3]string{"argocd", "churns", "every sync, forever"},
		[3]string{"flux", "unknown", "chart calls `lookup` (a.tpl:9)"},
		[3]string{"helm", "unknown", "chart calls `lookup` (a.tpl:9)"},
	)))

	for _, want := range []string{"argocd", "flux", "helm"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, want a row for %q", got, want)
		}
	}
}

func TestTextShoutsChurnsAndKeepsUnknownQuiet(t *testing.T) {
	// CHURNS is the one word that should catch the eye in a wall of output;
	// unknown is an admission and should not compete with it.
	got := text(t, chartWithVerdicts(verdicts(
		[3]string{"argocd", "churns", "every sync, forever"},
		[3]string{"flux", "unknown", "chart calls `lookup` (a.tpl:9)"},
	)))

	if !strings.Contains(got, "CHURNS") {
		t.Errorf("Text() = %q, want CHURNS in caps", got)
	}
	if !strings.Contains(got, "unknown") || strings.Contains(got, "UNKNOWN") {
		t.Errorf("Text() = %q, want unknown in lower case", got)
	}
}

func TestTextCollapsesARepeatedReasonToSame(t *testing.T) {
	// Flux and Helm reach the same answer for the same reason. Printing the
	// sentence twice makes the reader compare two identical lines to find out
	// they are identical.
	got := text(t, chartWithVerdicts(verdicts(
		[3]string{"argocd", "churns", "every sync, forever"},
		[3]string{"flux", "unknown", "chart calls `lookup` (a.tpl:9) — may guard this value"},
		[3]string{"helm", "unknown", "chart calls `lookup` (a.tpl:9) — may guard this value"},
	)))

	if strings.Count(got, "may guard this value") != 1 {
		t.Errorf("Text() = %q, want the repeated reason written once", got)
	}
	if !strings.Contains(got, "same") {
		t.Errorf("Text() = %q, want the repeat shown as `same`", got)
	}
}

func TestTextNamesTheChartDefectWhenNothingCouldStabiliseTheValue(t *testing.T) {
	// This is the whole point of three-engine verdicts: it tells the reader
	// whether to file an upstream issue or add an ignoreDifferences block.
	got := text(t, chartWithVerdicts(verdicts(
		[3]string{"argocd", "churns", "every sync, forever"},
		[3]string{"flux", "churns", "on every chart or values change"},
		[3]string{"helm", "churns", "on every `helm upgrade`"},
	)))

	if !strings.Contains(got, "No `lookup` anywhere") {
		t.Errorf("Text() = %q, want the sound reasoning stated", got)
	}
	if !strings.Contains(got, "upstream") {
		t.Errorf("Text() = %q, want the reader pointed upstream", got)
	}
}

func TestTextDoesNotClaimAChartDefectFromArgoCDAlone(t *testing.T) {
	// ArgoCD churning says nothing about whether a lookup could stabilise the
	// value - its repo-server has no cluster access either way. Only a sound
	// (inferred, not observed) CHURNS from a lookup-resolving engine does.
	got := text(t, chartWithVerdicts(verdicts(
		[3]string{"argocd", "churns", "every sync, forever"},
	)))

	if strings.Contains(got, "No `lookup` anywhere") {
		t.Errorf("Text() = %q, want no chart-defect claim from ArgoCD alone", got)
	}
}

func TestTextShowsNoVerdictBlockWhenThereAreNone(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{finding("t.yaml", "c", ".data.k")}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "CHURNS") {
		t.Errorf("Text() = %q, want no verdict rows", got)
	}
}

func TestTextEmitsOneRemediationBlockForTheWholeRun(t *testing.T) {
	// "so you paste once, not N times" - the block is per run, not per chart
	// and not per finding.
	got := text(t, Report{
		Charts: []Chart{
			{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "home-creds", ".data.key")}},
			{Name: "lab", Findings: []check.Finding{finding("lab/templates/db.yaml", "lab-creds", ".data.password")}},
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if n := strings.Count(got, "ignoreDifferences"); n != 1 {
		t.Errorf("Text() emitted %d remediation blocks, want 1:\n%s", n, got)
	}
	for _, want := range []string{"home-creds", "lab-creds", "RespectIgnoreDifferences=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, want it to contain %q", got, want)
		}
	}
	// An exit-code line follows the report, and must not read as part of the
	// YAML the user is about to paste.
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("Text() = %q, want a blank line after the block", got)
	}
}

func TestRemediationCarriesFieldsTheDisplayElided(t *testing.T) {
	// The display caps an object at a handful of fields. The block below it is
	// what the user pastes, and a capped block would fail to stop the churn it
	// claims to stop.
	var fields []string
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		fields = append(fields, ".data."+k)
	}
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "big", fields...)}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "more") {
		t.Fatalf("Text() = %q, want the display to have elided something", got)
	}
	if !strings.Contains(got, "/data/h") {
		t.Errorf("Text() = %q, want the elided field still present in the pasteable block", got)
	}
}

func TestNoRemediationBlockWhenNothingChurns(t *testing.T) {
	got := text(t, Report{Charts: []Chart{clean("home")}, Helm: "4.2.4", Rounds: 2})

	if strings.Contains(got, "ignoreDifferences") {
		t.Errorf("Text() = %q, want no remediation for a clean run", got)
	}
}

func TestRemediationBlockIsIndentedWithTheRestOfTheOutput(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "creds", ".data.key")}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	for line := range strings.SplitSeq(strings.TrimRight(got, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "  ") {
			t.Errorf("line %q is not indented; Text() = %q", line, got)
		}
	}
}

func TestRemediationSaysWhichDiffModeItsPointersAssume(t *testing.T) {
	// Under ServerSideDiff=true the ignore normalizer never touches the
	// rendered config at all - pointers must describe the API server's dry-run
	// output, which two `helm template` runs cannot see. The same block can be
	// correct in one ArgoCD install and inert in another, so idem says which
	// one it computed for rather than implying it works everywhere.
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "creds", ".data.key")}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "ServerSideDiff") {
		t.Errorf("Text() = %q, want the diff-mode caveat", got)
	}
}

func suppressed(name, pointer, file string, selfHeal, respected bool) delivery.Suppressed {
	return delivery.Suppressed{
		Finding: finding("home/templates/s.yaml", name, ".data.key"),
		By: delivery.Rule{
			Kind: "Secret", Name: name, Pointers: []string{pointer},
			File: file, SelfHeal: selfHeal, Respected: respected,
		},
	}
}

func TestTextListsAlreadySuppressedFindingsSeparately(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/home.yaml", true, true)},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "already suppressed") {
		t.Errorf("Text() = %q, want a section for what is already handled", got)
	}
	if !strings.Contains(got, "Secret/creds") {
		t.Errorf("Text() = %q, want the object named", got)
	}
}

func TestTextNamesTheManifestThatSuppressedAFinding(t *testing.T) {
	// A suppression that changes what idem says must be traceable, or the
	// reader cannot check the reasoning.
	got := text(t, Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/home.yaml", true, true)},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "deployment/apps/home.yaml") {
		t.Errorf("Text() = %q, want the manifest named", got)
	}
}

func TestTextFlagsASuppressionThatSelfHealWillUndo(t *testing.T) {
	// ignoreDifferences without RespectIgnoreDifferences hides the diff while
	// selfHeal re-applies the object anyway. The user believes this is handled
	// and it is not - which is worth more than the original finding.
	got := text(t, Report{
		Charts: []Chart{{
			Name:       "lab",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/lab.yaml", true, false)},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "selfHeal") {
		t.Errorf("Text() = %q, want the broken suppression called out", got)
	}
	if !strings.Contains(got, "RespectIgnoreDifferences=true") {
		t.Errorf("Text() = %q, want the one-line fix", got)
	}
}

func TestAWorkingSuppressionIsNotFlaggedAsBroken(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/home.yaml", true, true)},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "selfHeal will re-apply") {
		t.Errorf("Text() = %q, want no warning - RespectIgnoreDifferences is set", got)
	}
}

func TestASuppressionWithoutSelfHealIsNotFlagged(t *testing.T) {
	// Without selfHeal nothing re-applies behind the user's back, so the
	// missing sync option costs them nothing.
	got := text(t, Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/home.yaml", false, false)},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "selfHeal will re-apply") {
		t.Errorf("Text() = %q, want no warning - nothing re-applies", got)
	}
}

func TestASuppressedFindingStillCountsForNow(t *testing.T) {
	// Whether a matched suppression should leave the churn count, and so
	// change --strict's exit code, is deliberately still open. Until then it
	// is shown but counted, because silently dropping it would decide the
	// question by accident.
	r := Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/home.yaml", true, true)},
		}},
		Helm: "4.2.4", Rounds: 2,
	}

	if got := r.Churning(); got != 1 {
		t.Errorf("Churning() = %d, want 1", got)
	}
}

func TestProvenanceNamesTheDeliveryConfigItRead(t *testing.T) {
	// idem already says which helm and how many rounds. Reading files outside
	// the chart directory must be just as visible.
	got := text(t, Report{
		Charts:   []Chart{clean("home")},
		Helm:     "4.2.4",
		Rounds:   2,
		Delivery: []string{"deployment/apps/home.yaml", "deployment/apps/lab.yaml"},
	})

	if !strings.Contains(got, "delivery config") {
		t.Errorf("Text() = %q, want the delivery manifests acknowledged", got)
	}
}

func TestProvenanceSaysNothingWhenThereWasNoDeliveryConfig(t *testing.T) {
	got := text(t, Report{Charts: []Chart{clean("home")}, Helm: "4.2.4", Rounds: 2})

	if strings.Contains(got, "delivery config") {
		t.Errorf("Text() = %q, want no mention when none was found", got)
	}
}

func allThreeChurning() []engine.Verdict {
	return []engine.Verdict{
		{Engine: "argocd", Result: engine.Churns, Because: "every sync, forever", Observed: true},
		{Engine: "flux", Result: engine.Churns, Because: "on every chart or values change"},
		{Engine: "helm", Result: engine.Churns, Because: "on every `helm upgrade`"},
	}
}

func chartWithEngines(vs []engine.Verdict, show []string) Report {
	return Report{
		Charts: []Chart{{
			Name:     "home",
			Findings: []check.Finding{finding("home/templates/s.yaml", "creds", ".data.password")},
			Verdicts: vs,
		}},
		Engines: show,
		Helm:    "4.2.4", Rounds: 2,
	}
}

func TestTextShowsOnlyTheSelectedEngines(t *testing.T) {
	got := text(t, chartWithEngines(allThreeChurning(), []string{"argocd"}))

	if !strings.Contains(got, "every sync, forever") {
		t.Errorf("Text() = %q, want the argocd row", got)
	}
	if strings.Contains(got, "helm upgrade") {
		t.Errorf("Text() = %q, want no helm row when only argocd was asked for", got)
	}
}

func TestTheChartDefectNoteSurvivesNarrowingToOneEngine(t *testing.T) {
	// The whole reason all three are computed even when one is shown. "No
	// lookup anywhere, so this is a chart bug" is only reachable from an
	// engine that DOES resolve lookup - and it is the single most useful thing
	// idem says. Narrowing the display must not silently discard it.
	got := text(t, chartWithEngines(allThreeChurning(), []string{"argocd"}))

	if !strings.Contains(got, "No `lookup` anywhere") {
		t.Errorf("Text() = %q, want the chart-defect conclusion kept", got)
	}
}

func TestAnEmptyEngineSelectionShowsEveryVerdict(t *testing.T) {
	got := text(t, chartWithEngines(allThreeChurning(), nil))

	for _, want := range []string{"argocd", "flux", "helm"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, want the %s row", got, want)
		}
	}
}

func potentialUse(file string, line int, fn string) analyze.Use {
	return analyze.Use{Function: fn, File: file, Line: line, Call: true}
}

func TestTextListsPotentialFindingsInTheirOwnSection(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{
			Name:      "lab",
			Potential: []analyze.Use{potentialUse("lab/templates/registry.yaml", 22, "genSelfSignedCert")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "potential") {
		t.Errorf("Text() = %q, want a potential section", got)
	}
	if !strings.Contains(got, "not counted, not fatal") {
		t.Errorf("Text() = %q, want the section to disclaim itself", got)
	}
	for _, want := range []string{"lab/templates/registry.yaml:22", "genSelfSignedCert"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, want %q", got, want)
		}
	}
}

func TestPotentialFindingsAreNeverCounted(t *testing.T) {
	// A static warning is sometimes wrong - a pin may be perfectly sound - and
	// a tool that cries wolf about the potential case teaches you to distrust
	// it about the observed one.
	r := Report{
		Charts: []Chart{{
			Name:      "lab",
			Potential: []analyze.Use{potentialUse("lab/templates/s.yaml", 3, "randAlphaNum")},
		}},
		Helm: "4.2.4", Rounds: 2,
	}

	if got := r.Churning(); got != 0 {
		t.Errorf("Churning() = %d, want 0 - a potential finding is a warning, not a fact", got)
	}
	if !strings.Contains(text(t, r), "renders consistently") {
		t.Errorf("Text() = %q, want the clean verdict kept", text(t, r))
	}
}

func TestPotentialClaimsItDidNotFireOnlyWhenNothingChurned(t *testing.T) {
	// idem cannot attribute an observed difference to a particular function,
	// so on a chart that DID churn it must not claim this one stayed quiet.
	churning := text(t, Report{
		Charts: []Chart{{
			Name:      "lab",
			Findings:  []check.Finding{finding("lab/templates/s.yaml", "creds", ".data.password")},
			Potential: []analyze.Use{potentialUse("lab/templates/s.yaml", 3, "randAlphaNum")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(churning, "did not fire") {
		t.Errorf("Text() = %q, want no claim about which function fired", churning)
	}

	clean := text(t, Report{
		Charts: []Chart{{
			Name:      "lab",
			Potential: []analyze.Use{potentialUse("lab/templates/s.yaml", 3, "randAlphaNum")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(clean, "did not fire") {
		t.Errorf("Text() = %q, want the pin noted on a clean render", clean)
	}
}

func TestPotentialNamesWhyTheFunctionIsFlagged(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{
			Name:      "lab",
			Potential: []analyze.Use{potentialUse("lab/templates/s.yaml", 3, "genPrivateKey")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "new key material") {
		t.Errorf("Text() = %q, want the reason it is flagged", got)
	}
}

func TestNoPotentialSectionWhenThereIsNothingToWarnAbout(t *testing.T) {
	got := text(t, Report{Charts: []Chart{clean("home")}, Helm: "4.2.4", Rounds: 2})

	if strings.Contains(got, "potential") {
		t.Errorf("Text() = %q, want no empty section", got)
	}
}

func TestPotentialSaysWhichChartEachWarningCameFrom(t *testing.T) {
	// The paths are chart-relative, so "templates/job.yaml:351" on its own
	// names a file in some chart the reader then has to go and find.
	got := text(t, Report{
		Charts: []Chart{
			{Name: "home", Potential: []analyze.Use{potentialUse("templates/job.yaml", 351, "now")}},
			{Name: "lab", Potential: []analyze.Use{potentialUse("templates/job.yaml", 12, "bcrypt")}},
		},
		Helm: "4.2.4", Rounds: 2,
	})

	for _, want := range []string{"home", "lab"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, want the chart %q named", got, want)
		}
	}
}

func TestPotentialCapsTheWarningsShownPerChart(t *testing.T) {
	// A bitnami-style common library names half a dozen of these in one
	// helper. Uncapped, one chart can bury the findings above it.
	var uses []analyze.Use
	for i, fn := range []string{"randAlpha", "randNumeric", "randAscii", "shuffle", "randAlphaNum", "genCA", "now"} {
		uses = append(uses, potentialUse("templates/s.tpl", i+1, fn))
	}
	got := text(t, Report{
		Charts: []Chart{{Name: "lab", Potential: uses}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "more") {
		t.Errorf("Text() = %q, want the surplus counted rather than dumped", got)
	}
}

func TestPotentialLinesStayReadablyNarrow(t *testing.T) {
	// Column alignment pads to the widest cell, so a deeply vendored subchart
	// path in one row stretches every other row with it.
	got := text(t, Report{
		Charts: []Chart{{
			Name: "lab",
			Potential: []analyze.Use{
				potentialUse("gitea/charts/postgresql-ha/charts/common/templates/_secrets.tpl", 132, "randAlpha"),
				potentialUse("templates/s.yaml", 3, "now"),
			},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	for line := range strings.SplitSeq(got, "\n") {
		if len(line) > 120 {
			t.Errorf("line is %d chars, want under 120:\n%s", len(line), line)
		}
	}
}

func TestTextNamesWhatAChurningSecretCosts(t *testing.T) {
	// "rolls 2 Deployments" and "silent — no checksum" are the difference
	// between an annoyance and a credential drifting from its database.
	rolls := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{
			secretFinding("creds", ".data.password"),
			workload("Deployment", "api", checksumPath),
			workload("Deployment", "ui", checksumPath),
		}}},
		Helm: "4.2.4", Rounds: 2,
	})
	if !strings.Contains(rolls, "rolls 2 Deployments") {
		t.Errorf("Text() = %q, want the rollout counted", rolls)
	}

	silent := text(t, Report{
		Charts: []Chart{{Name: "lab", Findings: []check.Finding{secretFinding("pg", ".data.password")}}},
		Helm:   "4.2.4", Rounds: 2,
	})
	if !strings.Contains(silent, "silent — no checksum") {
		t.Errorf("Text() = %q, want the silent case named", silent)
	}
}

func changed(name string, findings ...check.Finding) Chart {
	return Chart{Name: name, Changed: true, Findings: findings}
}

func untouched(name string, findings ...check.Finding) Chart {
	return Chart{Name: name, Findings: findings}
}

func TestTheRatchetShowsOnlyChartsChangedSinceTheRevision(t *testing.T) {
	got := text(t, Report{
		Since: "main",
		Charts: []Chart{
			changed("home", secretFinding("home-creds", ".data.password")),
			untouched("lab", secretFinding("lab-creds", ".data.password")),
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "home-creds") {
		t.Errorf("Text() = %q, want the changed chart's finding", got)
	}
	if strings.Contains(got, "lab-creds") {
		t.Errorf("Text() = %q, want the pre-existing finding hidden", got)
	}
}

func TestTheRatchetCountsWhatItIsHiding(t *testing.T) {
	// Nothing is stored and nothing is suppressed: dropping the flag always
	// shows everything, and the count says how much that would be.
	got := text(t, Report{
		Since: "main",
		Charts: []Chart{
			changed("home", secretFinding("home-creds", ".data.password")),
			untouched("lab", secretFinding("a", ".data.p"), secretFinding("b", ".data.p")),
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "2 pre-existing findings not shown") {
		t.Errorf("Text() = %q, want the hidden findings counted", got)
	}
	if !strings.Contains(got, "drop the flag") {
		t.Errorf("Text() = %q, want the way to see them", got)
	}
}

func TestTheRatchetVerdictNamesTheRevision(t *testing.T) {
	got := text(t, Report{
		Since: "main",
		Charts: []Chart{
			changed("home", secretFinding("creds", ".data.password")),
			changed("ops"),
			untouched("lab"),
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "1 of the 2 charts changed since main will churn under ArgoCD") {
		t.Errorf("Text() = %q, want the ratchet's own sentence", got)
	}
}

func TestTheRatchetGatesOnlyOnWhatChanged(t *testing.T) {
	// A permanently red pipeline gets deleted rather than fixed. --strict must
	// fail on this branch's work, not on the estate's history.
	r := Report{
		Since: "main",
		Charts: []Chart{
			changed("home"),
			untouched("lab", secretFinding("creds", ".data.password")),
		},
		Helm: "4.2.4", Rounds: 2,
	}

	if got := r.Churning(); got != 0 {
		t.Errorf("Churning() = %d, want 0 - nothing this branch touched churns", got)
	}
}

func TestAPreExistingUnrenderableChartDoesNotFailTheRatchet(t *testing.T) {
	// Otherwise an estate with one broken chart can never adopt the flag, and
	// that is the situation the ratchet exists for.
	r := Report{
		Since:  "main",
		Charts: []Chart{changed("home"), {Name: "lab", Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	}

	if got := r.Unevaluable(); got != 0 {
		t.Errorf("Unevaluable() = %d, want 0 - that chart was already broken", got)
	}
}

func TestAChartThisBranchBrokeStillFailsTheRatchet(t *testing.T) {
	r := Report{
		Since:  "main",
		Charts: []Chart{{Name: "home", Changed: true, Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	}

	if got := r.Unevaluable(); got != 1 {
		t.Errorf("Unevaluable() = %d, want 1", got)
	}
}

func TestWithoutTheRatchetEverythingIsShown(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{untouched("lab", secretFinding("creds", ".data.password"))},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "creds") {
		t.Errorf("Text() = %q, want every finding when no revision was given", got)
	}
	if strings.Contains(got, "pre-existing") {
		t.Errorf("Text() = %q, want no ratchet wording", got)
	}
}

func TestTheRatchetSaysPlainlyWhenNothingChanged(t *testing.T) {
	// "All 0 charts render consistently" is true and useless; the reader wants
	// to know the gate had nothing to look at.
	got := text(t, Report{
		Since:  "main",
		Charts: []Chart{untouched("lab", secretFinding("creds", ".data.p"))},
		Helm:   "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "All 0 charts") {
		t.Errorf("Text() = %q, want no vacuous count", got)
	}
	if !strings.Contains(got, "No charts changed since main") {
		t.Errorf("Text() = %q, want it said plainly", got)
	}
}
