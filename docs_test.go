package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The docs promise only what the binary does, and this is what enforces it.
//
// An earlier version of this lived in readme_test.go and was deleted when the
// README was split into docs/. The property it protected did not go away with
// it: `idem diff` and `-v` were each documented for months without existing,
// three console blocks had drifted, and every one of those was found by reading
// rather than by testing. What changed in the split is only WHERE each claim
// lives - console blocks in README.md, flags in docs/usage.md, the CI recipe in
// docs/ci.md - so this checks the same three things in their new homes.

// The README's console blocks are the shop window. They may legitimately show
// LESS than the binary prints: the churn report omits the potential section,
// because the whole thing would drown the page it is meant to sell. What they
// may never do is show a line the binary does not print.
//
// Only blocks reproducible from `examples/` are pinned. Anything needing a
// cluster is written to say so rather than to look copied.
// tagline is the one sentence idem describes itself with.
//
// It lives in FOUR places - the GitHub repo description, action.yml's
// description, the Homebrew cask description in .goreleaser.yaml, and the
// README's opening - and until this test nothing held them together, so they
// drifted: the repo description dropped the tail, and the README said "chart
// is" where the rest said "charts are". The repo description is the one place
// a test cannot reach, so it is named in the failure to be changed with the
// others.
const tagline = "Check whether your Helm charts are idempotent under the GitOps engine you run"

// claim is the half of it that carries the meaning. The README needs a
// sentence of its own around it, so this is the part every place shares
// verbatim.
const claim = "your Helm charts are idempotent under the GitOps engine you run"

func TestTheTaglineReadsTheSameEverywhereItAppears(t *testing.T) {
	if !strings.HasSuffix(tagline, claim) {
		t.Fatalf("tagline %q does not end in the claim %q", tagline, claim)
	}

	// The README makes a sentence of it, so it shares the claim rather than
	// the whole line.
	for _, name := range []string{"action.yml", ".goreleaser.yaml", "README.md"} {
		if !strings.Contains(read(t, name), claim) {
			t.Errorf("%s does not carry %q - and the GitHub repo description needs the same sentence, which no test here can check", name, claim)
		}
	}

	// The two description fields carry it whole: they are one line each, and
	// there is nothing to wrap it in.
	for _, name := range []string{"action.yml", ".goreleaser.yaml"} {
		if !strings.Contains(read(t, name), tagline) {
			t.Errorf("%s does not carry the whole tagline %q", name, tagline)
		}
	}
}

func TestTheREADMEShowsWhatTheBinaryActuallyPrints(t *testing.T) {
	requireHelm(t)

	readme := read(t, "README.md")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"the churn report", []string{"./examples/churning-chart"}},
		{"the clean example", []string{"./examples/stable-chart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := invoke(t, tc.args...)
			if code == exitFatal {
				t.Fatalf("idem %v failed: %s%s", tc.args, stdout, stderr)
			}

			prompt := "$ idem " + strings.Join(tc.args, " ")
			shown := consoleBlock(readme, prompt)
			if len(shown) == 0 {
				t.Fatalf("no console block in README.md for `%s`", prompt)
			}

			// Line by line: a whole-block match would fail on the surrounding
			// markdown and say nothing about which line drifted.
			//
			// The helm version is normalised out of both sides. The blocks are
			// real captured output and so carry the version that produced them,
			// but which helm is on PATH is the ENVIRONMENT rather than the
			// content this pins - and idem is meant to run under Helm 3 and
			// Helm 4 alike, which is what CI's matrix checks. Without this the
			// docs could only ever be regenerated on one of them.
			printed := anyHelmVersion(stdout)
			for _, line := range shown {
				if !strings.Contains(printed, anyHelmVersion(line)) {
					t.Errorf("README.md shows a line the binary does not print:\n  %s", line)
				}
			}
		})
	}
}

// Every flag docs/usage.md documents has to exist, and every flag the binary
// takes has to be documented.
//
// Both directions, because each has failed on its own. `-v` was documented and
// missing; and a flag added to the binary and left out of the table is nobody's
// failure unless something looks for it.
func TestTheFlagTableAndTheBinaryAgree(t *testing.T) {
	_, help, _ := invoke(t, "--help")

	documented := documentedFlags(read(t, "docs/usage.md"))
	if len(documented) == 0 {
		t.Fatal("no flag table found in docs/usage.md")
	}
	actual := binaryFlags(help)
	if len(actual) == 0 {
		t.Fatal("no flags found in `idem --help`")
	}

	for name := range documented {
		if !actual[name] {
			t.Errorf("docs/usage.md documents -%s, which the binary does not accept", name)
		}
	}
	for name := range actual {
		if !documented[name] {
			t.Errorf("the binary accepts -%s, which docs/usage.md does not document", name)
		}
	}
}

// `-o markdown` writes NOTHING on a clean run, and internal/report/markdown.go
// justifies that by pointing at the snippet in the docs - so the snippet has to
// carry the guard the code relies on.
//
// It did, and an earlier rewrite dropped it, leaving a documented workflow that
// piped an empty file into `gh pr comment` on every pull request that touched a
// chart. Neither half was wrong alone; they stopped agreeing and nothing was
// watching the join.
func TestTheDocumentedCommentWorkflowGuardsAgainstAnEmptyFile(t *testing.T) {
	snippet := fencedBlockContaining(read(t, "docs/ci.md"), "gh pr comment")
	if snippet == "" {
		t.Fatal("docs/ci.md no longer documents the `gh pr comment` workflow")
	}

	if !strings.Contains(snippet, "hashFiles") {
		t.Errorf("the documented `gh pr comment` step does not guard on the file being non-empty:\n%s", snippet)
	}

	// And the guard has to be able to fire. hashFiles reads only files under
	// GITHUB_WORKSPACE and returns an empty string for anything else, so a
	// snippet writing to /tmp and guarding on hashFiles('/tmp/idem.md') is
	// ALWAYS false and the comment is never posted. That shipped for months,
	// and the keyword check above passed the whole time - which is why this
	// second assertion exists.
	for _, path := range regexp.MustCompile(`hashFiles\('([^']*)'\)`).FindAllStringSubmatch(snippet, -1) {
		if strings.HasPrefix(path[1], "/") {
			t.Errorf("hashFiles(%q) is outside GITHUB_WORKSPACE, so the guard can never be true", path[1])
		}
	}
}

// read loads a documentation file, failing the test rather than the package.
func read(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

// consoleBlock returns the output lines of the fenced block introduced by the
// given command, with the prompt line and blank lines dropped.
func consoleBlock(doc, prompt string) []string {
	_, after, found := strings.Cut(doc, prompt+"\n")
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

// fencedBlockContaining returns the first fenced block holding needle.
func fencedBlockContaining(doc, needle string) string {
	for block := range strings.SplitSeq(doc, "```") {
		if strings.Contains(block, needle) {
			return block
		}
	}
	return ""
}

// documentedFlags is the set of flag names in the first cell of the flag table.
//
// Only the first cell: the description column cites flags too (`--repo` is
// described as "as helm's `--repo`"), and counting those would let a
// documented-but-missing flag hide behind a mention of itself.
func documentedFlags(doc string) map[string]bool {
	_, table, found := strings.Cut(doc, "| Flag | What it does |")
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

// helmVersion matches the version idem prints in its provenance line.
var helmVersion = regexp.MustCompile(`helm \d+\.\d+\.\d+\S*`)

// anyHelmVersion normalises the helm version away, so a doc captured under one
// helm still checks out under another.
func anyHelmVersion(s string) string {
	return helmVersion.ReplaceAllString(s, "helm <version>")
}
