// Package globpath matches a slash-separated path against a glob pattern, with
// `**` meaning zero or more segments.
//
// It exists so idem has ONE implementation of that rule. ArgoCD globs its
// generator patterns with doublestar.FilepathGlob, verified in
// reposerver/repository/repository.go, whose comment says it is "consistent
// with AppSet generators". Go's path/filepath.Glob does not implement `**` at
// all, and taking doublestar as a dependency would be the second in a repo
// that advertises one.
package globpath

import (
	"path"
	"strings"
)

// Match reports whether name matches pattern. Both are slash-separated and
// relative; an unparseable pattern matches nothing rather than whatever it
// happens to expand to.
func Match(pattern, name string) bool {
	segments := strings.Split(pattern, "/")
	for _, seg := range segments {
		if _, err := path.Match(seg, ""); err != nil && seg != "**" {
			return false
		}
	}
	return MatchSegments(segments, strings.Split(name, "/"))
}

// MatchSegments is Match over patterns a caller has already split, and whose
// segments it has already validated.
func MatchSegments(pattern, name []string) bool { return match(pattern, name) }

// match walks the two segment by segment.
//
// `**` consumes zero or more segments - `a/**/b` matches `a/b` as well as
// `a/x/y/b`. Requiring at least one is the classic way to get this subtly
// wrong and drop a path without saying so.
func match(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}

	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if match(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}

	if len(name) == 0 {
		return false
	}
	// path.Match never matches across a separator, which is what makes this
	// segment-by-segment walk equivalent to globbing the whole path.
	ok, err := path.Match(pattern[0], name[0])
	if err != nil || !ok {
		return false
	}
	return match(pattern[1:], name[1:])
}
