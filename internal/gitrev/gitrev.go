// Package gitrev answers which files changed since a revision.
//
// The ratchet borrowed from golangci-lint: adding a checker to an existing
// estate finds a pile of pre-existing issues, and a permanently red pipeline
// gets deleted rather than fixed. Git revisions rather than a baseline file -
// nothing to store, nothing to review, and dropping the flag always shows
// everything.
package gitrev

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Changed lists paths differing from rev, relative to the repository root.
//
// Untracked files are included: a chart added in this branch is exactly what
// the flag exists to catch, and it has no committed state to diff against.
func Changed(ctx context.Context, root, rev string) ([]string, error) {
	if err := verify(ctx, root, rev); err != nil {
		return nil, err
	}

	// --end-of-options, and the trailing --, because rev is user data.
	//
	// Without them git reads any value starting with `-` as one of its own
	// options, and `git diff --output=FILE` TRUNCATES FILE. So
	// `--new-from-rev=--output=/anything` destroyed that file while idem
	// printed an ordinary report and exited 0. The -- also stops a rev that
	// names a path being taken as a pathspec, which silently disabled the
	// ratchet rather than failing.
	diff, err := git(ctx, root, "diff", "--name-only", "--end-of-options", rev, "--")
	if err != nil {
		return nil, err
	}

	untracked, err := git(ctx, root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}

	return lines(diff + "\n" + untracked), nil
}

// verify refuses anything git will not resolve as a commit.
//
// --end-of-options alone stops the value being read as a flag, but a value that
// is a PATH still diffs cleanly against nothing and reports no changes: the
// ratchet then hides every finding and exits 0, which is the failure mode it
// exists to prevent. rev-parse is git's own answer to "is this a revision".
func verify(ctx context.Context, root, rev string) error {
	if _, err := git(ctx, root, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}"); err != nil {
		return fmt.Errorf("%q is not a revision in this repository", rev)
	}
	return nil
}

// MergeBase resolves where ref and HEAD diverged.
//
// Diffing against the branch tip would blame this branch for everything that
// landed on the base since it was cut.
func MergeBase(ctx context.Context, root, ref string) (string, error) {
	if err := verify(ctx, root, ref); err != nil {
		return "", err
	}
	out, err := git(ctx, root, "merge-base", "--end-of-options", ref, "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func git(ctx context.Context, root string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.String(), nil
}

func lines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Touches reports whether any changed path lies inside dir.
//
// The granularity is deliberately a directory, not a line: a finding belongs
// to a rendered object, not to a source line, so a chart with any changed file
// is examined in full.
func Touches(changed []string, dir string) bool {
	if dir == "" {
		return false
	}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for _, path := range changed {
		if path == dir || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
