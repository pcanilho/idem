package remediate

import (
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/diff"
)

// FluxEntry is one spec.driftDetection.ignore entry.
//
// Flux and ArgoCD suppress the same churn with different config, and the
// difference is not cosmetic: the paths are evaluated against a different
// shape, so a pointer that works in one is inert in the other.
type FluxEntry struct {
	Group     string
	Version   string
	Kind      string
	Namespace string
	Name      string
	Paths     []string
}

// FluxEntries builds the driftDetection.ignore entries for a set of findings.
//
// Verified against fluxcd/pkg ssa/jsondiff: the desired object is server-side
// apply dry-run BEFORE the diff, and the ignore paths are then applied as JSON
// Patch remove operations to both that result and the live object. Two
// consequences idem has to honour - the paths describe the STORED shape, and
// `AllowMissingPathOnRemove: true` means a path addressing nothing fails
// silently, exactly the way a wrong ArgoCD pointer does.
func FluxEntries(findings []check.Finding) []FluxEntry {
	byKey := make(map[string]*FluxEntry)
	for _, f := range findings {
		if skip(f) {
			continue
		}

		ref := f.Change.Object
		key := ref.Key()
		entry, seen := byKey[key]
		if !seen {
			group, version := groupVersion(ref.APIVersion)
			entry = &FluxEntry{
				Group:     group,
				Version:   version,
				Kind:      ref.Kind,
				Namespace: ref.Namespace,
				Name:      ref.Name,
			}
			byKey[key] = entry
		}
		for _, p := range f.Change.Paths {
			// Flux's ignore entries are RFC 6901 removes, so the only thing
			// expressible about a reordered list is removing it whole - which
			// hides its contents to suppress its order. Same limitation as
			// ArgoCD's, reached by a different route; see Entries.
			if p.Reordered {
				continue
			}
			entry.Paths = append(entry.Paths, storedPointer(ref, p.Path.JSONPointer())...)
		}
	}

	out := make([]FluxEntry, 0, len(byKey))
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		entry := byKey[key]
		slices.Sort(entry.Paths)
		entry.Paths = slices.Compact(entry.Paths)

		// Every path for this object turned out to be one Flux would never
		// evaluate. An entry with no paths is a block that silently does
		// nothing, which is the failure this exists to avoid.
		if len(entry.Paths) == 0 {
			continue
		}
		out = append(out, *entry)
	}
	slices.SortFunc(out, func(a, b FluxEntry) int { return strings.Compare(fluxSortKey(a), fluxSortKey(b)) })
	return out
}

// storedPointer is the pointer Flux evaluates, which is the shape the API
// server would store rather than the shape helm rendered.
//
// Only one pointer, unlike ArgoCD: Flux applies these in a single place, after
// its own dry run, so there is no second code path that sees the raw manifest
// and no reason to emit a pointer for one.
func storedPointer(ref diff.ObjectRef, pointer string) []string {
	segments := pointerSegments(pointer)
	if len(segments) == 0 {
		return nil
	}

	// The dry run strips it from the desired object, so nothing is ever there
	// to remove.
	if pointer == "/metadata/creationTimestamp" {
		return nil
	}

	// stringData is write-only: the API server folds it into data and drops
	// the key, and Flux diffs what the API server returned.
	if groupOf(ref.APIVersion) == "" && ref.Kind == "Secret" && segments[0] == "stringData" {
		return []string{"/data" + strings.TrimPrefix(pointer, "/stringData")}
	}
	return []string{pointer}
}

// groupVersion splits an apiVersion. A core object has no group, and an empty
// selector field matches everything in Flux - which is what a core group is.
func groupVersion(apiVersion string) (string, string) {
	group, version, found := strings.Cut(apiVersion, "/")
	if !found {
		return "", apiVersion
	}
	return group, version
}

// regexPunctuation is what has to be escaped in a selector value.
//
// Flux anchors the pattern as ^(?:value)$ but it is still a regular
// expression, so an unescaped dot in a name matches any character and the rule
// would suppress drift on an object the user never named. Kubernetes names
// permit dots, so this is reachable rather than theoretical.
var regexPunctuation = regexp.MustCompile(`[.+*?()|\[\]{}^$\\]`)

// selectorValue is a value safe to use as a Flux selector, escaped only when
// it needs to be - escaping what does not makes every block harder to read.
func selectorValue(s string) string {
	if !regexPunctuation.MatchString(s) {
		return s
	}
	return regexPunctuation.ReplaceAllString(s, `\$0`)
}

// fluxPaths renders the path list at the indentation this block needs.
//
// Not shared with the ArgoCD emitter: the two sit at different depths, and a
// sequence indented for the wrong parent is either a parse error or - worse -
// a valid document meaning something else.
func fluxPaths(paths []string) string {
	if len(paths) == 1 && flowSafe(paths[0]) {
		return " [" + paths[0] + "]"
	}

	var b strings.Builder
	for _, p := range paths {
		b.WriteString("\n          - " + scalar(p))
	}
	return b.String()
}

func fluxSortKey(e FluxEntry) string {
	return strings.Join([]string{e.Kind, e.Group, e.Namespace, e.Name}, "\x00")
}

// FluxYAML renders the block a user pastes into their HelmRelease.
//
// The mode is included because driftDetection.ignore does nothing at all
// without it: a block pasted into a HelmRelease with drift detection off is
// one that silently never runs, which is the same class of failure as an
// inert pointer.
func FluxYAML(entries []FluxEntry) string {
	if len(entries) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("spec:\n")
	b.WriteString("  driftDetection:\n")
	b.WriteString("    mode: enabled\n")
	b.WriteString("    ignore:\n")

	for _, e := range entries {
		b.WriteString("      - paths:" + fluxPaths(e.Paths) + "\n")
		b.WriteString("        target:\n")
		for _, field := range []struct{ key, value string }{
			{"group", e.Group},
			{"version", e.Version},
			{"kind", e.Kind},
			{"name", e.Name},
			{"namespace", e.Namespace},
		} {
			if field.value == "" {
				continue
			}
			b.WriteString("          " + field.key + ": " + scalar(selectorValue(field.value)) + "\n")
		}
	}
	return b.String()
}
