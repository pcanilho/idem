package main

import (
	"os"
	"strings"
	"testing"
)

// The README's console blocks are the tool's shop window, and three of them had
// drifted from what the binary prints — a stale hero line, a promised flag that
// did not exist, an example whose first line was an error. Hand-checking that
// is how it happened; this is the check that stops it recurring.
//
// The property is "the README shows nothing the binary does not print". It may
// legitimately show less: the churn example is excerpted, because the whole
// 48-line output would drown the page it is meant to sell. Only blocks
// reproducible from `examples/` are pinned — anything needing a cluster is
// written to say so rather than to look copied.
func TestTheREADMEShowsWhatTheBinaryActuallyPrints(t *testing.T) {
	requireHelm(t)

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"the churning example", []string{"./examples/churning-chart"}},
		{"the clean example", []string{"./examples/stable-chart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := invoke(t, tc.args...)
			if code == exitFatal {
				t.Fatalf("idem %v failed: %s%s", tc.args, stdout, stderr)
			}

			shown := consoleBlock(t, string(readme), "$ idem "+tc.args[0])
			if len(shown) == 0 {
				t.Fatalf("no console block in the README for `idem %s`", tc.args[0])
			}

			// Line by line: a whole-block match would fail on the surrounding
			// markdown and say nothing about which line drifted.
			for _, line := range shown {
				if !strings.Contains(stdout, line) {
					t.Errorf("README shows a line the binary does not print:\n  %s", line)
				}
			}
		})
	}
}

// consoleBlock returns the output lines of the fenced block introduced by the
// given command, with the prompt line and blank lines dropped.
func consoleBlock(t *testing.T, readme, prompt string) []string {
	t.Helper()

	_, after, found := strings.Cut(readme, prompt+"\n")
	if !found {
		return nil
	}
	body, _, found := strings.Cut(after, "\n```")
	if !found {
		return nil
	}

	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// Every flag the README documents has to exist. `-v` and `idem diff` were both
// promised for months and neither was built.
func TestTheREADMEDocumentsOnlyFlagsThatExist(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}

	_, help, _ := invoke(t, "--help")
	for _, flag := range []string{
		"-f", "--set", "--rounds", "--strict", "-v", "-o", "--engine", "--context",
		"--namespace", "--repo", "--chart-version", "--jobs", "--new-from-rev",
		"--new-from-merge-base", "--dependency-update", "--no-deps", "--helm", "--version",
	} {
		if !strings.Contains(string(readme), "`"+flag+"`") {
			t.Errorf("README does not document %s", flag)
		}
		// helm's own flag listing is single-dash; the README writes the long
		// form the way a user types it.
		if !strings.Contains(help, "-"+strings.TrimLeft(flag, "-")) {
			t.Errorf("README documents %s, which the binary does not accept", flag)
		}
	}
}
