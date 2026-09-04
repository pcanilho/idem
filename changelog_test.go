package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The release workflow is the third interface file the Go build cannot see,
// after action.yml and the docs. It is EXECUTED here rather than read, for the
// reason action_test.go gives: an awk program that silently matches nothing
// reads exactly like one that works, and the failure would be a release
// published with empty notes.
type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string         `yaml:"name"`
			Uses string         `yaml:"uses"`
			Run  string         `yaml:"run"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(body, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// workflowStep returns one step by name, and fails rather than skipping if it
// is absent: a step that has been renamed away is exactly the regression these
// tests exist to catch.
func workflowStep(t *testing.T, w workflow, name string) (run string, with map[string]any) {
	t.Helper()
	for _, job := range w.Jobs {
		for _, s := range job.Steps {
			if s.Name == name {
				return s.Run, s.With
			}
		}
	}
	t.Fatalf("release.yml has no step named %q", name)
	return "", nil
}

// extractNotes runs the workflow's own extraction step for one tag, under the
// flags GitHub gives a `run:` body, and returns the notes it wrote.
func extractNotes(t *testing.T, tag string) (notes string, err error) {
	t.Helper()
	requireBash(t)

	run, _ := workflowStep(t, loadWorkflow(t, "release.yml"), "Extract changelog section")

	dir := t.TempDir()
	script := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(script, []byte(run), 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "github_output")
	if err := os.WriteFile(output, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", script)
	cmd.Env = append(os.Environ(),
		"TAG="+tag,
		"RUNNER_TEMP="+dir,
		"GITHUB_OUTPUT="+output,
	)
	if _, runErr := cmd.CombinedOutput(); runErr != nil {
		return "", runErr
	}

	written, readErr := os.ReadFile(filepath.Join(dir, "notes.md"))
	if readErr != nil {
		t.Fatalf("step reported success but wrote no notes: %v", readErr)
	}
	return string(written), nil
}

func TestTheReleaseWorkflowTakesItsNotesFromTheChangelog(t *testing.T) {
	notes, err := extractNotes(t, "v0.1.1")
	if err != nil {
		t.Fatalf("extraction failed: %v", err)
	}

	// The body, and only this version's body. The header line is dropped
	// because the release title already carries the version.
	if strings.Contains(notes, "## [0.1.1]") {
		t.Errorf("notes = %q, want the header line dropped", notes)
	}
	if !strings.Contains(notes, "The missing-values report ignored") {
		t.Errorf("notes = %q, want this version's body", notes)
	}
	// Stopping at the next header is the whole job: without it every release
	// would republish the entire history as its own notes.
	if strings.Contains(notes, "Initial public release") {
		t.Errorf("notes = %q, want it to stop at the next version", notes)
	}
}

func TestTheReleaseWorkflowRefusesATagWithNoChangelogSection(t *testing.T) {
	// Silence here would publish an immutable release with empty notes, and
	// immutable releases cannot be re-cut - so this has to be loud and fatal.
	if _, err := extractNotes(t, "v9.9.9"); err == nil {
		t.Error("extraction succeeded for a version with no section, want it to fail the release")
	}
}

func TestTheReleaseNotesReachGoreleaser(t *testing.T) {
	// Extracting notes nothing consumes is the failure that looks like success:
	// the release still publishes, with goreleaser's generated commit subjects.
	_, with := workflowStep(t, loadWorkflow(t, "release.yml"), "Release")

	args, _ := with["args"].(string)
	if !strings.Contains(args, "--release-notes") {
		t.Errorf("goreleaser args = %q, want the extracted notes passed with --release-notes", args)
	}
}

func TestTheChangelogIsNotGeneratedTwice(t *testing.T) {
	// Two sources of release notes is one too many: whichever loses is dead
	// config that reads as if it were live.
	body, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Changelog struct {
			Disable bool `yaml:"disable"`
		} `yaml:"changelog"`
	}
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Changelog.Disable {
		t.Error("goreleaser still generates a changelog, but release notes come from CHANGELOG.md")
	}
}
