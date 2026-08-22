// Package helm renders charts by shelling out to the helm binary.
//
// idem never vendors the Helm SDK. ArgoCD's repo-server shells out too
// (util/helm/cmd.go), so shelling out is what reproduces its behaviour rather
// than approximating it - and it means idem works with whichever helm the user
// actually runs, including the Helm 3/Helm 4 split that ArgoCD 3.5 introduced.
// Registry authentication comes along for free: helm reads the credentials the
// user already configured, and idem never sees one.
package helm

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"os"
	"os/exec"
	"path/filepath"

	"github.com/pcanilho/idem/internal/engine"
	"github.com/pcanilho/idem/internal/manifest"
)

// Helm renders charts with a helm binary on PATH.
type Helm struct {
	Bin string
}

// New returns a Helm using bin, or "helm" when bin is empty.
func New(bin string) Helm { return Helm{Bin: bin} }

func (h Helm) bin() string {
	if h.Bin == "" {
		return "helm"
	}
	return h.Bin
}

// semver matches a version helm could plausibly have printed. Anything else is
// left alone rather than trimmed into a confident-looking lie: the version line
// exists to be trusted, so a string idem cannot parse is shown verbatim.
var semver = regexp.MustCompile(`^v?\d+\.\d+\.\d+`)

// Version reports the helm binary's version, e.g. "4.2.4".
func (h Helm) Version(ctx context.Context) (string, error) {
	out, err := h.run(ctx, "version", "--short")
	if err != nil {
		return "", err
	}
	return parseVersion(out.String()), nil
}

// Render runs `helm template` once and parses the result.
//
// `helm template` is a client-side render: lookup resolves to {} by
// construction, which is precisely the condition ArgoCD's repo-server renders
// under. That is not an approximation of ArgoCD - it is the same operation.
func (h Helm) Render(ctx context.Context, spec engine.Spec) ([]manifest.Object, error) {
	out, err := h.run(ctx, templateArgs(spec)...)
	if err != nil {
		return nil, err
	}
	objs, err := manifest.Parse(bytes.NewReader(stripPullPreamble(out.Bytes())))
	if err != nil {
		return nil, fmt.Errorf("parsing %s output for %s: %w", h.bin(), spec.ChartRef, err)
	}
	return objs, nil
}

// pullPreamble are the lines `helm template` writes to STDOUT, ahead of the
// YAML, when it has to fetch the chart from a registry first.
//
// They are an allowlist rather than "drop everything before the first ---" on
// purpose: anything else appearing there is something idem does not understand,
// and the parser rejecting it loudly beats silently discarding output.
var pullPreamble = []string{"Pulled: ", "Digest: "}

// stripPullPreamble removes helm's registry chatter from the head of a render.
//
// Those two lines are a valid YAML mapping, so without this the stream decodes
// as a document with no 'kind' and every OCI chart comes back unevaluable.
func stripPullPreamble(out []byte) []byte {
	for {
		line, rest, found := bytes.Cut(out, []byte("\n"))
		if !found {
			return out
		}
		if !slices.ContainsFunc(pullPreamble, func(p string) bool {
			return bytes.HasPrefix(line, []byte(p))
		}) {
			return out
		}
		out = rest
	}
}

// DependencyBuild fetches the subcharts a chart's Chart.lock pins.
func (h Helm) DependencyBuild(ctx context.Context, dir string) error {
	_, err := h.run(ctx, "dependency", "build", dir)
	return err
}

// DependencyUpdate re-resolves subcharts and rewrites Chart.lock.
func (h Helm) DependencyUpdate(ctx context.Context, dir string) error {
	_, err := h.run(ctx, "dependency", "update", dir)
	return err
}

// run executes helm and returns stdout, folding stderr into any error.
//
// helm's own stderr is the actionable part of a failure - "found in Chart.yaml
// but missing in charts/ directory" tells the user what to do, where a bare
// exit status does not.
func (h Helm) run(ctx context.Context, args ...string) (*bytes.Buffer, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, h.bin(), args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s %s: %w: %s", h.bin(), args[0], err, msg)
		}
		return nil, fmt.Errorf("%s %s: %w", h.bin(), args[0], err)
	}
	return &stdout, nil
}

// templateArgs builds the argument list for one render.
// Pull fetches a chart into dir and returns the directory it unpacked into.
//
// The point is the chart SOURCE: a chart rendered from a registry never lands
// on disk, so `lookup` cannot be scanned for and the Flux and Helm verdicts
// degrade to unknown on exactly the charts idem exists for. Nothing is written
// outside dir, which the caller owns and removes.
func (h Helm) Pull(ctx context.Context, spec engine.Spec, dir string) (string, error) {
	if _, err := h.run(ctx, pullArgs(spec, dir)...); err != nil {
		return "", err
	}

	// helm unpacks into <dir>/<chart name>, which is the chart's OWN name and
	// need not match the reference it was pulled by - an OCI path segment, an
	// alias, a repo entry. Read it back rather than reconstructing it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("helm pull wrote no chart directory into %s", dir)
}

// pullArgs builds the fetch. Deliberately none of the render-only flags: a
// pull takes no values and knows nothing about a cluster, and passing them
// would either be ignored or rejected.
func pullArgs(spec engine.Spec, dir string) []string {
	args := []string{"pull", spec.ChartRef, "--untar", "--untardir", dir}
	if spec.Repo != "" {
		args = append(args, "--repo", spec.Repo)
	}
	if spec.Version != "" {
		args = append(args, "--version", spec.Version)
	}
	return args
}

func templateArgs(spec engine.Spec) []string {
	args := []string{"template", spec.Release, spec.ChartRef}

	for _, f := range spec.ValuesFiles {
		args = append(args, "-f", f)
	}
	for _, s := range spec.SetValues {
		args = append(args, "--set", s)
	}
	if spec.Namespace != "" {
		args = append(args, "--namespace", spec.Namespace)
	}
	if spec.Repo != "" {
		args = append(args, "--repo", spec.Repo)
	}
	if spec.Version != "" {
		args = append(args, "--version", spec.Version)
	}
	// A server dry run makes lookup resolve and hands the chart the cluster's
	// own KubeVersion and APIVersions - which are nothing like helm's
	// defaults: helm 4 assumes v1.36.0 where a real cluster may be on v1.26.
	// Nothing is applied; this is a render-time query.
	if spec.Cluster {
		args = append(args, "--dry-run=server")
		if spec.KubeContext != "" {
			args = append(args, "--kube-context", spec.KubeContext)
		}
		// Deliberately no --kube-version or --api-versions here: the server
		// already supplied the real ones, and overriding them would put back
		// exactly the guesses this flag exists to escape.
		return args
	}

	if spec.KubeVersion != "" {
		args = append(args, "--kube-version", spec.KubeVersion)
	}
	// Repeated rather than comma-joined: helm accepts a comma-joined value but
	// reads it back as one bogus group/version, so charts gated on
	// .Capabilities.APIVersions.Has would silently take the wrong branch.
	for _, v := range spec.APIVersions {
		args = append(args, "--api-versions", v)
	}
	return args
}

// parseVersion turns `helm version --short` output into a bare version.
func parseVersion(raw string) string {
	v := strings.TrimSpace(raw)
	if !semver.MatchString(v) {
		return v
	}
	v = strings.TrimPrefix(v, "v")
	if before, _, found := strings.Cut(v, "+"); found {
		return before
	}
	return v
}
