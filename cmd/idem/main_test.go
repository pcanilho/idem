package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/chartref"
)

func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
}

// invoke runs the command as a user would, returning its exit code and streams.
func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestACleanChartExitsZeroAndSaysWhatItChecked(t *testing.T) {
	requireHelm(t)

	code, stdout, stderr := invoke(t, "testdata/clean")

	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "renders consistently under ArgoCD") {
		t.Errorf("stdout = %q, want the clean verdict sentence", stdout)
	}
	// The provenance line is not decoration: ArgoCD 3.5 swapped Helm 3.19 for
	// 4.2 underneath everybody, and a pass that does not say which helm it
	// used is a pass you cannot act on.
	if !strings.Contains(stdout, "helm ") || !strings.Contains(stdout, "2 rounds") {
		t.Errorf("stdout = %q, want the helm version and round count", stdout)
	}
}

func TestARandAlphaNumChartIsReportedAsChurning(t *testing.T) {
	requireHelm(t)

	// The whole tool in one test: render twice, compare, name the field. This
	// is the bitnami idiom that put three Harbor Deployments at revision 658.
	code, stdout, stderr := invoke(t, "testdata/churn")

	if code != exitOK {
		t.Fatalf("exit = %d, want %d - findings are informative by default (stderr: %s)", code, exitOK, stderr)
	}
	for _, want := range []string{
		"churn/templates/main.yaml",
		"Secret/churn-secret",
		".data.password",
		"1 of 1 chart will churn under ArgoCD",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestStrictExitsOneOnFindings(t *testing.T) {
	requireHelm(t)

	code, _, _ := invoke(t, "testdata/churn", "--strict")

	if code != exitFinding {
		t.Errorf("exit = %d, want %d", code, exitFinding)
	}
}

func TestStrictStillExitsZeroWhenThereAreNoFindings(t *testing.T) {
	requireHelm(t)

	code, _, stderr := invoke(t, "testdata/clean", "--strict")

	if code != exitOK {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
}

func TestAChartThatCannotBeRenderedIsFatalEvenWithoutStrict(t *testing.T) {
	requireHelm(t)

	// Exit 2 is not negotiable: a chart that silently fails to render and is
	// then skipped is the same class of bug idem exists to catch.
	code, stdout, _ := invoke(t, "testdata/broken")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stdout, "could not be rendered") {
		t.Errorf("stdout = %q, want the chart reported rather than skipped", stdout)
	}
	if !strings.Contains(stdout, "auth.password is required") {
		t.Errorf("stdout = %q, want helm's own reason", stdout)
	}
}

func TestEveryChartUnderTheGivenDirectoryIsChecked(t *testing.T) {
	requireHelm(t)

	code, stdout, stderr := invoke(t, "testdata/many")

	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "All 2 charts") {
		t.Errorf("stdout = %q, want both charts checked", stdout)
	}
}

func TestFlagsMayFollowThePositionalPath(t *testing.T) {
	requireHelm(t)

	// The README writes `idem ./charts --strict`. Go's flag package stops at
	// the first operand by default, which would silently ignore --strict and
	// exit 0 on a CI gate the user believed was enforcing.
	code, _, _ := invoke(t, "testdata/churn", "--strict")
	if code != exitFinding {
		t.Errorf("exit = %d, want %d - a flag after the path must still apply", code, exitFinding)
	}
}

func TestRoundsFlagIsHonoured(t *testing.T) {
	requireHelm(t)

	code, stdout, _ := invoke(t, "testdata/clean", "--rounds", "3")

	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "3 rounds") {
		t.Errorf("stdout = %q, want the round count reported", stdout)
	}
}

func TestASingleRoundIsRejected(t *testing.T) {
	code, _, stderr := invoke(t, "testdata/clean", "--rounds", "1")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stderr, "2") {
		t.Errorf("stderr = %q, want it to say at least two renders are needed", stderr)
	}
}

func TestAnUnknownRepoAliasPrintsTheCommandTheUserNeeds(t *testing.T) {
	// helm answers "Error: repo bitnami not found", which says nothing about
	// what to do. idem classifies the reference before rendering so it can.
	code, _, stderr := invoke(t, "bitnami/postgresql")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stderr, "helm repo add bitnami") {
		t.Errorf("stderr = %q, want the setup command", stderr)
	}
}

func TestMoreThanOneChartReferenceIsRejected(t *testing.T) {
	code, _, stderr := invoke(t, "testdata/clean", "testdata/churn")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if stderr == "" {
		t.Error("stderr is empty, want an explanation")
	}
}

func TestAPathWithNoChartsIsReported(t *testing.T) {
	empty := t.TempDir()

	code, _, stderr := invoke(t, empty)

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stderr, empty) {
		t.Errorf("stderr = %q, want the path searched to be named", stderr)
	}
	if !strings.Contains(stderr, "Chart.yaml") {
		t.Errorf("stderr = %q, want it to say what was looked for", stderr)
	}
}

func TestMissingHelmBinaryIsReportedOnce(t *testing.T) {
	code, _, stderr := invoke(t, "testdata/clean", "--helm", "helm-does-not-exist")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stderr, "helm-does-not-exist") {
		t.Errorf("stderr = %q, want the binary idem tried to be named", stderr)
	}
}

func TestReleaseNameForRemoteReferences(t *testing.T) {
	// idem has no Application to take a release name from, so it derives one -
	// and the only property that matters is that every round gets the same one.
	for raw, want := range map[string]string{
		"oci://registry-1.docker.io/bitnamicharts/postgresql": "postgresql",
		"bitnami/postgresql":             "postgresql",
		"postgresql":                     "postgresql",
		"oci://ghcr.io/acme/charts/api/": "api",
	} {
		if got := releaseName(raw); got != want {
			t.Errorf("releaseName(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestANonZeroExitExplainsItselfInTheOutput(t *testing.T) {
	requireHelm(t)

	// A CI log that ends in a bare non-zero status makes the reader go looking
	// for the reason; idem states it on the line it exits on.
	strictCode, strictOut, _ := invoke(t, "testdata/churn", "--strict")
	if strictCode != exitFinding {
		t.Fatalf("exit = %d, want %d", strictCode, exitFinding)
	}
	if !strings.Contains(strictOut, "exit 1") {
		t.Errorf("stdout = %q, want the exit code stated", strictOut)
	}

	fatalCode, fatalOut, _ := invoke(t, "testdata/broken")
	if fatalCode != exitFatal {
		t.Fatalf("exit = %d, want %d", fatalCode, exitFatal)
	}
	if !strings.Contains(fatalOut, "exit 2 — a chart could not be rendered") {
		t.Errorf("stdout = %q, want the reason for the fatal exit", fatalOut)
	}
}

func TestACleanRunSaysNothingAboutExitCodes(t *testing.T) {
	requireHelm(t)

	_, stdout, _ := invoke(t, "testdata/clean")

	if strings.Contains(stdout, "exit") {
		t.Errorf("stdout = %q, want no exit-code line when there is nothing wrong", stdout)
	}
}

func TestVersionFlagPrintsTheCLIVersionNotAChartVersion(t *testing.T) {
	// helm spells the chart version --version, and mirroring that made
	// `idem --version` answer a question nobody asks it. The flag that every
	// CLI has means the same thing here.
	code, stdout, stderr := invoke(t, "--version")

	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.HasPrefix(stdout, "idem ") {
		t.Errorf("stdout = %q, want it to start with the tool name and a version", stdout)
	}
}

func TestVersionFlagRendersNothing(t *testing.T) {
	// It must not need helm, a chart, or a working directory to answer.
	_, stdout, _ := invoke(t, "--version")

	if strings.Contains(stdout, "ArgoCD") {
		t.Errorf("stdout = %q, want no verdict - --version renders nothing", stdout)
	}
}

func TestChartVersionReachesTheRenderSpec(t *testing.T) {
	// The capability helm's --version gave is still needed; it just moved to a
	// name that says which version it means.
	spec := specFor(
		chartref.Ref{Raw: "postgresql", Kind: chartref.RepoURL, Repo: "https://charts.example.com"},
		target{ref: "postgresql", release: "pg"},
		options{chartVersion: "12.1.0"},
	)

	if got, want := spec.Version, "12.1.0"; got != want {
		t.Errorf("Spec.Version = %q, want %q", got, want)
	}
	if got, want := spec.Repo, "https://charts.example.com"; got != want {
		t.Errorf("Spec.Repo = %q, want %q", got, want)
	}
}

func TestValuesFilesAndSetValuesReachTheRenderSpecInOrder(t *testing.T) {
	spec := specFor(
		chartref.Ref{Raw: "./c", Kind: chartref.Local},
		target{ref: "./c", release: "c"},
		options{
			valuesFiles: multiFlag{"base.yaml", "prod.yaml"},
			setValues:   multiFlag{"a=1"},
		},
	)

	if got := strings.Join(spec.ValuesFiles, ","); got != "base.yaml,prod.yaml" {
		t.Errorf("Spec.ValuesFiles = %q, want base.yaml,prod.yaml - later files win, so order is semantic", got)
	}
	if got := strings.Join(spec.SetValues, ","); got != "a=1" {
		t.Errorf("Spec.SetValues = %q, want a=1", got)
	}
}

func TestFormatVersion(t *testing.T) {
	// A Go pseudo-version already encodes the revision and the dirty marker
	// ("v0.0.0-20260821194753-6b61657effd1+dirty"), so appending them again
	// prints the same facts three times. A devel build encodes neither.
	for _, tc := range []struct {
		name     string
		main     string
		revision string
		modified bool
		want     string
	}{
		{"tagged release", "v0.1.0", "6b61657effd1", false, "v0.1.0"},
		{"pseudo-version is already complete", "v0.0.0-20260821-6b61657effd1+dirty", "6b61657effd1", true, "v0.0.0-20260821-6b61657effd1+dirty"},
		{"devel build names the commit", "(devel)", "6b61657effd1", false, "(devel) 6b61657"},
		{"devel build marks a dirty tree", "(devel)", "6b61657effd1", true, "(devel) 6b61657 (dirty)"},
		{"devel build with no vcs info", "(devel)", "", false, "(devel)"},
		{"nothing known at all", "", "", false, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatVersion(tc.main, tc.revision, tc.modified); got != tc.want {
				t.Errorf("formatVersion(%q, %q, %v) = %q, want %q", tc.main, tc.revision, tc.modified, got, tc.want)
			}
		})
	}
}

func TestJobsFlagIsAccepted(t *testing.T) {
	requireHelm(t)

	code, stdout, stderr := invoke(t, "testdata/many", "--jobs", "1")

	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "All 2 charts") {
		t.Errorf("stdout = %q, want both charts checked", stdout)
	}
}

func TestOutputIsIdenticalWhateverTheJobCount(t *testing.T) {
	requireHelm(t)

	// Charts finish in whatever order the pool happens to complete them. If
	// that leaked into the report, idem would be exhibiting the very
	// non-determinism it exists to report.
	_, sequential, _ := invoke(t, "testdata/many", "--jobs", "1")
	_, parallel, _ := invoke(t, "testdata/many", "--jobs", "8")

	if sequential != parallel {
		t.Errorf("--jobs 1 gave %q, --jobs 8 gave %q", sequential, parallel)
	}
}

func TestParallelRendersDoNotInterleaveHelmOutput(t *testing.T) {
	requireHelm(t)

	// Every helm invocation writes to its own buffer, so concurrent renders
	// cannot interleave on the terminal. The failing chart's reason must still
	// arrive whole and attributed.
	code, stdout, stderr := invoke(t, "testdata/broken", "--jobs", "8")

	if code != exitFatal {
		t.Fatalf("exit = %d, want %d", code, exitFatal)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want helm's output captured rather than leaked", stderr)
	}
	if !strings.Contains(stdout, "auth.password is required") {
		t.Errorf("stdout = %q, want the reason attributed to its chart", stdout)
	}
}

func TestAChartWithNoLookupIsReportedAsAChartDefect(t *testing.T) {
	requireHelm(t)

	// randAlphaNum with nothing guarding it: no engine can stabilise this, so
	// the answer is "file an upstream issue", not "add an ignoreDifferences
	// block". Telling those apart is the point of three-engine verdicts.
	_, stdout, _ := invoke(t, "testdata/churn")

	for _, want := range []string{"argocd", "flux", "helm", "CHURNS", "No `lookup` anywhere", "upstream"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestAChartThatCallsLookupIsUnknownForFluxAndHelm(t *testing.T) {
	requireHelm(t)

	// The README's opening idiom. ArgoCD churns as an observed fact; whether
	// that lookup guards this value under Flux or Helm is not something idem
	// will guess at.
	_, stdout, _ := invoke(t, "testdata/guarded")

	if !strings.Contains(stdout, "CHURNS") {
		t.Errorf("stdout = %q, want ArgoCD's observed verdict", stdout)
	}
	if !strings.Contains(stdout, "unknown") {
		t.Errorf("stdout = %q, want unknown for the lookup-resolving engines", stdout)
	}
	if !strings.Contains(stdout, "templates/secret.yaml") {
		t.Errorf("stdout = %q, want the lookup located as evidence", stdout)
	}
	if strings.Contains(stdout, "No `lookup` anywhere") {
		t.Errorf("stdout = %q, want no chart-defect claim - this chart does call lookup", stdout)
	}
}

func TestEngineFlagSelectsASingleEngine(t *testing.T) {
	requireHelm(t)

	_, stdout, _ := invoke(t, "testdata/churn", "--engine", "flux")

	if !strings.Contains(stdout, "flux") {
		t.Errorf("stdout = %q, want the flux verdict", stdout)
	}
	if strings.Contains(stdout, "every sync, forever") {
		t.Errorf("stdout = %q, want no argocd row when only flux was asked for", stdout)
	}
}

func TestEngineFlagRejectsAnUnknownEngine(t *testing.T) {
	code, _, stderr := invoke(t, "testdata/churn", "--engine", "fleet")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stderr, "fleet") || !strings.Contains(stderr, "argocd") {
		t.Errorf("stderr = %q, want the bad value and the valid ones", stderr)
	}
}

func TestACleanChartGetsNoVerdictBlock(t *testing.T) {
	requireHelm(t)

	_, stdout, _ := invoke(t, "testdata/clean")

	if strings.Contains(stdout, "CHURNS") {
		t.Errorf("stdout = %q, want no verdicts when there is nothing to explain", stdout)
	}
}

func TestAutoUsesTheEnginesTheRepositoryActuallyUses(t *testing.T) {
	got, err := selectEngines("auto", []string{"argocd"})
	if err != nil {
		t.Fatalf("selectEngines() error = %v", err)
	}
	if strings.Join(got, ",") != "argocd" {
		t.Errorf("selectEngines() = %v, want just argocd", got)
	}
}

func TestAutoShowsAllThreeWhenNoDeliveryConfigWasFound(t *testing.T) {
	// No signal either way is exactly when you are evaluating a chart and want
	// to know what every engine would do with it.
	got, err := selectEngines("auto", nil)
	if err != nil {
		t.Fatalf("selectEngines() error = %v", err)
	}
	if len(got) != 3 {
		t.Errorf("selectEngines() = %v, want all three", got)
	}
}

func TestAnExplicitEngineOverridesWhatWasDetected(t *testing.T) {
	got, err := selectEngines("flux", []string{"argocd"})
	if err != nil {
		t.Fatalf("selectEngines() error = %v", err)
	}
	if strings.Join(got, ",") != "flux" {
		t.Errorf("selectEngines() = %v, want flux", got)
	}
}

func TestAutoFallsBackToAllThreeWhenTheDetectedEngineIsUnknown(t *testing.T) {
	// A kind idem does not model must not narrow the answer to nothing.
	got, err := selectEngines("auto", []string{"fleet"})
	if err != nil {
		t.Fatalf("selectEngines() error = %v", err)
	}
	if len(got) != 3 {
		t.Errorf("selectEngines() = %v, want all three", got)
	}
}

func TestSelectEnginesStillRejectsAnUnknownFlag(t *testing.T) {
	if _, err := selectEngines("fleet", nil); err == nil {
		t.Error("selectEngines() error = nil, want the bad flag rejected")
	}
}

func TestJSONOutputParses(t *testing.T) {
	requireHelm(t)

	_, stdout, _ := invoke(t, "testdata/churn", "-o", "json")

	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output does not parse as JSON: %v\n%s", err, stdout)
	}
	if _, ok := got["findings"]; !ok {
		t.Errorf("JSON = %v, want a findings key", got)
	}
}

func TestAMachineFormatIsNotCorruptedByTheExitLine(t *testing.T) {
	requireHelm(t)

	// The text form appends "exit 2 — a chart could not be rendered". Doing
	// that to JSON would leave it unparseable for the tool consuming it.
	code, stdout, _ := invoke(t, "testdata/broken", "-o", "json")

	if code != exitFatal {
		t.Fatalf("exit = %d, want %d", code, exitFatal)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Errorf("output does not parse as JSON: %v\n%s", err, stdout)
	}
}

func TestMarkdownOutputIsEmptyForACleanRun(t *testing.T) {
	requireHelm(t)

	_, stdout, _ := invoke(t, "testdata/clean", "-o", "markdown")

	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty so CI posts no comment", stdout)
	}
}

func TestGitHubOutputEmitsWorkflowCommands(t *testing.T) {
	requireHelm(t)

	_, stdout, _ := invoke(t, "testdata/churn", "-o", "github")

	if !strings.Contains(stdout, "::") {
		t.Errorf("stdout = %q, want workflow commands", stdout)
	}
}

func TestAnUnknownOutputFormatIsRejected(t *testing.T) {
	code, _, stderr := invoke(t, "testdata/clean", "-o", "sarif")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	for _, want := range []string{"sarif", "json", "markdown", "github"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to mention %q", stderr, want)
		}
	}
}
