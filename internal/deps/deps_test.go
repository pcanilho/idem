package deps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// chartWith writes a chart declaring deps, and vendoring the named files.
func chartWith(t *testing.T, declares []string, vendored ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "parent")

	var body strings.Builder
	body.WriteString("apiVersion: v2\nname: parent\nversion: 0.1.0\n")
	if len(declares) > 0 {
		body.WriteString("dependencies:\n")
		for _, d := range declares {
			body.WriteString("  - name: " + d + "\n    version: 1.0.0\n    repository: https://example.com\n")
		}
	}
	write(t, filepath.Join(dir, "Chart.yaml"), body.String())

	for _, v := range vendored {
		write(t, filepath.Join(dir, "charts", v), "x")
	}
	return dir
}

func missing(t *testing.T, dir string) []string {
	t.Helper()
	got, err := Missing(dir)
	if err != nil {
		t.Fatalf("Missing() error = %v", err)
	}
	return got
}

func TestAChartWithNoDependenciesNeedsNothing(t *testing.T) {
	// The overwhelming majority of charts, and it must cost nothing.
	if got := missing(t, chartWith(t, nil)); len(got) != 0 {
		t.Errorf("Missing() = %v, want none", got)
	}
}

func TestVendoredArchivesCountAsPresent(t *testing.T) {
	// A GitOps monorepo commits charts/*.tgz, and that is the whole story.
	dir := chartWith(t, []string{"common", "redis"}, "common-2.4.0.tgz", "redis-19.0.1.tgz")

	if got := missing(t, dir); len(got) != 0 {
		t.Errorf("Missing() = %v, want none", got)
	}
}

func TestAnUnpackedSubchartDirectoryCountsAsPresent(t *testing.T) {
	dir := chartWith(t, []string{"common"})
	write(t, filepath.Join(dir, "charts", "common", "Chart.yaml"), "apiVersion: v2\nname: common\n")

	if got := missing(t, dir); len(got) != 0 {
		t.Errorf("Missing() = %v, want none", got)
	}
}

func TestADeclaredDependencyWithNothingVendoredIsMissing(t *testing.T) {
	dir := chartWith(t, []string{"common", "home", "lab"}, "common-2.4.0.tgz")

	got := missing(t, dir)

	for _, want := range []string{"home", "lab"} {
		if !slices.Contains(got, want) {
			t.Errorf("Missing() = %v, want %q", got, want)
		}
	}
	if slices.Contains(got, "common") {
		t.Errorf("Missing() = %v, want common treated as present", got)
	}
}

func TestAChartNameContainingAHyphenIsMatched(t *testing.T) {
	// postgresql-ha-9.4.1.tgz is the chart "postgresql-ha" at 9.4.1, not
	// "postgresql" at "ha-9.4.1". Splitting at the first hyphen loses this.
	dir := chartWith(t, []string{"postgresql-ha"}, "postgresql-ha-9.4.1.tgz")

	if got := missing(t, dir); len(got) != 0 {
		t.Errorf("Missing() = %v, want the hyphenated name matched", got)
	}
}

func TestAnArchiveWithNoVersionStillMatches(t *testing.T) {
	dir := chartWith(t, []string{"common"}, "common.tgz")

	if got := missing(t, dir); len(got) != 0 {
		t.Errorf("Missing() = %v, want none", got)
	}
}

// --- Prepare ---

// builder records which command was asked for, and where.
type builder struct {
	built, updated string
	err            error
}

func (b *builder) DependencyBuild(_ context.Context, dir string) error {
	b.built = dir
	return b.err
}

func (b *builder) DependencyUpdate(_ context.Context, dir string) error {
	b.updated = dir
	return b.err
}

// refuser fails the test if helm is invoked at all.
type refuser struct{ t *testing.T }

func (r refuser) DependencyBuild(context.Context, string) error {
	r.t.Error("DependencyBuild called, want no helm invocation")
	return nil
}

func (r refuser) DependencyUpdate(context.Context, string) error {
	r.t.Error("DependencyUpdate called, want no helm invocation")
	return nil
}

func TestPrepareLeavesAVendoredChartAlone(t *testing.T) {
	dir := chartWith(t, []string{"common"}, "common-2.4.0.tgz")

	got, kind, cleanup, err := TempDir.Prepare(context.Background(), dir, refuser{t})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got != dir {
		t.Errorf("Prepare() = %q, want the chart itself", got)
	}
	if kind != Vendored {
		t.Errorf("Kind = %v, want Vendored", kind)
	}
	if cleanup != nil {
		t.Error("cleanup is set, want none - nothing was created")
	}
}

func TestPrepareCopiesTheChartRatherThanTouchingIt(t *testing.T) {
	// idem never writes to your repository unless asked. A linter that leaves
	// git status dirty is a linter people stop running.
	dir := chartWith(t, []string{"common"})

	b := &builder{}
	got, kind, cleanup, err := TempDir.Prepare(context.Background(), dir, b)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	if got == dir || b.updated == dir || b.built == dir {
		t.Errorf("Prepare() worked in %q / touched %q %q, want neither to be the chart", got, b.built, b.updated)
	}
	if kind != Resolved {
		t.Errorf("Kind = %v, want Resolved", kind)
	}
	// The copy has to be a real chart, or helm cannot build anything in it.
	if _, err := os.Stat(filepath.Join(got, "Chart.yaml")); err != nil {
		t.Errorf("copied chart has no Chart.yaml: %v", err)
	}
}

func TestPrepareDiscardsTheCopyAfterwards(t *testing.T) {
	dir := chartWith(t, []string{"common"})

	got, _, cleanup, err := TempDir.Prepare(context.Background(), dir, &builder{})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	cleanup()

	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("temp copy still exists at %q", got)
	}
}

func TestPrepareInPlaceUsesTheChartItself(t *testing.T) {
	dir := chartWith(t, []string{"common"})

	b := &builder{}
	got, kind, _, err := InPlace.Prepare(context.Background(), dir, b)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got != dir || b.updated != dir {
		t.Errorf("Prepare() = %q, updated %q; want the chart itself", got, b.updated)
	}
	if kind != Updated {
		t.Errorf("Kind = %v, want Updated", kind)
	}
}

func TestPrepareWithNoDepsRefusesAndSaysWhatToRun(t *testing.T) {
	// Airgapped, or a run that must be byte-reproducible. The chart becomes
	// unevaluable, with the command that would fix it.
	dir := chartWith(t, []string{"common", "home"})

	_, _, _, err := Never.Prepare(context.Background(), dir, refuser{t})

	if err == nil {
		t.Fatal("Prepare() error = nil, want a refusal")
	}
	for _, want := range []string{"common", "home", "helm dependency build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestPrepareCleansUpWhenTheBuildFails(t *testing.T) {
	// A failed fetch must not leave a temp tree behind on every run.
	dir := chartWith(t, []string{"common"})

	got, _, cleanup, err := TempDir.Prepare(context.Background(), dir, &builder{err: errors.New("no such repo")})

	if err == nil {
		t.Fatal("Prepare() error = nil, want the build failure")
	}
	if cleanup != nil {
		t.Error("cleanup returned alongside an error, want it already run")
	}
	if got != "" {
		t.Errorf("Prepare() = %q, want no directory on failure", got)
	}
}

func TestALockedChartIsBuiltRatherThanReResolved(t *testing.T) {
	// Chart.lock pins the versions the repository was tested against.
	// Re-resolving could quietly pull a different one.
	dir := chartWith(t, []string{"common"})
	write(t, filepath.Join(dir, "Chart.lock"), "dependencies:\n  - name: common\n    version: 2.4.0\n")

	b := &builder{}
	if _, _, cleanup, err := TempDir.Prepare(context.Background(), dir, b); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	} else if cleanup != nil {
		defer cleanup()
	}

	if b.built == "" {
		t.Error("DependencyBuild was not called, want the lock respected")
	}
	if b.updated != "" {
		t.Errorf("DependencyUpdate called on %q, want the lock respected", b.updated)
	}
}

func TestAnUnlockedChartIsUpdatedBecauseBuildWouldFail(t *testing.T) {
	dir := chartWith(t, []string{"common"})

	b := &builder{}
	if _, _, cleanup, err := TempDir.Prepare(context.Background(), dir, b); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	} else if cleanup != nil {
		defer cleanup()
	}

	if b.updated == "" {
		t.Error("DependencyUpdate was not called; build would have failed with no lock file")
	}
}

func TestSiblingsAFileDependencyPointsAtAreCopiedToo(t *testing.T) {
	// An umbrella chart in a monorepo depends on its siblings by relative
	// path. Copying only the chart leaves file://../common pointing at
	// nothing, and helm fails with an error about a missing repository.
	root := t.TempDir()
	write(t, filepath.Join(root, "common", "Chart.yaml"), "apiVersion: v2\nname: common\nversion: 0.0.1\n")
	write(t, filepath.Join(root, "entrypoint", "Chart.yaml"),
		"apiVersion: v2\nname: entrypoint\nversion: 0.0.1\n"+
			"dependencies:\n  - name: common\n    version: 0.0.1\n    repository: file://../common\n")

	got, _, cleanup, err := TempDir.Prepare(context.Background(), filepath.Join(root, "entrypoint"), &builder{})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	// The sibling must land where "../common" resolves to from the copy.
	if _, err := os.Stat(filepath.Join(got, "..", "common", "Chart.yaml")); err != nil {
		t.Errorf("sibling not copied alongside: %v", err)
	}
}

func TestSiblingsAreCopiedTransitively(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "base", "Chart.yaml"), "apiVersion: v2\nname: base\nversion: 0.0.1\n")
	write(t, filepath.Join(root, "common", "Chart.yaml"),
		"apiVersion: v2\nname: common\nversion: 0.0.1\n"+
			"dependencies:\n  - name: base\n    version: 0.0.1\n    repository: file://../base\n")
	write(t, filepath.Join(root, "top", "Chart.yaml"),
		"apiVersion: v2\nname: top\nversion: 0.0.1\n"+
			"dependencies:\n  - name: common\n    version: 0.0.1\n    repository: file://../common\n")

	got, _, cleanup, err := TempDir.Prepare(context.Background(), filepath.Join(root, "top"), &builder{})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	if _, err := os.Stat(filepath.Join(got, "..", "base", "Chart.yaml")); err != nil {
		t.Errorf("transitive sibling not copied: %v", err)
	}
}

func TestACycleBetweenLocalChartsTerminates(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "Chart.yaml"),
		"apiVersion: v2\nname: a\nversion: 0.0.1\n"+
			"dependencies:\n  - name: b\n    version: 0.0.1\n    repository: file://../b\n")
	write(t, filepath.Join(root, "b", "Chart.yaml"),
		"apiVersion: v2\nname: b\nversion: 0.0.1\n"+
			"dependencies:\n  - name: a\n    version: 0.0.1\n    repository: file://../a\n")

	_, _, cleanup, err := TempDir.Prepare(context.Background(), filepath.Join(root, "a"), &builder{})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
}

func TestADependencyEscapingTheTreeIsRefused(t *testing.T) {
	// "file://../../../etc" would place a copy outside the temp root, which is
	// neither useful nor somewhere idem should be writing.
	root := t.TempDir()
	write(t, filepath.Join(root, "chart", "Chart.yaml"),
		"apiVersion: v2\nname: chart\nversion: 0.0.1\n"+
			"dependencies:\n  - name: x\n    version: 0.0.1\n    repository: file://../../../x\n")

	_, _, _, err := TempDir.Prepare(context.Background(), filepath.Join(root, "chart"), &builder{})

	if err == nil {
		t.Fatal("Prepare() error = nil, want the escape refused")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("error = %q, want it to say why", err)
	}
}

func TestADependencyInsideTheChartIsNotCopiedTwice(t *testing.T) {
	// A chart can vendor a subchart in its own tree and depend on it by
	// relative path: file://modules/x/helm/x. Copying the chart already
	// brought it along, and copying it again fails outright with "file
	// exists" - which surfaced as an unrenderable chart on a real estate.
	root := t.TempDir()
	write(t, filepath.Join(root, "lab", "Chart.yaml"),
		"apiVersion: v2\nname: lab\nversion: 0.0.1\n"+
			"dependencies:\n  - name: mirror\n    version: 0.0.1\n    repository: \"file://modules/mirror\"\n")
	write(t, filepath.Join(root, "lab", "modules", "mirror", "Chart.yaml"), "apiVersion: v2\nname: mirror\n")
	write(t, filepath.Join(root, "lab", "modules", "mirror", ".yamllint"), "rules: {}\n")

	got, _, cleanup, err := TempDir.Prepare(context.Background(), filepath.Join(root, "lab"), &builder{})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	if _, err := os.Stat(filepath.Join(got, "modules", "mirror", "Chart.yaml")); err != nil {
		t.Errorf("nested dependency missing from the copy: %v", err)
	}
}

func TestAVendoredArchiveWithAComplexVersionIsRecognised(t *testing.T) {
	// jupyterhub-4.2.1-0.dev.git.7086.hd53454d1.tgz is the chart "jupyterhub".
	// Guessing the name by stripping at the last hyphen-before-a-digit gives
	// "jupyterhub-4.2.1", so the dependency reads as missing and idem copies a
	// 19MB chart tree to resolve something that was never absent.
	dir := chartWith(t, []string{"jupyterhub"}, "jupyterhub-4.2.1-0.dev.git.7086.hd53454d1.tgz")

	if got := missing(t, dir); len(got) != 0 {
		t.Errorf("Missing() = %v, want none - the archive is right there", got)
	}
}

func TestAPrereleaseVersionIsRecognised(t *testing.T) {
	dir := chartWith(t, []string{"crossplane"}, "crossplane-2.4.0-rc.0.223.g2d040d2da.tgz")

	if got := missing(t, dir); len(got) != 0 {
		t.Errorf("Missing() = %v, want none", got)
	}
}

func TestADifferentChartSharingAPrefixDoesNotSatisfyTheDependency(t *testing.T) {
	dir := chartWith(t, []string{"redis"}, "redisinsight-1.0.0.tgz")

	if got := missing(t, dir); !slices.Contains(got, "redis") {
		t.Errorf("Missing() = %v, want redis still missing", got)
	}
}
