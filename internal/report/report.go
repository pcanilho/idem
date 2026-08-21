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

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/delivery"
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

	// Suppressed are findings the delivery config already covers, and Maybe
	// are those a jq expression might cover. Shown, never guessed at.
	Suppressed []delivery.Suppressed
	Maybe      []delivery.Suppressed

	// Potential are non-deterministic functions the chart calls but which did
	// not produce a difference this render. A warning, never a fact.
	Potential []analyze.Use

	// Err is set when the chart could not be rendered at all. That is exit 2
	// and always fatal - a chart silently skipped is the bug idem exists for.
	Err error
}

// Report is a whole run.
type Report struct {
	Charts []Chart
	Helm   string
	Rounds int

	// Delivery names the engine manifests idem read. Reading files outside the
	// chart directory has to be as visible as which helm it rendered with.
	Delivery []string

	// Engines are the engine names whose verdicts to display. Empty shows all.
	//
	// A chart's verdicts are always computed for every engine even when only
	// one is shown, because the "no lookup anywhere, so this is a chart defect"
	// conclusion is only reachable from an engine that resolves lookup.
	// Narrowing the display must not quietly discard it.
	Engines []string
}

// Churning counts charts with at least one finding.
func (r Report) Churning() int {
	n := 0
	for _, c := range r.Charts {
		// A suppressed finding still counts, for now. Whether a matched
		// suppression should leave the count - and so change --strict's exit
		// code - is still an open decision, and silently dropping it here
		// would settle that question by accident.
		if c.Err == nil && (len(c.Findings) > 0 || len(c.Suppressed) > 0) {
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
		writeChart(&b, c, r.Engines)
		detail = true
	}
	if writeSuppressed(&b, r.Charts) {
		detail = true
	}
	if writePotential(&b, r.Charts) {
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
	fmt.Fprintf(&b, "  helm %s · %d rounds%s\n", r.Helm, r.Rounds, r.deliveryNote())
	writeRemediation(&b, r.Charts)

	_, err := io.WriteString(w, b.String())
	return err
}

// writeChart prints one chart's findings, grouped by the template that
// produced each object - so a chart that regenerates six fields in one
// template reads as one place to look, not six.
func writeChart(b *strings.Builder, c Chart, show []string) {
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
			writeFinding(tw, f, c.Findings)
		}
		tw.Flush()
	}

	writeVerdicts(b, c.Verdicts, show)
}

// writeVerdicts prints what each engine does with this chart.
//
// One block per chart rather than per finding: today the answer turns on
// whether the chart calls lookup at all, which is a property of the chart. It
// becomes per-finding only with --cluster, where each value can be observed
// separately.
func writeVerdicts(b *strings.Builder, verdicts []engine.Verdict, show []string) {
	if len(verdicts) == 0 {
		return
	}

	shown := make([]engine.Verdict, 0, len(verdicts))
	for _, v := range verdicts {
		if len(show) == 0 || slices.Contains(show, v.Engine) {
			shown = append(shown, v)
		}
	}

	b.WriteString("\n")
	tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)

	var previous engine.Verdict
	for i, v := range shown {
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

func writeFinding(tw io.Writer, f check.Finding, siblings []check.Finding) {
	object := f.Change.Object.Display()

	// An object present in one render and absent from another has no differing
	// field to name; the disappearance is the finding.
	if len(f.Change.Paths) == 0 {
		fmt.Fprintf(tw, "    %s\t%s\t\n", object, f.Change.Type)
		return
	}

	// What it costs goes on the first line only: it is a property of the
	// object, not of each field.
	cost := consequenceOf(f, siblings).Text

	shown := min(len(f.Change.Paths), maxFields)
	for i, p := range f.Change.Paths[:shown] {
		if i == 0 {
			fmt.Fprintf(tw, "    %s\t%s\t%s\n", object, p.Path, cost)
			continue
		}
		fmt.Fprintf(tw, "    \t%s\t\n", p.Path)
	}
	if elided := len(f.Change.Paths) - shown; elided > 0 {
		fmt.Fprintf(tw, "    \t… and %d more %s\t\n", elided, plural(elided, "field", "fields"))
	}
}

// deliveryNote acknowledges the manifests idem read outside the chart tree.
func (r Report) deliveryNote() string {
	if len(r.Delivery) == 0 {
		return ""
	}
	return fmt.Sprintf(" · delivery config from %d %s", len(r.Delivery), plural(len(r.Delivery), "manifest", "manifests"))
}

// writeSuppressed lists findings the delivery config already covers, and
// reports whether there were any.
//
// Kept apart from the churning findings because they are a different kind of
// statement: one is what idem measured, the other is what the user's own
// config says about it.
func writeSuppressed(b *strings.Builder, charts []Chart) bool {
	var covered, broken []delivery.Suppressed
	for _, c := range charts {
		for _, s := range c.Suppressed {
			// ignoreDifferences hides the diff, but without
			// RespectIgnoreDifferences selfHeal re-applies the object anyway -
			// so the user believes this is handled when it is not.
			if s.By.SelfHeal && !s.By.Respected {
				broken = append(broken, s)
				continue
			}
			covered = append(covered, s)
		}
	}
	if len(covered) == 0 && len(broken) == 0 {
		return false
	}

	if len(covered) > 0 {
		b.WriteString("\n  already suppressed by your delivery config\n")
		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, s := range covered {
			fmt.Fprintf(tw, "    %s\t%s\t%s\n",
				s.Finding.Change.Object.Display(), strings.Join(s.By.Pointers, " "), s.By.File)
		}
		tw.Flush()
	}

	if len(broken) > 0 {
		b.WriteString("\n  suppressed, but selfHeal will re-apply it anyway\n")
		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, s := range broken {
			fmt.Fprintf(tw, "    %s\t%s\n", s.Finding.Change.Object.Display(), s.By.File)
		}
		tw.Flush()
		b.WriteString("      add RespectIgnoreDifferences=true to that Application's syncOptions\n")
	}

	return true
}

// writePotential lists functions that could make a chart churn but did not
// this time, and reports whether there were any.
//
// Its own section, never counted, never fatal - docs/design.md §5. A static
// warning is sometimes wrong, and a tool that cries wolf about the potential
// case teaches you to distrust it about the observed one. But the failure that
// motivated idem was a pin that silently stopped applying, so hiding these
// would hide the thing most worth knowing.
func writePotential(b *strings.Builder, charts []Chart) bool {
	var any bool
	for _, c := range charts {
		if len(c.Potential) == 0 {
			continue
		}
		if !any {
			b.WriteString("\n  — potential · not counted, not fatal —\n")
			any = true
		}

		// idem cannot attribute an observed difference to a particular
		// function, so on a chart that DID churn it must not claim this one
		// stayed quiet.
		settled := len(c.Findings) == 0 && len(c.Suppressed) == 0 && c.Err == nil

		// Grouped by chart because the paths are chart-relative: on its own,
		// "templates/job.yaml:351" names a file in some chart the reader then
		// has to go and find.
		fmt.Fprintf(b, "\n    %s\n", c.Name)

		shown := min(len(c.Potential), maxFields)
		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, u := range c.Potential[:shown] {
			note := analyze.Why(u.Function)
			if settled {
				note += ", did not fire this render"
			}
			// file:line last, and deliberately: alignment pads every row to
			// the widest cell, and a deeply vendored subchart path would drag
			// all of them out past a readable width.
			fmt.Fprintf(tw, "      %s\t%s\t%s:%d\n", u.Function, note, u.File, u.Line)
		}
		tw.Flush()

		if elided := len(c.Potential) - shown; elided > 0 {
			fmt.Fprintf(b, "      … and %d more\n", elided)
		}
	}

	return any
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
