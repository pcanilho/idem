package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hook is the subset of .pre-commit-hooks.yaml worth asserting on.
type hook struct {
	ID            string   `yaml:"id"`
	Entry         string   `yaml:"entry"`
	Args          []string `yaml:"args"`
	Files         string   `yaml:"files"`
	Language      string   `yaml:"language"`
	PassFilenames *bool    `yaml:"pass_filenames"`
}

func hooks(t *testing.T) []hook {
	t.Helper()

	body, err := os.ReadFile(".pre-commit-hooks.yaml")
	if err != nil {
		t.Fatalf("reading the hook manifest: %v", err)
	}
	var out []hook
	if err := yaml.Unmarshal(body, &out); err != nil {
		t.Fatalf("the hook manifest does not parse: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the hook manifest declares no hooks")
	}
	return out
}

// The manifest names flags, and nothing else checks that they exist.
//
// It is the same defect the README flag table had: a file outside the Go build
// naming a binary's interface, drifting the moment the interface moves. The
// difference is the blast radius - a wrong flag here does not print a stale
// sentence, it makes every consumer's commit fail with a usage error.
func TestThePreCommitHookOnlyUsesFlagsTheBinaryHas(t *testing.T) {
	_, help, _ := invoke(t, "--help")
	actual := binaryFlags(help)

	for _, h := range hooks(t) {
		for _, token := range append(strings.Fields(h.Entry), h.Args...) {
			name, ok := strings.CutPrefix(token, "-")
			if !ok {
				continue
			}
			name = strings.TrimPrefix(name, "-")
			// A flag written as --engine=argocd names the flag before the =.
			name, _, _ = strings.Cut(name, "=")
			if !actual[name] {
				t.Errorf("hook %q uses -%s, which the binary does not accept", h.ID, name)
			}
		}
	}
}

// The published hook has to actually gate a commit.
//
// TestThePreCommitHookOnlyUsesFlagsTheBinaryHas checks that the flags it names
// PARSE, and that was VACUOUS about the flags being there at all: with
// `args: []` the loop body never runs, the whole suite stays green, and every
// consumer silently gets a hook that reports and never blocks. `entry: idem .`
// -> `entry: idem` is the same class - bare idem prints help and exits 0.
func TestThePreCommitHookActuallyGatesTheCommit(t *testing.T) {
	for _, h := range hooks(t) {
		if got, want := strings.Fields(h.Entry), []string{"idem", "."}; !slices.Equal(got, want) {
			t.Errorf("hook %q entry = %v, want %v - bare `idem` prints help and exits 0", h.ID, got, want)
		}
		if !slices.Contains(h.Args, "--strict") {
			t.Errorf("hook %q args = %v, want --strict; without it the hook never fails a commit", h.ID, h.Args)
		}
	}
}

// idem takes ONE chart reference, so filenames must never be appended.
//
// A chart is a directory. Handing idem six template paths is not a smaller
// question than handing it the chart, it is a different one - and idem would
// refuse it outright with "one chart reference at a time". Flipping this flag
// is a one-word edit that breaks every consumer.
func TestThePreCommitHookNeverPassesFilenames(t *testing.T) {
	for _, h := range hooks(t) {
		if h.PassFilenames == nil || *h.PassFilenames {
			t.Errorf("hook %q passes filenames; idem takes one chart reference", h.ID)
		}
	}
}

// The file filter decides whether the hook runs at all, so a pattern that is
// too narrow is silent: the hook simply never fires and the repository looks
// clean. values.yaml is the case worth pinning - it changes what every template
// in the chart renders while touching no template at all.
func TestThePreCommitHookRunsOnWhatChangesARender(t *testing.T) {
	for _, h := range hooks(t) {
		re, err := regexp.Compile(h.Files)
		if err != nil {
			t.Fatalf("hook %q has an unparseable files pattern: %v", h.ID, err)
		}

		for _, tc := range []struct {
			path string
			want bool
		}{
			{"charts/home/Chart.yaml", true},
			{"charts/home/Chart.lock", true},
			{"charts/home/values.yaml", true},
			{"charts/home/values-prod.yaml", true},
			{"charts/home/values.yml", true},
			{"charts/home/templates/main.yaml", true},
			{"charts/home/templates/_helpers.tpl", true},
			{"Chart.yaml", true},

			// Not a render input. A hook that runs on every commit in the
			// repository is the one people learn to pass --no-verify to.
			{"README.md", false},
			{".pre-commit-config.yaml", false},
			{"docs/values.md", false},
			{"main.go", false},
			// Named like a chart file but is not one.
			{"scripts/values.yaml.tpl", false},
		} {
			if got := re.MatchString(tc.path); got != tc.want {
				t.Errorf("hook %q: files matches %q = %v, want %v", h.ID, tc.path, got, tc.want)
			}
		}
	}
}

// binaryFlags is the set of flag names `idem --help` prints: two spaces, one
// dash for a single letter or two for a word, the name, then either a space and
// a type or a tab and the description.
//
// Both dash forms are accepted, so this compares NAMES rather than spelling.
func binaryFlags(help string) map[string]bool {
	out := map[string]bool{}
	for line := range strings.SplitSeq(help, "\n") {
		rest, ok := strings.CutPrefix(line, "  -")
		if !ok {
			continue
		}
		rest = strings.TrimPrefix(rest, "-")
		// A tab for a boolean flag (`-v\texpand every…`), a space for one
		// that takes a value (`-o string`). Cut on whichever comes first.
		name := strings.TrimRight(rest, " \t")
		if i := strings.IndexAny(name, " \t"); i >= 0 {
			name = name[:i]
		}
		if name != "" {
			out[name] = true
		}
	}
	return out
}
