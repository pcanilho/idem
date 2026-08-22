package main

import (
	"os"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/doctor"
	"github.com/pcanilho/idem/internal/objpath"
	"github.com/pcanilho/idem/internal/report"
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
		{"the hero block", []string{"./examples/churning-chart", "--engine", "argocd"}},
		{"the churning example", []string{"./examples/churning-chart"}},
		{"the clean example", []string{"./examples/stable-chart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := invoke(t, tc.args...)
			if code == exitFatal {
				t.Fatalf("idem %v failed: %s%s", tc.args, stdout, stderr)
			}

			shown := consoleBlock(t, string(readme), "$ idem "+strings.Join(tc.args, " "))
			if len(shown) == 0 {
				t.Fatalf("no console block in the README for `idem %s`", strings.Join(tc.args, " "))
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

// `-o markdown` writes NOTHING on a clean run, and internal/report/markdown.go
// justifies that by pointing at the snippet in this README - so the snippet has
// to carry the guard the code is relying on.
//
// It did, and the rewrite that shortened the README dropped it, leaving a
// documented workflow that pipes an empty file into `gh pr comment` on every
// pull request that touches a chart. Neither half is wrong on its own; they
// stopped agreeing, and nothing was watching the join.
func TestTheDocumentedCommentWorkflowGuardsAgainstAnEmptyFile(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}

	_, snippet, found := strings.Cut(string(readme), "gh pr comment")
	if !found {
		t.Fatal("README no longer documents the `gh pr comment` workflow")
	}
	snippet, _, _ = strings.Cut(snippet, "```")

	if !strings.Contains(snippet, "hashFiles") {
		t.Errorf("the documented `gh pr comment` step does not guard on the file being non-empty:\n%s", snippet)
	}
}

// The README's flag table and the binary have to agree, in both directions.
//
// The first version of this checked `strings.Contains(help, "-"+name)`, which
// was vacuous in exactly the case it was written for: `-v` matched the `-v` in
// `-chart-version`, so deleting `-v` from the binary left the test green. It
// also carried a hand-written list of flag names, so a flag added to the
// binary and left out of the README was nobody's failure.
//
// Both halves are now derived - the flag names PrintDefaults emits, and the
// first cell of every row in the README's table - and compared as sets. `-v`
// and `idem diff` were each documented for months without existing; a flag
// that exists without being documented is the same defect facing the other
// way.
func TestTheREADMEAndTheBinaryAgreeOnEveryFlag(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}

	_, help, _ := invoke(t, "--help")

	documented := documentedFlags(string(readme))
	if len(documented) == 0 {
		t.Fatal("no flag table found in the README")
	}
	actual := binaryFlags(help)
	if len(actual) == 0 {
		t.Fatal("no flags found in `idem --help`")
	}

	for name := range documented {
		if !actual[name] {
			t.Errorf("README documents -%s, which the binary does not accept", name)
		}
	}
	for name := range actual {
		if !documented[name] {
			t.Errorf("the binary accepts -%s, which the README does not document", name)
		}
	}
}

// binaryFlags is the set of flag names `idem --help` prints. PrintDefaults
// writes each one as two spaces, a dash, the name, then either a space and a
// type or a tab and the description.
func binaryFlags(help string) map[string]bool {
	out := map[string]bool{}
	for line := range strings.SplitSeq(help, "\n") {
		rest, ok := strings.CutPrefix(line, "  -")
		if !ok {
			continue
		}
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

// documentedFlags is the set of flag names in the first cell of the README's
// flag table. Only the first cell: the description column cites flags too
// (`--repo` is described as "as helm's `--repo`"), and counting those would
// let a documented-but-missing flag hide behind a mention of itself.
func documentedFlags(readme string) map[string]bool {
	_, table, found := strings.Cut(readme, "| Flag | What it does |")
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
		// Odd fields of a backtick split are the quoted spans.
		parts := strings.Split(cell, "`")
		for i := 1; i < len(parts); i += 2 {
			if name := strings.TrimLeft(parts[i], "-"); name != parts[i] {
				out[name] = true
			}
		}
	}
	return out
}

// The README quotes output from the two paths that need a live cluster, so no
// console block can be generated from them and the drift check above cannot see
// them at all. They are the easiest lines in the documentation to leave behind.
//
// Rendered from the report package directly rather than by invoking the binary:
// what these say is a property of the formatter, and requiring a cluster to
// check the documentation would mean never checking it.
func TestTheREADMEQuotesTheClusterPathsCorrectly(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}

	ref := diff.ObjectRef{APIVersion: "v1", Kind: "Pod", Namespace: "lab", Name: "api"}

	var admission strings.Builder
	r := report.Report{
		Charts: []report.Chart{{
			Name: "home",
			Rewrites: []doctor.Rewrite{{
				Object:  ref,
				Changes: []doctor.Change{{Path: objpath.Path{}.Append(objpath.Key("spec")).Append(objpath.Key("dnsPolicy")), Value: "ClusterFirst"}},
			}},
		}},
		Helm: "4.2.4", Rounds: 2,
	}
	if err := r.Text(&admission); err != nil {
		t.Fatalf("Text() error = %v", err)
	}

	var drift strings.Builder
	if err := report.Drift(&drift, []doctor.Drift{{
		Object: ref,
		Changes: []diff.PathDiff{
			// One of each shape, because the README distinguishes them and the
			// distinction is the point: a field the controller ADDED is a
			// different problem from one it overwrote.
			{Path: objpath.Path{}.Append(objpath.Key("data")).Append(objpath.Key("token")), HasRight: true},
			{Path: objpath.Path{}.Append(objpath.Key("data")).Append(objpath.Key("ca")), HasLeft: true, HasRight: true},
		},
	}}, "lab"); err != nil {
		t.Fatalf("Drift() error = %v", err)
	}

	for _, tc := range []struct{ quoted, in string }{
		{"the cluster rewrites these on admission", admission.String()},
		{"applied absent, live set", drift.String()},
		{"applied and live differ", drift.String()},
	} {
		if !strings.Contains(string(readme), tc.quoted) {
			t.Errorf("README no longer quotes %q", tc.quoted)
			continue
		}
		if !strings.Contains(tc.in, tc.quoted) {
			t.Errorf("README quotes %q, which idem does not print:\n%s", tc.quoted, tc.in)
		}
	}
}
