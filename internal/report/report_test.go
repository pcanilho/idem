package report

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/delivery"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/doctor"
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

// The caveat used to print on every ArgoCD block, saying pointers "must
// describe the API server's dry-run output, which idem cannot see" - which read
// as "this block may not work" on a block that works fine.
//
// Checked against gitops-engine `pkg/diff/diff.go` rather than recalled: under
// server-side diff the ignore normalizer IS applied, to both the SSA dry-run
// result and the live object; only the pre-processing pass is skipped, via
// WithSkipFullNormalize(true). So a pointer at a field the chart renders still
// addresses it. Nothing to warn about, and a caveat on every block is a caveat
// nobody reads by the third one.
func TestNoDiffModeCaveatWhereTheModeChangesNothing(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "creds", ".data.key")}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "ignoreDifferences") {
		t.Fatalf("Text() = %q, want the ArgoCD block", got)
	}
	if strings.Contains(got, "ServerSideDiff") {
		t.Errorf("Text() = %q, want no caveat on a block server-side diff does not change", got)
	}
}

// `/stringData/KEY` is the one pointer that genuinely differs by diff mode.
//
// It exists to stop selfHeal overwriting the value on the RespectIgnoreDifferences
// sync path, which applies pointers to the raw target - the only place
// stringData still exists. The API server never stores it, so under server-side
// diff the predicted live object has only `data` and this pointer addresses
// nothing. Silently: `AllowMissingPathOnRemove` is ArgoCD's behaviour too.
func TestTheDiffModeCaveatAppearsOnTheOnePointerItChanges(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{finding("home/templates/s.yaml", "creds", ".stringData.key")}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "/stringData/key") {
		t.Fatalf("Text() = %q, want the stringData pointer in the block", got)
	}
	if !strings.Contains(got, "ServerSideDiff") {
		t.Errorf("Text() = %q, want the caveat where the mode changes the answer", got)
	}
}

// When a manifest states the mode, hedging about it is idem declining to read
// something in front of it. The hedge is right only while the answer is
// genuinely unknown - and it usually is, because the mode can also be set
// cluster-wide in argocd-cmd-params-cm, which is in no manifest idem reads.
func TestTheDiffModeCaveatIsDefiniteWhenTheManifestStatesTheMode(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{
			Name:           "home",
			ServerSideDiff: true,
			Findings:       []check.Finding{finding("home/templates/s.yaml", "creds", ".stringData.key")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "This Application sets ServerSideDiff=true") {
		t.Errorf("Text() = %q, want the caveat to state what the manifest says", got)
	}
	if strings.Contains(got, "path this install is on") {
		t.Errorf("Text() = %q, want no hedge about something idem can read", got)
	}
}

func TestTheDiffModeCaveatStaysHedgedWhenNoManifestStatesTheMode(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{
			Name:     "home",
			Findings: []check.Finding{finding("home/templates/s.yaml", "creds", ".stringData.key")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "path this install is on") {
		t.Errorf("Text() = %q, want the hedge - nothing idem read says which mode", got)
	}
}

// The definite sentence is singular - "This Application sets ServerSideDiff=true"
// - and it is printed once, beside a pointer belonging to one object from one
// Application. Deciding it with ANY over the whole run meant a second, unrelated
// chart could make idem state, as a fact, the opposite of what it read from the
// manifest the reader is about to edit.
func TestTheDiffModeCaveatHedgesWhenOnlySomeApplicationsStateTheMode(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{
			{
				Name:     "alpha",
				Findings: []check.Finding{finding("alpha/templates/s.yaml", "alpha-secret", ".stringData.key")},
			},
			{
				Name:           "beta",
				ServerSideDiff: true,
				Findings:       []check.Finding{finding("beta/templates/s.yaml", "beta-secret", ".data.key")},
			},
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "This Application sets ServerSideDiff=true") {
		t.Errorf("Text() = %q, want no claim about a mode alpha's Application does not set", got)
	}
	if !strings.Contains(got, "path this install is on") {
		t.Errorf("Text() = %q, want the hedge while the run disagrees with itself", got)
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

func TestAWorkingSuppressionLeavesTheChurnCount(t *testing.T) {
	// Settled 2026-08-22 against six comparable tools: not one of golangci-lint,
	// ESLint, Trivy, Semgrep, Checkov or Snyk lets a suppressed finding reach
	// the exit code. The finding is still printed - idem follows Checkov in
	// keeping it visible on its own ladder - but a chart whose churn the user
	// has genuinely handled must be able to turn a gate green, or the gate is
	// the kind nobody keeps.
	r := Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/home.yaml", true, true)},
		}},
		Helm: "4.2.4", Rounds: 2,
	}

	if got := r.Churning(); got != 0 {
		t.Errorf("Churning() = %d, want 0 - the delivery config covers it", got)
	}
	if got := text(t, r); !strings.Contains(got, "already suppressed") {
		t.Errorf("Text() = %q, want the finding still shown", got)
	}
}

func TestASuppressionSelfHealWillUndoStillCounts(t *testing.T) {
	// The one case where suppression is not suppression: the diff is hidden
	// and the object is re-applied anyway. Dropping this from the count would
	// hide churn the user is most confident is handled.
	r := Report{
		Charts: []Chart{{
			Name:       "lab",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/lab.yaml", true, false)},
		}},
		Helm: "4.2.4", Rounds: 2,
	}

	if got := r.Churning(); got != 1 {
		t.Errorf("Churning() = %d, want 1 - selfHeal re-applies it", got)
	}
}

func TestTheVerdictSentenceCreditsTheDeliveryConfigRatherThanClaimingCleanliness(t *testing.T) {
	// The chart DOES render inconsistently. Saying it "renders consistently"
	// because a rule covers it would be false, and would make the suppressed
	// section above read as decoration.
	got := text(t, Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "deployment/apps/home.yaml", true, true)},
		}, clean("lab")},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "render consistently") {
		t.Errorf("Text() = %q, want no claim that the charts render consistently", got)
	}
	if !strings.Contains(got, "delivery config") {
		t.Errorf("Text() = %q, want the config credited for the quiet", got)
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

// `-o github` resolved this path correctly while `-o text` and `-o json` printed
// a chart-relative one, so the same finding named two different files depending
// on how you asked for it. Grouping the rows under a chart heading told the
// reader WHICH chart, which is not the same as handing them a path they can
// open - and Phase 1 already decided that printing an openable path wins.
func TestAPotentialFindingPrintsAPathTheReaderCanOpen(t *testing.T) {
	root := repoWith(t, "charts/home/templates/_helpers.tpl")

	got := text(t, Report{
		Root: root,
		Charts: []Chart{{
			Name: "home", RepoDir: "charts/home",
			Potential: []analyze.Use{potentialUse("templates/_helpers.tpl", 12, "randAlphaNum")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "charts/home/templates/_helpers.tpl:12") {
		t.Errorf("Text() = %q, want the path resolved against the repository", got)
	}
}

// A subchart vendored as a .tgz produces a path that resolves to somewhere
// nobody can open. Observed findings print what helm gave rather than dropping
// it, and this section has to make the same choice: a path idem cannot place is
// still the only name the reader has for that file.
func TestAPotentialFindingThatCannotBePlacedPrintsWhatHelmGave(t *testing.T) {
	root := repoWith(t, "charts/home/Chart.yaml")

	got := text(t, Report{
		Root: root,
		Charts: []Chart{{
			Name: "home", RepoDir: "charts/home",
			Potential: []analyze.Use{potentialUse("charts/ollama/templates/common.yaml", 7, "randAlphaNum")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "charts/ollama/templates/common.yaml:7") {
		t.Errorf("Text() = %q, want the unresolvable path printed as helm gave it", got)
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

	if strings.Contains(churning, "Nothing differed") {
		t.Errorf("Text() = %q, want no claim about which function fired", churning)
	}

	clean := text(t, Report{
		Charts: []Chart{{
			Name:      "lab",
			Potential: []analyze.Use{potentialUse("lab/templates/s.yaml", 3, "randAlphaNum")},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(clean, "Nothing differed") {
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
	// Measured in RUNES, not bytes. The report is full of —, · and … at three
	// bytes each, so a byte count fails at about 113 visible columns and goes
	// red for a reason that has nothing to do with column width. Its sibling
	// TestTheProvenanceLineWrapsRatherThanRunningOn already counts runes.
	got := text(t, Report{
		Charts: []Chart{{
			Name: "lab",
			Potential: []analyze.Use{
				potentialUse("lab/charts/common/charts/postgresql/templates/secrets.yaml", 351, "genSelfSignedCert"),
			},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	// Scoped to the potential block: this test is named for that section, and
	// scanning the whole report meant unrelated prose could trip it.
	_, block, found := strings.Cut(got, "not counted, not fatal")
	if !found {
		t.Fatalf("Text() = %q, want a potential section", got)
	}
	for line := range strings.SplitSeq(block, "\n") {
		if n := len([]rune(line)); n > 120 {
			t.Errorf("potential line is %d columns, want at most 120:\n%s", n, line)
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

func TestAPreExistingUnrenderableChartStillFailsTheRatchet(t *testing.T) {
	// Settled 2026-08-22. golangci-lint special-cases exactly this in its own
	// diff processor - "Never hide typechecking errors" - and ESLint, mypy and
	// ruff all make an analysis failure unsuppressable by construction. The
	// ratchet promises that nothing you changed introduced a problem; a chart
	// it never checked makes that promise false rather than incomplete.
	r := Report{
		Since:  "main",
		Charts: []Chart{changed("home"), {Name: "lab", Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	}

	if got := r.Unevaluable(); got != 1 {
		t.Errorf("Unevaluable() = %d, want 1 - the ratchet filters findings, not coverage gaps", got)
	}
}

func TestTheRatchetShowsAChartItCouldNotRenderEvenWhenUnchanged(t *testing.T) {
	// Counting it without printing it would leave a non-zero exit with no
	// visible cause.
	got := text(t, Report{
		Since:  "main",
		Charts: []Chart{changed("home"), {Name: "lab", Err: errors.New("no repository definition")}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "lab") || !strings.Contains(got, "no repository definition") {
		t.Errorf("Text() = %q, want the unrenderable chart named with its reason", got)
	}
}

func TestAnUnrenderableChartIsNotAlsoCountedAsHeldBack(t *testing.T) {
	// It is shown, so reporting it as one of the "pre-existing findings not
	// shown" would count the same chart twice and in opposite directions.
	got := text(t, Report{
		Since:  "main",
		Charts: []Chart{changed("home"), {Name: "lab", Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "1 pre-existing finding not shown") {
		t.Errorf("Text() = %q, want the unrenderable chart not counted as hidden", got)
	}
}

func TestARatchetedRunThatChangedNothingStillReportsWhatWouldNotRender(t *testing.T) {
	// "No charts changed since main." with exit 2 and no other output is the
	// worst possible pairing: a failing gate whose reason is invisible.
	r := Report{
		Since:  "main",
		Charts: []Chart{untouched("home", secretFinding("creds", ".data.p")), {Name: "lab", Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	}

	if got := r.Unevaluable(); got != 1 {
		t.Errorf("Unevaluable() = %d, want 1", got)
	}
	if got := text(t, r); !strings.Contains(got, "could not be rendered") {
		t.Errorf("Text() = %q, want the sentence to say why the run is not clean", got)
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

// A chart can render identically under `helm template` and still differ with
// `lookup` resolving against a real cluster. ArgoCD is fine; Flux and Helm are
// not. Reporting that as a clean run hides observed churn.

func serverOnly(name string) Chart {
	return Chart{
		Name:       name,
		Dir:        "./" + name,
		ServerOnly: []check.Finding{finding("home/templates/secret.yaml", "creds", ".data.token")},
		Verdicts: []engine.Verdict{
			{Engine: "argocd", Result: engine.Stable, Because: "renders identically without cluster access (observed)", Observed: true},
			{Engine: "flux", Result: engine.Churns, Because: "on every chart or values change — differs even with lookup resolved (observed)", Observed: true},
		},
	}
}

func TestTextShowsChurnObservedOnlyWithLookupResolved(t *testing.T) {
	got := text(t, Report{Charts: []Chart{serverOnly("home")}, Helm: "4.2.4", Rounds: 2, Cluster: true})

	if !strings.Contains(got, "creds") {
		t.Errorf("Text() = %q, want the differing object named", got)
	}
	if !strings.Contains(got, "lookup") {
		t.Errorf("Text() = %q, want it to say the difference needs lookup resolved", got)
	}
}

func TestACleanClientRenderIsNotACleanRunWhenTheClusterConditionDiffers(t *testing.T) {
	got := text(t, Report{Charts: []Chart{serverOnly("home")}, Helm: "4.2.4", Rounds: 2, Cluster: true})

	if strings.Contains(got, "✓") {
		t.Errorf("Text() = %q, want no clean tick - a difference was observed", got)
	}
}

func TestChurnUnderLookupIsCountedApartFromArgoCDChurn(t *testing.T) {
	// The verdict sentence is framed on ArgoCD, and ArgoCD renders exactly the
	// condition that was identical. Folding this into that count would state a
	// falsehood about the engine the sentence names.
	r := Report{Charts: []Chart{clean("lab"), serverOnly("home")}, Helm: "4.2.4", Rounds: 2, Cluster: true}

	if got := r.Churning(); got != 0 {
		t.Errorf("Churning() = %d, want 0 - nothing churns under ArgoCD here", got)
	}
	if got := r.ChurningWithLookup(); got != 1 {
		t.Errorf("ChurningWithLookup() = %d, want 1", got)
	}
}

func TestTheVerdictSentenceNamesTheEnginesThatWillChurn(t *testing.T) {
	got := text(t, Report{Charts: []Chart{clean("lab"), serverOnly("home")}, Helm: "4.2.4", Rounds: 2, Cluster: true})

	if !strings.Contains(got, "Flux") || !strings.Contains(got, "Helm") {
		t.Errorf("Text() = %q, want the sentence to name Flux and Helm", got)
	}
}

func TestTheRatchetHidesChurnUnderLookupToo(t *testing.T) {
	// Otherwise a pre-existing server-only difference leaks past a ratchet that
	// promised to report only what changed.
	r := Report{Charts: []Chart{serverOnly("home")}, Helm: "4.2.4", Rounds: 2, Since: "main", Cluster: true}

	if got := r.ChurningWithLookup(); got != 0 {
		t.Errorf("ChurningWithLookup() = %d, want 0 - the chart is out of scope", got)
	}
	if got := text(t, r); !strings.Contains(got, "1 pre-existing finding not shown") {
		t.Errorf("Text() = %q, want the hidden finding counted", got)
	}
}

// Which namespace a chart rendered into decides the displayed identity of
// every object it produced, and it used to come from the local kube context -
// invisible state that differs between a laptop and CI. Where it came from is
// exactly the kind of fact the provenance line exists for.

func namespaced(name, ns, from string) Chart {
	c := clean(name)
	c.Namespace, c.NamespaceFrom = ns, from
	return c
}

func TestTheProvenanceLineSaysWhichNamespaceWasRenderedInto(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{namespaced("home", "home", "deployment/apps/home.app.argo.yaml")},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "namespace home") {
		t.Errorf("Text() = %q, want the namespace named", got)
	}
	if !strings.Contains(got, "home.app.argo.yaml") {
		t.Errorf("Text() = %q, want the manifest that decided it", got)
	}
}

func TestTheProvenanceLineAdmitsWhenIdemChoseTheNamespaceItself(t *testing.T) {
	// Nothing claimed the chart, so idem picked one. Silently rendering into
	// "default" and displaying it as fact would read as something the
	// repository said.
	got := text(t, Report{Charts: []Chart{namespaced("home", "elsewhere", "")}, Helm: "4.2.4", Rounds: 2})

	if !strings.Contains(got, "namespace elsewhere (idem's own, nothing claims this chart)") {
		t.Errorf("Text() = %q, want the fallback owned up to", got)
	}
}

func TestChartsInDifferentNamespacesAreSummarisedNotListed(t *testing.T) {
	// One clause per namespace on a 16-chart estate is a wall. The namespace
	// is on every object's own line already.
	got := text(t, Report{
		Charts: []Chart{
			namespaced("home", "home", "apps/home.yaml"),
			namespaced("lab", "lab", "apps/lab.yaml"),
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "namespaces from delivery config") {
		t.Errorf("Text() = %q, want the mixed case summarised", got)
	}
}

func TestAnExplicitNamespaceFlagSaysItWasTheFlag(t *testing.T) {
	got := text(t, Report{Charts: []Chart{namespaced("home", "lab", NamespaceFromFlag)}, Helm: "4.2.4", Rounds: 2})

	if !strings.Contains(got, "namespace lab (--namespace)") {
		t.Errorf("Text() = %q, want the flag credited", got)
	}
}

func TestTheDeliveryConfigIsCreditedEvenWhenSomethingElseFailed(t *testing.T) {
	// The estate shape: nothing churns, four findings are covered, and six
	// charts would not render. "10 charts render consistently" is false about
	// the covered one, and dropping the clause because of an unrelated failure
	// would make the suppressed section unexplained.
	got := text(t, Report{
		Charts: []Chart{
			{Name: "home", Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "apps/home.yaml", true, true)}},
			clean("lab"),
			{Name: "broken", Err: errors.New("no repository definition")},
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "delivery config") {
		t.Errorf("Text() = %q, want the covered finding accounted for", got)
	}
	if strings.Contains(got, "2 charts render consistently") {
		t.Errorf("Text() = %q, want the covered chart not counted as consistent", got)
	}
}

func TestTheCoveredClauseAgreesWithTheFindingsNotTheCharts(t *testing.T) {
	// "4 findings in 1 chart is covered" is what keying the verb on the wrong
	// noun produces, and the two counts differ often.
	got := text(t, Report{
		Charts: []Chart{{
			Name: "home",
			Suppressed: []delivery.Suppressed{
				suppressed("creds", "/data/a", "apps/home.yaml", true, true),
				suppressed("creds", "/data/b", "apps/home.yaml", true, true),
			},
		}, {Name: "broken", Err: errors.New("no repository definition")}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "2 findings in 1 chart are covered") {
		t.Errorf("Text() = %q, want the verb to agree with the findings", got)
	}
}

func TestASuppressionSelfHealWillUndoIsNeverCreditedAsCovered(t *testing.T) {
	// It counts as churn AND would read as handled - the sentence would say
	// both "1 of 1 chart will churn" and "1 finding is covered by your
	// delivery config" about the same finding, which is the exact reassurance
	// the trap exists to deny.
	got := text(t, Report{
		Charts: []Chart{{
			Name:       "lab",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "apps/lab.yaml", true, false)},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "covered by your delivery config") {
		t.Errorf("Text() = %q, want no claim that this is covered", got)
	}
}

func TestTheSentenceCountsOnlyChartsItActuallyReportsOn(t *testing.T) {
	// The counts come from two different populations now: charts in ratchet
	// scope, and every chart for the ones that would not render. Subtracting
	// one from the other produces a negative count.
	got := text(t, Report{
		Since: "main",
		Charts: []Chart{
			changed("home"),
			{Name: "a", Err: errors.New("boom")},
			{Name: "b", Err: errors.New("boom")},
			{Name: "c", Err: errors.New("boom")},
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "-2 charts") {
		t.Errorf("Text() = %q, want no negative count", got)
	}
	if !strings.Contains(got, "1 chart renders consistently") {
		t.Errorf("Text() = %q, want the one in-scope chart counted", got)
	}
}

func TestTheSentenceSaysTheRatchetDoesNotHideRenderFailures(t *testing.T) {
	// Under a ratchet the two counts come from different populations: charts
	// in scope, and every chart for the ones that would not render. Without
	// saying so, "0 charts render consistently; 6 could not be rendered" reads
	// as six of the charts this branch touched.
	got := text(t, Report{
		Since:  "main",
		Charts: []Chart{changed("home"), {Name: "lab", Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "which the ratchet does not hide") {
		t.Errorf("Text() = %q, want the two populations distinguished", got)
	}
}

func TestWithoutARatchetTheRenderFailureClauseStaysPlain(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{clean("home"), {Name: "lab", Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "ratchet") {
		t.Errorf("Text() = %q, want no ratchet wording when no revision was given", got)
	}
}

// A chart that renders perfectly well given the values its delivery config
// supplies is not a broken chart. When those values come from a generator idem
// cannot expand, idem could not BUILD the release — a different statement from
// "this chart could not be rendered", and not the same exit code.

func unbuiltChart(name string, keys ...string) Chart {
	return Chart{
		Name:       name,
		Err:        errors.New(`execution error: .Values.cluster is required`),
		Unresolved: keys,
	}
}

func TestAReleaseIdemCouldNotBuildIsNotAnUnrenderableChart(t *testing.T) {
	r := Report{Charts: []Chart{unbuiltChart("agent", "cluster")}, Helm: "4.2.4", Rounds: 2}

	if got := r.Unevaluable(); got != 0 {
		t.Errorf("Unevaluable() = %d, want 0 - the chart was never the problem", got)
	}
	if got := r.Unconstructed(); got != 1 {
		t.Errorf("Unconstructed() = %d, want 1", got)
	}
}

func TestAnUnbuiltReleaseNamesTheValuesItLacked(t *testing.T) {
	// Without the keys the reader cannot tell an idem limitation from a chart
	// that genuinely will not render, and the two need opposite responses.
	got := text(t, Report{Charts: []Chart{unbuiltChart("agent", "cluster")}, Helm: "4.2.4", Rounds: 2})

	if !strings.Contains(got, "cluster") {
		t.Errorf("Text() = %q, want the missing value named", got)
	}
	if !strings.Contains(got, "could not be built") {
		t.Errorf("Text() = %q, want it stated as its own kind of gap", got)
	}
}

func TestAnUnbuiltReleaseIsNotReportedAsUnrenderable(t *testing.T) {
	got := text(t, Report{Charts: []Chart{unbuiltChart("agent", "cluster")}, Helm: "4.2.4", Rounds: 2})

	if strings.Contains(got, "could not be rendered") {
		t.Errorf("Text() = %q, want it kept apart from a real render failure", got)
	}
}

func TestTheVerdictSentenceCountsWhatCouldNotBeBuilt(t *testing.T) {
	// Reported without being counted anywhere is how a coverage gap becomes
	// invisible. It is not fatal, but it is not silent either.
	got := text(t, Report{
		Charts: []Chart{clean("app"), unbuiltChart("agent", "cluster")},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "1 could not be built") {
		t.Errorf("Text() = %q, want the gap in the sentence", got)
	}
}

func TestAChartThatFailedForItsOwnReasonsIsStillUnrenderable(t *testing.T) {
	// No unresolved values, so nothing excuses it: this is exit 2 as before.
	r := Report{
		Charts: []Chart{{Name: "app", Err: errors.New("no repository definition")}},
		Helm:   "4.2.4", Rounds: 2,
	}

	if got := r.Unevaluable(); got != 1 {
		t.Errorf("Unevaluable() = %d, want 1", got)
	}
	if got := r.Unconstructed(); got != 0 {
		t.Errorf("Unconstructed() = %d, want 0", got)
	}
}

func TestAReleaseThatRenderedDespiteMissingValuesSaysSo(t *testing.T) {
	// It rendered, so idem has something to report - but about a release that
	// is not the one deployed, and the reader has to know which.
	got := text(t, Report{
		Charts: []Chart{{Name: "app", Unresolved: []string{"cluster"}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "cluster") {
		t.Errorf("Text() = %q, want the values idem could not supply named", got)
	}
}

// Since idem started reporting churn only Flux and Helm see, it has been
// naming a problem and offering the fix for a different engine. Flux suppresses
// drift with driftDetection.ignore, not ignoreDifferences.

func churnsUnderFlux(name string, findings ...check.Finding) Chart {
	return Chart{
		Name:     name,
		Findings: findings,
		Verdicts: []engine.Verdict{
			{Engine: "argocd", Result: engine.Churns, Because: "every sync", Observed: true},
			{Engine: "flux", Result: engine.Churns, Because: "on every chart or values change"},
		},
	}
}

func TestAChartThatChurnsUnderFluxGetsTheFluxBlockToo(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{churnsUnderFlux("home", secretFinding("creds", ".data.password"))},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "driftDetection") {
		t.Errorf("Text() = %q, want the Flux fix as well", got)
	}
	if !strings.Contains(got, "ignoreDifferences") {
		t.Errorf("Text() = %q, want the ArgoCD fix still there", got)
	}
}

func TestAChartFluxHandlesGetsNoFluxBlock(t *testing.T) {
	// A lookup stabilises the value under Flux, so there is no Flux drift to
	// suppress and a block would tell the reader to configure away a problem
	// they do not have.
	got := text(t, Report{
		Charts: []Chart{{
			Name:     "home",
			Findings: []check.Finding{secretFinding("creds", ".data.password")},
			Verdicts: []engine.Verdict{
				{Engine: "argocd", Result: engine.Churns, Because: "every sync", Observed: true},
				{Engine: "flux", Result: engine.Stable, Because: "lookup resolves", Observed: true},
			},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "driftDetection") {
		t.Errorf("Text() = %q, want no Flux block - Flux is stable here", got)
	}
}

func TestChurnOnlyFluxSeesGetsTheFluxBlock(t *testing.T) {
	// The case that started this: ArgoCD is fine, Flux churns, and until now
	// idem emitted nothing at all to fix it.
	got := text(t, Report{
		Charts: []Chart{serverOnly("home")},
		Helm:   "4.2.4", Rounds: 2, Cluster: true,
	})

	if !strings.Contains(got, "driftDetection") {
		t.Errorf("Text() = %q, want the Flux fix", got)
	}
	if strings.Contains(got, "ignoreDifferences") {
		t.Errorf("Text() = %q, want no ArgoCD block - ArgoCD does not churn here", got)
	}
}

func TestTheFluxBlockIsNotShownWhenOnlyArgoCDWasAskedFor(t *testing.T) {
	got := text(t, Report{
		Charts:  []Chart{churnsUnderFlux("home", secretFinding("creds", ".data.password"))},
		Engines: []string{"argocd"},
		Helm:    "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "driftDetection") {
		t.Errorf("Text() = %q, want only the engine that was asked for", got)
	}
}

// The path idem prints is the one a reader is meant to open. `-o github`
// already resolves it against the repository; `-o text` and `-o json` print a
// chart-relative path that opens from nowhere, so the human format is the one
// you cannot click and the JSON a policy engine reads cannot locate the file
// either.

func TestTextPrintsAPathTheReaderCanOpen(t *testing.T) {
	root := repoWith(t, "charts/home/templates/secrets.yaml")
	f := secretFinding("creds", ".data.password")
	f.Source = "home/templates/secrets.yaml"

	got := text(t, Report{
		Root:   root,
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Findings: []check.Finding{f}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "charts/home/templates/secrets.yaml") {
		t.Errorf("Text() = %q, want the path resolved against the repository", got)
	}
}

func TestAPathThatCannotBeResolvedIsPrintedAsItCame(t *testing.T) {
	// A subchart vendored as a .tgz produces a path that resolves to nowhere.
	// idem prints what helm said rather than inventing a location — the same
	// reason `-o github` declines to annotate it.
	f := secretFinding("creds", ".data.password")
	f.Source = "home/charts/vendored/templates/x.yaml"

	got := text(t, Report{
		Root:   t.TempDir(),
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Findings: []check.Finding{f}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "home/charts/vendored/templates/x.yaml") {
		t.Errorf("Text() = %q, want the original source kept when it cannot be resolved", got)
	}
}

func TestTheDiffCountAgreesWithItsVerb(t *testing.T) {
	// "1 object differ" is what keying plural() on the noun alone produces,
	// and it has now gone wrong three times in this codebase.
	var one, two strings.Builder
	f := secretFinding("creds", ".data.password")

	if err := Diff(&one, []check.Finding{f}, "a.yaml", "b.yaml", maxFields); err != nil {
		t.Fatal(err)
	}
	if err := Diff(&two, []check.Finding{f, secretFinding("other", ".data.x")}, "a.yaml", "b.yaml", maxFields); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(one.String(), "1 object differs") {
		t.Errorf("Diff() = %q, want singular agreement", one.String())
	}
	if !strings.Contains(two.String(), "2 objects differ.") {
		t.Errorf("Diff() = %q, want plural agreement", two.String())
	}
}

// `maxFields` caps a finding at five fields and prints "… and N more" with no
// way to see the rest. A Secret whose whole .data regenerates is exactly the
// case idem exists for, and it is the case the cap hides.

func manyFields(name string, n int) check.Finding {
	fields := make([]string, 0, n)
	for i := range n {
		fields = append(fields, fmt.Sprintf(".data.key%d", i))
	}
	return finding("home/templates/secret.yaml", name, fields...)
}

func TestByDefaultAFindingIsCappedAndSaysSo(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{manyFields("creds", 9)}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "and 4 more") {
		t.Errorf("Text() = %q, want the elision counted", got)
	}
	if strings.Contains(got, ".data.key8") {
		t.Errorf("Text() = %q, want the tail held back by default", got)
	}
}

func TestVerboseExpandsEveryField(t *testing.T) {
	got := text(t, Report{
		Charts:  []Chart{{Name: "home", Findings: []check.Finding{manyFields("creds", 9)}}},
		Verbose: true,
		Helm:    "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, ".data.key8") {
		t.Errorf("Text() = %q, want every field", got)
	}
	if strings.Contains(got, "more") {
		t.Errorf("Text() = %q, want nothing elided", got)
	}
}

func TestVerboseExpandsThePotentialSectionToo(t *testing.T) {
	// Same cap, same problem: a chart calling nine non-deterministic functions
	// shows five and hides the rest.
	uses := make([]analyze.Use, 0, 9)
	for i := range 9 {
		uses = append(uses, analyze.Use{Function: "randAlphaNum", File: fmt.Sprintf("templates/t%d.yaml", i), Line: i + 1})
	}

	got := text(t, Report{
		Charts:  []Chart{{Name: "home", Potential: uses}},
		Verbose: true,
		Helm:    "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "templates/t8.yaml") {
		t.Errorf("Text() = %q, want every potential finding", got)
	}
}

func TestTheCleanRunSaysNothingAboutADefaultedNamespace(t *testing.T) {
	// The modal case is "nothing claims this chart", so this clause printed on
	// essentially every run — including the two-line success, where it is half
	// the output and none of the news. Interesting when a manifest or the user
	// decided it; noise when idem just picked the default.
	got := text(t, Report{
		Charts: []Chart{namespaced("home", DefaultNamespace, "")},
		Helm:   "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "namespace") {
		t.Errorf("Text() = %q, want no namespace clause when idem simply defaulted", got)
	}
}

func TestANamespaceSomeoneChoseIsStillReported(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{namespaced("home", "home", "apps/home.yaml")},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "namespace home") {
		t.Errorf("Text() = %q, want a namespace the repository chose still named", got)
	}
}

func TestANamespaceIdemChoseIsStillReportedWhenItIsNotTheDefault(t *testing.T) {
	// Only the boring default goes quiet. Anything else idem picked is still
	// worth saying, because the reader cannot infer it.
	got := text(t, Report{
		Charts: []Chart{namespaced("home", "somewhere-else", "")},
		Helm:   "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "somewhere-else") {
		t.Errorf("Text() = %q, want it named", got)
	}
}

func TestTheTwoFixBlocksAreNotSeparatedByADoubleBlank(t *testing.T) {
	// The ArgoCD block ends with a trailing blank so an exit-code line does not
	// read as part of the YAML; the Flux block then opens with its own. Two in
	// a row is a rendering seam, and it shows on the most-copied output there
	// is.
	got := text(t, Report{
		Charts: []Chart{churnsUnderFlux("home", secretFinding("creds", ".data.password"))},
		Helm:   "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "\n\n\n") {
		t.Errorf("Text() = %q, want no doubled blank line", got)
	}
}

// The observed finding above gets three sentences saying what will happen. A
// potential finding gets a function name, a reason and a line — and a newcomer
// cannot tell from that whether they are supposed to do anything.

func TestAPotentialFindingOnACleanChartSaysWhyItIsListed(t *testing.T) {
	got := text(t, Report{
		Charts: []Chart{{
			Name:      "home",
			Potential: []analyze.Use{{Function: "randAlphaNum", File: "templates/main.yaml", Line: 7}},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, "Nothing differed this render") {
		t.Errorf("Text() = %q, want the reader told what this section is", got)
	}
	if !strings.Contains(got, "stopped applying") {
		t.Errorf("Text() = %q, want it to say when these start mattering", got)
	}
}

func TestAPotentialFindingOnAChurningChartDoesNotClaimItStayedQuiet(t *testing.T) {
	// idem cannot attribute an observed difference to a particular function,
	// so on a chart that DID churn it must not say this one was innocent.
	got := text(t, Report{
		Charts: []Chart{{
			Name:      "home",
			Findings:  []check.Finding{secretFinding("creds", ".data.password")},
			Potential: []analyze.Use{{Function: "randAlphaNum", File: "templates/main.yaml", Line: 7}},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "Nothing differed this render") {
		t.Errorf("Text() = %q, want no claim of innocence on a chart that churned", got)
	}
	if !strings.Contains(got, "cannot say which") {
		t.Errorf("Text() = %q, want it to say why these are listed anyway", got)
	}
}

func TestTwoChartsWithTheSameNameAreToldApart(t *testing.T) {
	// The potential section groups by chart name, so two charts called `app`
	// in different directories produce two identical blocks — with
	// chart-relative paths inside them, so nothing distinguishes the two.
	got := text(t, Report{
		Charts: []Chart{
			{Name: "app", RepoDir: "a/app", Potential: []analyze.Use{potentialUse("templates/s.yaml", 5, "randAlphaNum")}},
			{Name: "app", RepoDir: "b/app", Potential: []analyze.Use{potentialUse("templates/s.yaml", 5, "randAlphaNum")}},
		},
		Helm: "4.2.4", Rounds: 2,
	})

	for _, want := range []string{"a/app", "b/app"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, want %s named so the two blocks can be told apart", got, want)
		}
	}
}

func TestAChartWithAUniqueNameIsNotDecorated(t *testing.T) {
	// Qualifying every heading with a path would be noise on the common case,
	// where the name already says which chart it is.
	got := text(t, Report{
		Charts: []Chart{
			{Name: "app", RepoDir: "charts/app", Potential: []analyze.Use{potentialUse("templates/s.yaml", 5, "randAlphaNum")}},
		},
		Helm: "4.2.4", Rounds: 2,
	})

	if strings.Contains(got, "charts/app") {
		t.Errorf("Text() = %q, want just the name when it is unambiguous", got)
	}
}

// Alignment is why this output has no box drawing and no emoji — columns are
// the whole readability strategy, and a line that wraps destroys them for
// every row beneath it.

func TestARewrittenValueIsTruncatedRatherThanWrapping(t *testing.T) {
	// The API server returns whole annotation maps. One of them measured 298
	// characters on a real cluster, which wraps three times on a normal
	// terminal and takes the column layout with it.
	long := map[string]any{}
	for i := range 12 {
		long[fmt.Sprintf("pv.kubernetes.io/some-fairly-long-annotation-%d", i)] = "yes"
	}

	got := text(t, Report{
		Charts: []Chart{{Name: "home", Rewrites: []doctor.Rewrite{{
			Object:  diff.ObjectRef{APIVersion: "v1", Kind: "Service", Name: "api"},
			Changes: []doctor.Change{{Path: path(".metadata.annotations"), Value: long}},
		}}}},
		Helm: "4.2.4", Rounds: 2,
	})

	for line := range strings.SplitSeq(got, "\n") {
		if len([]rune(line)) > 120 {
			t.Errorf("line is %d characters, want it truncated:\n  %s", len([]rune(line)), line)
		}
	}
	if !strings.Contains(got, "…") {
		t.Errorf("Text() = %q, want the truncation visible rather than silent", got)
	}
}

func TestVerboseShowsTheWholeRewrittenValue(t *testing.T) {
	long := strings.Repeat("x", 300)

	got := text(t, Report{
		Charts: []Chart{{Name: "home", Rewrites: []doctor.Rewrite{{
			Object:  diff.ObjectRef{APIVersion: "v1", Kind: "Service", Name: "api"},
			Changes: []doctor.Change{{Path: path(".metadata.annotations"), Value: long}},
		}}}},
		Verbose: true,
		Helm:    "4.2.4", Rounds: 2,
	})

	if !strings.Contains(got, long) {
		t.Error("Text() truncated the value even with -v, which is the flag for seeing all of it")
	}
}

func TestTheProvenanceLineWrapsRatherThanRunningOn(t *testing.T) {
	// Five clauses is a normal estate run, and it measured 199 characters.
	got := text(t, Report{
		Charts:  []Chart{namespaced("home", "home", "apps/home.yaml")},
		Helm:    "4.2.4",
		Rounds:  2,
		Cluster: true,
		Context: "some-fairly-long-context-name",
		Delivery: []string{
			"a.yaml", "b.yaml", "c.yaml", "d.yaml", "e.yaml",
			"f.yaml", "g.yaml", "h.yaml", "i.yaml", "j.yaml",
		},
	})

	for line := range strings.SplitSeq(got, "\n") {
		if len([]rune(line)) > 120 {
			t.Errorf("provenance line is %d characters, want it wrapped:\n  %s", len([]rune(line)), line)
		}
	}
}

func TestSuppressedFindingsNameTheirManifestOnceNotPerRow(t *testing.T) {
	// Object identity plus a JSON pointer plus a manifest path is 163
	// characters on a real estate, and the path is identical on every row —
	// it is the one cell that is not per-finding.
	got := text(t, Report{
		Charts: []Chart{{
			Name: "home",
			Suppressed: []delivery.Suppressed{
				suppressed("home-ollama", "/spec/template/metadata/annotations/checksum~1secrets", "deployment/apps/home.app.argo.yaml", true, true),
				suppressed("home-ollama-ui", "/spec/template/metadata/annotations/checksum~1secrets", "deployment/apps/home.app.argo.yaml", true, true),
			},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	if n := strings.Count(got, "deployment/apps/home.app.argo.yaml"); n != 1 {
		t.Errorf("the manifest is named %d times, want once", n)
	}
	for line := range strings.SplitSeq(got, "\n") {
		if len([]rune(line)) > 120 {
			t.Errorf("line is %d characters:\n  %s", len([]rune(line)), line)
		}
	}
}
