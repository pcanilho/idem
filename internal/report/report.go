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
	"github.com/pcanilho/idem/internal/doctor"
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

	// RepoDir is the chart directory relative to the repository root, empty
	// when the chart is not inside it. Only used to place annotations.
	RepoDir string

	// Namespace is the release namespace this chart rendered into, and
	// NamespaceFrom what decided it: the delivery manifest that claims the
	// chart, NamespaceFromFlag when the user said, or empty when idem defaulted.
	//
	// Recorded because it changes the displayed identity of every object the
	// chart produced, and because reading it off the local kube context - as
	// idem used to - makes the same commit render differently on two machines.
	Namespace     string
	NamespaceFrom string

	// Deps is how the chart was made renderable, when that took any work.
	// Empty means it was renderable as it stood.
	Deps string

	// Changed marks a chart touched since Report.Since. Meaningless without it.
	Changed bool

	Findings []check.Finding

	// ServerOnly are differences observed only under the API-server condition,
	// on a chart whose `helm template` renders matched. Kept apart from
	// Findings because the two conditions answer for different engines: this
	// is churn under Flux and Helm, and demonstrably not under ArgoCD, whose
	// repo-server renders the condition that was identical.
	ServerOnly []check.Finding

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

	// Rewrites are fields the API server said it would change on admission.
	Rewrites []doctor.Rewrite

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

	// Root is the repository idem searched, used to check that a file an
	// annotation would point at actually exists.
	Root string

	// Since names the revision the ratchet measures from. Empty means no
	// ratchet, and every chart is reported.
	Since string

	// Cluster records that idem rendered against a cluster, and Context which
	// one. Which cluster it asked is exactly the kind of fact the provenance
	// line exists for.
	Cluster bool
	Context string

	// Engines are the engine names whose verdicts to display. Empty shows all.
	//
	// A chart's verdicts are always computed for every engine even when only
	// one is shown, because the "no lookup anywhere, so this is a chart defect"
	// conclusion is only reachable from an engine that resolves lookup.
	// Narrowing the display must not quietly discard it.
	Engines []string
}

// considered reports whether a chart is in scope for this run.
//
// With a ratchet, only charts changed since the revision are: adding a checker
// to an existing estate finds a pile of pre-existing issues, and a permanently
// red pipeline gets deleted rather than fixed.
func (r Report) considered(c Chart) bool {
	return r.Since == "" || c.Changed
}

// hidden counts what the ratchet is holding back, so it can be reported rather
// than silently dropped.
func (r Report) hidden() int {
	n := 0
	for _, c := range r.Charts {
		if r.considered(c) {
			continue
		}
		// A chart that would not render is NOT held back: it is reported and
		// it is fatal whatever the revision says. Counting it here as well
		// would report the same chart twice, in opposite directions.
		n += len(c.Findings) + len(c.Suppressed) + len(c.ServerOnly)
	}
	return n
}

// Churning counts charts that will churn.
//
// A finding the delivery config genuinely covers is NOT one of them: settled
// 2026-08-22 against golangci-lint, ESLint, Trivy, Semgrep, Checkov and Snyk,
// none of which let a suppressed finding reach the exit code. It is still
// printed, in its own section - Checkov's model, and idem's already - because
// visibility and the gate are different questions. The exception is a
// suppression selfHeal will undo, which is not suppression at all.
func (r Report) Churning() int {
	n := 0
	for _, c := range r.inScope() {
		if c.Err == nil && (len(c.Findings) > 0 || undone(c.Suppressed)) {
			n++
		}
	}
	return n
}

// undone reports whether any of these suppressions will not actually suppress.
//
// ignoreDifferences hides the diff, but without RespectIgnoreDifferences
// selfHeal re-applies the whole object anyway - so the field is rewritten on
// every sync while the Application reports Synced. That is churn, and it is
// the churn a user is most confident is handled.
func undone(suppressed []delivery.Suppressed) bool {
	for _, s := range suppressed {
		if s.By.SelfHeal && !s.By.Respected {
			return true
		}
	}
	return false
}

// inScope is the charts this run reports on.
func (r Report) inScope() []Chart {
	if r.Since == "" {
		return r.Charts
	}
	var out []Chart
	for _, c := range r.Charts {
		if c.Changed {
			out = append(out, c)
		}
	}
	return out
}

// ChurningWithLookup counts charts clean under `helm template` but differing
// with lookup resolved.
//
// Counted apart from Churning because the verdict sentence is framed on
// ArgoCD, and ArgoCD renders exactly the condition that was identical here.
// Folding the two together would state a falsehood about the named engine -
// but leaving it out of the run entirely would hide churn idem observed.
func (r Report) ChurningWithLookup() int {
	n := 0
	for _, c := range r.inScope() {
		if c.Err == nil && len(c.ServerOnly) > 0 {
			n++
		}
	}
	return n
}

// Unevaluable counts charts that could not be rendered.
//
// Scoped by the ratchet too: a chart that was already unrenderable before this
// branch is not this branch's problem, and an estate with one of those could
// otherwise never adopt the flag at all.
func (r Report) Unevaluable() int {
	n := 0
	// Every chart, in scope or not. The ratchet filters findings - claims idem
	// makes about a chart it analysed - and a chart it could not render is not
	// a finding, it is a gap in what was checked. Settled 2026-08-22 against
	// golangci-lint, which special-cases exactly this in its own diff
	// processor, and ESLint, mypy and ruff, which each make an analysis
	// failure unsuppressable by construction.
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

	scope := r.inScope()

	detail := false
	for _, c := range scope {
		if c.Err != nil || (len(c.Findings) == 0 && len(c.ServerOnly) == 0) {
			continue
		}
		writeChart(&b, c, r.Engines)
		detail = true
	}
	if writeSuppressed(&b, scope) {
		detail = true
	}
	if writePotential(&b, scope) {
		detail = true
	}
	if writeRewrites(&b, scope) {
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
	if n := r.hidden(); n > 0 {
		fmt.Fprintf(&b, "%s%d pre-existing %s not shown — drop the flag to see them.\n",
			indent, n, plural(n, "finding", "findings"))
	}
	fmt.Fprintf(&b, "  helm %s · %d rounds%s%s%s%s\n",
		r.Helm, r.Rounds, r.namespaceNote(), r.contextNote(), r.depsNote(), r.deliveryNote())
	writeRemediation(&b, scope)

	_, err := io.WriteString(w, b.String())
	return err
}

// writeChart prints one chart's findings, grouped by the template that
// produced each object - so a chart that regenerates six fields in one
// template reads as one place to look, not six.
func writeChart(b *strings.Builder, c Chart, show []string) {
	writeGroups(b, c, c.Findings)

	// Under its own heading, because the reader has to know these did NOT
	// happen under `helm template`: the object is real, the churn is real, and
	// the engine it applies to is not the one the rest of the output names.
	if len(c.ServerOnly) > 0 {
		b.WriteString("\n  identical under `helm template`; differs with `lookup` resolved\n")
		writeGroups(b, c, c.ServerOnly)
	}

	writeVerdicts(b, c.Verdicts, show)
}

// writeGroups prints findings grouped by the template that produced each
// object - so a chart that regenerates six fields in one template reads as one
// place to look, not six.
func writeGroups(b *strings.Builder, c Chart, findings []check.Finding) {
	groups := make(map[string][]check.Finding)
	for _, f := range findings {
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
			writeFinding(tw, f, findings)
		}
		tw.Flush()
	}
}

// writeVerdicts prints what each engine does with this chart.
//
// One block per chart rather than per finding: today the answer turns on
// whether the chart calls lookup at all, which is a property of the chart. It
// becomes per-finding only with --context, where each value can be observed
// separately.
func writeVerdicts(b *strings.Builder, verdicts []engine.Verdict, show []string) {
	if len(verdicts) == 0 {
		return
	}

	shown := make([]engine.Verdict, 0, len(verdicts))
	for _, v := range verdicts {
		if shows(show, v.Engine) {
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

// shows reports whether this engine's verdict is displayed. Empty shows all.
func shows(engines []string, name string) bool {
	return len(engines) == 0 || slices.Contains(engines, name)
}

// whyOf is analyze.Why, wrapped so the formatters share one spelling.
func whyOf(function string) string { return analyze.Why(function) }

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

// contextNote names the cluster idem asked, when it asked one.
//
// A pass that does not say what it checked is a pass you cannot trust, and
// "which cluster" changes the answer as much as which helm does.
func (r Report) contextNote() string {
	switch {
	case !r.Cluster:
		return ""
	case r.Context == "":
		return " · current kube context"
	}
	return " · context " + r.Context
}

// depsNote says what idem had to do to render at all.
//
// Only when something was actually resolved: on a repository that vendors its
// subcharts - which is most of them - nothing happened and saying so is noise.
func (r Report) depsNote() string {
	counts := make(map[string]int)
	for _, c := range r.Charts {
		if c.Deps != "" {
			counts[c.Deps]++
		}
	}
	if len(counts) == 0 {
		return ""
	}

	resolved := 0
	parts := make([]string, 0, len(counts)+1)
	for _, kind := range slices.Sorted(maps.Keys(counts)) {
		resolved += counts[kind]
		parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
	}
	if vendored := len(r.Charts) - resolved; vendored > 0 {
		parts = append([]string{fmt.Sprintf("%d vendored", vendored)}, parts...)
	}
	return " · " + strings.Join(parts, ", ")
}

// NamespaceFromFlag marks a namespace the user gave on the command line, so
// the provenance line credits the flag rather than inventing a file for it.
const NamespaceFromFlag = "--namespace"

// namespaceNote says which namespace the charts rendered into and who decided.
//
// Summarised rather than listed: one clause per namespace on a 16-chart estate
// is a wall, and every object's own line already carries its namespace.
func (r Report) namespaceNote() string {
	var ns, from string
	for _, c := range r.Charts {
		if c.Namespace == "" {
			continue
		}
		if ns != "" && ns != c.Namespace {
			return " · namespaces from delivery config"
		}
		ns, from = c.Namespace, c.NamespaceFrom
	}

	switch {
	case ns == "":
		return ""
	case from == "":
		return fmt.Sprintf(" · namespace %s (idem's own, nothing claims this chart)", ns)
	case from == NamespaceFromFlag:
		return fmt.Sprintf(" · namespace %s (%s)", ns, NamespaceFromFlag)
	}
	return fmt.Sprintf(" · namespace %s (%s)", ns, from)
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
		settled := len(c.Findings) == 0 && len(c.Suppressed) == 0 && len(c.ServerOnly) == 0 && c.Err == nil

		// Grouped by chart because the paths are chart-relative: on its own,
		// "templates/job.yaml:351" names a file in some chart the reader then
		// has to go and find.
		fmt.Fprintf(b, "\n    %s\n", c.Name)

		shown := min(len(c.Potential), maxFields)
		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, u := range c.Potential[:shown] {
			note := whyOf(u.Function)
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

// writeRewrites lists what the cluster would change as it admits an object.
//
// Kept apart from the findings, and hedged, because most of it is ordinary
// API-server defaulting that the engine already normalises away. The part that
// matters is a mutating webhook touching an object the engine manages, and the
// reader is better placed than idem to tell which is which.
func writeRewrites(b *strings.Builder, charts []Chart) bool {
	var any bool
	for _, c := range charts {
		for _, r := range c.Rewrites {
			if !any {
				b.WriteString("\n  the cluster rewrites these on admission\n")
				any = true
			}

			fmt.Fprintf(b, "\n    %s\n", r.Object.Display())

			tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
			shown := min(len(r.Changes), maxFields)
			for _, change := range r.Changes[:shown] {
				fmt.Fprintf(tw, "      %s\t%s\t%v\n", change.Path, kindOf(change), change.Value)
			}
			tw.Flush()

			if elided := len(r.Changes) - shown; elided > 0 {
				fmt.Fprintf(b, "      … and %d more %s\n", elided, plural(elided, "field", "fields"))
			}
			if r.Suppressed > 0 {
				// Never silently short: §9 explains why these cannot be
				// compared, and hiding the count would read as "that was all".
				fmt.Fprintf(b, "      %d %s not compared — quantities and ports are canonicalised on write\n",
					r.Suppressed, plural(r.Suppressed, "field", "fields"))
			}
		}
	}

	if any {
		b.WriteString("\n      Most of this is API-server defaulting, which your engine normalises\n")
		b.WriteString("      away. A mutating webhook touching an object it manages is not.\n")
	}
	return any
}

func kindOf(c doctor.Change) string {
	if c.Assigned {
		return "cluster assigns"
	}
	return "cluster defaults"
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
	scope := r.inScope()
	total, churning, unevaluable := len(scope), r.Churning(), r.Unevaluable()
	lookupOnly := r.ChurningWithLookup()

	// Nothing to gate on. "All 0 charts render consistently" is true and
	// useless; the reader wants to know the gate had nothing to look at.
	if r.Since != "" && total == 0 {
		return fmt.Sprintf("No charts changed since %s.", r.Since)
	}

	// The ratchet's sentence says what it measured against, because "1 of 2"
	// is a different claim when 14 other charts were left out of the count.
	if r.Since != "" && churning > 0 {
		return fmt.Sprintf("%d of the %d %s changed since %s will churn under ArgoCD%s.",
			churning, total, plural(total, "chart", "charts"), r.Since, lookupClause(lookupOnly))
	}

	if churning == 0 && unevaluable == 0 && lookupOnly == 0 {
		// Saying these charts "render consistently" would be false: they do
		// not, and idem measured that. What is true is that nothing will
		// churn, and the reason is the user's own config rather than the
		// chart - so the config gets the credit and the section above it
		// stops reading as decoration.
		if found, charts := r.covered(); found > 0 {
			return fmt.Sprintf("✓ Nothing will churn under ArgoCD — %d %s in %d of %d %s %s covered by your delivery config.",
				found, plural(found, "finding", "findings"), charts, total, plural(total, "chart", "charts"),
				plural(found, "is", "are"))
		}
		if total == 1 {
			return fmt.Sprintf("✓ %s renders consistently under ArgoCD.", r.Charts[0].Name)
		}
		return fmt.Sprintf("✓ All %d charts render consistently under ArgoCD.", total)
	}

	// ArgoCD is genuinely fine here and the sentence must not say otherwise -
	// but the engines that do a real install are not, and that is the finding.
	if churning == 0 && unevaluable == 0 {
		return fmt.Sprintf("%d of %d %s will churn under Flux and Helm: identical under `helm template`, different with `lookup` resolved.",
			lookupOnly, total, plural(total, "chart", "charts"))
	}

	// Nothing in scope rendered at all. "0 charts render consistently" is true
	// but reads as a verdict about charts that were never actually checked.
	//
	// Compared against the charts in scope, not against Unevaluable(), which
	// spans every chart: under a ratchet the two are different populations,
	// and comparing them makes one unrenderable chart elsewhere read as a
	// verdict about a chart this branch touched and idem rendered fine.
	if churning == 0 && total > 0 && total == r.failedInScope() {
		return fmt.Sprintf("%d %s could not be rendered.", unevaluable, plural(unevaluable, "chart", "charts"))
	}

	found, coveredCharts := r.covered()

	var s string
	if churning > 0 {
		s = fmt.Sprintf("%d of %d %s will churn under ArgoCD", churning, total, plural(total, "chart", "charts"))
	} else {
		// Counted, not subtracted. `total` is the charts in ratchet scope while
		// `unevaluable` spans every chart the run touched - the ratchet does
		// not hide a chart that would not render - so taking one from the
		// other mixes two populations and goes negative.
		clean := r.consistent()
		s = fmt.Sprintf("%d %s consistently under ArgoCD", clean, plural(clean, "chart renders", "charts render"))
	}
	if coveredCharts > 0 {
		// The verb agrees with the findings, not the charts: "4 findings in 1
		// chart is covered" is what keying it on the wrong noun produces.
		s += fmt.Sprintf("; %d %s in %d %s %s covered by your delivery config",
			found, plural(found, "finding", "findings"),
			coveredCharts, plural(coveredCharts, "chart", "charts"),
			plural(found, "is", "are"))
	}
	if unevaluable > 0 {
		s += fmt.Sprintf("; %d could not be rendered", unevaluable)
		// The counts either side of that semicolon come from different
		// populations under a ratchet - charts in scope, and every chart -
		// so without saying so it reads as N of the ones this branch touched.
		if r.Since != "" {
			s += ", which the ratchet does not hide"
		}
	}
	return s + lookupClause(lookupOnly) + "."
}

// covered counts findings the delivery config genuinely handles, and the
// charts they are in. Suppressions selfHeal will undo are not among them.
func (r Report) covered() (findings, charts int) {
	for _, c := range r.inScope() {
		n := 0
		for _, s := range c.Suppressed {
			if !s.By.SelfHeal || s.By.Respected {
				n++
			}
		}
		if n > 0 {
			findings += n
			charts++
		}
	}
	return findings, charts
}

// consistent counts charts in scope that idem rendered, compared, and found
// nothing to say about. A chart whose findings are all covered by the delivery
// config is not one of them: it did not render consistently, it is handled,
// and it gets its own clause.
func (r Report) consistent() int {
	n := 0
	for _, c := range r.inScope() {
		if c.Err != nil || len(c.Findings) > 0 || len(c.Suppressed) > 0 || len(c.ServerOnly) > 0 {
			continue
		}
		n++
	}
	return n
}

// failedInScope counts charts the ratchet reports on that would not render.
func (r Report) failedInScope() int {
	n := 0
	for _, c := range r.inScope() {
		if c.Err != nil {
			n++
		}
	}
	return n
}

// lookupClause reports churn only the API-server condition saw, without
// touching the ArgoCD claim the rest of the sentence makes.
func lookupClause(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; %d %s only with `lookup` resolved and will churn under Flux and Helm",
		n, plural(n, "differs", "differ"))
}

// siblingsOf is the finding set a consequence is judged against: a checksum
// annotation only protects the workload whose findings sit beside it.
//
// ServerOnly is populated only on a chart with no client findings at all, so
// one set or the other is empty and there is nothing to disambiguate.
func siblingsOf(c Chart) []check.Finding {
	if len(c.Findings) == 0 {
		return c.ServerOnly
	}
	return c.Findings
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
