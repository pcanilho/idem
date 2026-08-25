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

	// Library marks `type: library`, which helm refuses to render at all -
	// "library charts are not installable". Reported rather than dropped, so
	// the caller can say what it did not check instead of silently checking
	// less than the user asked for.
	Library bool
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
	// Resolved directories already walked, so a symlink pointing at an ancestor
	// cannot loop. Keyed by the RESOLVED path, because two different links to
	// one directory are the same chart.
	seen := map[string]bool{}

	if err := walk(root, root, seen, &out); err != nil {
		return nil, err
	}

	slices.SortFunc(out, func(a, b Chart) int { return strings.Compare(a.Dir, b.Dir) })

	if len(out) == 0 {
		return nil, fmt.Errorf("no chart found under %s: no directory contains a %s", root, chartFile)
	}
	return out, nil
}

// walk finds charts under dir, following symlinked directories.
//
// filepath.WalkDir deliberately does not follow symlinks: its DirEntry comes
// from Lstat, so `d.IsDir()` is false for a link even when it points at a
// directory. That silently dropped every symlinked chart from a run - no
// finding, no warning, not even an unevaluable count - which in a CI gate is
// exactly the coverage gap idem exists to prevent. Symlinked charts are
// ordinary: one chart shared between two estates, or a vendored path linked
// into place.
//
// Following them means owning loop detection, which is why WalkDir declines to.
func walk(root, dir string, seen map[string]bool, out *[]Chart) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// A broken link is not an error for the run: it names no chart, and
		// helm would give a better message than idem could invent.
		return nil
	}
	if seen[real] {
		return nil
	}
	seen[real] = true

	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		// A directory idem cannot enter holds no charts it can check. That is a
		// gap in coverage, not a reason to check nothing - returning the error
		// discarded every readable chart in the tree and exited 2 over one
		// chmod 000 directory. Skipped rather than reported: this walk also
		// crosses whatever a user happens to keep beside their charts.
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// A symlink: resolve it, and walk it if it is a directory.
		//
		// Except when it IS this walk's root - WalkDir reports its own starting
		// path through Lstat too, so without this the recursion re-enters,
		// finds the path already in seen, and returns having found nothing.
		if path != dir && d.Type()&fs.ModeSymlink != 0 {
			info, statErr := os.Stat(path)
			if statErr != nil || !info.IsDir() {
				return nil
			}
			return walk(root, path, seen, out)
		}

		if !d.IsDir() && path != dir {
			return nil
		}
		// Dot directories are caches and VCS metadata, never charts a user
		// means to check. The root itself is exempt: "idem ." must work.
		if path != root && path != dir && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(path, chartFile)); statErr != nil {
			return nil
		}
		name, library := metaOf(path)
		*out = append(*out, Chart{Dir: path, Name: name, Library: library})
		return fs.SkipDir
	})
}

// metaOf reads metadata.name and the chart type from Chart.yaml.
//
// An unreadable or nameless Chart.yaml falls back to the directory name rather
// than failing: the name is only used as the release name, and helm will
// produce a far better error about the malformed chart when it renders it.
//
// The type falls back to NOT a library for the same reason the over-matching in
// internal/analyze runs the other way: the two errors are not symmetrical.
// Missing a library chart means helm's own accurate "library charts are not
// installable", which is what happened before this existed. Wrongly calling an
// application chart a library would skip a chart the user asked about and
// report that it had - a silent coverage gap, which is the one thing a gate
// must never have. Only the exact string wins.
func metaOf(dir string) (name string, library bool) {
	fallback := filepath.Base(dir)
	b, err := os.ReadFile(filepath.Join(dir, chartFile))
	if err != nil {
		return fallback, false
	}
	var meta struct {
		Name string `yaml:"name"`
		Type string `yaml:"type"`
	}
	if err := yaml.Unmarshal(b, &meta); err != nil {
		return fallback, false
	}
	if meta.Name == "" {
		return fallback, meta.Type == "library"
	}
	return meta.Name, meta.Type == "library"
}
