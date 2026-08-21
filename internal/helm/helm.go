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
	"strings"

	"os/exec"

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
	objs, err := manifest.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("parsing %s output for %s: %w", h.bin(), spec.ChartRef, err)
	}
	return objs, nil
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
