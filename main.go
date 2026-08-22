// Command idem checks whether Helm charts render consistently.
//
// Classify the reference, render it more than once with `helm template`,
// compare the results structurally, and say what that means under each GitOps
// engine - discounting whatever the delivery config already suppresses. The
// Not here yet: the apply-side and post-apply drift checks, which need to read
// live objects, and doctor.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/chartref"
	"github.com/pcanilho/idem/internal/cluster"
	"github.com/pcanilho/idem/internal/delivery"
	"github.com/pcanilho/idem/internal/deps"
	"github.com/pcanilho/idem/internal/discover"
	"github.com/pcanilho/idem/internal/doctor"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/engines"
	"github.com/pcanilho/idem/internal/gitrev"
	"github.com/pcanilho/idem/internal/helm"
	"github.com/pcanilho/idem/internal/report"
	"github.com/pcanilho/idem/internal/scan"
)

// Exit codes. Findings are informative by default: a chart using `lookup` is
// correct Helm, so failing the build by default would often simply be wrong.
// Exit 2 is the exception and is never negotiable.
const (
	exitOK      = 0
	exitFinding = 1
	exitFatal   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// multiFlag collects a repeatable flag, preserving order. Order is semantic
// for -f: later values files win.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idem", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: idem [chart] [flags]\n\n")
		fs.PrintDefaults()
	}

	var opt options
	fs.Var(&opt.valuesFiles, "f", "values file, repeatable")
	fs.Var(&opt.valuesFiles, "values", "values file, repeatable")
	fs.Var(&opt.setValues, "set", "set a value, repeatable")
	fs.IntVar(&opt.rounds, "rounds", 2, "renders to compare")
	fs.BoolVar(&opt.strict, "strict", false, "exit non-zero on findings")
	fs.StringVar(&opt.helmBin, "helm", "", "helm binary to render with (default: first on PATH)")
	fs.StringVar(&opt.repo, "repo", "", "chart repository URL, as helm's --repo")
	fs.IntVar(&opt.jobs, "jobs", runtime.NumCPU(), "renders to run at once")
	fs.StringVar(&opt.engine, "engine", "auto", "argocd, flux, helm, all, or auto")
	fs.StringVar(&opt.output, "o", "text", "text, json, markdown or github")
	fs.BoolVar(&opt.dependencyUpdate, "dependency-update", false, "resolve missing dependencies in place, not a temp dir")
	fs.BoolVar(&opt.noDeps, "no-deps", false, "never fetch dependencies")
	fs.StringVar(&opt.newFromRev, "new-from-rev", "", "report only findings in charts changed since REV")
	fs.StringVar(&opt.newFromMergeBase, "new-from-merge-base", "", "same, against the merge base with REF")
	// One flag, not two. kubectl already has BOTH --cluster and --context, and
	// they mean different things - a kubeconfig cluster entry versus the
	// cluster+user+namespace triple - so a boolean --cluster collides with an
	// established meaning. Passing --context at all is what opts in; an empty
	// value means whichever context is current.
	fs.StringVar(&opt.kubeContext, "context", "", "kube context to resolve lookup and capabilities against")
	// helm spells this --version, but idem is the thing being invoked here, so
	// --version has to mean idem's own version - it is the one flag every CLI
	// has. The chart version keeps the capability under a name that says which
	// version it means.
	fs.StringVar(&opt.chartVersion, "chart-version", "", "chart version to fetch, as helm's --version")
	fs.BoolVar(&opt.showVersion, "version", false, "print idem's version and exit")

	target, err := parseArgs(fs, args)
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	// Presence, not value: --context= names no context but still asks idem to
	// use the current one, which is a different thing from not asking at all.
	opt.cluster = wasSet(fs, "context")

	if opt.showVersion {
		fmt.Fprintf(stdout, "idem %s\n", cliVersion())
		return exitOK
	}

	// Validated before anything is rendered, so a bad value fails in a
	// millisecond rather than after a minute of helm.
	if _, err := selectEngines(opt.engine, nil); err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	format, err := formatter(opt.output)
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	mode, err := dependencyMode(opt)
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	if opt.rounds < 2 {
		fmt.Fprintf(stderr, "idem: --rounds is %d: at least 2 renders are needed, because a single render cannot be compared to anything\n", opt.rounds)
		return exitFatal
	}

	// The one verb idem has. Disambiguated by disk, the same way a chart
	// reference is: a directory actually named "doctor" is a chart.
	if target == "doctor" && !exists("doctor") {
		return runDoctor(context.Background(), opt, stdout, stderr)
	}

	ref := chartref.ClassifyWithRepo(target, opt.repo, exists)
	if hint := ref.SetupHint(); hint != "" {
		fmt.Fprintf(stderr, "idem: %q is a helm repository alias, and %q is not configured on this machine.\n", ref.Raw, ref.Repo)
		fmt.Fprintf(stderr, "      run: %s\n", hint)
		fmt.Fprintf(stderr, "      or point idem at the repository directly: idem %s --repo <url>\n", ref.Chart)
		return exitFatal
	}

	charts, err := resolve(ref)
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	ctx := context.Background()

	// Read the delivery config before rendering: what the user already told
	// their engine to ignore changes what is worth reporting. Finding none is
	// the normal case - plenty of estates keep charts and config in separate
	// repositories - so a failure here is a note, never a stop.
	root := delivery.Root(target)
	deliveryCfg, err := delivery.Load(root)
	if err != nil {
		fmt.Fprintf(stderr, "idem: could not read delivery config under %s: %v\n", root, err)
		deliveryCfg = delivery.Config{}
	}

	shown, err := selectEngines(opt.engine, deliveryCfg.Engines)
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	since, touched, err := ratchet(ctx, opt, root)
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	h := helm.New(opt.helmBin)

	// Read the version first: it is printed with every result, and it fails
	// fast and clearly when there is no helm to render with at all.
	helmVersion, err := h.Version(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "idem: cannot run helm: %v\n", err)
		return exitFatal
	}

	queue := make([]scan.Chart, 0, len(charts))
	for _, c := range charts {
		chart := scan.Chart{Name: c.release, Dir: c.ref, Spec: specFor(ref, c, opt)}

		// A second, independent measurement of the same chart: through the API
		// server, where lookup resolves and the chart sees the cluster's real
		// capabilities. Read-only - a server dry run renders, never applies.
		if opt.cluster {
			server := chart.Spec
			server.Cluster = true
			server.KubeContext = opt.kubeContext
			chart.Server = &server
		}
		queue = append(queue, chart)
	}

	prepare, resolutions := preparer(ctx, mode, h)

	rep := report.Report{
		Helm: helmVersion, Rounds: opt.rounds,
		Delivery: deliveryCfg.Files, Engines: shown, Root: root, Since: since,
		Cluster: opt.cluster, Context: opt.kubeContext,
	}
	for _, result := range scan.Charts(ctx, h, queue, opt.rounds, opt.jobs, inspector(ref), prepare) {
		applied := delivery.Apply(deliveryCfg.For(chartPath(root, result.Chart.Dir)), result.Findings)

		evidence := engines.Evidence{
			Uses: analyze.Of(result.Uses, analyze.Lookup),
			Err:  result.InspectErr,
		}

		if result.ServerRendered {
			stable := len(result.ServerFindings) == 0
			evidence.Cluster = &stable
		}

		rep.Charts = append(rep.Charts, report.Chart{
			Name:       result.Chart.Name,
			Dir:        result.Chart.Dir,
			RepoDir:    chartPath(root, result.Chart.Dir),
			Deps:       resolutions.of(result.Chart.Dir),
			Changed:    gitrev.Touches(touched, chartPath(root, result.Chart.Dir)),
			Findings:   applied.Churning,
			Suppressed: applied.Suppressed,
			Maybe:      applied.Maybe,
			Verdicts:   verdictsFor(result, evidence),
			Potential:  analyze.Potential(result.Uses),
			Err:        result.Err,
		})
	}

	if err := format(rep, stdout); err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	// The exit code is stated in the output as well as returned: a CI log that
	// ends in a bare non-zero status makes the reader go looking for a reason
	// already on screen. Text only - appending a sentence to JSON would leave
	// it unparseable, and every machine format already carries the counts.
	text := opt.output == "text"
	switch {
	case rep.Unevaluable() > 0:
		if text {
			fmt.Fprintln(stdout, "  exit 2 — a chart could not be rendered")
		}
		return exitFatal
	case opt.strict && rep.Churning() > 0:
		if text {
			fmt.Fprintln(stdout, "  exit 1")
		}
		return exitFinding
	}
	return exitOK
}

// parseArgs parses flags that may appear before or after the chart reference.
//
// Go's flag package stops at the first operand, so `idem ./charts --strict`
// would silently ignore --strict - and a CI gate the user believes is
// enforcing but is not is worse than no gate at all.
func parseArgs(fs *flag.FlagSet, args []string) (string, error) {
	var operands []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			break
		}
		operands = append(operands, fs.Arg(0))
		rest = fs.Args()[1:]
	}

	switch len(operands) {
	case 0:
		return ".", nil
	case 1:
		return operands[0], nil
	default:
		return "", fmt.Errorf("one chart reference at a time, got %d: %s", len(operands), strings.Join(operands, " "))
	}
}

// options are the parsed flags.
type options struct {
	valuesFiles  multiFlag
	setValues    multiFlag
	rounds       int
	strict       bool
	helmBin      string
	repo         string
	chartVersion string
	jobs         int
	engine       string
	output       string
	showVersion  bool

	dependencyUpdate bool
	noDeps           bool

	newFromRev       string
	newFromMergeBase string

	cluster     bool
	kubeContext string
}

// specFor builds the render request for one chart.
func specFor(ref chartref.Ref, t target, opt options) engine.Spec {
	return engine.Spec{
		ChartRef:    t.ref,
		Release:     t.release,
		Repo:        ref.Repo,
		Version:     opt.chartVersion,
		ValuesFiles: opt.valuesFiles,
		SetValues:   opt.setValues,
	}
}

// target is one chart to render.
type target struct {
	ref     string
	release string
}

// resolve turns a classified reference into the charts to check.
//
// A local path may hold many charts; every other form names exactly one.
func resolve(ref chartref.Ref) ([]target, error) {
	if ref.Kind != chartref.Local {
		return []target{{ref: ref.Raw, release: releaseName(ref.Raw)}}, nil
	}

	found, err := discover.Charts(ref.Raw)
	if err != nil {
		return nil, err
	}
	out := make([]target, 0, len(found))
	for _, c := range found {
		out = append(out, target{ref: c.Dir, release: c.Name})
	}
	return out, nil
}

// releaseName derives a release name for a remote chart.
//
// idem has no Application or HelmRelease to take one from. The only property
// that matters is that every round of a run gets the same name, since
// .Release.Name appears in rendered output and a name that varied between
// rounds would manufacture the very churn idem is looking for.
func releaseName(raw string) string {
	return path.Base(strings.TrimSuffix(raw, "/"))
}

// chartPath expresses a chart directory the way an Application names it:
// relative to the repository root.
//
// Empty when the chart sits outside the repository idem found, which joins to
// nothing rather than to everything - delivery.For refuses an empty path for
// exactly that reason.
func chartPath(root, dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// formatter resolves -o to the rendering it names.
//
// Validated before anything is rendered, so a typo costs a millisecond rather
// than a minute of helm.
func formatter(name string) (func(report.Report, io.Writer) error, error) {
	switch name {
	case "text":
		return report.Report.Text, nil
	case "json":
		return report.Report.JSON, nil
	case "markdown":
		return report.Report.Markdown, nil
	case "github":
		return report.Report.GitHub, nil
	}
	return nil, fmt.Errorf("unknown output format %q: valid values are text, json, markdown, github", name)
}

// selectEngines resolves which engines' verdicts to display.
//
// "auto" takes the engines the repository actually uses, which is what the
// delivery config just told us. With no signal either way it shows all three,
// because that is exactly when you are evaluating a chart and want to know.
func selectEngines(flag string, detected []string) ([]string, error) {
	if flag != "auto" {
		targets, err := engines.Select(flag)
		if err != nil {
			return nil, err
		}
		return names(targets), nil
	}

	var out []string
	for _, id := range detected {
		if targets, err := engines.Select(id); err == nil {
			out = append(out, names(targets)...)
		}
	}
	// Nothing detected, or nothing idem models. Either way, narrowing to
	// nothing would report less than it checked.
	if len(out) == 0 {
		return names(engines.All()), nil
	}
	return out, nil
}

func names(targets []engines.Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Name())
	}
	return out
}

// verdictsFor works out what each engine does with this chart.
//
// Always every engine, whatever --engine selects for display: the "no lookup
// anywhere, so this is a chart defect" conclusion is only reachable from an
// engine that resolves lookup, and it is worth having even when the reader
// only asked about ArgoCD.
// runDoctor asks the cluster what has already been happening.
//
// No chart, no git, no rendering. A cluster is the entire point here, so
// unlike the check path this needs one: without --context it uses whichever
// context is current.
func runDoctor(ctx context.Context, opt options, stdout, stderr io.Writer) int {
	client := cluster.New("", opt.kubeContext)

	workloads, err := client.Workloads(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "idem: cannot read the cluster: %v\n", err)
		return exitFatal
	}

	// Best-effort: a cluster with no ArgoCD simply has no Applications to map,
	// and doctor still has everything the workloads themselves told it.
	sources := client.Sources(ctx)

	diagnosis := doctor.Diagnose(workloads, time.Now())
	if err := report.Doctor(stdout, diagnosis, opt.kubeContext, sources); err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	// Ranking suspects is triage, not a verdict. Nothing here fails a build.
	return exitOK
}

// wasSet reports whether a flag was given at all, however it was valued.
func wasSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

// ratchet resolves which charts this run is allowed to fail on.
//
// Everything is still rendered and compared: the flag governs what fails the
// build, not what gets checked, which is what lets idem say how many
// pre-existing findings it is holding back.
func ratchet(ctx context.Context, opt options, root string) (string, []string, error) {
	switch {
	case opt.newFromRev != "" && opt.newFromMergeBase != "":
		return "", nil, fmt.Errorf("--new-from-rev and --new-from-merge-base are two ways to pick the same baseline; give one")

	case opt.newFromRev != "":
		changed, err := gitrev.Changed(ctx, root, opt.newFromRev)
		return opt.newFromRev, changed, err

	case opt.newFromMergeBase != "":
		base, err := gitrev.MergeBase(ctx, root, opt.newFromMergeBase)
		if err != nil {
			return "", nil, err
		}
		changed, err := gitrev.Changed(ctx, root, base)
		return opt.newFromMergeBase, changed, err
	}
	return "", nil, nil
}

// dependencyMode turns the two dependency flags into one decision.
func dependencyMode(opt options) (deps.Mode, error) {
	switch {
	case opt.noDeps && opt.dependencyUpdate:
		return deps.Never, fmt.Errorf("--no-deps and --dependency-update contradict each other: one forbids fetching, the other writes the result into your chart")
	case opt.noDeps:
		return deps.Never, nil
	case opt.dependencyUpdate:
		return deps.InPlace, nil
	}
	return deps.TempDir, nil
}

// resolutions records how each chart was made renderable, for the provenance
// line. Written from the worker pool, so it is guarded.
type resolutions struct {
	mu   sync.Mutex
	kind map[string]string
}

func (r *resolutions) record(dir string, kind deps.Kind) {
	if kind == deps.Vendored {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kind[dir] = kind.String()
}

func (r *resolutions) of(dir string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.kind[dir]
}

// preparer resolves a chart's dependencies before its rounds render.
//
// A chart whose subcharts are absent cannot render at all, and the fix must
// not dirty the working tree: by default the chart is copied out, resolved
// there, and the copy discarded.
func preparer(ctx context.Context, mode deps.Mode, h helm.Helm) (scan.Prepare, *resolutions) {
	seen := &resolutions{kind: map[string]string{}}

	return func(c scan.Chart) (engine.Spec, func(), error) {
		dir, kind, cleanup, err := mode.Prepare(ctx, c.Dir, h)
		if err != nil {
			return engine.Spec{}, nil, err
		}
		seen.record(c.Dir, kind)

		spec := c.Spec
		spec.ChartRef = dir
		return spec, cleanup, nil
	}, seen
}

// inspector reads a chart's source for the functions idem flags.
//
// Every chart is scanned, not only the churning ones: a chart that renders
// identically today while calling randAlphaNum is being held by a pin, and a
// pin that silently stops applying is the failure idem exists for. It runs in
// the render pool rather than in a pass afterwards, so it overlaps rendering
// instead of adding its own serial tail.
func inspector(ref chartref.Ref) scan.Inspect {
	return func(c scan.Chart) ([]analyze.Use, error) {
		if ref.Kind != chartref.Local {
			// Rendered straight from a registry and never landed on disk, so
			// there is no source to scan. Reported as unknown rather than as
			// "no lookup", which would be a sound CHURNS verdict off no
			// evidence at all. Resolving this needs the same temp-dir fetch
			// that dependency handling will build.
			return nil, fmt.Errorf("chart was rendered from %s, so idem has no chart source to scan", ref.Kind)
		}
		return analyze.Find(c.Dir)
	}
}

// Only charts that actually churn get verdicts: a verdict answers "what does
// this finding mean for me", and a clean chart has no finding to explain.
func verdictsFor(result scan.Result, ev engines.Evidence) []engine.Verdict {
	if result.Err != nil || len(result.Findings) == 0 {
		return nil
	}

	all := engines.All()
	out := make([]engine.Verdict, 0, len(all))
	for _, t := range all {
		out = append(out, t.Verdict(ev))
	}
	return out
}

// version is stamped by the release build with -ldflags. Empty in a source
// build, where the module's own build info answers instead - idem reports what
// it can establish rather than carrying a hardcoded string that goes stale.
var version = ""

// cliVersion describes this binary.
func cliVersion() string {
	if version != "" {
		return version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return formatVersion(info.Main.Version, revision, modified)
}

// formatVersion describes a build from what the toolchain recorded.
//
// A module version - tagged or pseudo - already carries the commit and the
// dirty marker, so it is printed alone. Only a "(devel)" build, which carries
// neither, gets them appended.
func formatVersion(main, revision string, modified bool) string {
	if main == "" {
		return "unknown"
	}
	if main != "(devel)" {
		return main
	}

	if revision == "" {
		return main
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if modified {
		return main + " " + revision + " (dirty)"
	}
	return main + " " + revision
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
