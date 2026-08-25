package report

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"time"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/cluster"
	"github.com/pcanilho/idem/internal/delivery"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/doctor"
	"github.com/pcanilho/idem/internal/engine"
	"gopkg.in/yaml.v3"
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

// `-o yaml` is the SAME document as `-o json`, differently encoded.
//
// Compared by decoding both and requiring deep equality, rather than by
// eyeballing two golden files. This repository has twice shipped two output
// formats that disagreed about the same run - the Flux fix block existing only
// in text, and potential findings carrying a different path in json than in
// github - and a hand-maintained YAML renderer would be the third. The
// comparison goes through JSON on the YAML side too, so an int decoded as int
// by YAML and float64 by JSON does not read as a difference.
func TestYAMLIsTheJSONContractInAnotherEncoding(t *testing.T) {
	r := Report{
		Root: repoWith(t, "charts/home/templates/s.yaml"),
		Charts: []Chart{func() Chart {
			c := churnsUnderFlux("home", secretFinding("creds", ".data.password"))
			c.RepoDir = "charts/home"
			c.Namespace = "home"
			c.Potential = []analyze.Use{{Function: "randAlphaNum", File: "templates/s.yaml", Line: 3, Call: true}}
			return c
		}()},
		Helm: "4.2.4", Rounds: 2, Engines: []string{"argocd", "flux"},
	}

	var jb, yb strings.Builder
	if err := r.JSON(&jb); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if err := r.YAML(&yb); err != nil {
		t.Fatalf("YAML() error = %v", err)
	}

	var fromJSON any
	if err := json.Unmarshal([]byte(jb.String()), &fromJSON); err != nil {
		t.Fatalf("emitted JSON does not parse: %v", err)
	}

	var decoded any
	if err := yaml.Unmarshal([]byte(yb.String()), &decoded); err != nil {
		t.Fatalf("emitted YAML does not parse: %v\n%s", err, yb.String())
	}
	// Round-trip so numbers land in the same Go types on both sides.
	normalised, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("YAML did not survive a JSON round trip: %v", err)
	}
	var fromYAML any
	if err := json.Unmarshal(normalised, &fromYAML); err != nil {
		t.Fatalf("round-tripped YAML does not parse: %v", err)
	}

	if !reflect.DeepEqual(fromJSON, fromYAML) {
		t.Errorf("YAML and JSON disagree.\n json: %s\n yaml: %s", jb.String(), normalised)
	}
}

// The no-trailing-whitespace invariant holds for EVERY format, not just text.
//
// text was the one that broke it, but diff/doctor/drift use the same tabwriter
// and markdown embeds an indented YAML block, so any of them could regress.
// json and yaml are encoder output and should never have it - asserting them
// too costs nothing and pins the encoders' settings.
func TestNoFormatEndsALineInWhitespace(t *testing.T) {
	r := Report{
		Root: repoWith(t, "charts/home/templates/s.yaml"),
		Charts: []Chart{func() Chart {
			c := churnsUnderFlux("home", secretFinding("creds", ".stringData.password"))
			c.RepoDir = "charts/home"
			// A finding with no consequence: its last column is empty, which is
			// what makes tabwriter pad the one before it.
			c.Findings = append(c.Findings, finding("home/templates/cm.yaml", "home-cm", ".data.token"))
			c.Potential = []analyze.Use{{Function: "randAlphaNum", File: "templates/s.yaml", Line: 3, Call: true}}
			return c
		}()},
		Helm: "4.2.4", Rounds: 2,
	}

	for _, tc := range []struct {
		name string
		f    func(Report, io.Writer) error
	}{
		{"text", Report.Text},
		{"json", Report.JSON},
		{"yaml", Report.YAML},
		{"markdown", Report.Markdown},
		{"github", Report.GitHub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, line := range strings.Split(render(t, r, tc.f), "\n") {
				if line != strings.TrimRight(line, " \t") {
					t.Errorf("line %d ends in whitespace: %q", i+1, line)
				}
			}
		})
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
		"### idem: 1 of 1 chart will churn under ArgoCD",
		"| chart | object | field | consequence |",
		"<details>",
		"<summary>Fix: add to your ArgoCD Application</summary>",
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

// -o markdown is the PR-comment channel action.yml documents, and it was the
// one format left out when text, json and github were aligned on engine scope
// and on fluxFindings. A Flux-only estate got an ArgoCD ignoreDifferences block
// posted on its pull request, and never got the fix that would have worked.
func TestMarkdownRespectsTheEngineScopeAndCarriesTheFluxFix(t *testing.T) {
	r := Report{
		Charts:  []Chart{churnsUnderFlux("home", secretFinding("creds", ".data.password"))},
		Engines: []string{"flux"},
		Helm:    "4.2.4", Rounds: 2,
	}

	got := render(t, r, Report.Markdown)

	if strings.Contains(got, "ignoreDifferences") {
		t.Errorf("Markdown() = %q, want no ArgoCD block when only flux is shown", got)
	}
	if !strings.Contains(got, "driftDetection") {
		t.Errorf("Markdown() = %q, want the Flux fix", got)
	}
}

func TestMarkdownStillCarriesTheArgoBlockWhenArgoIsShown(t *testing.T) {
	got := render(t, Report{
		Charts:  []Chart{churnsUnderFlux("home", secretFinding("creds", ".data.password"))},
		Engines: []string{"argocd"},
		Helm:    "4.2.4", Rounds: 2,
	}, Report.Markdown)

	if !strings.Contains(got, "ignoreDifferences") {
		t.Errorf("Markdown() = %q, want the ArgoCD block", got)
	}
	if strings.Contains(got, "driftDetection") {
		t.Errorf("Markdown() = %q, want no Flux block when only argocd is shown", got)
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

// `-o json` is the machine-readable contract, and a consumer that cannot open
// the file it names has to guess which chart directory to prefix. `findings[].source`
// is already resolved against the repository; `potential[].file` was not, so one
// document carried two different path conventions with nothing saying which.
func TestJSONPlacesAPotentialFindingInTheRepository(t *testing.T) {
	root := repoWith(t, "charts/home/templates/_helpers.tpl")

	got := render(t, Report{
		Root: root,
		Charts: []Chart{{
			Name: "home", RepoDir: "charts/home",
			Potential: []analyze.Use{{Function: "randAlphaNum", File: "templates/_helpers.tpl", Line: 12, Call: true}},
		}},
		Helm: "4.2.4", Rounds: 2,
	}, Report.JSON)

	if !strings.Contains(got, `"file": "charts/home/templates/_helpers.tpl"`) {
		t.Errorf("JSON() = %q, want the path resolved against the repository", got)
	}
}

// `-o json` is the seam idem tells people to gate on, so a fix `-o text` prints
// and `-o json` omits is a fix their policy engine cannot see. The Flux block
// was text-only, and the ArgoCD entries carried no engine field - so a consumer
// could not tell which engine the config it was reading was even for.
//
// Decoded rather than substring-matched: `"engine": "flux"` appears in the
// verdicts array on any run that reports a Flux verdict at all, so a Contains
// check here passes without a Flux fix ever being emitted. It did, first time.
func TestJSONCarriesBothEnginesFixesAndSaysWhichIsWhich(t *testing.T) {
	got := asJSON(t, Report{
		Charts: []Chart{churnsUnderFlux("home", secretFinding("creds", ".data.password"))},
		Helm:   "4.2.4", Rounds: 2,
	})

	byEngine := remediationByEngine(t, got)
	argo, ok := byEngine["argocd"]
	if !ok {
		t.Fatalf("remediation = %v, want an argocd entry", got["remediation"])
	}
	if _, ok := argo["jsonPointers"]; !ok {
		t.Errorf("argocd entry = %v, want jsonPointers", argo)
	}

	flux, ok := byEngine["flux"]
	if !ok {
		t.Fatalf("remediation = %v, want a flux entry", got["remediation"])
	}
	// Flux's own spelling, because the two are evaluated against different
	// shapes and one field name would imply they are interchangeable.
	if _, ok := flux["paths"]; !ok {
		t.Errorf("flux entry = %v, want paths", flux)
	}
}

// The same scoping the text form applies: a chart a lookup stabilises has no
// Flux drift, so offering config to suppress it would be config for a problem
// the reader does not have - in the output a machine acts on without a human
// reading it first.
func TestJSONOmitsTheFluxFixWhereFluxDoesNotChurn(t *testing.T) {
	got := asJSON(t, Report{
		Charts: []Chart{{
			Name:     "home",
			Findings: []check.Finding{secretFinding("creds", ".data.password")},
			Verdicts: []engine.Verdict{
				{Engine: "argocd", Result: engine.Churns},
				{Engine: "flux", Result: engine.Stable},
			},
		}},
		Helm: "4.2.4", Rounds: 2,
	})

	byEngine := remediationByEngine(t, got)
	if _, ok := byEngine["flux"]; ok {
		t.Errorf("remediation = %v, want no flux entry where flux is stable", got["remediation"])
	}
	if _, ok := byEngine["argocd"]; !ok {
		t.Errorf("remediation = %v, want the argocd entry still there", got["remediation"])
	}
}

// remediationByEngine indexes the remediation array by its engine field, and
// fails if any entry does not name one - an unlabelled fix block is the defect
// these tests exist for.
func remediationByEngine(t *testing.T, got map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	entries, _ := got["remediation"].([]any)
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("remediation entry = %v, want an object", e)
		}
		name, ok := entry["engine"].(string)
		if !ok {
			t.Fatalf("remediation entry = %v, want an engine field", entry)
		}
		out[name] = entry
	}
	return out
}

func TestGitHubMessageReadsAsOneSentence(t *testing.T) {
	// "at .data.password, silent, no checksum" makes the reader parse two
	// commas to find the clause boundary.
	root := repoWith(t, "charts/home/templates/s.yaml")
	f := secretFinding("creds", ".data.password")
	f.Source = "home/templates/s.yaml"

	got := render(t, Report{
		Root:   root,
		Charts: []Chart{{Name: "home", RepoDir: "charts/home", Findings: []check.Finding{f}}},
		Helm:   "4.2.4", Rounds: 2,
	}, Report.GitHub)

	if strings.Contains(got, ", silent, ") {
		t.Errorf("GitHub() = %q, want the consequence set off cleanly", got)
	}
	if !strings.Contains(got, "(silent, no checksum)") {
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

// -o json marks a reordered list, so a consumer gating on the contract can tell
// a path it could suppress from one it cannot.
//
// Decoded and indexed rather than Contains: the word "reordered" appears in the
// note this feature also prints, so a substring check would pass with the field
// missing entirely. Two JSON tests in this file were caught passing that way.
func TestJSONMarksAReorderedPathAsUnsuppressable(t *testing.T) {
	r := Report{
		Helm: "4.2.4", Rounds: 3, Engines: []string{"argocd"},
		Charts: []Chart{{
			Name: "api", Dir: "./api",
			Findings: []check.Finding{
				reordered("templates/main.yaml", "api", ".spec.env", 4),
				finding("templates/main.yaml", "creds", ".data.password"),
			},
		}},
	}

	var doc struct {
		Findings []struct {
			Paths []struct {
				Path      string `json:"path"`
				Pointer   string `json:"pointer"`
				Reordered bool   `json:"reordered"`
			} `json:"paths"`
		} `json:"findings"`
		Remediation []struct {
			Engine       string   `json:"engine"`
			JSONPointers []string `json:"jsonPointers"`
		} `json:"remediation"`
	}
	if err := json.Unmarshal([]byte(render(t, r, Report.JSON)), &doc); err != nil {
		t.Fatalf("JSON does not decode: %v", err)
	}

	byPointer := map[string]bool{}
	for _, f := range doc.Findings {
		for _, p := range f.Paths {
			byPointer[p.Pointer] = p.Reordered
		}
	}

	got, ok := byPointer["/spec/env"]
	if !ok {
		t.Fatalf("no path at /spec/env: %+v", doc.Findings)
	}
	if !got {
		t.Errorf("reordered = false at /spec/env, want true")
	}
	if byPointer["/data/password"] {
		t.Errorf("reordered = true at /data/password, want false: an ordinary regenerated value")
	}

	// And the remediation must carry the password's pointer without the list's.
	for _, e := range doc.Remediation {
		if slices.Contains(e.JSONPointers, "/spec/env") {
			t.Errorf("%s remediation offers /spec/env, which suppresses the list's contents: %+v", e.Engine, e.JSONPointers)
		}
	}
}

// The PR comment says the list reordered, and says why no fix is attached.
//
// Markdown is the channel a reviewer actually reads, and it was the format left
// behind the last two times a finding gained a shape. A bare field name in the
// table reads as "this value churns", and the reviewer then looks for the
// collapsed fix block that every other churning finding carries - and finds
// nothing, with no explanation.
func TestMarkdownSaysAListReorderedAndWhyNoFixIsAttached(t *testing.T) {
	r := Report{
		Helm: "4.2.4", Rounds: 3, Engines: []string{"argocd"},
		Charts: []Chart{{
			Name: "api", Dir: "./api",
			Findings: []check.Finding{reordered("templates/main.yaml", "api", ".spec.env", 6)},
			Verdicts: churningVerdict(),
		}},
	}
	got := render(t, r, Report.Markdown)

	// The ROW, not merely the word: the note below the table also says
	// "reordered", so a bare Contains passes with the row unmarked - which is
	// how the first version of this test passed with the annotation deleted.
	if !strings.Contains(got, "`.spec.env` (reordered, same 6 elements)") {
		t.Errorf("the table row does not say the list reordered:\n%s", got)
	}
	if !strings.Contains(got, "sortAlpha") {
		t.Errorf("no explanation of why there is no fix block:\n%s", got)
	}
	// The collapsed block, not the word: the sentence explaining why there is
	// no block necessarily names `ignoreDifferences` itself.
	if strings.Contains(got, "<summary>Fix") {
		t.Errorf("offered a fix block for a reordered list:\n%s", got)
	}
}

// The inline annotation says it too, since -o github is the other PR channel
// and carries no fix block at all to fall back on.
func TestGitHubAnnotatesAReorderAsAnOrderingProblem(t *testing.T) {
	root := repoWith(t, "charts/api/templates/main.yaml")

	f := reordered("api/templates/main.yaml", "api", ".spec.env", 6)
	r := Report{
		Helm: "4.2.4", Rounds: 3, Engines: []string{"argocd"}, Root: root,
		Charts: []Chart{{
			Name: "api", RepoDir: "charts/api",
			Findings: []check.Finding{f},
			Verdicts: churningVerdict(),
		}},
	}
	got := render(t, r, Report.GitHub)

	if !strings.Contains(got, "reordered") {
		t.Errorf("annotation does not say the list reordered:\n%s", got)
	}
	if !strings.Contains(got, "sortAlpha") {
		t.Errorf("annotation does not name the fix, and there is no fix block to carry it:\n%s", got)
	}
}

// Every reader-facing format says a reordered list cannot be suppressed.
//
// One test across the formats rather than three separate ones, because "the
// format left behind" is this repository's most repeated defect: the Flux fix
// block once existed only in -o text, and -o markdown once ignored --engine
// entirely and commented an ArgoCD block onto a Flux user's pull request. A
// format added later fails here rather than shipping silent.
//
// json and yaml are excluded deliberately: they carry `reordered` as a field on
// the path, which TestJSONMarksAReorderedPathAsUnsuppressable checks by
// decoding. Prose in a machine contract would be noise.
func TestEveryReaderFacingFormatSaysAReorderCannotBeSuppressed(t *testing.T) {
	root := repoWith(t, "charts/api/templates/main.yaml")
	r := Report{
		Helm: "4.2.4", Rounds: 3, Engines: []string{"argocd"}, Root: root,
		Charts: []Chart{{
			Name: "api", RepoDir: "charts/api",
			Findings: []check.Finding{reordered("api/templates/main.yaml", "api", ".spec.env", 6)},
			Verdicts: churningVerdict(),
		}},
	}

	for _, tc := range []struct {
		name string
		f    func(Report, io.Writer) error
	}{
		{"text", Report.Text},
		{"markdown", Report.Markdown},
		{"github", Report.GitHub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := render(t, r, tc.f)
			if !strings.Contains(got, "reordered, same 6 elements") {
				t.Errorf("does not mark the path as a reorder:\n%s", got)
			}
			if !strings.Contains(got, "sortAlpha") {
				t.Errorf("does not name the fix:\n%s", got)
			}
			if !strings.Contains(got, "ordering alone") {
				t.Errorf("does not say why no config can suppress it:\n%s", got)
			}
		})
	}
}

// -o json carries the jq-reachable set too, since it is the policy seam.
//
// A gate reading the contract has the same question the reader does: is this
// finding one my own config might already handle? idem computed the answer and
// emitted it nowhere. `maybeSuppressed` rather than folding it into
// `suppressed`, because a consumer that treated the two alike would silently
// drop findings idem never confirmed were covered.
func TestJSONCarriesFindingsAJQRuleMightCover(t *testing.T) {
	f := finding("templates/main.yaml", "creds", ".data.password")
	r := Report{
		Helm: "4.2.4", Rounds: 3, Engines: []string{"argocd"},
		Charts: []Chart{{
			Name: "api", Dir: "./api",
			Findings: []check.Finding{f},
			Maybe: []delivery.Suppressed{{
				Finding: f,
				By:      delivery.Rule{File: "apps/prod.yaml", JQ: []string{`.data | keys`}},
			}},
		}},
	}

	var doc struct {
		Suppressed []struct {
			By struct {
				File string `json:"file"`
			} `json:"by"`
		} `json:"suppressed"`
		MaybeSuppressed []struct {
			By struct {
				File string   `json:"file"`
				JQ   []string `json:"jqPathExpressions"`
			} `json:"by"`
		} `json:"maybeSuppressed"`
		Summary struct {
			Churning   int `json:"churning"`
			Suppressed int `json:"suppressed"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(render(t, r, Report.JSON)), &doc); err != nil {
		t.Fatalf("JSON does not decode: %v", err)
	}

	if len(doc.MaybeSuppressed) != 1 {
		t.Fatalf("maybeSuppressed = %+v, want one entry", doc.MaybeSuppressed)
	}
	if doc.MaybeSuppressed[0].By.File != "apps/prod.yaml" {
		t.Errorf("by.file = %q, want the manifest carrying the rule", doc.MaybeSuppressed[0].By.File)
	}
	if len(doc.MaybeSuppressed[0].By.JQ) != 1 {
		t.Errorf("by.jqPathExpressions = %v, want the expression idem could not evaluate", doc.MaybeSuppressed[0].By.JQ)
	}

	// It must NOT appear as suppressed, and must still count as churn.
	if len(doc.Suppressed) != 0 {
		t.Errorf("suppressed = %+v, want none: idem never confirmed this is covered", doc.Suppressed)
	}
	if doc.Summary.Suppressed != 0 {
		t.Errorf("summary.suppressed = %d, want 0", doc.Summary.Suppressed)
	}
	if doc.Summary.Churning != 1 {
		t.Errorf("summary.churning = %d, want 1: unconfirmed is not covered", doc.Summary.Churning)
	}
}

// --- the verbs' machine-readable forms ---

func diagnosis() doctor.Diagnosis {
	return doctor.Diagnosis{
		Scanned: 75,
		Median:  0.07,
		Suspects: []doctor.Suspect{{
			Workload: cluster.Workload{
				Kind: "Deployment", Namespace: "lab", Name: "lab-harbor-core",
				Revision: 660, Checksums: []string{"checksum/configmap", "checksum/secret"},
				Owner: cluster.Owner{Engine: "argocd", Name: "lab-app", Chart: "harbor"},
			},
			PerDay: 0.89,
			Age:    745 * 24 * time.Hour,
		}},
	}
}

// `idem doctor -o json` exists, because -o json is the seam idem tells people
// to gate on.
//
// It was refused outright: "renders text only". idem cut its rules system on
// the grounds that `-o json | conftest` is the extension point, and then left
// two of its three verbs unable to reach it - doctor most of all, since a
// ranked table of what keeps rolling is exactly what someone would alert on.
func TestDoctorHasAMachineReadableForm(t *testing.T) {
	var b strings.Builder
	if err := DoctorJSON(&b, diagnosis(), "truenas", map[string]string{"lab-app": "charts/lab"}); err != nil {
		t.Fatalf("DoctorJSON: %v", err)
	}

	var doc struct {
		Context  string  `json:"context"`
		Scanned  int     `json:"scanned"`
		Median   float64 `json:"median"`
		Suspects []struct {
			Kind      string   `json:"kind"`
			Namespace string   `json:"namespace"`
			Name      string   `json:"name"`
			Revision  int      `json:"revision"`
			PerDay    float64  `json:"perDay"`
			AgeDays   int      `json:"ageDays"`
			Checksums []string `json:"checksums"`
			Chart     string   `json:"chart"`
			Owner     struct {
				Engine string `json:"engine"`
				Name   string `json:"name"`
			} `json:"owner"`
		} `json:"suspects"`
	}
	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("DoctorJSON does not decode: %v\n%s", err, b.String())
	}

	if doc.Scanned != 75 || doc.Median != 0.07 || doc.Context != "truenas" {
		t.Errorf("scanned/median/context = %d/%v/%q, want 75/0.07/truenas", doc.Scanned, doc.Median, doc.Context)
	}
	if len(doc.Suspects) != 1 {
		t.Fatalf("suspects = %+v, want one", doc.Suspects)
	}
	s := doc.Suspects[0]
	if s.Kind != "Deployment" || s.Namespace != "lab" || s.Name != "lab-harbor-core" {
		t.Errorf("identity = %s/%s/%s", s.Kind, s.Namespace, s.Name)
	}
	if s.Revision != 660 || s.PerDay != 0.89 || s.AgeDays != 745 {
		t.Errorf("rev/perDay/ageDays = %d/%v/%d, want 660/0.89/745", s.Revision, s.PerDay, s.AgeDays)
	}
	// Every checksum, not the "+ N more" the table shows: the text form elides
	// for width, and a contract that elided would be lying to a consumer.
	if len(s.Checksums) != 2 {
		t.Errorf("checksums = %v, want both - the text form's elision is a display concern", s.Checksums)
	}
	if s.Owner.Engine != "argocd" || s.Owner.Name != "lab-app" {
		t.Errorf("owner = %+v", s.Owner)
	}
	if s.Chart != "charts/lab" {
		t.Errorf("chart = %q, want the path the confirm command uses", s.Chart)
	}
}

// A clean doctor run is still a document, not an empty stream.
//
// Same rule as `.findings` never being null: the clean run is the case a
// consumer's pipeline hits most often, and it must not have to special-case it.
func TestDoctorJSONIsADocumentEvenWithNothingToReport(t *testing.T) {
	var b strings.Builder
	if err := DoctorJSON(&b, doctor.Diagnosis{Scanned: 12, Median: 0.02}, "", nil); err != nil {
		t.Fatalf("DoctorJSON: %v", err)
	}
	var doc struct {
		Scanned  int   `json:"scanned"`
		Suspects []any `json:"suspects"`
	}
	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("does not decode: %v\n%s", err, b.String())
	}
	if doc.Scanned != 12 {
		t.Errorf("scanned = %d, want 12", doc.Scanned)
	}
	if doc.Suspects == nil {
		t.Errorf("suspects is null, want an empty array:\n%s", b.String())
	}
}

func TestDriftHasAMachineReadableForm(t *testing.T) {
	drifts := []doctor.Drift{{
		Object:   diff.ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: "lab", Name: "creds"},
		Writer:   "external-secrets",
		Evidence: "field manager",
		Changes: []diff.PathDiff{{
			Path: path(".data.password"), Left: "a", Right: "b", HasLeft: true, HasRight: true,
		}},
	}}

	var b strings.Builder
	if err := DriftJSON(&b, drifts, "lab"); err != nil {
		t.Fatalf("DriftJSON: %v", err)
	}

	var doc struct {
		Namespace string `json:"namespace"`
		Drifts    []struct {
			Object   jsonObject `json:"object"`
			Writer   string     `json:"writer"`
			Evidence string     `json:"evidence"`
			Changes  []struct {
				Pointer string `json:"pointer"`
			} `json:"changes"`
		} `json:"drifts"`
	}
	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("does not decode: %v\n%s", err, b.String())
	}
	if doc.Namespace != "lab" {
		t.Errorf("namespace = %q", doc.Namespace)
	}
	if len(doc.Drifts) != 1 || doc.Drifts[0].Writer != "external-secrets" {
		t.Fatalf("drifts = %+v", doc.Drifts)
	}
	if doc.Drifts[0].Object.Name != "creds" {
		t.Errorf("object = %+v", doc.Drifts[0].Object)
	}
	if len(doc.Drifts[0].Changes) != 1 || doc.Drifts[0].Changes[0].Pointer != "/data/password" {
		t.Errorf("changes = %+v", doc.Drifts[0].Changes)
	}
}

func TestDiffHasAMachineReadableForm(t *testing.T) {
	f := secretFinding("creds", ".data.password")

	var b strings.Builder
	if err := DiffJSON(&b, []check.Finding{f}, "a.yaml", "b.yaml"); err != nil {
		t.Fatalf("DiffJSON: %v", err)
	}

	var doc struct {
		Left     string `json:"left"`
		Right    string `json:"right"`
		Findings []struct {
			Object jsonObject `json:"object"`
			Type   string     `json:"type"`
			Paths  []struct {
				Pointer   string `json:"pointer"`
				Reordered bool   `json:"reordered"`
			} `json:"paths"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("does not decode: %v\n%s", err, b.String())
	}
	if doc.Left != "a.yaml" || doc.Right != "b.yaml" {
		t.Errorf("left/right = %q/%q", doc.Left, doc.Right)
	}
	if len(doc.Findings) != 1 {
		t.Fatalf("findings = %+v, want one", doc.Findings)
	}
	if doc.Findings[0].Type != "differs" {
		t.Errorf("type = %q, want the name not the ordinal", doc.Findings[0].Type)
	}
	if doc.Findings[0].Paths[0].Pointer != "/data/password" {
		t.Errorf("pointer = %q", doc.Findings[0].Paths[0].Pointer)
	}
}

// Each verb's YAML is the same document as its JSON, decoded and compared -
// the rule TestYAMLIsTheJSONContractInAnotherEncoding already applies to the
// chart report, extended to the two verbs that just gained a contract.
func TestEachVerbsYAMLIsItsJSONInAnotherEncoding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		json, yaml func(io.Writer) error
	}{
		{
			"doctor",
			func(w io.Writer) error { return DoctorJSON(w, diagnosis(), "truenas", nil) },
			func(w io.Writer) error { return DoctorYAML(w, diagnosis(), "truenas", nil) },
		},
		{
			"drift",
			func(w io.Writer) error { return DriftJSON(w, nil, "lab") },
			func(w io.Writer) error { return DriftYAML(w, nil, "lab") },
		},
		{
			"diff",
			func(w io.Writer) error {
				return DiffJSON(w, []check.Finding{secretFinding("creds", ".data.password")}, "a.yaml", "b.yaml")
			},
			func(w io.Writer) error {
				return DiffYAML(w, []check.Finding{secretFinding("creds", ".data.password")}, "a.yaml", "b.yaml")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var jb, yb strings.Builder
			if err := tc.json(&jb); err != nil {
				t.Fatalf("JSON: %v", err)
			}
			if err := tc.yaml(&yb); err != nil {
				t.Fatalf("YAML: %v", err)
			}

			var fromJSON any
			if err := json.Unmarshal([]byte(jb.String()), &fromJSON); err != nil {
				t.Fatalf("JSON does not parse: %v", err)
			}
			var decoded any
			if err := yaml.Unmarshal([]byte(yb.String()), &decoded); err != nil {
				t.Fatalf("YAML does not parse: %v\n%s", err, yb.String())
			}
			// Round-trip so numbers land in the same Go types on both sides.
			normalised, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("YAML did not survive a JSON round trip: %v", err)
			}
			var fromYAML any
			if err := json.Unmarshal(normalised, &fromYAML); err != nil {
				t.Fatalf("round-tripped YAML does not parse: %v", err)
			}
			if !reflect.DeepEqual(fromJSON, fromYAML) {
				t.Errorf("YAML and JSON disagree.\n json: %s\n yaml: %s", jb.String(), normalised)
			}
		})
	}
}

// idem's own machine-readable output does not change between two runs of the
// same command.
//
// perDay and median are derived from time.Now(), so at full float64 precision
// every one of them differs a second later - and `idem doctor -o json` piped to
// a file, hashed, or diffed would show churn produced entirely by idem. A tool
// that reports non-determinism cannot exhibit it; this is the same rule that
// made internal/doctor redact the API server's minted token names.
//
// Rounded to the two decimals the text form has always displayed. The extra
// precision is spurious anyway: the inputs are a rollout count and an age in
// days.
func TestTheDoctorContractDoesNotDriftBetweenTwoRunsASecondApart(t *testing.T) {
	now := diagnosis()
	later := diagnosis()
	// A second of age changes every derived rate.
	later.Suspects[0].Age += time.Second
	later.Median += 1e-9
	later.Suspects[0].PerDay += 1e-9

	var a, b strings.Builder
	if err := DoctorJSON(&a, now, "truenas", nil); err != nil {
		t.Fatalf("DoctorJSON: %v", err)
	}
	if err := DoctorJSON(&b, later, "truenas", nil); err != nil {
		t.Fatalf("DoctorJSON: %v", err)
	}

	if a.String() != b.String() {
		t.Errorf("two runs a second apart produced different documents:\n%s\n%s", a.String(), b.String())
	}
	if !strings.Contains(a.String(), "0.89") {
		t.Errorf("perDay is not rounded to the two decimals the text form shows:\n%s", a.String())
	}
}
