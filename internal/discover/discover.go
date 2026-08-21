// Package discover finds charts under a path.
//
// The rule is one line long: a directory holding a Chart.yaml is a chart, and
// the walk does not descend into it. Vendored subcharts under charts/ are then
// skipped for free rather than by a special case - and a special case is
// exactly what would go wrong, because "charts/" is only a convention and a
// chart may legitimately hold a directory of that name elsewhere.
package discover

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// chartFile is the marker that makes a directory a chart.
const chartFile = "Chart.yaml"

// Chart is one chart found on disk.
type Chart struct {
	// Dir is the chart directory, as given - not resolved or made absolute,
	// so that findings echo the path the user typed.
	Dir string

	// Name is metadata.name from Chart.yaml, falling back to the directory
	// name. It becomes the release name at render time, where the only
	// property that matters is that it is the same in every round.
	Name string
}

// Charts finds every chart at or below root, in path order.
//
// Sorted because filepath.WalkDir's lexical order is per-directory rather than
// global, and idem's output must not depend on where a chart happens to sit in
// the tree.
func Charts(root string) ([]Chart, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory; idem takes a chart directory or a directory of charts", root)
	}

	var out []Chart
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		// Dot directories are caches and VCS metadata, never charts a user
		// means to check. The root itself is exempt: "idem ." must work.
		if path != root && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(path, chartFile)); statErr != nil {
			return nil
		}
		out = append(out, Chart{Dir: path, Name: nameOf(path)})
		return fs.SkipDir
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Chart) int { return strings.Compare(a.Dir, b.Dir) })

	if len(out) == 0 {
		return nil, fmt.Errorf("no chart found under %s: no directory contains a %s", root, chartFile)
	}
	return out, nil
}

// nameOf reads metadata.name from Chart.yaml.
//
// An unreadable or nameless Chart.yaml falls back to the directory name rather
// than failing: the name is only used as the release name, and helm will
// produce a far better error about the malformed chart when it renders it.
func nameOf(dir string) string {
	fallback := filepath.Base(dir)
	b, err := os.ReadFile(filepath.Join(dir, chartFile))
	if err != nil {
		return fallback
	}
	var meta struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(b, &meta); err != nil || meta.Name == "" {
		return fallback
	}
	return meta.Name
}
