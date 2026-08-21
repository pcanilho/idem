// Package remediate turns findings into the config that stops the churn.
//
// This is the consumer-facing payload, and the reason idem is a portability
// checker rather than a linter: its users are chart *consumers*, who cannot
// patch a third-party chart and can only tell their engine to stop fighting
// it. One block for the whole run, so it is pasted once rather than N times.
package remediate

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/diff"
)

// Entry is one ArgoCD ignoreDifferences entry.
type Entry struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Pointers  []string
}

// Entries builds the ignoreDifferences entries for a set of findings.
//
// Findings that cannot be addressed precisely are left out rather than
// approximated - see skip.
func Entries(findings []check.Finding) []Entry {
	byKey := make(map[string]*Entry)
	for _, f := range findings {
		ref := f.Change.Object
		if skip(f) {
			continue
		}

		key := ref.Key()
		entry, seen := byKey[key]
		if !seen {
			entry = &Entry{Group: groupOf(ref.APIVersion), Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}
			byKey[key] = entry
		}
		for _, p := range f.Change.Paths {
			entry.Pointers = append(entry.Pointers, EvaluablePointers(ref, p.Path.JSONPointer())...)
		}
	}

	out := make([]Entry, 0, len(byKey))
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		entry := byKey[key]
		slices.Sort(entry.Pointers)
		entry.Pointers = slices.Compact(entry.Pointers)
		// Every pointer for this object turned out to be one ArgoCD would
		// never evaluate. An entry with no jsonPointers is a block that
		// silently does nothing, which is the failure being removed here.
		if len(entry.Pointers) == 0 {
			continue
		}
		out = append(out, *entry)
	}

	// By Kind first: it is what the reader scans for, and it keeps the block
	// readable when one app churns across several kinds.
	slices.SortFunc(out, func(a, b Entry) int {
		return strings.Compare(sortKey(a), sortKey(b))
	})
	return out
}

// groupOf extracts the API group. A core object's apiVersion is just "v1",
// which is no group at all rather than a group named "v1".
func groupOf(apiVersion string) string {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return ""
	}
	return group
}

// EvaluablePointers turns one pointer derived from a RENDER into the pointers
// ArgoCD will actually evaluate - which is not the same thing.
//
// Exported because matching needs exactly the same translation as emitting: a
// user's /data/KEY rule has to match a finding idem derived as /stringData/KEY,
// or idem re-reports churn that was handled long ago.
//
// ArgoCD normalises an object before applying ignoreDifferences, so a pointer
// describing the rendered shape can address a path that no longer exists. When
// that happens the RFC6902 remove op fails and argo-cd's shouldLogError
// explicitly suppresses "doc is missing path" - no error, no warning, not even
// a debug line, and the suppression the user pasted simply never applies.
//
// Returns two pointers where both forms are needed, and none where the path is
// not reliably addressable at all.
func EvaluablePointers(ref diff.ObjectRef, pointer string) []string {
	segments := pointerSegments(pointer)
	if len(segments) == 0 {
		return nil
	}

	// Removed from both sides before ignoreDifferences runs, so a pointer at
	// it can never match.
	if pointer == "/metadata/creationTimestamp" {
		return nil
	}
	// ArgoCD injects a */* -> /status ignore by default, making this redundant.
	if segments[0] == "status" {
		return nil
	}

	group := groupOf(ref.APIVersion)
	switch {
	case group == "" && ref.Kind == "Secret":
		if segments[0] != "stringData" {
			break
		}
		// NormalizeSecret base64-encodes stringData into data and deletes the
		// stringData key, so the diff only ever sees /data. But the
		// RespectIgnoreDifferences sync path applies pointers to the raw
		// target, which still has stringData - so /data suppresses the diff
		// while /stringData is what stops selfHeal overwriting the value.
		// Whichever path a user is on, the other pointer is a silent no-op.
		return []string{"/data" + strings.TrimPrefix(pointer, "/stringData"), pointer}

	case group == "rbac.authorization.k8s.io" && (ref.Kind == "Role" || ref.Kind == "ClusterRole"):
		// normalizeRole nulls an empty rules array, and nulls rules entirely
		// for an aggregated ClusterRole. An index into it addresses nothing
		// dependable.
		if indexedUnder(segments, "rules") {
			return nil
		}

	case group == "" && ref.Kind == "Endpoints":
		// normalizeEndpoint sorts subsets before diffing, so index N in the
		// render is not index N in what ArgoCD compares.
		if indexedUnder(segments, "subsets") {
			return nil
		}
	}

	return []string{pointer}
}

// pointerSegments splits an RFC 6901 pointer.
//
// Exact rather than heuristic: the encoding escapes "/" as "~1", so a segment
// can never contain a raw slash.
func pointerSegments(pointer string) []string {
	if pointer == "" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(pointer, "/"), "/")
}

// indexedUnder reports whether the pointer indexes into the named top-level array.
func indexedUnder(segments []string, field string) bool {
	if len(segments) < 2 || segments[0] != field {
		return false
	}
	_, err := strconv.Atoi(segments[1])
	return err == nil
}

func sortKey(e Entry) string {
	return strings.Join([]string{e.Kind, e.Group, e.Namespace, e.Name}, "\x00")
}

// skip reports whether a finding cannot be expressed as an ignoreDifferences
// entry, in which case emitting one would be worse than emitting nothing.
func skip(f check.Finding) bool {
	// Nothing to point at. The object renders in one round and not another,
	// which ignoreDifferences has no way to express.
	if len(f.Change.Paths) == 0 {
		return true
	}

	// The API server assigns the name at apply time, so there is no name to
	// match on - and an entry without one matches every object of that kind.
	return f.Change.Object.Name == ""
}

// YAML renders the block a user pastes into their Application.
func YAML(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("spec:\n")
	b.WriteString("  ignoreDifferences:\n")

	for _, e := range entries {
		lead := "    - "
		for _, field := range []struct{ key, value string }{
			{"group", e.Group},
			{"kind", e.Kind},
			{"name", e.Name},
			{"namespace", e.Namespace},
		} {
			// group and namespace are omitted when empty: ArgoCD then matches
			// any, which is right for a core object or a cluster-scoped one.
			// Writing "" would instead read as an intentional empty match.
			if field.value == "" {
				continue
			}
			b.WriteString(lead + field.key + ": " + scalar(field.value) + "\n")
			lead = "      "
		}
		b.WriteString(lead + "jsonPointers:" + pointers(e.Pointers) + "\n")
	}

	// Without this, ignoreDifferences hides the diff but selfHeal still
	// re-applies the object, so the churn continues and idem's advice looks
	// like it did not work.
	b.WriteString("  syncPolicy:\n")
	b.WriteString("    syncOptions: [RespectIgnoreDifferences=true]\n")

	return b.String()
}

// pointers renders the list, inline when it is one short safe value.
func pointers(ptrs []string) string {
	if len(ptrs) == 1 && flowSafe(ptrs[0]) {
		return " [" + ptrs[0] + "]"
	}

	var b strings.Builder
	for _, p := range ptrs {
		b.WriteString("\n        - " + scalar(p))
	}
	return b.String()
}

// flowSafe reports whether a value can sit unquoted inside [ ... ].
//
// Flow context ends a scalar at a comma or a bracket, so a key containing one
// would silently split into two pointers that address nothing.
func flowSafe(s string) bool {
	return s != "" && !strings.ContainsAny(s, ",[]{}") && scalar(s) == s
}

// scalar quotes a value the way YAML requires.
//
// Delegated to the encoder rather than hand-rolled: the rules cover far more
// than punctuation - a pointer reading `no` or `~` would otherwise come back
// as a boolean or a null.
func scalar(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return `""`
	}
	return strings.TrimSuffix(string(out), "\n")
}
