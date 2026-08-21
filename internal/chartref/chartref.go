// Package chartref classifies how a chart reference should be resolved.
//
// Four forms are accepted, and only one of them can fail on a clean machine:
//
//	./charts/home                                   local, no setup
//	oci://registry.example.com/charts/postgresql    no setup
//	postgresql --repo https://charts.example.com    no setup
//	bitnami/postgresql                              needs `helm repo add`
//
// The last form is the trap. "bitnami/postgresql" and "charts/home" are
// syntactically identical, so only disk existence separates them, and a user
// who has never run `helm repo add bitnami` gets "Error: repo bitnami not
// found" from a command that looked like it should just work.
package chartref

import (
	"path/filepath"
	"strings"
)

// Kind is how a chart reference must be fetched.
type Kind int

const (
	// Local is a directory or .tgz on this machine.
	Local Kind = iota
	// OCI is an oci:// reference. Needs no prior helm configuration.
	OCI
	// RepoURL is a chart name plus an explicit --repo URL. Needs no prior
	// helm configuration either.
	RepoURL
	// RepoAlias is "alias/chart", which requires `helm repo add` first.
	RepoAlias
)

func (k Kind) String() string {
	switch k {
	case Local:
		return "local"
	case OCI:
		return "oci"
	case RepoURL:
		return "repo-url"
	case RepoAlias:
		return "repo-alias"
	}
	return "unknown"
}

// Ref is a classified chart reference.
type Ref struct {
	Raw   string
	Kind  Kind
	Repo  string
	Chart string
}

// NeedsNetwork reports whether resolving this reference requires network access.
func (r Ref) NeedsNetwork() bool {
	return r.Kind != Local
}

// SetupHint returns the command the user must run first, or "" if none.
//
// Only a repo alias can fail for want of configuration, so only a repo alias
// produces a hint.
func (r Ref) SetupHint() string {
	if r.Kind != RepoAlias {
		return ""
	}
	return "helm repo add " + r.Repo + " <url> && helm repo update"
}

// Classify determines how raw should be resolved.
func Classify(raw string, exists func(string) bool) Ref {
	ref := Ref{Raw: raw}

	if strings.HasPrefix(raw, "oci://") {
		ref.Kind = OCI
		return ref
	}

	// An explicit path prefix means the user meant a path, whether or not it
	// exists. "not found" is a better error than a surprise network call.
	if strings.HasPrefix(raw, "./") ||
		strings.HasPrefix(raw, "../") ||
		strings.HasPrefix(raw, "~") ||
		filepath.IsAbs(raw) {
		ref.Kind = Local
		return ref
	}

	if exists(raw) {
		ref.Kind = Local
		return ref
	}

	// Not on disk. "alias/chart" is a repository lookup; anything else can
	// only have been a path the user got wrong.
	if repo, chart, ok := strings.Cut(raw, "/"); ok && repo != "" && chart != "" && !strings.Contains(chart, "/") {
		ref.Kind = RepoAlias
		ref.Repo = repo
		ref.Chart = chart
		return ref
	}

	ref.Kind = Local
	return ref
}

// ClassifyWithRepo classifies raw given an explicit --repo URL.
//
// An explicit --repo always wins: it is the zero-setup way to reach a chart
// that would otherwise need `helm repo add`.
func ClassifyWithRepo(raw, repoURL string, exists func(string) bool) Ref {
	if repoURL == "" {
		return Classify(raw, exists)
	}
	return Ref{
		Raw:   raw,
		Kind:  RepoURL,
		Repo:  repoURL,
		Chart: raw,
	}
}
