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
	diff, err := git(ctx, root, "diff", "--name-only", rev)
	if err != nil {
		return nil, err
	}

	untracked, err := git(ctx, root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}

	return lines(diff + "\n" + untracked), nil
}

// MergeBase resolves where ref and HEAD diverged.
//
// Diffing against the branch tip would blame this branch for everything that
// landed on the base since it was cut.
func MergeBase(ctx context.Context, root, ref string) (string, error) {
	out, err := git(ctx, root, "merge-base", ref, "HEAD")
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
