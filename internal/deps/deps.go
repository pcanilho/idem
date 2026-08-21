// Package deps makes a chart renderable without touching the working tree.
//
// A chart whose subcharts are not present cannot render at all, and a GitOps
// monorepo usually vendors them, so the common case must cost nothing. What it
// must never do is leave `git status` dirty: a linter that edits your
// repository is a linter people stop running.
package deps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode is how far idem may go to make a chart renderable.
type Mode int

const (
	// TempDir resolves missing dependencies in a copy and discards it. The
	// default, because it never writes to the user's repository.
	TempDir Mode = iota
	// InPlace populates the chart's own charts/ directory. --dependency-update.
	InPlace
	// Never fetches nothing. --no-deps, for airgapped or byte-reproducible runs.
	Never
)

// Kind is what idem actually had to do, for the provenance line.
type Kind int

const (
	// Vendored means the chart was renderable as it stands.
	Vendored Kind = iota
	// Resolved means dependencies were fetched into a temp copy.
	Resolved
	// Updated means they were fetched into the chart itself.
	Updated
)

func (k Kind) String() string {
	switch k {
	case Resolved:
		return "resolved in a temp dir"
	case Updated:
		return "resolved in place"
	}
	return "vendored"
}

// Missing lists dependencies declared in Chart.yaml that are not present in
// charts/.
//
// A static check rather than a helm invocation: the answer for a vendored
// chart is two file reads, and the overwhelming majority of charts are either
// vendored or have no dependencies at all. helm's own error is still what
// decides the failure path - this only decides whether to do any work.
func Missing(dir string) ([]string, error) {
	declared, err := declared(dir)
	if err != nil || len(declared) == 0 {
		return nil, err
	}

	present, err := present(dir)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, name := range declared {
		if !slices.ContainsFunc(present, func(entry string) bool { return satisfies(entry, name) }) {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// declared reads the dependency names from Chart.yaml.
func declared(dir string) ([]string, error) {
	body, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		// No readable Chart.yaml is not this package's problem to report:
		// helm will say so far better when it tries to render.
		return nil, nil
	}

	var chart struct {
		Dependencies []struct {
			Name string `yaml:"name"`
		} `yaml:"dependencies"`
	}

	if err := yaml.Unmarshal(body, &chart); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, "Chart.yaml"), err)
	}

	var names []string
	for _, d := range chart.Dependencies {
		if d.Name != "" {
			names = append(names, d.Name)
		}
	}
	return names, nil
}

// present lists what charts/ actually holds, as bare names.
//
// helm accepts a subchart either unpacked as a directory or vendored as
// <name>-<version>.tgz.
func present(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "charts"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			out = append(out, name)
			continue
		}
		if before, ok := strings.CutSuffix(name, ".tgz"); ok {
			out = append(out, before)
		}
	}
	return out, nil
}

// satisfies reports whether a charts/ entry provides the named dependency.
//
// Matched by prefix rather than by parsing a version off the end. Helm version
// strings are not reliably parseable - jupyterhub-4.2.1-0.dev.git.7086.hd53454d1
// is one chart, not a name of "jupyterhub-4.2.1" - and getting it wrong the
// other way is expensive: a dependency wrongly read as absent makes idem copy
// and resolve a whole chart tree that was never missing anything.
//
// The failure direction is deliberate. Over-matching means idem skips work and
// helm reports its own, accurate error; under-matching means idem does a great
// deal of work for nothing.
func satisfies(entry, name string) bool {
	return entry == name || strings.HasPrefix(entry, name+"-")
}

// Builder fetches a chart's subcharts.
type Builder interface {
	// DependencyBuild installs exactly what Chart.lock pins.
	DependencyBuild(ctx context.Context, dir string) error
	// DependencyUpdate re-resolves and rewrites Chart.lock.
	DependencyUpdate(ctx context.Context, dir string) error
}

// resolve fetches subcharts, preferring the pinned versions.
//
// `helm dependency build` installs exactly what Chart.lock says, which keeps a
// run reproducible; but it fails outright when there is no lock file, and
// plenty of charts have none. So the lock decides which command to run rather
// than always re-resolving, which could quietly pull a different version than
// the one the repository was tested against.
func resolve(ctx context.Context, b Builder, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "Chart.lock")); err == nil {
		return b.DependencyBuild(ctx, dir)
	}
	return b.DependencyUpdate(ctx, dir)
}

// localRepos returns the paths of a chart's file:// dependencies, relative to
// the chart itself.
//
// An umbrella chart in a monorepo routinely depends on its siblings this way.
// Copying only the chart would leave "file://../common" pointing at nothing,
// so the siblings have to come along at the same relative positions.
func localRepos(dir string) ([]string, error) {
	body, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		return nil, nil
	}

	var chart struct {
		Dependencies []struct {
			Repository string `yaml:"repository"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(body, &chart); err != nil {
		return nil, nil
	}

	var out []string
	for _, d := range chart.Dependencies {
		rel, found := strings.CutPrefix(d.Repository, "file://")
		if !found || filepath.IsAbs(rel) {
			continue
		}
		out = append(out, filepath.Clean(rel))
	}
	return out, nil
}

// copyWithLocalRepos copies a chart into root, bringing every sibling its
// file:// dependencies point at, transitively.
func copyWithLocalRepos(root, src, dst string, seen map[string]bool) error {
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	if seen[abs] {
		return nil
	}
	seen[abs] = true

	// A chart can depend on something inside its own tree
	// (file://modules/mirror), which the chart's own copy already brought
	// along. Copying over it fails outright with "file exists", so only copy
	// what is not there yet - and still follow its dependencies, which may
	// point somewhere outside.
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
			return fmt.Errorf("copying %s: %w", src, err)
		}
	}

	repos, err := localRepos(src)
	if err != nil {
		return err
	}
	for _, rel := range repos {
		sibling := filepath.Join(dst, rel)
		// A dependency pointing outside the copy would escape the temp tree,
		// which is neither useful nor something idem should be writing to.
		if !within(root, sibling) {
			return fmt.Errorf("dependency file://%s in %s points outside the chart tree", rel, src)
		}
		if err := copyWithLocalRepos(root, filepath.Join(src, rel), sibling, seen); err != nil {
			return err
		}
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Prepare returns the directory to render, and a cleanup to run afterwards.
func (m Mode) Prepare(ctx context.Context, dir string, b Builder) (string, Kind, func(), error) {
	missing, err := Missing(dir)
	if err != nil {
		return "", Vendored, nil, err
	}
	if len(missing) == 0 {
		return dir, Vendored, nil, nil
	}

	switch m {
	case Never:
		return "", Vendored, nil, fmt.Errorf(
			"missing subcharts (%s) and --no-deps was passed; run: helm dependency build %s",
			strings.Join(missing, ", "), dir)

	case InPlace:
		if err := resolve(ctx, b, dir); err != nil {
			return "", Vendored, nil, err
		}
		return dir, Updated, nil, nil
	}

	return intoTempDir(ctx, dir, b)
}

// intoTempDir copies the chart out, resolves there, and discards it after.
//
// Chart source is small - Bitnami's postgresql is a few hundred kilobytes of
// templates - so copying is cheap next to rendering, and it is the only way to
// resolve dependencies without writing to the user's repository.
func intoTempDir(ctx context.Context, dir string, b Builder) (string, Kind, func(), error) {
	tmp, err := os.MkdirTemp("", "idem-chart-")
	if err != nil {
		return "", Vendored, nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }

	// A directory inside the temp root, because helm names the release
	// directory in its own errors and "idem-chart-1234" would be baffling.
	target := filepath.Join(tmp, filepath.Base(dir))
	if err := copyWithLocalRepos(tmp, dir, target, map[string]bool{}); err != nil {
		cleanup()
		return "", Vendored, nil, err
	}

	if err := resolve(ctx, b, target); err != nil {
		cleanup()
		return "", Vendored, nil, err
	}
	return target, Resolved, cleanup, nil
}
