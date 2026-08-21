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
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pcanilho/idem/internal/check"
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
			group, _, _ := strings.Cut(ref.APIVersion, "/")
			if !strings.Contains(ref.APIVersion, "/") {
				// A core object's apiVersion is just "v1" - no group at all.
				group = ""
			}
			entry = &Entry{Group: group, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}
			byKey[key] = entry
		}
		for _, p := range f.Change.Paths {
			entry.Pointers = append(entry.Pointers, p.Path.JSONPointer())
		}
	}

	out := make([]Entry, 0, len(byKey))
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		entry := byKey[key]
		slices.Sort(entry.Pointers)
		entry.Pointers = slices.Compact(entry.Pointers)
		out = append(out, *entry)
	}

	// By Kind first: it is what the reader scans for, and it keeps the block
	// readable when one app churns across several kinds.
	slices.SortFunc(out, func(a, b Entry) int {
		return strings.Compare(sortKey(a), sortKey(b))
	})
	return out
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
