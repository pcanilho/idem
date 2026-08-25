package report

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/pcanilho/idem/internal/doctor"
)

// Drift writes what changed after it was applied.
//
// Neither a chart problem nor an engine problem: two owners for one object.
// The chart can be perfectly deterministic and the object still never settle,
// which is why this is reported apart from everything else.
func Drift(w io.Writer, drifts []doctor.Drift, namespace string) error {
	var b strings.Builder

	if len(drifts) == 0 {
		fmt.Fprintf(&b, "\n  Nothing has been written after apply in %s.\n", scope(namespace))
		return emit(w, b.String())
	}

	fmt.Fprintf(&b, "\n  Written after apply, in %s\n", scope(namespace))

	for _, d := range drifts {
		fmt.Fprintf(&b, "\n    %s\n", d.Object.Display())

		tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
		for _, c := range d.Changes {
			// "absent" and "present but different" are different problems, so
			// the two are not collapsed into one word.
			switch {
			case !c.HasLeft:
				fmt.Fprintf(tw, "      %s\tapplied absent, live set\n", c.Path)
			case !c.HasRight:
				fmt.Fprintf(tw, "      %s\tapplied set, live absent\n", c.Path)
			default:
				fmt.Fprintf(tw, "      %s\tapplied and live differ\n", c.Path)
			}
		}
		tw.Flush()

		if d.Writer == "" {
			// Naming a controller idem cannot identify would be a guess, and
			// the reader would chase it.
			b.WriteString("      written after apply by something idem cannot identify\n")
		} else {
			fmt.Fprintf(&b, "      written after apply by %s (%s)\n", d.Writer, d.Evidence)
		}

		writeTwoOwners(&b, d)
	}

	return emit(w, b.String())
}

func scope(namespace string) string {
	if namespace == "" {
		return "every namespace"
	}
	return "namespace " + namespace
}

// writeTwoOwners suggests handing the field to whoever is actually writing it.
//
// The fix is not to make the chart deterministic - it already is. It is to
// stop two things owning one field.
func writeTwoOwners(b *strings.Builder, d doctor.Drift) {
	var roots []string
	for _, c := range d.Changes {
		root := "/" + strings.SplitN(strings.TrimPrefix(c.Path.JSONPointer(), "/"), "/", 2)[0]

		// A controller stamps its own labels and annotations on what it
		// writes. Those are evidence of who did it, not the field two things
		// are fighting over - and handing an engine's whole /metadata to
		// something else would ignore far more than the problem.
		if root == "/metadata" || slices.Contains(roots, root) {
			continue
		}
		roots = append(roots, root)
	}
	slices.Sort(roots)

	if len(roots) == 0 {
		// Only bookkeeping changed. Worth showing, not worth a suppression.
		b.WriteString("\n      Only labels and annotations, which is the writer marking its own\n")
		b.WriteString("      work rather than two things fighting over a value.\n")
		return
	}

	b.WriteString("\n      Not a chart problem and not an engine problem: two owners for one\n")
	b.WriteString("      object. Stop your engine managing the field:\n\n")
	fmt.Fprintf(b, "        - kind: %s\n", d.Object.Kind)
	fmt.Fprintf(b, "          name: %s\n", d.Object.Name)
	fmt.Fprintf(b, "          jsonPointers: [%s]\n", strings.Join(roots, ", "))
}
