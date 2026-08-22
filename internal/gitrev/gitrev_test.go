package gitrev

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// repo builds a real repository: what idem asserts about git is only true if
// git actually behaves that way.
func repo(t *testing.T) string {
	t.Helper()
	requireGit(t)

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, message string) {
	t.Helper()
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", message)
}

func TestChangedFindsAnEditedFile(t *testing.T) {
	dir := repo(t)
	write(t, dir, "charts/home/values.yaml", "a: 1\n")
	commit(t, dir, "first")
	write(t, dir, "charts/home/values.yaml", "a: 2\n")

	got, err := Changed(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatalf("Changed() error = %v", err)
	}
	if !slices.Contains(got, "charts/home/values.yaml") {
		t.Errorf("Changed() = %v, want the edited file", got)
	}
}

func TestChangedIncludesAnUntrackedFile(t *testing.T) {
	// A chart added in this branch is exactly what the flag exists to catch,
	// and it has no committed state to diff against.
	dir := repo(t)
	write(t, dir, "charts/home/values.yaml", "a: 1\n")
	commit(t, dir, "first")
	write(t, dir, "charts/new/Chart.yaml", "apiVersion: v2\nname: new\n")

	got, err := Changed(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatalf("Changed() error = %v", err)
	}
	if !slices.Contains(got, "charts/new/Chart.yaml") {
		t.Errorf("Changed() = %v, want the untracked chart", got)
	}
}

func TestChangedIgnoresAnIgnoredFile(t *testing.T) {
	dir := repo(t)
	write(t, dir, ".gitignore", "secret.txt\n")
	commit(t, dir, "first")
	write(t, dir, "secret.txt", "x\n")

	got, err := Changed(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatalf("Changed() error = %v", err)
	}
	if slices.Contains(got, "secret.txt") {
		t.Errorf("Changed() = %v, want ignored files left out", got)
	}
}

func TestChangedReportsAnUnknownRevision(t *testing.T) {
	// Silently treating a typo'd revision as "nothing changed" would make the
	// gate pass on everything.
	dir := repo(t)
	write(t, dir, "a.txt", "x\n")
	commit(t, dir, "first")

	if _, err := Changed(context.Background(), dir, "no-such-rev"); err == nil {
		t.Error("Changed() error = nil, want an unknown revision reported")
	}
}

func TestMergeBaseIgnoresWhatLandedOnTheBaseSinceTheBranchWasCut(t *testing.T) {
	// Diffing against the branch tip would blame this branch for other
	// people's commits.
	dir := repo(t)
	write(t, dir, "base.txt", "1\n")
	commit(t, dir, "first")

	run(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "mine.txt", "1\n")
	commit(t, dir, "mine")

	run(t, dir, "checkout", "-q", "main")
	write(t, dir, "theirs.txt", "1\n")
	commit(t, dir, "theirs")
	run(t, dir, "checkout", "-q", "feature")

	base, err := MergeBase(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("MergeBase() error = %v", err)
	}
	got, err := Changed(context.Background(), dir, base)
	if err != nil {
		t.Fatalf("Changed() error = %v", err)
	}

	if !slices.Contains(got, "mine.txt") {
		t.Errorf("Changed() = %v, want this branch's own file", got)
	}
	if slices.Contains(got, "theirs.txt") {
		t.Errorf("Changed() = %v, want the base's later commit excluded", got)
	}
}

func TestTouchesIsDirectoryScoped(t *testing.T) {
	changed := []string{"charts/home/templates/secret.yaml"}

	if !Touches(changed, "charts/home") {
		t.Error("Touches() = false, want the chart examined in full")
	}
	if Touches(changed, "charts/lab") {
		t.Error("Touches() = true for an untouched chart")
	}
}

func TestTouchesDoesNotMatchASiblingSharingAPrefix(t *testing.T) {
	// charts/home-extra is a different chart from charts/home.
	changed := []string{"charts/home-extra/values.yaml"}

	if Touches(changed, "charts/home") {
		t.Error("Touches() = true, want a prefix-sharing sibling excluded")
	}
}

func TestTouchesWithNoDirectoryMatchesNothing(t *testing.T) {
	// A chart outside the repository has no repo-relative path, and must not
	// be treated as changed by everything.
	if Touches([]string{"a.txt"}, "") {
		t.Error("Touches() = true for an empty directory")
	}
}

// A revision is DATA, and git must be told so.
//
// `git diff --name-only <rev>` lets any value starting with `-` be read by git
// as an option, and `git diff --output=FILE` truncates FILE. So
// `--new-from-rev=--output=/path/to/anything` silently destroyed that file
// while idem printed an ordinary report and exit 0 - verified: a sentinel file
// went to zero bytes.
//
// Two guards, because either alone is insufficient: --end-of-options stops git
// reading the value as a flag, and rev-parse --verify stops a value that is a
// PATH from silently disabling the whole gate ("No charts changed since
// charts.", exit 0, ratchet off).
func TestARevisionIsNeverReadAsAGitOption(t *testing.T) {
	root := repo(t)
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("SENTINEL"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Changed(context.Background(), root, "--output="+victim); err == nil {
		t.Error("Changed() accepted an option-shaped revision, want it refused")
	}

	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("victim file is gone: %v", err)
	}
	if string(body) != "SENTINEL" {
		t.Errorf("victim file = %q, want it untouched - git wrote through the revision argument", body)
	}
}

// A path is not a revision. Accepting one turned the ratchet off silently:
// "No charts changed since charts." with every finding hidden and exit 0.
func TestAPathIsNotAcceptedAsARevision(t *testing.T) {
	root := repo(t)

	if _, err := Changed(context.Background(), root, "charts"); err == nil {
		t.Error("Changed() accepted a path as a revision, want it refused")
	}
}
