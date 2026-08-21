// Command idem checks whether Helm charts render consistently.
//
// This is the walking skeleton: classify the reference, render it more than
// once with `helm template`, compare the results structurally, and print the
// verdict. Engine verdicts, remediation blocks, the static analyzer and the
// other output formats are not here yet.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/pcanilho/idem/internal/chartref"
	"github.com/pcanilho/idem/internal/discover"
	"github.com/pcanilho/idem/internal/engine"
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

	if opt.showVersion {
		fmt.Fprintf(stdout, "idem %s\n", cliVersion())
		return exitOK
	}

	if opt.rounds < 2 {
		fmt.Fprintf(stderr, "idem: --rounds is %d: at least 2 renders are needed, because a single render cannot be compared to anything\n", opt.rounds)
		return exitFatal
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

	h := helm.New(opt.helmBin)
	ctx := context.Background()

	// Read the version first: it is printed with every result, and it fails
	// fast and clearly when there is no helm to render with at all.
	helmVersion, err := h.Version(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "idem: cannot run helm: %v\n", err)
		return exitFatal
	}

	queue := make([]scan.Chart, 0, len(charts))
	for _, c := range charts {
		queue = append(queue, scan.Chart{Name: c.release, Dir: c.ref, Spec: specFor(ref, c, opt)})
	}

	rep := report.Report{Helm: helmVersion, Rounds: opt.rounds}
	for _, result := range scan.Charts(ctx, h, queue, opt.rounds, opt.jobs) {
		rep.Charts = append(rep.Charts, report.Chart{
			Name:     result.Chart.Name,
			Dir:      result.Chart.Dir,
			Findings: result.Findings,
			Err:      result.Err,
		})
	}

	if err := rep.Text(stdout); err != nil {
		fmt.Fprintf(stderr, "idem: %v\n", err)
		return exitFatal
	}

	// The exit code is stated in the output as well as returned. A CI log that
	// ends in a bare non-zero status makes the reader go looking for the
	// reason, and the reason is already on screen.
	switch {
	case rep.Unevaluable() > 0:
		fmt.Fprintln(stdout, "  exit 2 — a chart could not be rendered")
		return exitFatal
	case opt.strict && rep.Churning() > 0:
		fmt.Fprintln(stdout, "  exit 1")
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
	showVersion  bool
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
