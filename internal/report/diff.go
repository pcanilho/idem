package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/pcanilho/idem/internal/check"
)

// Diff writes what differs between two renders the user produced themselves.
//
// Deliberately smaller than the chart report: this is the comparison engine
// exposed on its own, so it says what differs and stops there. Verdicts and a
// fix block would be claims about an engine and a chart idem was never shown -
// `idem <chart>` is where those are earned, because it knows what it rendered
// and what the repository says about it.
func Diff(w io.Writer, findings []check.Finding, left, right string) error {
	var b strings.Builder

	if len(findings) == 0 {
		fmt.Fprintf(&b, "\n  ✓ %s and %s are identical.\n", left, right)
		_, err := io.WriteString(w, b.String())
		return err
	}

	fmt.Fprintf(&b, "\n  %s → %s\n\n", left, right)

	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	for _, f := range findings {
		writeFinding(tw, f, findings)
	}
	tw.Flush()

	// Counted in objects rather than fields: one object whose whole .data
	// regenerates is one thing to look at, not forty. The verb agrees with the
	// count too - keying plural() on the noun alone produces "1 object
	// differ", which is how this went wrong twice before.
	n := len(findings)
	fmt.Fprintf(&b, "\n  %d %s.\n", n, plural(n, "object differs", "objects differ"))

	_, err := io.WriteString(w, b.String())
	return err
}
