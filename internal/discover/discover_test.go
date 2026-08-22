package discover

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// chart writes a minimal Chart.yaml, creating parents.
func chart(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "apiVersion: v2\nversion: 0.1.0\n"
	if name != "" {
		body += "name: " + name + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dirs(charts []Chart) []string {
	out := make([]string, len(charts))
	for i, c := range charts {
		out[i] = c.Dir
	}
	return out
}

func TestChartsTreatsTheRootItselfAsAChart(t *testing.T) {
	root := t.TempDir()
	chart(t, root, "home")

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Charts() found %d charts, want 1: %v", len(got), dirs(got))
	}
	if got[0].Dir != root {
		t.Errorf("Dir = %q, want %q", got[0].Dir, root)
	}
	if got[0].Name != "home" {
		t.Errorf("Name = %q, want %q", got[0].Name, "home")
	}
}

func TestChartsFindsEveryChartInADirectoryOfCharts(t *testing.T) {
	root := t.TempDir()
	chart(t, filepath.Join(root, "home"), "home")
	chart(t, filepath.Join(root, "lab"), "lab")

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Charts() found %d charts, want 2: %v", len(got), dirs(got))
	}
}

func TestChartsDoesNotDescendIntoVendoredSubcharts(t *testing.T) {
	// A GitOps monorepo vendors its dependencies as charts/*. Those are
	// subcharts of the parent, not top-level charts to check in their own
	// right - checking them separately would report findings for objects the
	// parent never renders.
	root := t.TempDir()
	chart(t, filepath.Join(root, "home"), "home")
	chart(t, filepath.Join(root, "home", "charts", "common"), "common")
	chart(t, filepath.Join(root, "home", "charts", "redis"), "redis")

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Charts() found %d charts, want 1 (the parent only): %v", len(got), dirs(got))
	}
	if got[0].Name != "home" {
		t.Errorf("Name = %q, want %q", got[0].Name, "home")
	}
}

func TestChartsSkipsHiddenDirectories(t *testing.T) {
	root := t.TempDir()
	chart(t, filepath.Join(root, "home"), "home")
	chart(t, filepath.Join(root, ".cache", "stale"), "stale")

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Charts() found %d charts, want 1: %v", len(got), dirs(got))
	}
	if got[0].Name != "home" {
		t.Errorf("Name = %q, want %q", got[0].Name, "home")
	}
}

func TestChartsFindsChartsNestedSeveralLevelsDeep(t *testing.T) {
	root := t.TempDir()
	chart(t, filepath.Join(root, "clusters", "prod", "api"), "api")

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Charts() found %d charts, want 1: %v", len(got), dirs(got))
	}
	if got[0].Name != "api" {
		t.Errorf("Name = %q, want %q", got[0].Name, "api")
	}
}

func TestChartsAreReturnedInSortedOrder(t *testing.T) {
	// idem's own output must be deterministic; a tool that reports
	// non-determinism cannot exhibit it. Directory order from the filesystem
	// is not guaranteed on every platform.
	root := t.TempDir()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		chart(t, filepath.Join(root, name), name)
	}

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts() error = %v", err)
	}
	want := []string{
		filepath.Join(root, "alpha"),
		filepath.Join(root, "mid"),
		filepath.Join(root, "zeta"),
	}
	for i := range want {
		if i >= len(got) || got[i].Dir != want[i] {
			t.Fatalf("Charts() = %v, want %v", dirs(got), want)
		}
	}
}

func TestChartNameFallsBackToTheDirectoryNameWhenChartYamlHasNone(t *testing.T) {
	root := t.TempDir()
	chart(t, filepath.Join(root, "unnamed"), "")

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Charts() found %d charts, want 1: %v", len(got), dirs(got))
	}
	if got[0].Name != "unnamed" {
		t.Errorf("Name = %q, want %q (the directory)", got[0].Name, "unnamed")
	}
}

func TestChartsReportsWhenNothingUnderRootIsAChart(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Charts(root)
	if err == nil {
		t.Fatal("Charts() error = nil, want an error naming the path")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("error %q does not name the path searched", err)
	}
}

func TestChartsReportsAMissingPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	if _, err := Charts(root); err == nil {
		t.Fatal("Charts() error = nil, want an error for a path that does not exist")
	}
}

func TestChartsSortGloballyNotPerDirectory(t *testing.T) {
	// WalkDir sorts entries WITHIN a directory and recurses depth-first, which
	// is not the same as sorting the resulting paths. "apps-extra" sorts before
	// "apps/web" ('-' < '/'), but the walk reaches apps/web first because it
	// descends into "apps" before visiting "apps-extra".
	root := t.TempDir()
	chart(t, filepath.Join(root, "apps", "web"), "web")
	chart(t, filepath.Join(root, "apps-extra"), "extra")

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts() error = %v", err)
	}
	want := []string{
		filepath.Join(root, "apps-extra"),
		filepath.Join(root, "apps", "web"),
	}
	for i := range want {
		if i >= len(got) || got[i].Dir != want[i] {
			t.Fatalf("Charts() = %v, want %v", dirs(got), want)
		}
	}
}

// A symlinked chart directory must be found, not silently skipped.
//
// Charts validated its root with os.Stat (which follows symlinks) but walked
// with filepath.WalkDir, whose DirEntry comes from Lstat - so `!d.IsDir()`
// rejected every symlink and a linked chart vanished from the run with no
// warning and no unevaluable count. In a CI gate that is a silent coverage
// gap, which is the failure idem exists to prevent.
//
// Symlinked chart directories are ordinary: a monorepo sharing one chart
// between two estates, or a vendored path linked into place.
func TestASymlinkedChartIsFound(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(t.TempDir(), "linked-chart")
	chart(t, real, "linked")
	chart(t, filepath.Join(root, "plain"), "plain")
	if err := os.Symlink(real, filepath.Join(root, "linked")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts: %v", err)
	}

	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"linked", "plain"}) {
		t.Errorf("Charts() = %v, want both plain and linked", names)
	}
}

// A symlink loop must not hang or recurse forever.
func TestASymlinkLoopTerminates(t *testing.T) {
	root := t.TempDir()
	chart(t, filepath.Join(root, "real"), "real")
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	got, err := Charts(root)
	if err != nil {
		t.Fatalf("Charts: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Charts() found %d charts, want 1: %+v", len(got), got)
	}
}
