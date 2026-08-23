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
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pcanilho/idem/internal/analyze"
	"gopkg.in/yaml.v3"

	"github.com/pcanilho/idem/internal/chartref"
	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/cluster"
	"github.com/pcanilho/idem/internal/delivery"
	"github.com/pcanilho/idem/internal/deps"
	"github.com/pcanilho/idem/internal/discover"
	"github.com/pcanilho/idem/internal/doctor"
	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/engines"
	"github.com/pcanilho/idem/internal/gitrev"
	"github.com/pcanilho/idem/internal/helm"
	"github.com/pcanilho/idem/internal/manifest"
	"github.com/pcanilho/idem/internal/report"
	"github.com/pcanilho/idem/internal/scan"
)

// Exit codes. Findings are informative by default: a chart using `lookup` is
// correct Helm, so failing the build by default would often simply be wrong.
// Exit 2 is the exception and is never negotiable.
// maxRounds caps --rounds, and maxJobs --jobs. See the notes at their uses.
const (
	maxRounds = 20
	maxJobs   = 256
)

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
	// Discarded, not stderr: the flag package writes the parse error itself,
	// and idem formats the same error below, so a bad flag printed twice.
	fs.SetOutput(io.Discard)
	// Suppressed, because the flag package calls this for BOTH a help request
	// and a usage error, and those are opposite outcomes: one is a question
	// answered on stdout, the other a mistake reported on stderr. Handled
	// below instead, where the two can be told apart.
	fs.Usage = func() {}

	var opt options
	fs.Var(&opt.valuesFiles, "f", "values file, repeatable")
	fs.Var(&opt.valuesFiles, "values", "values file, repeatable")
	fs.Var(&opt.setValues, "set", "set a value, repeatable")
	fs.IntVar(&opt.rounds, "rounds", 3, "renders to compare")
	fs.BoolVar(&opt.strict, "strict", false, "exit non-zero on findings")
	fs.BoolVar(&opt.verbose, "v", false, "expand every finding instead of capping each at five fields")
	fs.StringVar(&opt.helmBin, "helm", "", "helm binary to render with (default: first on PATH)")
	fs.StringVar(&opt.repo, "repo", "", "chart repository URL, as helm's --repo")
	fs.IntVar(&opt.jobs, "jobs", runtime.NumCPU(), "renders to run at once")
	fs.StringVar(&opt.engine, "engine", "auto", "argocd, flux, helm, all, or auto")
	fs.StringVar(&opt.output, "o", "text", "text, json, yaml, markdown or github")
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
	fs.StringVar(&opt.namespace, "namespace", "", "render into this namespace instead of the one the delivery config names\n(for doctor: look for post-apply drift in this namespace)")
	// helm spells this --version, but idem is the thing being invoked here, so
	// --version has to mean idem's own version - it is the one flag every CLI
	// has. The chart version keeps the capability under a name that says which
	// version it means.
	fs.StringVar(&opt.chartVersion, "chart-version", "", "chart version to fetch, as helm's --version")
	fs.BoolVar(&opt.showVersion, "version", false, "print idem's version and exit")

	operands, err := parseArgs(fs, args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		// Asking for help is not an error. It goes to stdout so a pipe can
		// read it, and exits 0 so `idem --help` can be scripted as a check
		// that the binary is sane - which is what people do with it.
		writeHelp(stdout, fs)
		return exitOK
	case err != nil:
		fmt.Fprintf(stderr, "idem: %v\n", err)
		fmt.Fprintf(stderr, "\nRun `idem --help` to see what idem takes.\n")
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
	// An upper bound as well, because the lower one taught the wrong lesson:
	// `--rounds 2147483647` allocated gigabytes, printed nothing, and span
	// forever. Past a handful the false-negative rate is already floored - the
	// measurement behind the default of 3 is in docs/design.md §9 - so a large
	// value is always a typo.
	if opt.rounds > maxRounds {
		fmt.Fprintf(stderr, "idem: --rounds is %d: more than %d renders finds nothing %d does not, and costs a render per chart each\n",
			opt.rounds, maxRounds, maxRounds)
		return exitFatal
	}

	// A bare `idem` used to mean `.` and recurse without bound - typed in a
	// home directory it walks everything looking for a Chart.yaml. Someone
	// typing the bare name is asking what this is, not asking for their whole
	// disk to be scanned, and git, kubectl and helm all answer that question
	// the same way. `idem .` is still there for the deliberate case.
	//
	// Ahead of verb dispatch, which indexes operands[0].
	if len(operands) == 0 {
		writeHelp(stdout, fs)
		return exitOK
	}

	// --jobs is validated here as well as coerced in scan.resolveJobs, and the
	// two are not redundant: the package boundary defends itself, while this
	// refuses a value the user meant something by. Silently turning `--jobs 0`
	// into NumCPU gave the opposite of what was asked for and said nothing, and
	// `--jobs 999999` was honoured literally.
	if opt.jobs < 1 || opt.jobs > maxJobs {
		fmt.Fprintf(stderr, "idem: --jobs is %d: it takes 1 to %d renders at once\n", opt.jobs, maxJobs)
		return exitFatal
	}

	// The verbs. Disambiguated by disk, the same way a chart reference is: a
	// directory actually named "doctor" is a chart, not the verb.
	//
	// Both take text, json and yaml. markdown and github are REFUSED rather
	// than ignored: those two are shapes, not encodings - markdown is a
	// pull-request comment about a chart in a diff, github annotates a file at
	// a line - and a cluster's rollout history belongs to neither. Rendering
	// one anyway would hand a script something it cannot parse while telling
	// it nothing went wrong, which is the same failure as a flag silently
	// dropped after the chart path.
	if name := operands[0]; verb(operands, "diff") || verb(operands, "doctor") {
		if !verbFormat(opt.output) {
			fmt.Fprintf(stderr, "idem: `idem %s` does not render %s, which describes a chart in a pull request\n", name, opt.output)
			fmt.Fprintf(stderr, "      it takes text, json or yaml; `idem <chart> -o %s` is the one that renders %s\n", opt.output, opt.output)
			return exitFatal
		}
		if name == "diff" {
			return runDiff(operands[1:], opt, stdout, stderr)
		}
		return runDoctor(context.Background(), opt, stdout, stderr)
	}

	target, err := chartTarget(operands)
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	ref := chartref.ClassifyWithRepo(target, opt.repo, exists)
	if hint := ref.SetupHint(); hint != "" {
		fmt.Fprintf(stderr, "idem: %q is a helm repository alias, and %q is not configured on this machine.\n", ref.Raw, ref.Repo)
		fmt.Fprintf(stderr, "      run: %s\n", hint)
		fmt.Fprintf(stderr, "      or point idem at the repository directly: idem %s --repo <url>\n", ref.Chart)
		return exitFatal
	}

	charts, libraries, err := resolve(ref)
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	ctx := context.Background()

	// Read the delivery config before rendering: what the user already told
	// their engine to ignore changes what is worth reporting. Finding none is
	// the normal case - plenty of estates keep charts and config in separate
	// repositories - so a failure here is a note, never a stop.
	//
	// Only for a reference that IS a path. A registry reference names no
	// repository on this machine, so there is nothing to walk up from and
	// nothing to read; attempting it lstats `oci://...` and prints a failure
	// as the first line of the run a stranger is most likely to make - "should
	// I adopt someone else's chart".
	var root string
	var deliveryCfg delivery.Config
	if ref.Kind == chartref.Local {
		root = delivery.Root(target)
		var err error
		if deliveryCfg, err = delivery.Load(root); err != nil {
			fmt.Fprintf(stderr, "idem: could not read delivery config under %s: %v\n", root, err)
			deliveryCfg = delivery.Config{}
		}
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

	// One directory for every values block idem has to materialise, removed
	// when the run ends. Never written into the user's tree: idem does not
	// dirty a working directory it was only asked to read.
	inlineDir, err := os.MkdirTemp("", "idem-values-")
	if err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}
	defer os.RemoveAll(inlineDir)

	queue := make([]scan.Chart, 0, len(charts))
	releases := make(map[string]release, len(charts))
	for _, c := range charts {
		for _, rel := range resolveReleases(deliveryCfg, opt, root, chartPath(root, c.ref), c.release, inlineDir) {
			// The namespace is decided per release: an ApplicationSet gives
			// each element its own, and --namespace overrides all of them.
			rel.namespace, rel.from = releaseNamespaceFor(deliveryCfg, opt, chartPath(root, c.ref), rel)

			label := rel.label(c.release)
			releases[label] = rel

			chart := scan.Chart{Name: label, Dir: c.ref, Spec: specFor(ref, c, opt, rel)}

			// A second, independent measurement of the same chart: through the
			// API server, where lookup resolves and the chart sees the
			// cluster's real capabilities. Read-only - a server dry run
			// renders, never applies.
			if opt.cluster {
				server := chart.Spec
				server.Cluster = true
				server.KubeContext = opt.kubeContext
				chart.Server = &server
			}
			queue = append(queue, chart)
		}
	}

	prepare, resolutions := preparer(ctx, mode, h)

	rep := report.Report{
		Helm: helmVersion, Rounds: opt.rounds,
		Delivery: deliveryCfg.Files, Engines: shown, Root: root, Since: since, Verbose: opt.verbose,
		Cluster: opt.cluster, Context: opt.kubeContext, Libraries: libraries,
	}
	for _, result := range scan.Charts(ctx, h, queue, opt.rounds, opt.jobs, scan.Hooks{Inspect: inspector(ctx, ref, h), Prepare: prepare, Admission: admission(ctx, opt)}) {
		applied := delivery.Apply(deliveryCfg.For(chartPath(root, result.Chart.Dir)), result.Findings)

		evidence := engines.Evidence{
			Uses: analyze.Of(result.Uses, analyze.Lookup),
			Err:  result.InspectErr,
		}

		// The `helm template` condition is measured on every run, and it is
		// the condition an engine without cluster access reconciles under -
		// so it answers for ArgoCD in both directions, not just when it
		// differs. Left unset on a chart that would not render: nothing was
		// established there, and nil is how idem says so.
		if result.Err == nil {
			identical := len(result.Findings) == 0
			evidence.Client = &identical
		}

		if result.ServerRendered {
			stable := len(result.ServerFindings) == 0
			evidence.Cluster = &stable
		}

		// A chart identical under `helm template` can still differ with lookup
		// resolving. Carried separately because it is churn under Flux and
		// Helm and demonstrably not under ArgoCD; folded into Findings it
		// would make every ArgoCD-framed count and sentence say the opposite
		// of what idem measured.
		//
		// Deliberately not run through the delivery config. An ArgoCD
		// ignoreDifferences cannot suppress churn ArgoCD does not have, and
		// the rule that could - Flux driftDetection.ignore - names no chart
		// path to join on, so it can never be matched to this chart anyway.
		// Applying Argo rules here would report "already suppressed" about a
		// suppression that does nothing for the engines that will churn.
		var serverOnly []check.Finding
		if len(result.Findings) == 0 {
			serverOnly = result.ServerFindings
		}

		// Said, not swallowed. idem is a tool about knowing what was and was
		// not checked, so a question it could not ask has to be visible.
		// The cluster render condition failed. Said, not inferred around: without
		// it the flux and helm verdicts fall back to their inferred branch and
		// the provenance line still names the context, so a run that never
		// reached the cluster reads exactly like one that did.
		if result.ServerErr != nil {
			fmt.Fprintf(stderr, "idem: could not render %s against %s: %v\n",
				result.Chart.Name, contextName(opt), result.ServerErr)
		}

		if result.RewriteErr != nil {
			fmt.Fprintf(stderr, "idem: could not ask the cluster what it would do with %s: %v\n",
				result.Chart.Name, result.RewriteErr)
			stated, _ := deliveryCfg.NamespaceFor(chartPath(root, result.Chart.Dir))
			fmt.Fprint(stderr, unaskedNote(result.RewriteErr,
				releases[result.Chart.Name].namespace, stated,
				deliveryCfg.CreatesNamespace(chartPath(root, result.Chart.Dir))))
		}

		rep.Charts = append(rep.Charts, report.Chart{
			Name:           result.Chart.Name,
			Dir:            result.Chart.Dir,
			Release:        deliveredRelease(releases[result.Chart.Name], result.Chart.Name),
			Namespace:      releases[result.Chart.Name].namespace,
			NamespaceFrom:  releases[result.Chart.Name].from,
			Unresolved:     releases[result.Chart.Name].unresolved,
			RepoDir:        chartPath(root, result.Chart.Dir),
			Deps:           resolutions.of(result.Chart.Dir),
			Changed:        gitrev.Touches(touched, chartPath(root, result.Chart.Dir)),
			Findings:       applied.Churning,
			ServerOnly:     serverOnly,
			Suppressed:     applied.Suppressed,
			Maybe:          applied.Maybe,
			Verdicts:       verdictsFor(result, evidence),
			Potential:      analyze.Potential(result.Uses),
			Rewrites:       result.Rewrites,
			Skipped:        result.Skipped,
			ServerSideDiff: deliveryCfg.ServerSideDiff(chartPath(root, result.Chart.Dir)),
			Err:            result.Err,
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
	case opt.strict && rep.Churning()+rep.ChurningWithLookup() > 0:
		if text {
			fmt.Fprintln(stdout, "  exit 1")
		}
		return exitFinding
	}
	return exitOK
}

// writeHelp prints what idem is and what to type, then the flags.
//
// Verbs and examples first: a flag list says what exists, not what to run, and
// the first thing a stranger needs is one line they can paste. `doctor` in
// particular is the easiest thing here to try - it needs no chart, only a
// cluster you already run - and it was previously findable only by reading the
// README.
func writeHelp(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `idem — check your Helm charts against the GitOps engine you actually run.

It renders a chart more than once, compares the results, and tells you which
objects will never settle — under ArgoCD, under Flux, under plain Helm — along
with the config that stops it.

usage:
  idem [chart] [flags]     check a chart, or every chart under a directory
  idem diff a.yaml b.yaml  compare two renders you produced yourself
  idem doctor [flags]      ask a cluster you already run what keeps rolling

examples:
  idem ./charts                      check every chart in a directory
  idem ./charts --strict             fail CI when something will churn
  idem myapp --repo https://…        check a chart before you adopt it
  idem doctor                        find churn that has already happened

flags:
`)

	writeFlags(w, fs)
}

// writeFlags lists the flags the way the README documents them.
//
// Reimplemented rather than delegated to flag.PrintDefaults, which writes ONE
// dash for every name. Go's parser accepts one dash and two interchangeably, so
// `--strict` has always worked - but the help said `-strict` while the README's
// table said `--strict`, and a reader of `--help` would reasonably conclude
// long flags were not supported. Two halves of idem's own documentation
// disagreeing about its interface is the same defect readme_test.go exists to
// catch, in the one place that test could not see.
//
// A flag LIBRARY would give real POSIX - clustering, attached short values -
// but idem has one dependency and that is a stated property of the repository.
// This is a display change and nothing else: the parser is untouched, and both
// dash forms still work.
//
// The layout follows PrintDefaults deliberately, down to the four spaces before
// the tab that align on 4- and 8-space tab stops.
func writeFlags(w io.Writer, fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		var b strings.Builder
		fmt.Fprintf(&b, "  %s", dashed(f.Name))

		name, usage := flag.UnquoteUsage(f)
		if name != "" {
			b.WriteString(" ")
			b.WriteString(name)
		}

		// A one-letter flag with no value keeps its usage on the same line,
		// which is what makes `-v` read as one row rather than two.
		if b.Len() <= 4 {
			b.WriteString("\t")
		} else {
			b.WriteString("\n    \t")
		}
		b.WriteString(strings.ReplaceAll(usage, "\n", "\n    \t"))

		if def, ok := defaultOf(f); ok {
			b.WriteString(" (default " + def + ")")
		}
		fmt.Fprintln(w, b.String())
	})
}

// dashed is the POSIX-shaped spelling: one dash for a single letter, two for a
// word. It is what the README's flag table already uses.
func dashed(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// defaultOf reports a flag's default, and whether it is worth printing.
//
// flag.isZeroValue is unexported, so the zero values are named here instead.
// Strings are quoted because PrintDefaults quotes them and `(default text)`
// reads like prose rather than a value.
func defaultOf(f *flag.Flag) (string, bool) {
	switch f.DefValue {
	case "", "false", "0":
		return "", false
	}
	if g, ok := f.Value.(flag.Getter); ok {
		if _, isString := g.Get().(string); isString {
			return strconv.Quote(f.DefValue), true
		}
	}
	return f.DefValue, true
}

// parseArgs parses flags that may appear before or after the chart reference.
//
// Go's flag package stops at the first operand, so `idem ./charts --strict`
// would silently ignore --strict - and a CI gate the user believes is
// enforcing but is not is worse than no gate at all.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var operands []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		operands = append(operands, fs.Arg(0))
		rest = fs.Args()[1:]
	}

	return operands, nil
}

// verb reports whether the operands name one of idem's verbs.
//
// Disambiguated by disk: a directory actually named "diff" or "doctor" is a
// chart, and idem checks it rather than second-guessing the user.
func verb(operands []string, name string) bool {
	return len(operands) > 0 && operands[0] == name && !exists(name)
}

// runDiff compares two renders the user produced themselves.
//
// The comparison engine on its own - no helm, no network, no cluster.
// Detecting non-determinism means rendering more than once, which is why
// `idem <chart>` invokes the renderer itself; this is for when you would
// rather produce the renderings yourself, and it is what makes kustomize a
// target: `kustomize build a/ > a.yaml`, twice, then diff.
func runDiff(files []string, opt options, stdout, stderr io.Writer) int {
	if len(files) != 2 {
		fmt.Fprintf(stderr, "idem: diff takes exactly two files, got %d\n", len(files))
		fmt.Fprintf(stderr, "      usage: idem diff a.yaml b.yaml\n")
		return exitFatal
	}

	rounds := make([][]manifest.Object, 0, len(files))
	for _, name := range files {
		objects, err := readManifests(name)
		if err != nil {
			fmt.Fprintf(stderr, "idem: %v\n", err)
			return exitFatal
		}
		rounds = append(rounds, objects)
	}

	result, err := check.Compare(rounds)
	if err != nil {
		// Unwrapped once: check.Compare frames its errors as "comparing round 1
		// against round N", which is true of a chart rendered N times and
		// meaningless here - `idem diff` has two files and no rounds at all.
		if inner := errors.Unwrap(err); inner != nil {
			err = inner
		}
		fmt.Fprintf(stderr, "idem: comparing %s and %s: %v\n", files[0], files[1], err)
		return exitFatal
	}

	var render error
	switch opt.output {
	case "json":
		render = report.DiffJSON(stdout, result.Findings, files[0], files[1])
	case "yaml":
		render = report.DiffYAML(stdout, result.Findings, files[0], files[1])
	default:
		render = report.Diff(stdout, result.Findings, files[0], files[1], report.Fields(opt.verbose))
	}
	if render != nil {
		fmt.Fprintf(stderr, "idem: %v\n", render)
		return exitFatal
	}
	return exitOK
}

// verbFormat reports whether `idem diff` and `idem doctor` can render this
// output. See the note at the refusal in run.
func verbFormat(name string) bool {
	return name == "text" || name == "json" || name == "yaml"
}

// readManifests parses one file of rendered Kubernetes objects.
func readManifests(name string) ([]manifest.Object, error) {
	body, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	defer body.Close()

	objects, err := manifest.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return objects, nil
}

// chartTarget is the single chart reference a check runs against.
func chartTarget(operands []string) (string, error) {
	switch len(operands) {
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
	verbose      bool
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
	namespace   string
}

// specFor builds the render request for one chart.
func specFor(ref chartref.Ref, t target, opt options, rel release) engine.Spec {
	return engine.Spec{
		ChartRef:    t.ref,
		Release:     rel.name,
		Namespace:   rel.namespace,
		Repo:        ref.Repo,
		Version:     opt.chartVersion,
		ValuesFiles: rel.files,
		SetValues:   rel.sets,
	}
}

// defaultNamespace is what idem renders into when nothing claims the chart.
//
// Explicit, and never read from the kube context. helm with no --namespace
// takes the current context's namespace, so the same commit rendered on a
// laptop and in CI produces different object identities and can make a
// namespaced ignoreDifferences rule match in one place and not the other.
// idem's own output has to be reproducible; that means saying which namespace,
// not inheriting one.
const defaultNamespace = report.DefaultNamespace

// releaseNamespace decides the namespace a chart renders into, and what
// decided it.
//
// Precedence is user, then repository, then idem: --namespace is an
// instruction, spec.destination.namespace is a fact the repository states, and
// "default" is a choice idem made and says so.
func releaseNamespace(cfg delivery.Config, opt options, chartPath string) (string, string) {
	if opt.namespace != "" {
		return opt.namespace, report.NamespaceFromFlag
	}
	if ns, file := cfg.NamespaceFor(chartPath); ns != "" {
		return ns, file
	}
	return defaultNamespace, ""
}

// releaseNamespaceFor is releaseNamespace with the release's own answer first:
// an ApplicationSet gives every element its own namespace, so the chart-level
// question only applies when the element did not answer it.
func releaseNamespaceFor(cfg delivery.Config, opt options, chartPath string, rel release) (string, string) {
	if opt.namespace != "" {
		return opt.namespace, report.NamespaceFromFlag
	}
	if rel.namespace != "" {
		return rel.namespace, rel.from
	}
	return releaseNamespace(cfg, opt, chartPath)
}

// deliveredRelease is the release name to report, which is nothing at all when
// it is simply the chart name: saying "release home, chart home" on every line
// of a 16-chart estate is noise, and the interesting case is when they differ.
func deliveredRelease(rel release, chart string) string {
	if rel.name == chart {
		return ""
	}
	return rel.name
}

// release is everything the delivery config decided about rendering one chart.
//
// idem's unit of analysis is a release - chart plus values plus engine - and
// this is the half that does not come from the chart directory.
type release struct {
	namespace string
	from      string
	name      string
	files     []string
	sets      []string

	// instance names the generator element this release came from, empty when
	// a plain Application (or nothing at all) claims the chart.
	instance string

	// unresolved names values a generator substitutes, which idem refused to
	// invent. Carried so a chart that will not render can say what it lacked.
	unresolved []string
}

// resolveRelease reads what the delivery config renders this chart with.
//
// Order is ArgoCD's: valueFiles, then values/valuesObject, then parameters.
// The user's own -f and --set go last in each, because a flag typed at the
// terminal is a deliberate override of what the repository says.
func resolveReleases(cfg delivery.Config, opt options, root, chartPath, chartName, inlineDir string) []release {
	found := cfg.ReleasesFor(chartPath)
	if len(found) == 0 {
		// Nothing claims this chart, so there is one release to check: the
		// chart as it stands, with whatever the user passed.
		return []release{withUser(release{name: chartName}, opt)}
	}

	out := make([]release, 0, len(found))
	for _, vals := range found {
		rel := release{name: chartName, instance: vals.Instance, unresolved: vals.Templated, from: vals.File}
		if vals.Name != "" {
			rel.name = vals.Name
		}

		for _, f := range vals.ValueFiles {
			rel.files = append(rel.files, filepath.Join(root, f))
		}

		// One file per release rather than per chart: two releases of one
		// chart have different values, and sharing a name would have the
		// second overwrite the first.
		inline, err := writeInline(inlineDir, rel.label(chartName), vals.Inline)
		if err != nil {
			rel.unresolved = append(rel.unresolved, "valuesObject")
		}
		if inline != "" {
			rel.files = append(rel.files, inline)
		}

		rel.sets = append(rel.sets, vals.Sets...)
		out = append(out, withUser(rel, opt))
	}
	return out
}

// withUser puts the flags typed at the terminal last, so an explicit -f or
// --set overrides what the repository says rather than the other way round.
func withUser(rel release, opt options) release {
	rel.files = append(rel.files, opt.valuesFiles...)
	rel.sets = append(rel.sets, opt.setValues...)
	return rel
}

// label distinguishes one release of a chart from another, and is the chart
// name alone when there is only one - a suffix nobody needs is noise on every
// line of the report.
func (r release) label(chart string) string {
	if r.instance == "" {
		return chart
	}
	return chart + " (" + r.instance + ")"
}

// valuesFileName turns a release label into a single filename.
//
// The label is not a filename and must not be used as one. For a git `files`
// generator the element IS a repository path, so the label reads
// `api (envs/prod.yaml)` - and joining that onto a directory asked os.WriteFile
// to write into `api (envs`, which does not exist. It failed, the inline values
// were dropped, and the chart rendered with DEFAULTS: a release nobody deploys,
// which is the exact failure reading the delivery config exists to prevent. It
// surfaced only as "missing valuesObject" and exited 0. `path: "envs/*.yaml"`
// is the ordinary ArgoCD pattern, so this was most real ApplicationSets.
//
// Separators are replaced rather than the directories created, because the
// label is built from repository input and a path is precisely what escapes a
// temp directory. The hash keeps two labels that differ ONLY where the
// replacement bites - `envs/prod.yaml` and `envs_prod.yaml` - from landing on
// one file, which would hand helm one element's values while the report named
// the other's. It is a hash of the label, so it is stable across runs like
// everything else idem writes.
func valuesFileName(label string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == os.PathSeparator || r == 0 {
			return '_'
		}
		return r
	}, label)

	sum := sha256.Sum256([]byte(label))
	return fmt.Sprintf("%s-%x-values.yaml", safe, sum[:4])
}

// writeInline materialises spec.source.helm.valuesObject as a values file,
// because that is the only way to hand helm a nested structure faithfully -
// --set has its own escaping rules and would mangle a value containing a comma
// or a dot.
func writeInline(dir string, name string, values map[string]any) (string, error) {
	if len(values) == 0 {
		return "", nil
	}

	body, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, valuesFileName(name))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// target is one chart to render.
type target struct {
	ref     string
	release string
}

// resolve turns a classified reference into the charts to check, and the count
// of library charts it deliberately left out.
//
// A local path may hold many charts; every other form names exactly one.
//
// `type: library` charts are excluded because helm will not render one -
// "library charts are not installable" - and handing it over anyway made the
// refusal look like a chart idem could not render. That is exit 2, which is
// always fatal AND escapes the ratchet, so a single library chart anywhere in a
// tree produced a permanently red gate with no way out: idem has no exclusion
// mechanism, by design. A library chart is correct Helm and the recommended way
// to share templates, so this is idem being wrong about it rather than the
// chart being wrong.
//
// The count comes back rather than being dropped, because a gate that checks
// less than it was pointed at has to say so.
func resolve(ref chartref.Ref) ([]target, int, error) {
	if ref.Kind != chartref.Local {
		return []target{{ref: ref.Raw, release: releaseName(ref.Raw)}}, 0, nil
	}

	found, err := discover.Charts(ref.Raw)
	if err != nil {
		return nil, 0, err
	}
	out := make([]target, 0, len(found))
	libraries := 0
	for _, c := range found {
		if c.Library {
			libraries++
			continue
		}
		out = append(out, target{ref: c.Dir, release: c.Name})
	}
	if len(out) == 0 && libraries > 0 {
		// Distinguished from discover's "no directory contains a Chart.yaml",
		// which would be false: idem found charts and chose not to render them.
		noun := "library charts"
		if libraries == 1 {
			noun = "library chart"
		}
		return nil, libraries, fmt.Errorf("found %d %s under %s and nothing else: helm cannot render a `type: library` chart, so there is nothing to compare",
			libraries, noun, ref.Raw)
	}
	return out, libraries, nil
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

// contextName names the cluster a message is about. `--context=` with no value
// means whichever context is current, and saying so beats printing an empty
// pair of quotes.
func contextName(opt options) string {
	if opt.kubeContext == "" {
		return "the current kube context"
	}
	return "context " + opt.kubeContext
}

// unaskedNote explains a dry run idem could not make, when the delivery config
// already says why.
//
// Returns nothing unless the error really is the missing namespace this
// Application would have created. CreateNamespace=true does not explain an
// unreachable cluster or a denied request, and naming it as the reason would
// send the reader after the wrong thing - so this fails closed.
func unaskedNote(err error, rendered, stated string, creates bool) string {
	if !creates || rendered == "" {
		return ""
	}
	// The Application names a namespace and idem rendered into a different one,
	// because --namespace overrides the delivery config. ArgoCD would create
	// the one the Application names, so blaming the render namespace would send
	// the reader to a manifest that never mentions it. Stay silent: this note
	// only ever adds a sentence, so saying nothing costs nothing.
	if stated != "" && stated != rendered {
		return ""
	}
	// kubectl says `namespaces "lab" not found`. Matched on the quoted name so
	// an Application that creates `lab` cannot explain a failure about `prod`.
	msg := err.Error()
	if !strings.Contains(msg, `"`+rendered+`"`) || !strings.Contains(msg, "not found") {
		return ""
	}
	return fmt.Sprintf("      the Application sets CreateNamespace=true, so ArgoCD would create %s first\n", rendered)
}

// admission asks the API server what it would change about a chart's rendered
// output, or nil when no cluster was asked for.
//
// Returned as a hook rather than called over the finished results: it is a
// round trip per chart, and running them after the pool had drained took a
// 16-chart estate from ~10s to ~41s. Inside the pool each one overlaps other
// charts' renders and --jobs bounds them like everything else.
func admission(ctx context.Context, opt options) scan.Admission {
	if !opt.cluster {
		return nil
	}

	c := cluster.New("", opt.kubeContext)
	return func(chart scan.Chart, rendered []manifest.Object) ([]doctor.Rewrite, error) {
		manifests, err := manifest.Encode(rendered)
		if err != nil {
			return nil, err
		}
		returned, err := c.DryRunApply(ctx, chart.Spec.Namespace, manifests)
		if err != nil {
			return nil, err
		}
		return doctor.Rewrites(rendered, returned)
	}
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
	case "yaml":
		return report.Report.YAML, nil
	case "markdown":
		return report.Report.Markdown, nil
	case "github":
		return report.Report.GitHub, nil
	}
	return nil, fmt.Errorf("unknown output format %q: valid values are text, json, yaml, markdown, github", name)
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

	// --namespace asks a different question: not "what has been rolling" but
	// "what is being written after it was applied". A dry run cannot see that
	// - the write happens later - so the only evidence is the live object
	// against its own record of what was applied.
	if opt.namespace != "" {
		objects, err := client.Objects(ctx, opt.namespace, "secrets,configmaps")
		if err != nil {
			fmt.Fprintf(stderr, "idem: cannot read the cluster: %v\n", err)
			return exitFatal
		}
		drifts := doctor.PostApply(objects)
		var render error
		switch opt.output {
		case "json":
			render = report.DriftJSON(stdout, drifts, opt.namespace)
		case "yaml":
			render = report.DriftYAML(stdout, drifts, opt.namespace)
		default:
			render = report.Drift(stdout, drifts, opt.namespace)
		}
		if render != nil {
			fmt.Fprintf(stderr, "idem: %v\n", render)
			return exitFatal
		}
		return exitOK
	}

	workloads, err := client.Workloads(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "idem: cannot read the cluster: %v\n", err)
		return exitFatal
	}

	// Best-effort: a cluster with no ArgoCD simply has no Applications to map,
	// and doctor still has everything the workloads themselves told it.
	sources := client.Sources(ctx)

	diagnosis := doctor.Diagnose(workloads, time.Now())
	var render error
	switch opt.output {
	case "json":
		render = report.DoctorJSON(stdout, diagnosis, opt.kubeContext, sources)
	case "yaml":
		render = report.DoctorYAML(stdout, diagnosis, opt.kubeContext, sources)
	default:
		render = report.Doctor(stdout, diagnosis, opt.kubeContext, sources)
	}
	if render != nil {
		fmt.Fprintf(stderr, "idem: %v\n", render)
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
func inspector(ctx context.Context, ref chartref.Ref, h helm.Helm) scan.Inspect {
	if ref.Kind != chartref.Local {
		return remoteInspector(ref, fetcher{ctx: ctx, helm: h})
	}
	return func(c scan.Chart) ([]analyze.Use, error) { return analyze.Find(c.Dir) }
}

// puller fetches a chart's source. An interface so the wiring is testable
// without a registry, and so idem's own tests do not depend on a network.
type puller interface {
	Pull(ctx context.Context, spec engine.Spec, into string) (string, error)
}

// fetcher is the real one.
type fetcher struct {
	ctx  context.Context
	helm helm.Helm
}

func (f fetcher) Pull(_ context.Context, spec engine.Spec, into string) (string, error) {
	return f.helm.Pull(f.ctx, spec, into)
}

// remoteInspector fetches a chart rendered from a registry so its source can
// be scanned for `lookup`.
//
// Without it both Flux and Helm degrade to `unknown` on every remote chart -
// which is most of what idem's users check, since a consumer of a third-party
// chart is precisely the person who cannot patch it. A fetch that fails stays
// an honest unknown carrying its reason: "could not fetch" is not evidence
// that the chart has no lookup, and treating it as such would turn a failed
// download into a CHURNS verdict §5 states as sound.
func remoteInspector(ref chartref.Ref, p puller) scan.Inspect {
	return func(c scan.Chart) ([]analyze.Use, error) {
		dir, err := os.MkdirTemp("", "idem-chart-")
		if err != nil {
			return nil, fmt.Errorf("fetching %s to scan it: %w", c.Name, err)
		}
		defer os.RemoveAll(dir)

		// The spec idem actually rendered, so the copy scanned is the version
		// the verdict is about rather than whatever is newest.
		source, err := p.Pull(context.Background(), c.Spec, dir)
		if err != nil {
			return nil, fmt.Errorf("fetching %s from %s to scan it: %w", c.Name, ref.Kind, err)
		}
		return analyze.Find(source)
	}
}

// Only charts that actually churn get verdicts: a verdict answers "what does
// this finding mean for me", and a clean chart has no finding to explain.
func verdictsFor(result scan.Result, ev engines.Evidence) []engine.Verdict {
	// Verdicts explain an observed difference, so a chart with none needs no
	// block - but "none" spans both conditions. Gating on the client findings
	// alone is what made a chart that churns only with lookup resolved report
	// as silence.
	if result.Err != nil || (len(result.Findings) == 0 && len(result.ServerFindings) == 0) {
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
