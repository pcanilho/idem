package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// action.yml is the channel idem is most likely to be adopted through, and it
// is the one interface file the Go build cannot see.
//
// docs_test.go pins the README's console blocks, docs/usage.md's flag table and
// docs/ci.md's comment recipe; hooks_test.go pins .pre-commit-hooks.yaml. The
// action had none of that, and it showed: its exit-code annotation was
// unreachable code, because `shell: bash` runs with -e and the script read
// `idem ...; status=$?`. Nothing in the repository could notice, so the tests
// below execute the step's script rather than only reading it.

// action is the subset of action.yml worth asserting on.
type action struct {
	Inputs map[string]struct {
		Description string `yaml:"description"`
		Default     string `yaml:"default"`
	} `yaml:"inputs"`
	Outputs map[string]struct {
		Description string `yaml:"description"`
		Value       string `yaml:"value"`
	} `yaml:"outputs"`
	Runs struct {
		Using string `yaml:"using"`
		Steps []struct {
			Name string `yaml:"name"`
			ID   string `yaml:"id"`
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"runs"`
}

func loadAction(t *testing.T) action {
	t.Helper()

	body, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatalf("reading action.yml: %v", err)
	}
	var a action
	if err := yaml.Unmarshal(body, &a); err != nil {
		t.Fatalf("action.yml does not parse: %v", err)
	}
	if len(a.Inputs) == 0 {
		t.Fatal("action.yml declares no inputs")
	}
	if len(a.Runs.Steps) == 0 {
		t.Fatal("action.yml declares no steps")
	}
	return a
}

// step returns the script of the step with the given id.
func step(t *testing.T, a action, id string) string {
	t.Helper()
	for _, s := range a.Runs.Steps {
		if s.ID == id {
			return s.Run
		}
	}
	t.Fatalf("action.yml has no step with id %q", id)
	return ""
}

// installScript returns the body of the install step, which carries no id.
func installScript(t *testing.T, a action) string {
	t.Helper()
	for _, s := range a.Runs.Steps {
		if strings.Contains(s.Run, "GITHUB_PATH") {
			return s.Run
		}
	}
	t.Fatal("action.yml has no step that puts idem on PATH")
	return ""
}

// --- executing the run step ------------------------------------------------

// stubIdem stands in for the binary so the SHELL can be tested on its own.
//
// It records the argv it was handed, which is the only way to assert that
// `args:` word-splits into separate arguments and that -o is appended after
// them rather than folded into the string.
const stubIdem = `#!/bin/sh
printf '%s\n' "$@" > "$STUB_ARGV"
if [ -n "$STUB_STDOUT" ]; then printf '%s\n' "$STUB_STDOUT"; fi
if [ -n "$STUB_STDERR" ]; then printf '%s\n' "$STUB_STDERR" >&2; fi
exit "$STUB_CODE"
`

type stepResult struct {
	code      int
	stdout    string
	stderr    string
	outputs   map[string]string
	argv      []string
	ranIdem   bool
	workspace string
}

// runStep executes the action's run step the way GitHub does.
//
// The flags are GitHub's own - `bash --noprofile --norc -eo pipefail {0}`, from
// the workflow-syntax shell table - and getting them right is the entire point:
// under -e a non-zero idem ends the script where it stands, and every line the
// action writes after that is dead.
func runStep(t *testing.T, args, output, reportFile string, code int, stdout, stderr string) stepResult {
	t.Helper()
	requireBash(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	ws := filepath.Join(dir, "workspace")
	runnerTemp := filepath.Join(dir, "runner-temp")
	for _, d := range []string{bin, ws, runnerTemp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("preparing %s: %v", d, err)
		}
	}

	if err := os.WriteFile(filepath.Join(bin, "idem"), []byte(stubIdem), 0o755); err != nil {
		t.Fatalf("writing the idem stub: %v", err)
	}

	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte(step(t, loadAction(t), "run")), 0o644); err != nil {
		t.Fatalf("writing the step script: %v", err)
	}

	githubOutput := filepath.Join(dir, "github-output")
	argv := filepath.Join(dir, "argv")

	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", script)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ARGS="+args,
		"OUTPUT="+output,
		"REPORT_FILE="+reportFile,
		"RUNNER_TEMP="+runnerTemp,
		"GITHUB_WORKSPACE="+ws,
		"GITHUB_OUTPUT="+githubOutput,
		"STUB_ARGV="+argv,
		"STUB_CODE="+fmt.Sprint(code),
		"STUB_STDOUT="+stdout,
		"STUB_STDERR="+stderr,
	)

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()

	got := stepResult{stdout: outBuf.String(), stderr: errBuf.String(), workspace: ws, outputs: map[string]string{}}
	if err != nil {
		var exit *exec.ExitError
		if !asExitError(err, &exit) {
			t.Fatalf("running the step script: %v (stderr: %s)", err, got.stderr)
		}
		got.code = exit.ExitCode()
	}

	if body, err := os.ReadFile(githubOutput); err == nil {
		for line := range strings.SplitSeq(string(body), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				got.outputs[k] = v
			}
		}
	}
	if body, err := os.ReadFile(argv); err == nil {
		got.ranIdem = true
		for line := range strings.SplitSeq(strings.TrimSuffix(string(body), "\n"), "\n") {
			got.argv = append(got.argv, line)
		}
	}
	return got
}

func asExitError(err error, out **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*out = e
	}
	return ok
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
}

// --- behaviour -------------------------------------------------------------

// Exit 2 has to be ANNOTATED, and this is the test that could have said so.
//
// The step ran `idem ...` and then `status=$?`, under the -e that `shell: bash`
// supplies and that `set -uo pipefail` does not clear. Every line after the
// idem call was unreachable on a non-zero exit, so the annotation had never
// once printed. The exit code propagated regardless, which is exactly why
// reading the file did not reveal it.
func TestTheActionAnnotatesAFatalExitWithIdemsOwnDiagnostic(t *testing.T) {
	const diagnostic = "idem: flag provided but not defined: -nope"
	got := runStep(t, "./charts --nope", "github", "", exitFatal, "", diagnostic)

	if got.code != exitFatal {
		t.Fatalf("exit = %d, want %d", got.code, exitFatal)
	}
	if !strings.Contains(got.stdout, "::error::") {
		t.Fatalf("a fatal exit printed no ::error:: annotation:\nstdout: %s\nstderr: %s", got.stdout, got.stderr)
	}

	// And it must say what idem said. Exit 2 is not only "could not render":
	// a typo in `args:`, an absent helm and a shallow checkout all reach it,
	// and the old wording told those users their chart was broken.
	if !strings.Contains(got.stdout, "flag provided but not defined") {
		t.Errorf("the annotation does not carry idem's diagnostic:\n%s", got.stdout)
	}
}

// A finding under --strict is a deliberate gate, not idem failing, so exit 1
// must reach the caller unannotated.
func TestTheActionDoesNotAnnotateAStrictFindingAsAFailure(t *testing.T) {
	got := runStep(t, "./charts --strict", "github", "", exitFinding, "", "")

	if got.code != exitFinding {
		t.Fatalf("exit = %d, want %d", got.code, exitFinding)
	}
	if strings.Contains(got.stdout, "::error::idem exited") {
		t.Errorf("exit %d was annotated as an idem failure:\n%s", exitFinding, got.stdout)
	}
}

// The outputs exist so a caller does not have to read `steps.x.outcome` and
// guess. Writing them after the idem call is the same unreachable-code trap as
// the annotation, so this is asserted on a non-zero exit on purpose.
func TestTheActionReportsIdemsExitCodeAsAnOutput(t *testing.T) {
	for _, code := range []int{exitOK, exitFinding, exitFatal} {
		got := runStep(t, "./charts", "text", "", code, "", "boom")
		if want := fmt.Sprint(code); got.outputs["exit-code"] != want {
			t.Errorf("exit-code output = %q, want %q (exit %d)", got.outputs["exit-code"], want, code)
		}
	}
}

// `args:` is a list of arguments, not a string, and -o is idem's to receive
// last. A quoted expansion would hand idem one operand containing spaces.
func TestTheActionPassesArgsThroughAsSeparateArguments(t *testing.T) {
	got := runStep(t, "./charts --strict --jobs 4", "json", "", exitOK, "", "")

	want := []string{"./charts", "--strict", "--jobs", "4", "-o", "json"}
	if !slices.Equal(got.argv, want) {
		t.Errorf("idem argv = %q, want %q", got.argv, want)
	}
}

// An empty `args:` must not be a green run.
//
// `idem -o github` with no operands prints help and exits 0, so an `args:`
// wired from a step that produced nothing would report success having checked
// nothing. .pre-commit-hooks.yaml pins `entry: idem .` against the identical
// failure; the action had no equivalent.
func TestTheActionRefusesAnEmptyChartReference(t *testing.T) {
	for _, args := range []string{"", "   "} {
		got := runStep(t, args, "github", "", exitOK, "", "")

		if got.code == exitOK {
			t.Errorf("args = %q exited %d; a run that checks nothing must not be green", args, got.code)
		}
		if got.ranIdem {
			t.Errorf("args = %q still invoked idem, with argv %q", args, got.argv)
		}
	}
}

// -o in `args:` is silently overridden by the action's own -o, because the last
// one wins. Refusing is the only option that does not discard what was asked
// for without saying so.
func TestTheActionRefusesAnOutputFormatPassedThroughArgs(t *testing.T) {
	for _, args := range []string{"./charts -o json", "./charts --output json", "./charts -o=json"} {
		got := runStep(t, args, "github", "", exitOK, "", "")

		if got.code == exitOK {
			t.Errorf("args = %q exited %d; the user's -o would have been discarded silently", args, got.code)
		}
	}

	// An argument that merely CONTAINS the spelling is not the flag. Checking
	// the raw string for " -o " instead of each argument would refuse both of
	// these, and neither asks for an output format.
	got := runStep(t, "./charts/-o-notes --set key=-o", "github", "", exitOK, "", "")
	if !got.ranIdem {
		t.Errorf("arguments merely containing the -o spelling were refused as if they were the flag: %q", got.stdout)
	}
}

// The report has to land where hashFiles() can see it, which is the guard the
// documented `gh pr comment` recipe depends on.
func TestTheActionWritesTheReportWhereHashFilesCanSeeIt(t *testing.T) {
	got := runStep(t, "./charts", "markdown", "idem.md", exitOK, "## idem found churn", "")

	body, err := os.ReadFile(filepath.Join(got.workspace, "idem.md"))
	if err != nil {
		t.Fatalf("reading the report the action promised to write: %v", err)
	}
	if !strings.Contains(string(body), "idem found churn") {
		t.Errorf("report file = %q, want idem's stdout", body)
	}
	if got.outputs["report-file"] != "idem.md" {
		t.Errorf("report-file output = %q, want %q", got.outputs["report-file"], "idem.md")
	}

	// Written AND printed: routing the report to a file must not silently
	// stop `-o github` annotations reaching the log, which is where the
	// workflow commands are read from.
	if !strings.Contains(got.stdout, "idem found churn") {
		t.Errorf("the report was written but not echoed to the log:\n%s", got.stdout)
	}
}

// hashFiles reads nothing outside GITHUB_WORKSPACE and returns "" for anything
// else, so an absolute report-file makes the documented guard permanently false
// and the comment is never posted. That exact bug shipped once already, in
// docs/ci.md; refusing the path is how it does not ship again through here.
func TestTheActionLeavesNoEmptyReportBehind(t *testing.T) {
	// hashFiles hashes any file that MATCHES and never looks at its size. The
	// runner's hashFiles.ts sets hasMatch on the file count, so a zero-byte
	// report yields a real digest and the documented
	// `if: hashFiles('idem.md') != ''` guard fires on every run. The redirect
	// creates the file before idem writes anything, so a clean run left one
	// behind and the guard could not tell "nothing to say" from "something".
	got := runStep(t, "./charts", "markdown", "idem.md", exitOK, "", "")

	if got.code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s", got.code, exitOK, got.stderr)
	}
	if _, err := os.Stat(filepath.Join(got.workspace, "idem.md")); !os.IsNotExist(err) {
		t.Errorf("idem.md is still there after a run that wrote nothing; hashFiles will hash it and the comment step will run")
	}
}

func TestTheActionKeepsAReportWithSomethingInIt(t *testing.T) {
	got := runStep(t, "./charts", "markdown", "idem.md", exitFinding, "### idem: 1 of 1 chart will churn", "")

	body, err := os.ReadFile(filepath.Join(got.workspace, "idem.md"))
	if err != nil {
		t.Fatalf("reading the report: %v", err)
	}
	if !strings.Contains(string(body), "will churn") {
		t.Errorf("report = %q, want what idem wrote", body)
	}
}

func TestTheActionRefusesAReportFileOutsideTheWorkspace(t *testing.T) {
	for _, path := range []string{"/tmp/idem.md", "../idem.md", "out/../../idem.md"} {
		got := runStep(t, "./charts", "markdown", path, exitOK, "## churn", "")

		if got.code == exitOK {
			t.Errorf("report-file = %q was accepted; hashFiles() could never see it", path)
		}
	}
}

// --- the manifest against the binary ---------------------------------------

// Every format the action offers has to be one the binary renders, and every
// format the binary renders has to be offered.
//
// Both directions, as docs/usage.md's flag table is checked: an action that
// offers a format idem does not have exits 2 on a typo the user cannot see, and
// a format added to idem and left out of here is invisible to the people most
// likely to want it. The binary's list comes from formatter's own error rather
// than a literal restated here, so there is one source and not two.
func TestTheActionOnlyOffersOutputFormatsTheBinaryRenders(t *testing.T) {
	a := loadAction(t)

	offered := map[string]bool{}
	for line := range strings.SplitSeq(a.Inputs["output"].Description, "\n") {
		// The enum is the leading block; prose follows a blank line.
		if strings.TrimSpace(line) == "" {
			break
		}
		if name, _, _ := strings.Cut(strings.TrimSpace(line), " "); name != "" {
			offered[name] = true
		}
	}
	if len(offered) == 0 {
		t.Fatal("the output input documents no formats")
	}

	_, err := formatter("a format that does not exist")
	if err == nil {
		t.Fatal("formatter accepted a nonsense format")
	}
	_, list, ok := strings.Cut(err.Error(), "valid values are ")
	if !ok {
		t.Fatalf("formatter's error no longer lists the valid values: %v", err)
	}
	renders := map[string]bool{}
	for name := range strings.SplitSeq(list, ", ") {
		renders[strings.TrimSpace(name)] = true
	}

	for name := range offered {
		if !renders[name] {
			t.Errorf("action.yml offers output %q, which the binary does not render", name)
		}
	}
	for name := range renders {
		if !offered[name] {
			t.Errorf("the binary renders %q, which action.yml does not offer", name)
		}
	}

	if d := a.Inputs["output"].Default; !renders[d] {
		t.Errorf("action.yml defaults output to %q, which the binary does not render", d)
	}
}

// The exit codes the action's comment explains, and the code it branches on,
// have to be the constants the binary actually returns.
func TestTheActionsExitCodeCommentMatchesTheConstants(t *testing.T) {
	script := step(t, loadAction(t), "run")

	for _, code := range []int{exitOK, exitFinding, exitFatal} {
		if !strings.Contains(script, fmt.Sprintf("%d = ", code)) {
			t.Errorf("the run step does not explain exit %d", code)
		}
	}
	if want := fmt.Sprintf(`[ "$status" -eq %d ]`, exitFatal); !strings.Contains(script, want) {
		t.Errorf("the run step does not branch on %s, so a fatal exit is not annotated", want)
	}
}

// The 11-line comment above the run step argues that inputs must arrive through
// env: a ${{ }} expression is pasted into the script TEXT before bash parses
// it, so an `args:` wired from a pull-request title could execute on the
// runner. Nothing enforced the property the comment defends, and one careless
// edit is arbitrary command execution in a consumer's repository.
func TestTheActionNeverInterpolatesAnExpressionIntoAShellScript(t *testing.T) {
	for _, s := range loadAction(t).Runs.Steps {
		if s.Run != "" && strings.Contains(s.Run, "${{") {
			t.Errorf("step %q interpolates an expression into its script; pass it through env: instead", s.Name)
		}
	}
}

// A third-party action inside a composite action runs in the CALLER's
// workflow, so a floating tag here is a floating tag in somebody else's
// pipeline - and in their actions allowlist and their Scorecard.
func TestTheActionPinsItsThirdPartyActionsToACommit(t *testing.T) {
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)

	for _, s := range loadAction(t).Runs.Steps {
		if s.Uses == "" {
			continue
		}
		_, ref, ok := strings.Cut(s.Uses, "@")
		if !ok || !sha.MatchString(ref) {
			t.Errorf("step %q uses %q; pin it to a full commit SHA", s.Name, s.Uses)
		}
	}
}

// .goreleaser.yaml's own header calls these names load-bearing and says
// changing either "breaks the action silently, on someone else's CI". Nothing
// was watching the join.
func TestTheActionDownloadsTheAssetNamesGoreleaserPublishes(t *testing.T) {
	body, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatalf("reading .goreleaser.yaml: %v", err)
	}
	var rel struct {
		Builds []struct {
			Goos   []string `yaml:"goos"`
			Goarch []string `yaml:"goarch"`
		} `yaml:"builds"`
		Archives []struct {
			Formats         []string `yaml:"formats"`
			NameTemplate    string   `yaml:"name_template"`
			WrapInDirectory *bool    `yaml:"wrap_in_directory"`
		} `yaml:"archives"`
		Checksum struct {
			NameTemplate string `yaml:"name_template"`
		} `yaml:"checksum"`
	}
	if err := yaml.Unmarshal(body, &rel); err != nil {
		t.Fatalf(".goreleaser.yaml does not parse: %v", err)
	}
	if len(rel.Archives) == 0 || len(rel.Builds) == 0 {
		t.Fatal(".goreleaser.yaml declares no builds or no archives")
	}

	script := installScript(t, loadAction(t))
	archive := rel.Archives[0]

	// goreleaser composes name_template + the format extension; the action
	// composes the same name from uname. They have to agree literally.
	if got, want := archive.NameTemplate, "idem_{{ .Os }}_{{ .Arch }}"; got != want {
		t.Errorf("archive name_template = %q, want %q - the action builds the name itself", got, want)
	}
	if !slices.Equal(archive.Formats, []string{"tar.gz"}) {
		t.Errorf("archive formats = %v, want [tar.gz] - the action runs tar -xzf", archive.Formats)
	}
	if want := `archive="idem_${os}_${arch}.tar.gz"`; !strings.Contains(script, want) {
		t.Errorf("the install step no longer composes %s", want)
	}

	// The action untars straight into a directory it puts on PATH, so a
	// wrapping directory would leave nothing named idem on PATH.
	if archive.WrapInDirectory == nil || *archive.WrapInDirectory {
		t.Error("wrap_in_directory is not false; the action untars onto PATH and needs the binary at the archive root")
	}

	if !strings.Contains(script, rel.Checksum.NameTemplate) {
		t.Errorf("the install step does not download %q", rel.Checksum.NameTemplate)
	}

	// Every platform goreleaser builds must be one the action's uname guard
	// accepts, and vice versa - the guard is what turns an unbuilt platform
	// into an honest message instead of a checksum mismatch.
	for _, b := range rel.Builds {
		for _, os := range b.Goos {
			if !strings.Contains(script, os) {
				t.Errorf("goreleaser builds %s, which the install step's uname guard does not accept", os)
			}
		}
		for _, arch := range b.Goarch {
			if !strings.Contains(script, "arch="+arch) {
				t.Errorf("goreleaser builds %s, which the install step's uname guard does not map to", arch)
			}
		}
	}
}

// --- the manifest against the docs -----------------------------------------

// Every input and output the action declares has to be documented, and every
// one documented has to exist.
//
// Both directions, exactly as TestTheFlagTableAndTheBinaryAgree checks the flag
// table: `output` and `working-directory` existed for months and appeared in no
// document as inputs at all, which is the same drift docs_test.go was built to
// catch, one level up.
func TestTheActionsInputsAndOutputsAreDocumented(t *testing.T) {
	a := loadAction(t)
	doc := read(t, "docs/ci.md")

	for _, tc := range []struct {
		what    string
		header  string
		declare map[string]bool
	}{
		{"input", "| Input | What it does |", keys(a.Inputs)},
		{"output", "| Output | What it is |", keys(a.Outputs)},
	} {
		documented := documentedCells(doc, tc.header)
		if len(documented) == 0 {
			t.Fatalf("docs/ci.md has no %s table (looked for %q)", tc.what, tc.header)
		}
		for name := range tc.declare {
			if !documented[name] {
				t.Errorf("action.yml declares the %s %q, which docs/ci.md does not document", tc.what, name)
			}
		}
		for name := range documented {
			if !tc.declare[name] {
				t.Errorf("docs/ci.md documents the %s %q, which action.yml does not declare", tc.what, name)
			}
		}
	}
}

// keys is the name set of a map, whatever it holds.
func keys[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// documentedCells is the set of backticked names in the first column of the
// table under header.
//
// The first cell only: descriptions cite other names, and counting those would
// let a documented-but-missing entry hide behind a mention of itself.
func documentedCells(doc, header string) map[string]bool {
	_, table, found := strings.Cut(doc, header)
	if !found {
		return nil
	}
	table, _, _ = strings.Cut(table, "\n\n")

	out := map[string]bool{}
	for line := range strings.SplitSeq(table, "\n") {
		cell, _, ok := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		if !ok {
			continue
		}
		parts := strings.Split(cell, "`")
		for i := 1; i < len(parts); i += 2 {
			if name := strings.TrimSpace(parts[i]); name != "" {
				out[name] = true
			}
		}
	}
	return out
}
