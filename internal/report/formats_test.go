package report

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/delivery"
)

func render(t *testing.T, r Report, f func(Report, io.Writer) error) string {
	t.Helper()
	var b strings.Builder
	if err := f(r, &b); err != nil {
		t.Fatalf("format error = %v", err)
	}
	return b.String()
}

func asJSON(t *testing.T, r Report) map[string]any {
	t.Helper()
	var b strings.Builder
	if err := r.JSON(&b); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("emitted JSON does not parse: %v\n%s", err, b.String())
	}
	return got
}

func churningReport() Report {
	return Report{
		Charts: []Chart{{
			Name: "home",
			Findings: []check.Finding{
				secretFinding("creds", ".data.password"),
				workload("Deployment", "api", checksumPath),
			},
		}},
		Helm: "4.2.4", Rounds: 2,
	}
}

// --- json ---

func TestJSONCarriesTheContract(t *testing.T) {
	got := asJSON(t, churningReport())

	if got["helm"] != "4.2.4" {
		t.Errorf("helm = %v", got["helm"])
	}
	summary, ok := got["summary"].(map[string]any)
	if !ok || summary["churning"] != float64(1) {
		t.Errorf("summary = %v, want churning 1", got["summary"])
	}
	if len(got["findings"].([]any)) != 2 {
		t.Errorf("findings = %v, want 2", got["findings"])
	}
}

func TestJSONFindingsIsAlwaysAnArray(t *testing.T) {
	// A consumer iterating .findings should not have to guard against null on
	// the clean run - that is the case their pipeline hits most often.
	got := asJSON(t, Report{Charts: []Chart{clean("home")}, Helm: "4.2.4", Rounds: 2})

	findings, ok := got["findings"].([]any)
	if !ok {
		t.Fatalf("findings = %#v, want an array", got["findings"])
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want empty", findings)
	}
}

func TestJSONExposesConsequenceAsASelectableCategory(t *testing.T) {
	// The README documents `.findings[] | select(.consequence == "rolls")`, so
	// this is a category, not the prose.
	got := asJSON(t, churningReport())

	var kinds []string
	for _, f := range got["findings"].([]any) {
		if c, ok := f.(map[string]any)["consequence"].(string); ok {
			kinds = append(kinds, c)
		}
	}
	if !strings.Contains(strings.Join(kinds, ","), "rolls") {
		t.Errorf("consequences = %v, want a selectable \"rolls\"", kinds)
	}
}

func TestJSONCarriesTheRemediation(t *testing.T) {
	got := asJSON(t, churningReport())

	entries, ok := got["remediation"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("remediation = %#v, want entries", got["remediation"])
	}
	first := entries[0].(map[string]any)
	if _, ok := first["jsonPointers"]; !ok {
		t.Errorf("entry = %v, want jsonPointers in ArgoCD's spelling", first)
	}
}

// --- markdown ---

func TestMarkdownIsEmptyWhenThereIsNothingToSay(t *testing.T) {
	// The documented CI snippet guards on hashFiles(...) != '', so a clean run
	// must not post a comment on every pull request that touches a chart.
	got := render(t, Report{Charts: []Chart{clean("home")}, Helm: "4.2.4", Rounds: 2}, Report.Markdown)

	if got != "" {
		t.Errorf("Markdown() = %q, want empty", got)
	}
}

func TestMarkdownHasATableAndACollapsedFix(t *testing.T) {
	got := render(t, churningReport(), Report.Markdown)

	for _, want := range []string{
		"### idem — 1 of 1 chart will churn under ArgoCD",
		"| chart | object | field | consequence |",
		"<details>",
		"<summary>Fix — add to your ArgoCD Application</summary>",
		"ignoreDifferences",
		"<sub>helm 4.2.4 · 2 rounds</sub>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Markdown() missing %q:\n%s", want, got)
		}
	}
}

func TestMarkdownEscapesAPipeSoTheTableSurvives(t *testing.T) {
	// A ConfigMap key or annotation name is user data, and a pipe ends a cell
	// even inside backticks.
	f := secretFinding("creds", ".data.a|b")
	got := render(t, Report{
		Charts: []Chart{{Name: "home", Findings: []check.Finding{f}}},
		Helm:   "4.2.4", Rounds: 2,
	}, Report.Markdown)

	if !strings.Contains(got, `a\|b`) {
		t.Errorf("Markdown() = %q, want the pipe escaped", got)
	}
}

func TestMarkdownFooterCountsWhatCouldNotBeRendered(t *testing.T) {
	got := render(t, Report{
		Charts: []Chart{{Name: "lab", Err: errors.New("boom")}},
		Helm:   "4.2.4", Rounds: 2,
	}, Report.Markdown)

	if !strings.Contains(got, "could not be rendered") {
		t.Errorf("Markdown() = %q, want the unevaluable chart reported", got)
	}
}

// --- github ---

// repoWith writes real files, because the annotator refuses to point at a path
// that does not exist.
func repoWith(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGitHubAnnotatesAnObservedFindingAtFileLevel(t *testing.T) {
	// helm's "# Source:" carries no line number, and guessing one would
	// annotate the wrong line - worse than annotating nothing.
	root := repoWith(t, "charts/home/templates/secrets.yaml")
	f := secretFinding("creds", ".data.password")
	f.Source = "home/templates/secrets.yaml"

	got := render(t, Report{
		Root:   root,
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Findings: []check.Finding{f}}},
		Helm:   "4.2.4", Rounds: 2,
	}, Report.GitHub)

	if !strings.Contains(got, "::error file=charts/home/templates/secrets.yaml::") {
		t.Errorf("GitHub() = %q, want a file-level error annotation", got)
	}
	if strings.Contains(got, "line=") {
		t.Errorf("GitHub() = %q, want no invented line number", got)
	}
}

func TestGitHubAnnotatesAPotentialFindingAtItsExactLine(t *testing.T) {
	root := repoWith(t, "charts/home/templates/_helpers.tpl")

	got := render(t, Report{
		Root: root,
		Charts: []Chart{{
			Name: "home", RepoDir: "charts/home",
			Potential: []analyze.Use{{Function: "randAlphaNum", File: "templates/_helpers.tpl", Line: 12, Call: true}},
		}},
		Helm: "4.2.4", Rounds: 2,
	}, Report.GitHub)

	if !strings.Contains(got, "line=12") {
		t.Errorf("GitHub() = %q, want the exact call site", got)
	}
	if !strings.Contains(got, "::warning") {
		t.Errorf("GitHub() = %q, want a warning rather than an error", got)
	}
}

func TestGitHubWillNotAnnotateAFileThatIsNotThere(t *testing.T) {
	// A subchart vendored as a .tgz produces sources resolving to a path no
	// one can open. Those go to the summary instead.
	root := repoWith(t, "charts/home/Chart.yaml")
	f := secretFinding("creds", ".data.password")
	f.Source = "home/charts/ollama/templates/common.yaml"

	got := render(t, Report{
		Root:   root,
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Findings: []check.Finding{f}}},
		Helm:   "4.2.4", Rounds: 2,
	}, Report.GitHub)

	if strings.Contains(got, "::error file=") {
		t.Errorf("GitHub() = %q, want no annotation on a file that does not exist", got)
	}
	if !strings.Contains(got, "no file in this repository") {
		t.Errorf("GitHub() = %q, want it counted rather than dropped", got)
	}
}

func TestGitHubReportsTheCapRatherThanTruncatingSilently(t *testing.T) {
	// GitHub renders a limited number per step. A silently truncated list
	// reads as "that was everything".
	root := repoWith(t, "charts/home/templates/s.yaml")

	var findings []check.Finding
	for i := range 15 {
		f := secretFinding("creds", ".data.k")
		f.Source = "home/templates/s.yaml"
		f.Change.Object.Name = "creds" + string(rune('a'+i))
		findings = append(findings, f)
	}

	got := render(t, Report{
		Root:   root,
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Findings: findings}},
		Helm:   "4.2.4", Rounds: 2,
	}, Report.GitHub)

	if n := strings.Count(got, "::error file="); n != annotationCap {
		t.Errorf("emitted %d annotations, want the cap of %d", n, annotationCap)
	}
	if !strings.Contains(got, "5 more findings not annotated") {
		t.Errorf("GitHub() = %q, want the remainder counted", got)
	}
}

func TestGitHubEscapesWhatWouldBreakTheCommand(t *testing.T) {
	// An unescaped newline ends the workflow command and the rest is echoed as
	// plain log output.
	root := repoWith(t, "charts/home/Chart.yaml")

	got := render(t, Report{
		Root:   root,
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Err: errors.New("boom\nsecond line")}},
		Helm:   "4.2.4", Rounds: 2,
	}, Report.GitHub)

	if strings.Contains(got, "boom\nsecond") {
		t.Errorf("GitHub() = %q, want the newline escaped", got)
	}
	if !strings.Contains(got, "%0A") {
		t.Errorf("GitHub() = %q, want %%0A", got)
	}
}

func TestGitHubDoesNotClaimAFunctionStayedQuietOnAChurningChart(t *testing.T) {
	// Same rule as the text form: idem cannot attribute an observed difference
	// to a particular function, so on a chart that churned it must not say
	// this one did not fire.
	root := repoWith(t, "charts/home/templates/s.yaml")
	f := secretFinding("creds", ".data.password")
	f.Source = "home/templates/s.yaml"

	got := render(t, Report{
		Root: root,
		Charts: []Chart{{
			Name: "home", RepoDir: "charts/home",
			Findings:  []check.Finding{f},
			Potential: []analyze.Use{{Function: "randAlphaNum", File: "templates/s.yaml", Line: 3, Call: true}},
		}},
		Helm: "4.2.4", Rounds: 2,
	}, Report.GitHub)

	if strings.Contains(got, "did not fire") {
		t.Errorf("GitHub() = %q, want no claim about which function fired", got)
	}
}

func TestGitHubMessageReadsAsOneSentence(t *testing.T) {
	// "at .data.password — silent — no checksum" makes the reader parse two
	// dashes to find the clause boundary.
	root := repoWith(t, "charts/home/templates/s.yaml")
	f := secretFinding("creds", ".data.password")
	f.Source = "home/templates/s.yaml"

	got := render(t, Report{
		Root:   root,
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Findings: []check.Finding{f}}},
		Helm:   "4.2.4", Rounds: 2,
	}, Report.GitHub)

	if strings.Contains(got, "— silent — ") {
		t.Errorf("GitHub() = %q, want the consequence set off cleanly", got)
	}
	if !strings.Contains(got, "(silent — no checksum)") {
		t.Errorf("GitHub() = %q, want the consequence parenthesised", got)
	}
}

func TestMarkdownSaysNothingWhenTheDeliveryConfigCoversEverything(t *testing.T) {
	// Operationally this run is clean, and the documented CI snippet guards on
	// the file being non-empty. A comment saying "4 findings, all handled" on
	// every pull request is exactly the noise that guard exists to prevent.
	got := render(t, Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "apps/home.yaml", true, true)},
		}},
		Helm: "4.2.4", Rounds: 2,
	}, Report.Markdown)

	if got != "" {
		t.Errorf("Markdown() = %q, want empty", got)
	}
}

func TestMarkdownOmitsAnEmptyTable(t *testing.T) {
	// A suppression selfHeal will undo counts as churn and has no rows to
	// show, so the count can be non-zero with nothing to tabulate. A header
	// over nothing reads as a rendering bug.
	got := render(t, Report{
		Charts: []Chart{{
			Name:       "home",
			Suppressed: []delivery.Suppressed{suppressed("creds", "/data/key", "apps/home.yaml", true, false)},
		}},
		Helm: "4.2.4", Rounds: 2,
	}, Report.Markdown)

	if strings.Contains(got, "| chart | object |") {
		t.Errorf("Markdown() = %q, want no table when there are no rows", got)
	}
	if !strings.Contains(got, "selfHeal") {
		t.Errorf("Markdown() = %q, want the reason the count is not zero", got)
	}
}

// Every machine format has to carry churn seen only with `lookup` resolved.
// A CI gate reading -o json, or a reviewer reading the PR comment, must not be
// told a chart is clean because ArgoCD's condition happened to be.

func TestJSONMarksWhichConditionAFindingWasObservedUnder(t *testing.T) {
	got := asJSON(t, Report{Charts: []Chart{serverOnly("home")}, Helm: "4.2.4", Rounds: 2, Cluster: true})

	findings, ok := got["findings"].([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings = %v, want 1", got["findings"])
	}
	if c := findings[0].(map[string]any)["condition"]; c != "cluster" {
		t.Errorf("condition = %v, want \"cluster\" - this was not seen under `helm template`", c)
	}

	summary := got["summary"].(map[string]any)
	if summary["churning"] != float64(0) {
		t.Errorf("summary.churning = %v, want 0 - nothing churns under ArgoCD", summary["churning"])
	}
	if summary["churningWithLookup"] != float64(1) {
		t.Errorf("summary.churningWithLookup = %v, want 1", summary["churningWithLookup"])
	}
}

func TestJSONNamesTheClientConditionExplicitly(t *testing.T) {
	// "condition" absent would leave a policy unable to tell the two apart
	// without knowing which array it came from.
	got := asJSON(t, churningReport())

	for _, f := range got["findings"].([]any) {
		if c := f.(map[string]any)["condition"]; c != "client" {
			t.Errorf("condition = %v, want \"client\"", c)
		}
	}
}

func TestMarkdownIsNotEmptyWhenOnlyTheClusterConditionDiffers(t *testing.T) {
	got := render(t, Report{Charts: []Chart{serverOnly("home")}, Helm: "4.2.4", Rounds: 2, Cluster: true}, Report.Markdown)

	if got == "" {
		t.Fatal("Markdown() = \"\", want the observed difference reported")
	}
	if !strings.Contains(got, "lookup") {
		t.Errorf("Markdown() = %q, want the condition named", got)
	}
}

func TestGitHubAnnotatesADifferenceSeenOnlyWithLookupResolved(t *testing.T) {
	root := repoWith(t, "charts/home/templates/secret.yaml")
	c := serverOnly("home")
	c.RepoDir = "charts/home"

	got := render(t, Report{Root: root, Charts: []Chart{c}, Helm: "4.2.4", Rounds: 2, Cluster: true}, Report.GitHub)

	if !strings.Contains(got, "::error file=charts/home/templates/secret.yaml::") {
		t.Errorf("GitHub() = %q, want the file annotated", got)
	}
	if !strings.Contains(got, "lookup") {
		t.Errorf("GitHub() = %q, want the message to say which condition saw it", got)
	}
}

func TestEveryFormatReportsAnUnrenderableChartOutsideRatchetScope(t *testing.T) {
	// Exit 2 with a machine format that shows nothing is a failing gate whose
	// cause is invisible to the thing consuming it.
	r := Report{
		Since:  "main",
		Charts: []Chart{{Name: "home", Changed: true}, {Name: "lab", Err: errors.New("no repository definition")}},
		Helm:   "4.2.4", Rounds: 2,
	}

	if got := asJSON(t, r)["unevaluable"]; got == nil {
		t.Errorf("unevaluable = %v, want the chart reported", got)
	}
	if got := render(t, r, Report.Markdown); !strings.Contains(got, "lab") {
		t.Errorf("Markdown() = %q, want the chart reported", got)
	}
	if got := render(t, r, Report.GitHub); !strings.Contains(got, "lab could not be rendered") {
		t.Errorf("GitHub() = %q, want the chart reported", got)
	}
}

func TestJSONCarriesAPathAPolicyEngineCanOpen(t *testing.T) {
	root := repoWith(t, "charts/home/templates/secrets.yaml")
	f := secretFinding("creds", ".data.password")
	f.Source = "home/templates/secrets.yaml"

	got := asJSON(t, Report{
		Root:   root,
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Findings: []check.Finding{f}}},
		Helm:   "4.2.4", Rounds: 2,
	})

	source := got["findings"].([]any)[0].(map[string]any)["source"]
	if source != "charts/home/templates/secrets.yaml" {
		t.Errorf("source = %v, want the repository-relative path", source)
	}
}
