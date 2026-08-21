// Package report renders what check found, for humans.
//
// The default output is a verdict sentence rather than a stat line: the reader
// needs to know what will happen to their cluster, not how many objects were
// compared. Everything printed is either observed or explicitly unknown.
package report

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/remediate"
)

// maxFields caps how many differing fields one object lists.
//
// A Secret whose whole .data regenerates produces one line per key, and an
// uncapped list is hundreds of lines of terminal for a single finding. The
// count of what was elided is still printed, so nothing disappears silently.
const maxFields = 5

// unknownSource is the group heading for findings whose render carried no
// "# Source:" comment. Named explicitly rather than guessed at.
const unknownSource = "(source unknown)"

// Chart is one chart's outcome.
type Chart struct {
	Name string
	Dir  string

	Findings []check.Finding

	// Verdicts is what each selected engine does with this chart. Empty when
	// the chart is clean, or when no engine lens was requested.
	Verdicts []engine.Verdict

	// Err is set when the chart could not be rendered at all. That is exit 2
	// and always fatal - a chart silently skipped is the bug idem exists for.
	Err error
}

// Report is a whole run.
type Report struct {
	Charts []Chart
	Helm   string
	Rounds int
}

// Churning counts charts with at least one finding.
func (r Report) Churning() int {
	n := 0
	for _, c := range r.Charts {
		if c.Err == nil && len(c.Findings) > 0 {
			n++
		}
	}
	return n
}

// Unevaluable counts charts that could not be rendered.
func (r Report) Unevaluable() int {
	n := 0
	for _, c := range r.Charts {
		if c.Err != nil {
			n++
		}
	}
	return n
}

// Text writes the default human-readable form.
func (r Report) Text(w io.Writer) error {
	var b strings.Builder

	detail := false
	for _, c := range r.Charts {
		if c.Err != nil || len(c.Findings) == 0 {
			continue
		}
		writeChart(&b, c)
		detail = true
	}
	if writeUnevaluable(&b, r.Charts) {
		detail = true
	}

	// A clean run is two lines and no leading blank; anything with detail above
	// it gets a blank line and the same indentation as that detail.
	indent := ""
	if detail {
		b.WriteString("\n")
		indent = "  "
	}
	fmt.Fprintf(&b, "%s%s\n", indent, r.verdict())
	fmt.Fprintf(&b, "  helm %s · %d rounds\n", r.Helm, r.Rounds)
	writeRemediation(&b, r.Charts)

	_, err := io.WriteString(w, b.String())
	return err
}

// writeChart prints one chart's findings, grouped by the template that
// produced each object - so a chart that regenerates six fields in one
// template reads as one place to look, not six.
func writeChart(b *strings.Builder, c Chart) {
	groups := make(map[string][]check.Finding)
	for _, f := range c.Findings {
		key := f.Source
		if key == "" {
			key = c.Name + " " + unknownSource
		}
		groups[key] = append(groups[key], f)
	}

	for _, source := range slices.Sorted(maps.Keys(groups)) {
		fmt.Fprintf(b, "\n  %s\n", source)

		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, f := range groups[source] {
			writeFinding(tw, f)
		}
		tw.Flush()
	}

	writeVerdicts(b, c.Verdicts)
}

// writeVerdicts prints what each engine does with this chart.
//
// One block per chart rather than per finding: today the answer turns on
// whether the chart calls lookup at all, which is a property of the chart. It
// becomes per-finding only with --cluster, where each value can be observed
// separately.
func writeVerdicts(b *strings.Builder, verdicts []engine.Verdict) {
	if len(verdicts) == 0 {
		return
	}

	b.WriteString("\n")
	tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)

	var previous engine.Verdict
	for i, v := range verdicts {
		because := v.Because
		// Flux and Helm reach the same answer for the same reason. Printing
		// the sentence twice makes the reader compare two identical lines to
		// discover they are identical.
		if i > 0 && v.Result == previous.Result && v.Because == previous.Because {
			because = "same"
		}
		fmt.Fprintf(tw, "      %s\t%s\t%s\n", v.Engine, result(v.Result), because)
		previous = v
	}
	tw.Flush()

	if defect(verdicts) {
		b.WriteString("\n")
		b.WriteString("      No `lookup` anywhere in this chart, so nothing can stabilise this value.\n")
		b.WriteString("      That is a chart defect rather than an ArgoCD limitation — worth reporting\n")
		b.WriteString("      upstream, and pinning the value meanwhile.\n")
	}
}

// result renders a verdict word. CHURNS is the one word that should catch the
// eye in a wall of output; unknown is an admission and must not compete.
func result(r engine.Result) string {
	if r == engine.Churns {
		return strings.ToUpper(r.String())
	}
	return r.String()
}

// defect reports whether the findings are the chart's own fault.
//
// True only when an engine that DOES resolve lookup still churns, which idem
// concludes solely from there being no lookup in the chart. ArgoCD churning
// proves nothing here: its repo-server has no cluster access either way.
func defect(verdicts []engine.Verdict) bool {
	for _, v := range verdicts {
		if v.Result == engine.Churns && !v.Observed {
			return true
		}
	}
	return false
}

func writeFinding(tw io.Writer, f check.Finding) {
	object := f.Change.Object.Display()

	// An object present in one render and absent from another has no differing
	// field to name; the disappearance is the finding.
	if len(f.Change.Paths) == 0 {
		fmt.Fprintf(tw, "    %s\t%s\n", object, f.Change.Type)
		return
	}

	shown := min(len(f.Change.Paths), maxFields)
	for i, p := range f.Change.Paths[:shown] {
		if i == 0 {
			fmt.Fprintf(tw, "    %s\t%s\n", object, p.Path)
			continue
		}
		fmt.Fprintf(tw, "    \t%s\n", p.Path)
	}
	if elided := len(f.Change.Paths) - shown; elided > 0 {
		fmt.Fprintf(tw, "    \t… and %d more %s\n", elided, plural(elided, "field", "fields"))
	}
}

// writeUnevaluable lists charts that never rendered, and reports whether there
// were any.
//
// helm's stderr is several lines with blank lines in it. Printed verbatim it
// breaks out of the indentation and reads as idem itself having crashed, so
// every line is re-indented under the chart it belongs to.
func writeUnevaluable(b *strings.Builder, charts []Chart) bool {
	var failed []Chart
	for _, c := range charts {
		if c.Err != nil {
			failed = append(failed, c)
		}
	}
	if len(failed) == 0 {
		return false
	}

	b.WriteString("\n  could not be rendered\n")
	for _, c := range failed {
		fmt.Fprintf(b, "    %s\n", c.Name)
		for _, line := range errorLines(c.Err) {
			fmt.Fprintf(b, "      %s\n", line)
		}
	}
	return true
}

// errorLines flattens an error to indentable lines, dropping the blank runs.
func errorLines(err error) []string {
	var out []string
	for line := range strings.SplitSeq(err.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// writeRemediation prints the config that stops the churn.
//
// One block for the entire run, after the verdict, so it is pasted once rather
// than once per chart. It is emitted from the full finding - not from what the
// display showed - because the display caps fields per object and a block
// missing those fields would not actually stop the churn.
func writeRemediation(b *strings.Builder, charts []Chart) {
	var findings []check.Finding
	for _, c := range charts {
		if c.Err == nil {
			findings = append(findings, c.Findings...)
		}
	}

	entries := remediate.Entries(findings)
	if len(entries) == 0 {
		return
	}

	b.WriteString("\n  Add to your ArgoCD Application to stop the churn:\n\n")
	for line := range strings.SplitSeq(strings.TrimRight(remediate.YAML(entries), "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	// These pointers are computed for ArgoCD's default diff. Under
	// ServerSideDiff=true the ignore normalizer never sees the rendered config
	// at all - pointers must describe the API server's dry-run output, which
	// two `helm template` runs cannot observe. Saying so beats implying the
	// block works everywhere. (ServerSideApply=true is a different option on a
	// different code path and does not affect these pointers.)
	b.WriteString("\n  Computed for ArgoCD's default diff. With ServerSideDiff=true, pointers must\n")
	b.WriteString("  describe the API server's dry-run output, which idem cannot see.\n")

	// Blank line so an exit-code line printed after the report does not read
	// as part of the YAML the user is about to paste.
	b.WriteString("\n")
}

// verdict is the sentence the whole run reduces to.
//
// It names ArgoCD because `helm template` renders exactly as ArgoCD's
// repo-server does - lookup resolves to {} by construction - so this is an
// observation about ArgoCD, not an extrapolation to it.
func (r Report) verdict() string {
	total, churning, unevaluable := len(r.Charts), r.Churning(), r.Unevaluable()

	if churning == 0 && unevaluable == 0 {
		if total == 1 {
			return fmt.Sprintf("✓ %s renders consistently under ArgoCD.", r.Charts[0].Name)
		}
		return fmt.Sprintf("✓ All %d charts render consistently under ArgoCD.", total)
	}

	// Nothing rendered at all. "0 charts render consistently" is true but
	// reads as a verdict about charts that were never actually checked.
	if churning == 0 && total == unevaluable {
		return fmt.Sprintf("%d %s could not be rendered.", unevaluable, plural(unevaluable, "chart", "charts"))
	}

	var s string
	if churning > 0 {
		s = fmt.Sprintf("%d of %d %s will churn under ArgoCD", churning, total, plural(total, "chart", "charts"))
	} else {
		clean := total - unevaluable
		s = fmt.Sprintf("%d %s render consistently under ArgoCD", clean, plural(clean, "chart", "charts"))
	}
	if unevaluable > 0 {
		s += fmt.Sprintf("; %d could not be rendered", unevaluable)
	}
	return s + "."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
