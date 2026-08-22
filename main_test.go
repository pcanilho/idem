package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/chartref"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/scan"
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
		release{namespace: defaultNamespace, name: "pg"},
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
		release{namespace: defaultNamespace, name: "c", files: []string{"base.yaml", "prod.yaml"}, sets: []string{"a=1"}},
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
	// Keyed on the argocd verdict's own reason rather than on a passing
	// phrase: this assertion went vacuous once the wording changed, which is
	// exactly what it was meant to catch.
	if strings.Contains(stdout, "without cluster access") {
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

func TestNoDepsAndDependencyUpdateContradictEachOther(t *testing.T) {
	code, _, stderr := invoke(t, "testdata/clean", "--no-deps", "--dependency-update")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stderr, "contradict") {
		t.Errorf("stderr = %q, want the conflict explained", stderr)
	}
}

func TestAChartMissingSubchartsUnderNoDepsSaysWhatToRun(t *testing.T) {
	requireHelm(t)

	// Airgapped or byte-reproducible runs fetch nothing. The chart becomes
	// unevaluable rather than silently skipped, with the command that fixes it.
	code, stdout, _ := invoke(t, "testdata/umbrella", "--no-deps")

	if code != exitFatal {
		t.Fatalf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stdout, "helm dependency build") {
		t.Errorf("stdout = %q, want the command named", stdout)
	}
}

func TestAChartMissingSubchartsIsResolvedWithoutTouchingIt(t *testing.T) {
	requireHelm(t)

	// The default path: copy out, resolve there, discard. idem never writes to
	// your repository unless you pass --dependency-update.
	code, stdout, stderr := invoke(t, "testdata/umbrella")

	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "resolved in a temp dir") {
		t.Errorf("stdout = %q, want the resolution reported", stdout)
	}
	if _, err := os.Stat("testdata/umbrella/parent/charts"); err == nil {
		t.Error("testdata/umbrella/parent/charts exists; idem wrote into the chart")
	}
}

func TestTheTwoRatchetFlagsAreTwoWaysToSayOneThing(t *testing.T) {
	code, _, stderr := invoke(t, "testdata/clean", "--new-from-rev", "HEAD", "--new-from-merge-base", "main")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stderr, "give one") {
		t.Errorf("stderr = %q, want the conflict explained", stderr)
	}
}

func TestAnUnknownRevisionIsReportedRatherThanIgnored(t *testing.T) {
	// Treating a typo'd revision as "nothing changed" would quietly make the
	// gate pass on everything.
	code, _, stderr := invoke(t, "testdata/clean", "--new-from-rev", "no-such-revision-xyz")

	if code != exitFatal {
		t.Errorf("exit = %d, want %d", code, exitFatal)
	}
	if !strings.Contains(stderr, "no-such-revision-xyz") {
		t.Errorf("stderr = %q, want the bad revision named", stderr)
	}
}

func requireCluster(t *testing.T) {
	t.Helper()
	requireHelm(t)
	if err := exec.Command("kubectl", "cluster-info").Run(); err != nil {
		t.Skip("no reachable cluster")
	}
}

func TestAnEmptyContextStillAsksTheCluster(t *testing.T) {
	requireCluster(t)

	// --context= names no context but still opts in, which is a different
	// thing from not passing the flag at all. Presence is what decides.
	_, stdout, _ := invoke(t, "testdata/guarded", "--context=")

	if !strings.Contains(stdout, "observed") {
		t.Errorf("stdout = %q, want the cluster consulted", stdout)
	}
	if !strings.Contains(stdout, "current kube context") {
		t.Errorf("stdout = %q, want the provenance to say which cluster", stdout)
	}
}

func TestWithoutTheFlagNoClusterIsTouched(t *testing.T) {
	requireHelm(t)

	_, stdout, _ := invoke(t, "testdata/guarded")

	if strings.Contains(stdout, "kube context") || strings.Contains(stdout, "context ") {
		t.Errorf("stdout = %q, want no cluster mentioned", stdout)
	}
	if !strings.Contains(stdout, "unknown") {
		t.Errorf("stdout = %q, want the verdict left unknown", stdout)
	}
}

func TestClusterTurnsAnUnknownVerdictIntoAMeasurement(t *testing.T) {
	requireCluster(t)

	// Without a cluster, a chart calling lookup leaves Flux and Helm unknown:
	// idem will not guess whether that lookup guards this value. With one, it
	// watches and reports what happened.
	_, without, _ := invoke(t, "testdata/guarded")
	if !strings.Contains(without, "unknown") {
		t.Fatalf("stdout = %q, want unknown without a cluster", without)
	}

	_, with, stderr := invoke(t, "testdata/guarded", "--context", "")
	if strings.Contains(with, "unknown") {
		t.Errorf("stdout = %q, want no unknown once measured (stderr: %s)", with, stderr)
	}
	if !strings.Contains(with, "observed") {
		t.Errorf("stdout = %q, want the verdict marked as observed", with)
	}
	if !strings.Contains(with, "context") {
		t.Errorf("stdout = %q, want the provenance to name the cluster", with)
	}
}

func TestAnUnreachableClusterDoesNotFailTheRun(t *testing.T) {
	requireHelm(t)

	// The chart renders perfectly well without a cluster, and the client-side
	// answer does not depend on one, so a context that does not exist leaves
	// the Flux and Helm verdicts unknown rather than failing everything.
	code, stdout, _ := invoke(t, "testdata/guarded", "--context", "no-such-context-xyz")

	if code == exitFatal {
		t.Errorf("exit = %d, want the run to survive an unreachable cluster", code)
	}
	if !strings.Contains(stdout, "unknown") {
		t.Errorf("stdout = %q, want the verdict left unknown", stdout)
	}
}

func TestTheClusterRewritesAreReported(t *testing.T) {
	requireCluster(t)

	// Admission is synchronous: the rewrite happens between sending an object
	// and it being stored, so only a dry run can show it.
	_, stdout, stderr := invoke(t, "testdata/defaulted", "--context=")

	if !strings.Contains(stdout, "rewrites these on admission") {
		t.Fatalf("stdout = %q, want the admission section (stderr: %s)", stdout, stderr)
	}
	if !strings.Contains(stdout, "cluster assigns") {
		t.Errorf("stdout = %q, want an assigned value distinguished from a default", stdout)
	}
	if !strings.Contains(stdout, "not compared") {
		t.Errorf("stdout = %q, want the uncomparable fields counted rather than hidden", stdout)
	}
}

func TestWithoutAContextTheClusterIsNotAsked(t *testing.T) {
	requireHelm(t)

	_, stdout, _ := invoke(t, "testdata/defaulted")

	if strings.Contains(stdout, "rewrites these on admission") {
		t.Errorf("stdout = %q, want no cluster questions without --context", stdout)
	}
}

func TestChurnVisibleOnlyAgainstTheClusterIsStillReported(t *testing.T) {
	requireCluster(t)

	// The chart is byte-identical under `helm template`, so every count and
	// every section keyed on that condition is zero. Before this was wired,
	// the whole run reported "renders consistently" and said nothing else.
	code, stdout, _ := invoke(t, "testdata/inverted", "--context=")

	if !strings.Contains(stdout, "lookup") {
		t.Errorf("stdout = %q, want the difference reported", stdout)
	}
	if !strings.Contains(stdout, "Flux") {
		t.Errorf("stdout = %q, want the engines that churn named", stdout)
	}
	if strings.Contains(stdout, "✓") {
		t.Errorf("stdout = %q, want no clean tick", stdout)
	}
	if code != exitOK {
		t.Errorf("exit = %d, want %d - findings are informative by default", code, exitOK)
	}
}

func TestChurnVisibleOnlyAgainstTheClusterFailsStrict(t *testing.T) {
	requireCluster(t)

	// --strict is the CI gate. Churn idem observed must trip it whichever
	// condition observed it, or the gate silently exempts Flux and Helm.
	code, _, _ := invoke(t, "testdata/inverted", "--context=", "--strict")

	if code != exitFinding {
		t.Errorf("exit = %d, want %d", code, exitFinding)
	}
}

func TestTheClientConditionKeepsItsOwnVerdict(t *testing.T) {
	requireCluster(t)

	// ArgoCD renders exactly the condition that was identical. Saying it
	// churns would send the reader after churn it will never have.
	_, stdout, _ := invoke(t, "testdata/inverted", "--context=")

	if !strings.Contains(stdout, "argocd") || !strings.Contains(stdout, "stable") {
		t.Errorf("stdout = %q, want argocd reported stable", stdout)
	}
}

// tree writes files into a fresh directory that looks like a repository, so
// delivery discovery has a root to walk and stops there.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files[".git/HEAD"] = "ref: refs/heads/main\n"

	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const ownedChart = `apiVersion: v2
name: owned
version: 0.1.0
`

const ownedTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
  namespace: {{ .Release.Namespace }}
data:
  where: {{ .Release.Namespace }}
`

func TestTheRenderNamespaceComesFromTheApplicationThatOwnsTheChart(t *testing.T) {
	requireHelm(t)

	// Not cosmetic: .Release.Namespace decides the identity idem reports and
	// the identity an ignoreDifferences rule matches against. Taken from the
	// kube context - helm's own default - it would differ between a laptop and
	// CI for the same commit.
	dir := tree(t, map[string]string{
		"apps/owned.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: owned}
spec:
  destination: {namespace: owned-ns}
  source: {path: charts/owned}
`,
		"charts/owned/Chart.yaml":               ownedChart,
		"charts/owned/templates/configmap.yaml": ownedTemplate,
	})

	_, stdout, _ := invoke(t, filepath.Join(dir, "charts/owned"), "-o", "json")

	if !strings.Contains(stdout, "owned-ns") {
		t.Errorf("stdout = %q, want the Application's namespace used", stdout)
	}
}

func TestAChartNoApplicationClaimsRendersIntoAStatedDefault(t *testing.T) {
	requireHelm(t)

	// Whatever idem picks must not depend on the machine it runs on, and it
	// has to be recoverable. Reported through the machine contract rather than
	// the provenance line: "idem defaulted the namespace" is the modal case,
	// and printing it on every run made the two-line success half boilerplate.
	dir := tree(t, map[string]string{
		"charts/owned/Chart.yaml":               ownedChart,
		"charts/owned/templates/configmap.yaml": ownedTemplate,
	})

	_, stdout, _ := invoke(t, filepath.Join(dir, "charts/owned"), "-o", "json")

	if !strings.Contains(stdout, `"namespace": "default"`) {
		t.Errorf("stdout = %q, want the namespace idem chose still recorded", stdout)
	}
	if strings.Contains(stdout, `"from"`) {
		t.Errorf("stdout = %q, want no source claimed for a namespace nobody chose", stdout)
	}
}

func TestTheNamespaceFlagOverridesTheApplication(t *testing.T) {
	requireHelm(t)

	dir := tree(t, map[string]string{
		"apps/owned.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: owned}
spec:
  destination: {namespace: owned-ns}
  source: {path: charts/owned}
`,
		"charts/owned/Chart.yaml":               ownedChart,
		"charts/owned/templates/configmap.yaml": ownedTemplate,
	})

	_, stdout, _ := invoke(t, filepath.Join(dir, "charts/owned"), "--namespace", "elsewhere")

	if !strings.Contains(stdout, "namespace elsewhere (--namespace)") {
		t.Errorf("stdout = %q, want the flag to win and be credited", stdout)
	}
}

const guardedChart = `apiVersion: v2
name: needs
version: 0.1.0
`

// requiredTemplate is the shape the estate uses: a chart that refuses to
// render without a value its Application supplies. That guard working is the
// chart being correct, not the chart being broken.
const requiredTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cm
data:
  cluster: {{ required "cluster is required (the Application supplies it)" .Values.cluster }}
  tag: {{ .Values.image.tag | default "none" }}
`

func TestAChartRendersWithTheValuesItsApplicationSupplies(t *testing.T) {
	requireHelm(t)

	dir := tree(t, map[string]string{
		"apps/needs.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: needs}
spec:
  source:
    path: charts/needs
    helm:
      valuesObject:
        cluster: prod-a
      parameters:
        - name: image.tag
          value: v1.2.3
`,
		"charts/needs/Chart.yaml":        guardedChart,
		"charts/needs/templates/cm.yaml": requiredTemplate,
	})

	code, stdout, stderr := invoke(t, filepath.Join(dir, "charts/needs"))

	if code != exitOK {
		t.Fatalf("exit = %d, want %d — the Application supplies the value\n%s%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "renders consistently") {
		t.Errorf("stdout = %q, want a clean render", stdout)
	}
}

func TestTheReleaseNameComesFromTheApplicationToo(t *testing.T) {
	requireHelm(t)

	// .Release.Name is in the name of nearly every object a chart produces, so
	// taking it from the chart directory reports identities the cluster will
	// never have.
	dir := tree(t, map[string]string{
		"apps/needs.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: needs}
spec:
  source:
    path: charts/needs
    helm:
      releaseName: chosen
      valuesObject: {cluster: prod-a}
`,
		"charts/needs/Chart.yaml":        guardedChart,
		"charts/needs/values.yaml":       "image: {}\n",
		"charts/needs/templates/cm.yaml": requiredTemplate,
	})

	_, stdout, _ := invoke(t, filepath.Join(dir, "charts/needs"), "-o", "json")

	if !strings.Contains(stdout, "chosen") {
		t.Errorf("stdout = %q, want the Application's releaseName used", stdout)
	}
}

func TestAValueFileNamedByTheApplicationIsPassedToHelm(t *testing.T) {
	requireHelm(t)

	dir := tree(t, map[string]string{
		"apps/needs.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: needs}
spec:
  source:
    path: charts/needs
    helm:
      valueFiles:
        - /shared/base.yaml
`,
		"shared/base.yaml":               "cluster: from-a-file\n",
		"charts/needs/Chart.yaml":        guardedChart,
		"charts/needs/values.yaml":       "image: {}\n",
		"charts/needs/templates/cm.yaml": requiredTemplate,
	})

	code, stdout, stderr := invoke(t, filepath.Join(dir, "charts/needs"))

	if code != exitOK {
		t.Fatalf("exit = %d, want %d — a leading slash is repo-root relative\n%s%s", code, exitOK, stdout, stderr)
	}
}

func TestAReleaseIdemCannotBuildIsReportedWithoutFailingTheRun(t *testing.T) {
	requireHelm(t)

	// The chart's `required` guard fires because idem withheld a value the
	// generator supplies. Calling that "could not be rendered" would blame the
	// chart, and exiting 2 would make every estate driven by a cluster-reading
	// generator permanently red.
	dir := tree(t, map[string]string{
		"apps/agents.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: agents}
spec:
  goTemplate: true
  generators:
    - clusters: {}
  template:
    spec:
      source:
        path: charts/needs
        helm:
          valuesObject:
            cluster: '{{ .name }}'
`,
		"charts/needs/Chart.yaml":        guardedChart,
		"charts/needs/values.yaml":       "image: {}\n",
		"charts/needs/templates/cm.yaml": requiredTemplate,
	})

	code, stdout, _ := invoke(t, filepath.Join(dir, "charts/needs"))

	if code != exitOK {
		t.Errorf("exit = %d, want %d — idem could not build it, the chart is fine", code, exitOK)
	}
	if !strings.Contains(stdout, "could not be built") {
		t.Errorf("stdout = %q, want the gap stated as its own kind", stdout)
	}
	if !strings.Contains(stdout, "cluster") {
		t.Errorf("stdout = %q, want the value it lacked named", stdout)
	}
}

func TestAGitFilesGeneratorIsCheckedOncePerElement(t *testing.T) {
	requireHelm(t)

	// Two files, two releases, both rendered from the repository alone.
	dir := tree(t, map[string]string{
		"apps/tenants.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: tenants}
spec:
  goTemplate: true
  generators:
    - git:
        repoURL: https://example.com/repo.git
        files:
          - path: "config/*.yaml"
  template:
    spec:
      source:
        path: charts/needs
        helm:
          releaseName: '{{ .cluster }}'
          valueFiles:
            - '/{{ .path.path }}/{{ .path.filename }}'
`,
		"config/alpha.yaml":              "cluster: alpha\n",
		"config/beta.yaml":               "cluster: beta\n",
		"charts/needs/Chart.yaml":        guardedChart,
		"charts/needs/values.yaml":       "image: {}\n",
		"charts/needs/templates/cm.yaml": requiredTemplate,
	})

	code, stdout, stderr := invoke(t, filepath.Join(dir, "charts/needs"), "-o", "json")

	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, exitOK, stdout, stderr)
	}
	for _, want := range []string{`"release": "alpha"`, `"release": "beta"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want %s — one release per matched file", stdout, want)
		}
	}
}

// A chart rendered from a registry has no source on disk, so `lookup` cannot
// be scanned for and both Flux and Helm degrade to unknown — on exactly the
// charts idem exists for, since the consumer of a third-party chart is the
// user who cannot patch it.

// fakePuller stands in for helm, so the wiring is testable without a network.
type fakePuller struct {
	dir  string
	err  error
	from string
	into string
}

func (p *fakePuller) Pull(_ context.Context, spec engine.Spec, into string) (string, error) {
	p.from, p.into = spec.ChartRef, into
	if p.err != nil {
		return "", p.err
	}
	return p.dir, nil
}

func TestARemoteChartIsFetchedSoItsSourceCanBeScanned(t *testing.T) {
	// The chart the analyzer should end up reading.
	chart := tree(t, map[string]string{
		"pulled/Chart.yaml":            "apiVersion: v2\nname: pulled\nversion: 0.1.0\n",
		"pulled/templates/secret.yaml": "x: {{ lookup \"v1\" \"Secret\" \"\" \"creds\" }}\n",
	})

	p := &fakePuller{dir: filepath.Join(chart, "pulled")}
	inspect := remoteInspector(chartref.Ref{Kind: chartref.RepoURL, Repo: "https://example.com"}, p)

	uses, err := inspect(scan.Chart{Name: "pulled", Dir: "pulled", Spec: engine.Spec{ChartRef: "pulled"}})
	if err != nil {
		t.Fatalf("inspect() error = %v", err)
	}
	if len(analyze.Of(uses, analyze.Lookup)) == 0 {
		t.Errorf("uses = %v, want the lookup found in the fetched source", uses)
	}
	if p.from != "pulled" {
		t.Errorf("pulled %q, want the chart reference idem rendered", p.from)
	}
}

func TestAChartThatCannotBeFetchedStaysAnHonestUnknown(t *testing.T) {
	// A private registry, no network, a bad credential: none of those are
	// evidence that the chart has no lookup, and reporting them as such would
	// turn a failed fetch into a sound CHURNS verdict.
	p := &fakePuller{err: errors.New("unauthorized")}
	inspect := remoteInspector(chartref.Ref{Kind: chartref.OCI}, p)

	_, err := inspect(scan.Chart{Name: "private", Spec: engine.Spec{ChartRef: "oci://example.com/private"}})

	if err == nil {
		t.Fatal("inspect() error = nil, want the fetch failure reported")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("error = %v, want it to say why it could not look", err)
	}
}

func TestTheFetchedCopyIsNotLeftBehind(t *testing.T) {
	// idem downloads a chart to read it, not to keep it. A tool run over an
	// estate would otherwise leave one unpacked chart per remote reference.
	chart := tree(t, map[string]string{
		"pulled/Chart.yaml": "apiVersion: v2\nname: pulled\nversion: 0.1.0\n",
	})

	p := &fakePuller{dir: filepath.Join(chart, "pulled")}
	inspect := remoteInspector(chartref.Ref{Kind: chartref.OCI}, p)

	if _, err := inspect(scan.Chart{Name: "pulled", Spec: engine.Spec{ChartRef: "oci://example.com/pulled"}}); err != nil {
		t.Fatalf("inspect() error = %v", err)
	}
	if p.into == "" {
		t.Fatal("the puller was never given a directory")
	}
	if _, err := os.Stat(p.into); !os.IsNotExist(err) {
		t.Errorf("%s still exists, want the fetched copy removed", p.into)
	}
}

// `--help` is the first thing anyone runs against an unfamiliar binary, and
// people script it to check a binary is sane. Exiting 2 with the text on
// stderr says "you used me wrong" about a request that was correct.

func TestHelpSucceedsAndGoesToStdout(t *testing.T) {
	code, stdout, stderr := invoke(t, "--help")

	if code != exitOK {
		t.Errorf("exit = %d, want %d — asking for help is not an error", code, exitOK)
	}
	if stdout == "" {
		t.Error("stdout is empty, want the help text where a pipe can read it")
	}
	if strings.Contains(stderr, "help requested") {
		t.Errorf("stderr = %q, want no error about a request that succeeded", stderr)
	}
}

func TestHelpNamesTheDoctorVerb(t *testing.T) {
	// doctor is a top-level verb and the easiest thing in the tool to try —
	// it needs no chart, only a cluster you already run. Mentioned nowhere in
	// the help, it is discoverable only by reading 838 lines of README.
	_, stdout, _ := invoke(t, "--help")

	if !strings.Contains(stdout, "idem doctor") {
		t.Errorf("stdout = %q, want the doctor verb shown as a verb", stdout)
	}
}

func TestHelpShowsWhatToActuallyType(t *testing.T) {
	// A flag list tells you what exists, not what to run. The first thing a
	// stranger needs is one line they can paste.
	_, stdout, _ := invoke(t, "--help")

	if !strings.Contains(stdout, "idem ./charts") {
		t.Errorf("stdout = %q, want a runnable example", stdout)
	}
}

func TestAGenuineFlagErrorStillFails(t *testing.T) {
	// The other half of the contract: a real mistake must still be an error,
	// on stderr, with a non-zero exit.
	code, _, stderr := invoke(t, "testdata/clean", "--nonesuch")

	if code == exitOK {
		t.Error("exit = 0, want a real flag error to fail")
	}
	if !strings.Contains(stderr, "nonesuch") {
		t.Errorf("stderr = %q, want the offending flag named", stderr)
	}
}

func TestARemoteChartDoesNotOpenWithADeliveryConfigError(t *testing.T) {
	requireHelm(t)

	// A registry reference is not a path, so there is no repository under it
	// to read a delivery config from — and lstat'ing it fails, loudly, as the
	// first line a stranger sees on the single highest-value first run:
	// deciding whether to adopt someone else's chart.
	//
	// Run from outside any repository, because that is what exposes it: with a
	// .git above the working directory, delivery.Root finds that repo and
	// reads it instead, so the bug hides whenever the tests' own cwd is used.
	t.Chdir(t.TempDir())

	_, _, stderr := invoke(t, "nothing", "--repo", "https://example.invalid/charts")

	if strings.Contains(stderr, "delivery config") {
		t.Errorf("stderr = %q, want no delivery-config complaint about a reference that is not a path", stderr)
	}
}

func TestALocalChartStillReadsItsDeliveryConfig(t *testing.T) {
	requireHelm(t)

	// The other half: the skip must be about the reference being remote, not
	// about giving up on reading delivery config.
	dir := tree(t, map[string]string{
		"apps/needs.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: needs}
spec:
  source:
    path: charts/needs
    helm:
      valuesObject: {cluster: prod-a}
`,
		"charts/needs/Chart.yaml":        guardedChart,
		"charts/needs/values.yaml":       "image: {}\n",
		"charts/needs/templates/cm.yaml": requiredTemplate,
	})

	code, stdout, stderr := invoke(t, filepath.Join(dir, "charts/needs"))

	if code != exitOK {
		t.Fatalf("exit = %d, want the Application's values still read\n%s%s", code, stdout, stderr)
	}
}

// `idem diff a.yaml b.yaml` has been documented since before the CLI existed
// (README.md, docs/design.md §7) and never shipped. It is the comparison
// engine exposed directly — no helm, no network, no cluster — and it is what
// makes kustomize a target: `kustomize build a/ > a.yaml`, twice, then diff.

const renderA = `apiVersion: v1
kind: Secret
metadata: {name: creds, namespace: home}
data: {password: aaa}
`

const renderB = `apiVersion: v1
kind: Secret
metadata: {name: creds, namespace: home}
data: {password: bbb}
`

func TestDiffComparesTwoRendersYouMadeYourself(t *testing.T) {
	dir := tree(t, map[string]string{"a.yaml": renderA, "b.yaml": renderB})

	code, stdout, stderr := invoke(t, "diff", filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml"))

	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "Secret/home/creds") {
		t.Errorf("stdout = %q, want the differing object named", stdout)
	}
	if !strings.Contains(stdout, ".data.password") {
		t.Errorf("stdout = %q, want the differing field named", stdout)
	}
}

func TestDiffNeedsNoHelmAndNoCluster(t *testing.T) {
	// The whole point of the verb: the comparison engine on its own. Pointed
	// at a helm that does not exist, it must still work.
	dir := tree(t, map[string]string{"a.yaml": renderA, "b.yaml": renderB})

	code, _, stderr := invoke(t, "diff", filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml"), "--helm", "/nonexistent/helm")

	if code != exitOK {
		t.Errorf("exit = %d, want %d — diff renders nothing, so it needs no helm: %s", code, exitOK, stderr)
	}
}

func TestDiffOfIdenticalRendersIsClean(t *testing.T) {
	dir := tree(t, map[string]string{"a.yaml": renderA, "b.yaml": renderA})

	code, stdout, _ := invoke(t, "diff", filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml"))

	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if strings.Contains(stdout, "password") {
		t.Errorf("stdout = %q, want nothing reported", stdout)
	}
}

func TestDiffWantsExactlyTwoFiles(t *testing.T) {
	code, _, stderr := invoke(t, "diff", "only-one.yaml")

	if code == exitOK {
		t.Error("exit = 0, want a clear error")
	}
	if !strings.Contains(stderr, "two") {
		t.Errorf("stderr = %q, want it to say how many files it takes", stderr)
	}
}

func TestDiffSaysWhichFileItCouldNotRead(t *testing.T) {
	dir := tree(t, map[string]string{"a.yaml": renderA})

	_, _, stderr := invoke(t, "diff", filepath.Join(dir, "a.yaml"), filepath.Join(dir, "missing.yaml"))

	if !strings.Contains(stderr, "missing.yaml") {
		t.Errorf("stderr = %q, want the unreadable file named", stderr)
	}
}

func TestHelpNamesTheDiffVerbNowThatItExists(t *testing.T) {
	// The rule this whole phase exists to enforce: the help and the README
	// promise only what the binary does.
	_, stdout, _ := invoke(t, "--help")

	if !strings.Contains(stdout, "idem diff") {
		t.Errorf("stdout = %q, want the diff verb shown", stdout)
	}
}
