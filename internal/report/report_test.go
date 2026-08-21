package report

import (
	"errors"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/check"
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
