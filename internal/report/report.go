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
	"math"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/delivery"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/doctor"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/remediate"
)

// fields is how many entries a section lists before eliding, which is
// maxFields unless the reader asked for all of them.
func (r Report) fields() int { return Fields(r.Verbose) }

// Fields is the same answer for callers that render without a Report - `idem
// diff` has no chart and no engine, but it has the same cap and the same flag.
func Fields(verbose bool) int {
	if verbose {
		return math.MaxInt
	}
	return maxFields
}

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

	// Release is the release name, when the delivery config named one that
	// differs from the chart name. Empty means idem used the chart name.
	// Reported for the same reason the namespace is: .Release.Name is in the
	// name of nearly every object below.
	Release string

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

	// Skipped counts objects excluded from the comparison because no engine
	// applies them - Helm test and rollback hooks. Reported rather than
	// dropped in silence: what was NOT checked is part of what was checked.
	Skipped int

	// ServerSideDiff records that a manifest claiming this chart asks for
	// server-side diff. False means no manifest said so - NOT that the mode is
	// off, which idem cannot know.
	ServerSideDiff bool

	// Unresolved names values the delivery config supplies through a generator
	// idem cannot expand - one that reads the cluster rather than the
	// repository. With Err set it means idem could not BUILD this release,
	// which is a gap in idem rather than a defect in the chart; without Err it
	// means the chart rendered, but as a release nobody deploys.
	Unresolved []string

	// Err is set when the chart could not be rendered at all. That is exit 2
	// and always fatal - a chart silently skipped is the bug idem exists for -
	// unless Unresolved says why, in which case it is Unconstructed instead.
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

	// Verbose expands every finding rather than capping each at maxFields.
	//
	// The cap keeps a Secret whose whole .data regenerates from filling the
	// terminal, but that is also the case idem exists for - so there has to be
	// a way to see the rest, or "… and 4 more" is a dead end.
	Verbose bool

	// Engines are the engine names whose verdicts to display. Empty shows all.
	//
	// A chart's verdicts are always computed for every engine even when only
	// one is shown, because the "no lookup anywhere, so this is a chart defect"
	// conclusion is only reachable from an engine that resolves lookup.
	// Narrowing the display must not quietly discard it.
	Engines []string

	// Libraries counts `type: library` charts found and deliberately not
	// rendered. They are not failures - helm itself refuses to template one -
	// but they are charts the user pointed idem at and did not get an answer
	// about, so the count is printed rather than swallowed.
	Libraries int
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
// unbuilt reports whether idem could not construct this release, as opposed to
// the chart failing on its own terms.
//
// The distinction is the whole point: a chart whose `required` guard fires
// because idem withheld a value its Application supplies is a chart working
// exactly as written.
func unbuilt(c Chart) bool { return c.Err != nil && len(c.Unresolved) > 0 }

// Unconstructed counts releases idem could not build.
//
// Reported and counted but never fatal: idem could not construct the release,
// which is a limit of idem rather than a defect in the chart, and failing a
// build for it would make every estate driven by a cluster-reading generator
// permanently red. Counted so the gap cannot go unnoticed.
func (r Report) Unconstructed() int {
	n := 0
	for _, c := range r.Charts {
		if unbuilt(c) {
			n++
		}
	}
	return n
}

func (r Report) Unevaluable() int {
	n := 0
	// Every chart, in scope or not. The ratchet filters findings - claims idem
	// makes about a chart it analysed - and a chart it could not render is not
	// a finding, it is a gap in what was checked. Settled 2026-08-22 against
	// golangci-lint, which special-cases exactly this in its own diff
	// processor, and ESLint, mypy and ruff, which each make an analysis
	// failure unsuppressable by construction.
	for _, c := range r.Charts {
		if c.Err != nil && !unbuilt(c) {
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
		writeChart(&b, r, c, r.Engines)
		detail = true
	}
	if writeSuppressed(&b, scope) {
		detail = true
	}
	if r.writePotential(&b, scope, r.fields()) {
		detail = true
	}
	if writeRewrites(&b, scope, r.fields()) {
		detail = true
	}
	if writeUnconstructed(&b, r.Charts) {
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
	b.WriteString(provenance(fmt.Sprintf("  helm %s · %d rounds%s%s%s%s%s%s%s",
		r.Helm, r.Rounds, r.releaseNote(), r.namespaceNote(), r.contextNote(), r.skippedNote(), r.libraryNote(), r.depsNote(), r.deliveryNote())))
	writeRemediation(&b, scope, r.Engines)

	return emit(w, b.String())
}

// writeChart prints one chart's findings, grouped by the template that
// produced each object - so a chart that regenerates six fields in one
// template reads as one place to look, not six.
func writeChart(b *strings.Builder, r Report, c Chart, show []string) {
	writeGroups(b, r, c, c.Findings)

	// Under its own heading, because the reader has to know these did NOT
	// happen under `helm template`: the object is real, the churn is real, and
	// the engine it applies to is not the one the rest of the output names.
	if len(c.ServerOnly) > 0 {
		b.WriteString("\n  identical under `helm template`; differs with `lookup` resolved\n")
		writeGroups(b, r, c, c.ServerOnly)
	}

	writeVerdicts(b, c, show)
}

// reorders reports whether any of a chart's findings is a reordered list.
func reorders(c Chart) bool {
	for _, f := range slices.Concat(c.Findings, c.ServerOnly) {
		for _, p := range f.Change.Paths {
			if p.Reordered {
				return true
			}
		}
	}
	return false
}

// churnsBeyondOrdering reports whether anything other than a list's order
// differed.
//
// It gates the lookup conclusion. On a chart whose only churn is ordering,
// "nothing can stabilise this value ... pin the value meanwhile" is a confident
// wrong answer twice over: no value changed, and ordering IS stabilisable - at
// the source, which is what the reorder note says instead.
func churnsBeyondOrdering(c Chart) bool {
	for _, f := range slices.Concat(c.Findings, c.ServerOnly) {
		// An object that renders in one round and not another carries no
		// paths at all, and is emphatically not a reordering.
		if len(f.Change.Paths) == 0 {
			return true
		}
		for _, p := range f.Change.Paths {
			if !p.Reordered {
				return true
			}
		}
	}
	return false
}

// writeGroups prints findings grouped by the template that produced each
// object - so a chart that regenerates six fields in one template reads as one
// place to look, not six.
func writeGroups(b *strings.Builder, r Report, c Chart, findings []check.Finding) {
	groups := make(map[string][]check.Finding)
	for _, f := range findings {
		key := r.sourcePath(c, f.Source)
		if key == "" {
			key = c.Name + " " + unknownSource
		}
		groups[key] = append(groups[key], f)
	}

	for _, source := range slices.Sorted(maps.Keys(groups)) {
		fmt.Fprintf(b, "\n  %s\n", source)

		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, f := range groups[source] {
			writeFinding(tw, f, findings, r.fields())
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
func writeVerdicts(b *strings.Builder, c Chart, show []string) {
	verdicts := c.Verdicts
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

	if defect(verdicts) && churnsBeyondOrdering(c) {
		b.WriteString("\n")
		b.WriteString("      No `lookup` anywhere in this chart, so nothing can stabilise this value.\n")
		b.WriteString("      That is a chart defect rather than an ArgoCD limitation — worth reporting\n")
		b.WriteString("      upstream, and pinning the value meanwhile.\n")
	}

	// Why this finding comes with no config to paste. Verified against both
	// engines rather than assumed: ArgoCD's ignoreDifferences offers
	// jsonPointers, jqPathExpressions and managedFieldsManagers, and none of
	// them ignores ORDER while still comparing CONTENT - for the equivalent
	// HPA spec.metrics reordering ArgoCD's own docs say to reorder the source
	// in Git. Flux's driftDetection.ignore entries are RFC 6901 removes, so
	// the only expressible thing is removing the list whole. A pointer at the
	// list would suppress its contents to hide its order, and a genuine change
	// to any element would then never sync.
	//
	// It names the class of cause and not the call. `keys` and `values` build
	// a slice from Go's map iteration order and `sortAlpha` is their fix, but
	// `shuffle` and a `lookup` returning cluster order produce the same shape -
	// and a rendered value cannot be traced back to the call that produced it.
	//
	// It must not read as reassurance either. A reordered initContainers list
	// changes execution order and is a real bug; these are facts and a fix,
	// not an all-clear.
	if reorders(c) {
		b.WriteString("\n")
		b.WriteString("      The elements are unchanged; only their order differs. No\n")
		b.WriteString("      ignoreDifferences or driftDetection.ignore can ignore ordering alone —\n")
		b.WriteString("      a list built from a Go map has no order, and `sortAlpha` gives it one.\n")
	}
}

// sourcePath is the path to print for a finding: resolved against the
// repository when idem can confirm the file is there, and exactly what helm
// said when it cannot.
//
// The printed path is the one the reader is meant to open, so a chart-relative
// path opens from nowhere. Resolution is `locate`, the same check `-o github`
// already uses to decide whether to annotate - a path that resolves to a file
// nobody has is worse than the original, which at least says what helm saw.
func (r Report) sourcePath(c Chart, source string) string {
	if source == "" {
		return ""
	}
	if path, ok := r.locate(c, trimChartPrefix(source)); ok {
		return path
	}
	return source
}

// statesServerSideDiff reports whether EVERY chart contributing to this fix
// block was claimed by a manifest asking for server-side diff.
//
// All, not any. This was written as any, on the reasoning that the manifest
// which states the mode is the informative one - but the sentence it gates is
// singular ("This Application sets ServerSideDiff=true") and is printed once,
// beside a pointer belonging to one object from one Application. With two
// charts and one annotation, idem stated as a fact the opposite of what it had
// just read from the manifest the reader was about to edit.
//
// Disagreement therefore falls back to the hedge, which is also what an empty
// run gets. False never means "the mode is off": it can be set cluster-wide by
// controller.diff.server.side in argocd-cmd-params-cm, which is in no manifest
// idem reads.
func statesServerSideDiff(charts []Chart) bool {
	var stated bool
	for _, c := range charts {
		if c.Err != nil || len(c.Findings) == 0 {
			continue
		}
		if !c.ServerSideDiff {
			return false
		}
		stated = true
	}
	return stated
}

// stringDataPointer reports whether a block carries a pointer whose evaluation
// depends on which diff mode ArgoCD is in.
//
// Only stringData does. idem cannot see which mode an install is in - the
// global switch is `controller.diff.server.side` in argocd-cmd-params-cm, which
// is not in any manifest idem reads - so the caveat has to be earned by what
// the block CONTAINS rather than by guessing at the cluster's configuration.
func stringDataPointer(entries []remediate.Entry) bool {
	for _, e := range entries {
		for _, p := range e.Pointers {
			if strings.HasPrefix(p, "/stringData/") {
				return true
			}
		}
	}
	return false
}

// usePath is sourcePath for a non-deterministic call site.
//
// No trimChartPrefix, and that is the whole difference: helm's `# Source:`
// leads with the chart name, while analyze walks the chart directory and
// reports paths already relative to it. Trimming here would eat `templates/`
// off every path and place none of them.
func (r Report) usePath(c Chart, file string) string {
	if path, ok := r.locate(c, file); ok {
		return path
	}
	return file
}

// emit writes a rendered report, with trailing whitespace removed from every
// line.
//
// One choke point rather than a conditional at each of the eleven tabwriter
// call sites. tabwriter pads every column to the width of the widest cell
// INCLUDING the last, so any row whose final cell is empty - a finding with no
// consequence, a continuation row - ends in spaces. Invisible on a terminal,
// very visible in a diff of captured output, which is what a CI log and a
// golden file both are. Doing it here also means a row added later cannot
// reintroduce it.
func emit(w io.Writer, s string) error {
	var b strings.Builder
	b.Grow(len(s))
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.TrimRight(line, " \t"))
	}
	_, err := io.WriteString(w, b.String())
	return err
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

func writeFinding(tw io.Writer, f check.Finding, siblings []check.Finding, limit int) {
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

	shown := min(len(f.Change.Paths), limit)
	for i, p := range f.Change.Paths[:shown] {
		if i == 0 {
			fmt.Fprintf(tw, "    %s\t%s\t%s\n", object, pathCell(p), cost)
			continue
		}
		fmt.Fprintf(tw, "    \t%s\t\n", pathCell(p))
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

// skippedNote reports objects left out of the comparison.
//
// Only when there were any. ArgoCD skips manifests carrying hooks it does not
// support, and a helm test hook is created only by `helm test` and never
// reconciled - so churn in one is not churn. Saying nothing at all would make
// this indistinguishable from having compared them.
func (r Report) skippedNote() string {
	n := 0
	for _, c := range r.inScope() {
		n += c.Skipped
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d hook %s not compared", n, plural(n, "object", "objects"))
}

// libraryNote reports charts idem found and deliberately did not render.
//
// `type: library` is correct Helm and helm itself will not template one:
// "library charts are not installable". Skipping it is right; skipping it
// SILENTLY is not, because a gate that quietly checks less than the user
// pointed it at is the failure mode idem exists to prevent. Same rule as
// skippedNote above.
func (r Report) libraryNote() string {
	if r.Libraries == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d library %s not rendered", r.Libraries, plural(r.Libraries, "chart", "charts"))
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

// DefaultNamespace is the namespace idem renders into when nothing claims the
// chart. Named here because the report has to know which value is the boring
// one that needs no explaining.
const DefaultNamespace = "default"

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
	case from == "" && ns == DefaultNamespace:
		// idem picked the boring default and nothing claimed the chart, which
		// is the modal case. Saying so on every run - including the two-line
		// success, where it is half the output - is noise, and the reader
		// loses nothing: the default is what "unsaid" means.
		return ""
	case from == "":
		return fmt.Sprintf(" · namespace %s (idem's own, nothing claims this chart)", ns)
	case from == NamespaceFromFlag:
		return fmt.Sprintf(" · namespace %s (%s)", ns, NamespaceFromFlag)
	}
	return fmt.Sprintf(" · namespace %s (%s)", ns, from)
}

// releaseNote says the release names did not come from the chart directories.
//
// Summarised, not listed: it is one clause however many charts it covers, and
// each object's own line already carries the name it produced.
func (r Report) releaseNote() string {
	for _, c := range r.Charts {
		if c.Release != "" {
			return " · release names from delivery config"
		}
	}
	return ""
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
	maybe := 0
	for _, c := range charts {
		maybe += len(c.Maybe)
	}
	if len(covered) == 0 && len(broken) == 0 && maybe == 0 {
		return false
	}

	if len(covered) > 0 {
		b.WriteString("\n  already suppressed by your delivery config\n")
		writeByManifest(b, covered)
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

	writeMaybe(b, charts)
	return true
}

// writeMaybe names findings a rule reaches only through a jq expression.
//
// idem does not vendor a jq engine, so it cannot say whether such a rule covers
// the finding - and this set was computed, carried on the Report, and rendered
// nowhere, which meant idem knew the user's config MIGHT already handle this
// and said nothing. Absent knowledge is reported as absent; knowledge withheld
// is the same defect the provenance rule exists to prevent.
//
// Its own heading, below the covered ones, because "may be covered" and "is
// covered" are different claims and the counted total treats them differently:
// these findings are still counted and still fail --strict, exactly because
// idem could not confirm them.
func writeMaybe(b *strings.Builder, charts []Chart) {
	var maybe []delivery.Suppressed
	for _, c := range charts {
		maybe = append(maybe, c.Maybe...)
	}
	if len(maybe) == 0 {
		return
	}

	b.WriteString("\n  may already be covered — idem does not evaluate jq, so these still count\n")
	writeByManifest(b, maybe)
}

// writeUnconstructed lists releases idem could not build, and charts it built
// only partially, naming the values it could not supply.
//
// Its own section and its own wording: "could not be rendered" would blame the
// chart for a value idem withheld, and the two need opposite responses - one is
// a chart to fix, the other is a generator idem cannot expand.
func writeUnconstructed(b *strings.Builder, charts []Chart) bool {
	var missing, partial []Chart
	for _, c := range charts {
		switch {
		case unbuilt(c):
			missing = append(missing, c)
		case len(c.Unresolved) > 0:
			partial = append(partial, c)
		}
	}
	if len(missing) == 0 && len(partial) == 0 {
		return false
	}

	if len(missing) > 0 {
		b.WriteString("\n  could not be built — values come from a generator idem cannot expand\n")
		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, c := range missing {
			fmt.Fprintf(tw, "    %s\tneeds %s\n", c.Name, strings.Join(c.Unresolved, ", "))
		}
		tw.Flush()
		b.WriteString("      The chart is not at fault: its guards fired because idem withheld a value.\n")
	}

	if len(partial) > 0 {
		// Not "only a generator can supply": the same shape now carries a
		// multi-source $ref, which is another repository rather than a
		// generator, and Flux valuesFrom, which is a cluster read.
		b.WriteString("\n  rendered without values idem cannot reach\n")
		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, c := range partial {
			fmt.Fprintf(tw, "    %s\tmissing %s\n", c.Name, strings.Join(c.Unresolved, ", "))
		}
		tw.Flush()
		b.WriteString("      Findings above are about this release, which is not the one deployed.\n")
	}
	return true
}

// maxLine is the width the provenance line wraps at.
//
// It accumulates a clause per thing worth saying - release names, namespaces,
// which cluster, how dependencies resolved, how many delivery manifests - and
// a full estate run measured 199 characters. Wrapped rather than trimmed,
// because every clause is there to be read.
const maxLine = 96

// provenance wraps the run's footer on its own separators, so a continuation
// line still reads as part of the same sentence.
func provenance(line string) string {
	var out strings.Builder
	width := 0

	for i, part := range strings.Split(line, " · ") {
		switch {
		case i == 0:
			width = len([]rune(part))
			out.WriteString(part)
		case width+len([]rune(part))+3 > maxLine:
			width = len([]rune(part)) + 4
			out.WriteString("\n    · " + part)
		default:
			width += len([]rune(part)) + 3
			out.WriteString(" · " + part)
		}
	}

	out.WriteString("\n")
	return out.String()
}

// maxValue is how much of a rewritten value is printed.
//
// The API server hands back whole annotation maps - one measured 298
// characters on a real cluster, which wraps three times and takes the column
// layout with it. Columns are this output's entire readability strategy, which
// is also why it has no box drawing and no emoji.
const maxValue = 60

// clip renders a value at a width the columns can hold, unless the reader
// asked for all of it. The ellipsis is there so the truncation is visible
// rather than silent.
func clip(value any, limit int) string {
	text := fmt.Sprint(value)
	if limit > maxFields || len([]rune(text)) <= maxValue {
		return text
	}
	return string([]rune(text)[:maxValue]) + "…"
}

// pathCell renders the path column, annotating a reordered list.
//
// Without the annotation the row is a bare `.spec.env`, which a reader takes
// for "this field's value changed" - and then looks for the pointer to
// suppress, which is exactly the fix that must not be offered here. Saying how
// many elements there are, and that they are the same ones, is the whole
// finding in four words.
//
// The annotation is appended AFTER clipping, so a long path never eats it.
func pathCell(p diff.PathDiff) string {
	cell := clipPath(p.Path.String())
	if !p.Reordered {
		return cell
	}
	return cell + reorderNote(p)
}

// reorderNote annotates a path as a reordering, or says nothing.
//
// Shared by text, markdown and github so the three channels cannot describe the
// same finding differently - markdown is a PR comment and github an inline
// annotation, and a reviewer may well see both.
func reorderNote(p diff.PathDiff) string {
	if !p.Reordered {
		return ""
	}
	// Either side gives the count - they hold the same elements, which is the
	// finding. Left first only because it is the round everything else is
	// compared against.
	n, ok := elementCount(p.Left)
	if !ok {
		n, ok = elementCount(p.Right)
	}
	if !ok {
		return " (reordered)"
	}
	return fmt.Sprintf(" (reordered — same %d elements)", n)
}

// orderingHasNoSuppression is the one-line form of the note writeVerdicts
// prints wrapped, for the two channels that carry no fix block to explain
// themselves with.
const orderingHasNoSuppression = "A reordered list has no fix block: no `ignoreDifferences` or " +
	"`driftDetection.ignore` can ignore ordering alone, and one that tried would suppress the " +
	"list's contents too. `sortAlpha` fixes it at the source."

func elementCount(v any) (int, bool) {
	list, ok := v.([]any)
	return len(list), ok
}

// maxPath is how many columns an object path may occupy.
const maxPath = 80

// clipPath shortens an object path from the MIDDLE.
//
// An object path has no length bound - a deeply nested difference printed a
// single 4,000-character line, destroying the column alignment that the
// no-emoji and no-box-drawing rules exist to protect.
//
// Middle, not end, and unlike a FILE path it is clipped at all. A file path is
// never truncated because Phase 1 made printed paths openable and that wins. An
// object path is not opened: it is a coordinate, and both ends carry meaning -
// the root says which section, the leaf says which field. `.spec…[3].image`
// identifies far more than the first eighty characters of
// `.spec.template.spec.containers…` would.
func clipPath(p string) string {
	r := []rune(p)
	if len(r) <= maxPath {
		return p
	}
	keep := maxPath - 1
	head := keep / 2
	return string(r[:head]) + "…" + string(r[len(r)-(keep-head):])
}

// heading names a chart, qualified by its directory only when another chart in
// the same run shares the name.
//
// Two charts called `app` produce two identical blocks of chart-relative
// paths, and nothing in either says which is which. Qualifying every heading
// would be noise on the common case, where the name is already the answer.
func heading(c Chart, charts []Chart) string {
	if c.RepoDir == "" {
		return c.Name
	}
	for _, other := range charts {
		if other.Name == c.Name && other.RepoDir != c.RepoDir {
			return c.RepoDir
		}
	}
	return c.Name
}

// writeByManifest groups suppressed findings under the file that suppressed
// them.
//
// The manifest path is the one cell that is not per-finding - on a real estate
// it is identical on every row, and repeating it alongside an object identity
// and a JSON pointer is what pushes those rows past any terminal width.
func writeByManifest(b *strings.Builder, suppressed []delivery.Suppressed) {
	byFile := make(map[string][]delivery.Suppressed)
	for _, s := range suppressed {
		byFile[s.By.File] = append(byFile[s.By.File], s)
	}

	for _, file := range slices.Sorted(maps.Keys(byFile)) {
		fmt.Fprintf(b, "\n    %s\n", file)

		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, s := range byFile[file] {
			fmt.Fprintf(tw, "      %s\t%s\n",
				s.Finding.Change.Object.Display(), strings.Join(s.By.Pointers, " "))
		}
		tw.Flush()
	}
}

// writePotential lists functions that could make a chart churn but did not
// this time, and reports whether there were any.
//
// Its own section, never counted, never fatal - docs/design.md §5. A static
// warning is sometimes wrong, and a tool that cries wolf about the potential
// case teaches you to distrust it about the observed one. But the failure that
// motivated idem was a pin that silently stopped applying, so hiding these
// would hide the thing most worth knowing.
func (r Report) writePotential(b *strings.Builder, charts []Chart, limit int) bool {
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

		// Grouped by chart because these are a property of the chart rather
		// than of any one object, and the closing sentence below is one per
		// chart: whether anything churned decides which of the two it is.
		fmt.Fprintf(b, "\n    %s\n", heading(c, charts))

		shown := min(len(c.Potential), limit)
		tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
		for _, u := range c.Potential[:shown] {
			// Just the reason. That nothing fired this render is said once
			// below, for the whole chart, rather than repeated on every row.
			note := whyOf(u.Function)
			// file:line last, and deliberately: alignment pads every row to
			// the widest cell, and a deeply vendored subchart path would drag
			// all of them out past a readable width.
			fmt.Fprintf(tw, "      %s\t%s\t%s:%d\n", u.Function, note, r.usePath(c, u.File), u.Line)
		}
		tw.Flush()

		if elided := len(c.Potential) - shown; elided > 0 {
			fmt.Fprintf(b, "      … and %d more\n", elided)
		}

		// The observed findings above get three sentences saying what will
		// happen. Without one here, a reader has a function name, a reason and
		// a line, and no way to tell whether any of it asks something of them.
		if settled {
			b.WriteString("\n      Nothing differed this render, so nothing is wrong today. These are the\n")
			b.WriteString("      places it could start: whatever pins them is holding, and the failure\n")
			b.WriteString("      idem exists for was a pin that silently stopped applying.\n")
			continue
		}
		b.WriteString("\n      This chart already churns, and idem cannot say which function did it —\n")
		b.WriteString("      a rendered value cannot be traced back to the call that produced it.\n")
		b.WriteString("      These are where to look first.\n")
	}

	return any
}

// writeRewrites lists what the cluster would change as it admits an object.
//
// Kept apart from the findings, and hedged, because most of it is ordinary
// API-server defaulting that the engine already normalises away. The part that
// matters is a mutating webhook touching an object the engine manages, and the
// reader is better placed than idem to tell which is which.
func writeRewrites(b *strings.Builder, charts []Chart, limit int) bool {
	var any bool
	for _, c := range charts {
		for _, r := range c.Rewrites {
			if !any {
				b.WriteString("\n  the cluster rewrites these on admission\n")
				any = true
			}

			fmt.Fprintf(b, "\n    %s\n", r.Object.Display())

			tw := tabwriter.NewWriter(b, 0, 0, 3, ' ', 0)
			shown := min(len(r.Changes), limit)
			for _, change := range r.Changes[:shown] {
				fmt.Fprintf(tw, "      %s\t%s\t%s\n", change.Path, kindOf(change), clip(change.Value, limit))
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
		if c.Err != nil && !unbuilt(c) {
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
func writeRemediation(b *strings.Builder, charts []Chart, show []string) {
	var findings []check.Finding
	for _, c := range charts {
		if c.Err == nil {
			findings = append(findings, c.Findings...)
		}
	}

	// Whether ArgoCD's block printed decides whether Flux's needs its own
	// leading blank: the ArgoCD block already ends with one, and two in a row
	// reads as a rendering seam on the most-copied output idem has.
	wrote := writeArgoRemediation(b, findings, show, statesServerSideDiff(charts))
	writeFluxRemediation(b, charts, show, wrote)
}

// fluxFindings selects the findings a Flux block should cover, so every format
// offering that fix offers the same one.
//
// Scoped by verdict rather than emitted for everything: a chart whose value a
// `lookup` stabilises does not drift under Flux, and handing the reader config
// to suppress a problem they do not have is how a fix block stops being
// trusted. Findings the ArgoCD condition saw count only when the Flux verdict
// says Flux churns too; findings only the cluster condition saw always do,
// because that verdict is an observation of exactly this engine.
func fluxFindings(charts []Chart) []check.Finding {
	var findings []check.Finding
	for _, c := range charts {
		if c.Err != nil {
			continue
		}
		findings = append(findings, c.ServerOnly...)
		if churnsUnder(c.Verdicts, "flux") {
			findings = append(findings, c.Findings...)
		}
	}
	return findings
}

// writeFluxRemediation emits driftDetection.ignore for the churn Flux will
// actually have, after the ArgoCD block when there was one.
func writeFluxRemediation(b *strings.Builder, charts []Chart, show []string, afterArgo bool) {
	if !shows(show, "flux") {
		return
	}

	entries := remediate.FluxEntries(fluxFindings(charts))
	if len(entries) == 0 {
		return
	}

	if !afterArgo {
		b.WriteString("\n")
	}
	b.WriteString("  Add to your HelmRelease to stop the churn under Flux:\n\n")
	for line := range strings.SplitSeq(strings.TrimRight(remediate.FluxYAML(entries), "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	// The paths are useless without it, and a reader pasting this into a
	// HelmRelease that never had drift detection on would get silence rather
	// than a fix.
	b.WriteString("\n  driftDetection.ignore only applies while drift detection is on; `mode: warn`\n")
	b.WriteString("  reports without correcting.\n\n")
}

// churnsUnder reports whether this engine's verdict says the chart churns.
func churnsUnder(verdicts []engine.Verdict, name string) bool {
	for _, v := range verdicts {
		if v.Engine == name {
			return v.Result == engine.Churns
		}
	}
	return false
}

func writeArgoRemediation(b *strings.Builder, findings []check.Finding, show []string, stated bool) bool {
	if !shows(show, "argocd") {
		return false
	}

	entries := remediate.Entries(findings)
	if len(entries) == 0 {
		return false
	}

	b.WriteString("\n  Add to your ArgoCD Application to stop the churn:\n\n")
	for line := range strings.SplitSeq(strings.TrimRight(remediate.YAML(entries), "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	// Only where the diff mode changes the answer, which is `/stringData`
	// alone.
	//
	// Verified in gitops-engine `pkg/diff/diff.go` rather than recalled: under
	// server-side diff the ignore normalizer IS applied - to the SSA dry-run
	// result and to the live object - and only the pre-processing pass is
	// skipped, via WithSkipFullNormalize(true). A pointer at a field the chart
	// renders therefore still addresses it, and this used to print on every
	// block as though it might not. stringData is the exception because the
	// API server never stores it: it survives only on the raw target, which is
	// what the RespectIgnoreDifferences sync path applies pointers to.
	//
	// (ServerSideApply=true is a different option on a different code path and
	// does not affect these pointers. The two are routinely conflated.)
	if stringDataPointer(entries) {
		if stated {
			// No hedge about something idem just read. Both pointers still go
			// in: the sync path RespectIgnoreDifferences drives applies them
			// to the raw target, where stringData is the only one that exists.
			b.WriteString("\n  This Application sets ServerSideDiff=true, so only `/data` is evaluated\n")
			b.WriteString("  in the diff. `/stringData` is kept because the RespectIgnoreDifferences\n")
			b.WriteString("  sync path applies pointers to the raw target, where it is the live one.\n")
		} else {
			b.WriteString("\n  The `/stringData` pointer works on ArgoCD's default diff. Under\n")
			b.WriteString("  ServerSideDiff=true only `/data` is evaluated, so keep both: whichever\n")
			b.WriteString("  path this install is on, the other pointer is a silent no-op.\n")
		}
	}

	// Blank line so an exit-code line printed after the report does not read
	// as part of the YAML the user is about to paste.
	b.WriteString("\n")
	return true
}

// primaryEngine names the engine the verdict sentence speaks for.
//
// ArgoCD unless the delivery config says otherwise, because the render-side
// condition idem measures without a cluster IS ArgoCD's - and because with no
// signal at all, a reader evaluating a chart wants the strictest answer. But a
// repository holding only HelmReleases has told idem what it runs, and naming
// ArgoCD at it in the first line of output is naming an engine it does not use.
func (r Report) primaryEngine() string {
	if len(r.Engines) == 1 {
		switch r.Engines[0] {
		case "flux":
			return "Flux"
		case "helm":
			return "Helm"
		}
	}
	return "ArgoCD"
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
		return fmt.Sprintf("%d of the %d %s changed since %s will churn under %s%s.",
			churning, total, plural(total, "chart", "charts"), r.Since, r.primaryEngine(), lookupClause(lookupOnly))
	}

	if churning == 0 && unevaluable == 0 && lookupOnly == 0 && r.Unconstructed() == 0 {
		// Saying these charts "render consistently" would be false: they do
		// not, and idem measured that. What is true is that nothing will
		// churn, and the reason is the user's own config rather than the
		// chart - so the config gets the credit and the section above it
		// stops reading as decoration.
		if found, charts := r.covered(); found > 0 {
			return fmt.Sprintf("✓ Nothing will churn under %s — %d %s in %d of %d %s %s covered by your delivery config.",
				r.primaryEngine(), found, plural(found, "finding", "findings"), charts, total, plural(total, "chart", "charts"),
				plural(found, "is", "are"))
		}
		if total == 1 {
			return fmt.Sprintf("✓ %s renders consistently under %s.", r.Charts[0].Name, r.primaryEngine())
		}
		return fmt.Sprintf("✓ All %d charts render consistently under %s.", total, r.primaryEngine())
	}

	// ArgoCD is genuinely fine here and the sentence must not say otherwise -
	// but the engines that do a real install are not, and that is the finding.
	if churning == 0 && unevaluable == 0 && lookupOnly > 0 {
		return fmt.Sprintf("%d of %d %s will churn under Flux and Helm: identical under `helm template`, different with `lookup` resolved.%s",
			lookupOnly, total, plural(total, "chart", "charts"), builtClause(r.Unconstructed()))
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
		s = fmt.Sprintf("%d of %d %s will churn under %s", churning, total, plural(total, "chart", "charts"), r.primaryEngine())
	} else {
		// Counted, not subtracted. `total` is the charts in ratchet scope while
		// `unevaluable` spans every chart the run touched - the ratchet does
		// not hide a chart that would not render - so taking one from the
		// other mixes two populations and goes negative.
		clean := r.consistent()
		s = fmt.Sprintf("%d %s consistently under %s", clean, plural(clean, "chart renders", "charts render"), r.primaryEngine())
	}
	if coveredCharts > 0 {
		// The verb agrees with the findings, not the charts: "4 findings in 1
		// chart is covered" is what keying it on the wrong noun produces.
		s += fmt.Sprintf("; %d %s in %d %s %s covered by your delivery config",
			found, plural(found, "finding", "findings"),
			coveredCharts, plural(coveredCharts, "chart", "charts"),
			plural(found, "is", "are"))
	}
	if unbuiltCount := r.Unconstructed(); unbuiltCount > 0 {
		s += fmt.Sprintf("; %d could not be built", unbuiltCount)
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
		if c.Err != nil && !unbuilt(c) {
			n++
		}
	}
	return n
}

// builtClause reports releases idem could not construct, as a sentence ending
// rather than a clause, so it can be appended to a verdict that already ends.
func builtClause(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" %d could not be built.", n)
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
