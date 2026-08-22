// Package cluster reads a live cluster, read-only, by shelling out to kubectl.
//
// Shelling out for the same reason idem shells out to helm (§3): vendoring
// client-go would be by far the largest dependency in the project, and the
// user's own kubectl already holds their auth, their contexts and their exec
// credential plugins. idem never sees a credential and never writes anything.
//
// Unlike helm, kubectl is not guaranteed to be present, so its absence is
// reported plainly rather than assumed.
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Client runs kubectl against one context.
type Client struct {
	Bin string

	// Context is the kubeconfig context. Empty uses whichever is current.
	Context string
}

// New returns a Client using bin, or "kubectl" when bin is empty.
func New(bin, kubeContext string) Client { return Client{Bin: bin, Context: kubeContext} }

func (c Client) bin() string {
	if c.Bin == "" {
		return "kubectl"
	}
	return c.Bin
}

// Workload is a thing that runs pods and can therefore roll.
type Workload struct {
	Kind      string
	Namespace string
	Name      string

	// Revision is how many times it has rolled out. Deployments record this
	// directly; StatefulSets and DaemonSets do not, so their generation - which
	// increments on every spec change - stands in.
	Revision int

	Created time.Time

	// Checksums are the checksum/* annotations on the pod template. A chart
	// hashing a Secret into one is doing the right thing; it is also what
	// turns a non-deterministic Secret into pods restarting on every sync.
	Checksums []string

	// Owner is whatever put this here, read from the labels and annotations
	// the engines stamp on what they apply.
	Owner Owner
}

// Owner is the delivery object responsible for a workload.
//
// Every engine leaves a mark on what it applies, so a workload can say who
// owns it without idem having to ask anything else. That turns "this rolls too
// often" into "this rolls too often, and here is the Application to look at".
type Owner struct {
	// Engine is argocd, flux or helm, and empty when nothing said.
	Engine string

	// Name is the Application, HelmRelease or Helm release.
	Name string

	// Chart is what it was rendered from, when that was recorded.
	Chart string
}

// String is the short form for output, e.g. "argocd lab-app".
func (o Owner) String() string {
	switch {
	case o.Engine == "" && o.Name == "":
		return ""
	case o.Name == "":
		return o.Engine
	}
	return o.Engine + " " + o.Name
}

// ownerOf works out what applied a workload.
//
// Ordered most specific first. ArgoCD's tracking-id names the Application
// outright; a bare instance label only names a release, which several things
// could have set.
func ownerOf(labels, annotations map[string]string) Owner {
	chart := labels["helm.sh/chart"]
	if chart == "" {
		chart = labels["chart"]
	}

	// "lab-app:apps/Deployment:lab/lab-harbor-registry" - the Application is
	// everything before the first colon.
	if id := annotations["argocd.argoproj.io/tracking-id"]; id != "" {
		name, _, _ := strings.Cut(id, ":")
		return Owner{Engine: "argocd", Name: name, Chart: chart}
	}
	if name := labels["argocd.argoproj.io/instance"]; name != "" {
		return Owner{Engine: "argocd", Name: name, Chart: chart}
	}
	if name := labels["helm.toolkit.fluxcd.io/name"]; name != "" {
		return Owner{Engine: "flux", Name: name, Chart: chart}
	}
	if name := labels["kustomize.toolkit.fluxcd.io/name"]; name != "" {
		return Owner{Engine: "flux", Name: name, Chart: chart}
	}
	if name := annotations["meta.helm.sh/release-name"]; name != "" {
		return Owner{Engine: "helm", Name: name, Chart: chart}
	}
	if labels["app.kubernetes.io/managed-by"] == "Helm" {
		return Owner{Engine: "helm", Name: labels["app.kubernetes.io/instance"], Chart: chart}
	}
	return Owner{Chart: chart}
}

// Workloads lists every Deployment, StatefulSet and DaemonSet in the cluster.
//
// One query. `doctor` needs no chart, no git and no rendering - it asks the
// cluster what has already been happening.
func (c Client) Workloads(ctx context.Context) ([]Workload, error) {
	out, err := c.run(ctx, "get", "deployments,statefulsets,daemonsets", "--all-namespaces", "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseWorkloads(out)
}

// Sources maps an ArgoCD Application name to the chart path it deploys.
//
// Read from the cluster rather than from git, so `doctor` can turn "this rolls
// too often" into a command the reader can run. A cluster without the CRD is
// not an error: it simply has no ArgoCD, and doctor still has everything else.
func (c Client) Sources(ctx context.Context) map[string]string {
	out, err := c.run(ctx, "get", "applications.argoproj.io", "--all-namespaces", "-o", "json")
	if err != nil {
		return nil
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Source  struct{ Path string }   `json:"source"`
				Sources []struct{ Path string } `json:"sources"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil
	}

	paths := make(map[string]string, len(list.Items))
	for _, item := range list.Items {
		switch {
		case item.Spec.Source.Path != "":
			paths[item.Metadata.Name] = item.Spec.Source.Path
		case len(item.Spec.Sources) > 0:
			paths[item.Metadata.Name] = item.Spec.Sources[0].Path
		}
	}
	return paths
}

func (c Client) run(ctx context.Context, args ...string) ([]byte, error) {
	return c.runWithInput(ctx, nil, args...)
}

func (c Client) runWithInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	if c.Context != "" {
		args = append(args, "--context", c.Context)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s %s: %w: %s", c.bin(), args[0], err, msg)
		}
		return nil, fmt.Errorf("%s %s: %w", c.bin(), args[0], err)
	}
	return stdout.Bytes(), nil
}

// revisionAnnotation is where a Deployment records how many times it has
// rolled out.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// parseWorkloads reads a kubectl list into the fields doctor reasons about.
func parseWorkloads(body []byte) ([]Workload, error) {
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name              string            `json:"name"`
				Namespace         string            `json:"namespace"`
				Generation        int               `json:"generation"`
				CreationTimestamp time.Time         `json:"creationTimestamp"`
				Labels            map[string]string `json:"labels"`
				Annotations       map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Metadata struct {
						Annotations map[string]string `json:"annotations"`
					} `json:"metadata"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("reading kubectl output: %w", err)
	}

	out := make([]Workload, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, Workload{
			Kind:      item.Kind,
			Namespace: item.Metadata.Namespace,
			Name:      item.Metadata.Name,
			Revision:  revisionOf(item.Metadata.Annotations, item.Metadata.Generation),
			Created:   item.Metadata.CreationTimestamp,
			Checksums: checksumsIn(item.Spec.Template.Metadata.Annotations),
			Owner:     ownerOf(item.Metadata.Labels, item.Metadata.Annotations),
		})
	}
	return out, nil
}

// revisionOf prefers the Deployment's own count, falling back to generation.
func revisionOf(annotations map[string]string, generation int) int {
	if n, err := strconv.Atoi(annotations[revisionAnnotation]); err == nil {
		return n
	}
	return generation
}

func checksumsIn(annotations map[string]string) []string {
	var out []string
	for name := range annotations {
		if strings.HasPrefix(name, "checksum") {
			out = append(out, name)
		}
	}
	// Sorted so idem's own output does not vary between runs: Go map
	// iteration order is deliberately random.
	slices.Sort(out)
	return out
}
