package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pcanilho/idem/internal/manifest"
)

// lastApplied is where client-side apply records what was sent.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// serverFields are set by the API server, not by whoever applied the object.
// Comparing them would report the cluster being a cluster.
var serverFields = [][]string{
	{"metadata", "resourceVersion"},
	{"metadata", "uid"},
	{"metadata", "generation"},
	{"metadata", "creationTimestamp"},
	{"metadata", "selfLink"},
	{"metadata", "managedFields"},
	{"metadata", "annotations", lastApplied},
	{"status"},
}

// LiveObject is an object as it exists now, beside the record of what was
// last applied to it.
type LiveObject struct {
	Live manifest.Object

	// Applied is what the last apply sent, from the last-applied annotation.
	Applied    manifest.Object
	HasApplied bool

	// Managers are the field managers from managedFields. Often empty even
	// when something else is writing: a client-side apply records nothing
	// there, which is exactly the case that matters.
	Managers []string

	Labels      map[string]string
	Annotations map[string]string
}

// Objects reads live objects of the given kinds from one namespace.
func (c Client) Objects(ctx context.Context, namespace, kinds string) ([]LiveObject, error) {
	args := []string{"get", kinds, "-o", "json"}
	if namespace == "" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", namespace)
	}

	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseLive(out)
}

// DryRunApply asks the API server what it would store, without storing it.
//
// This is the only way to see cause 3: a mutating webhook rewrites an object
// as it is admitted, and no amount of rendering reveals that. Nothing is
// persisted - a server dry run is a question, not an apply.
func (c Client) DryRunApply(ctx context.Context, namespace string, manifests []byte) ([]manifest.Object, error) {
	args := []string{"apply", "--dry-run=server", "-f", "-", "-o", "json"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}

	out, err := c.runWithInput(ctx, manifests, args...)
	if err != nil {
		return nil, err
	}

	// kubectl emits a List when given several objects and a bare object when
	// given one.
	var probe struct {
		Kind  string            `json:"kind"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("reading kubectl output: %w", err)
	}

	if probe.Kind != "List" {
		return manifest.Parse(strings.NewReader(string(out)))
	}

	var objects []manifest.Object
	for _, raw := range probe.Items {
		parsed, err := manifest.Parse(strings.NewReader(string(raw)))
		if err != nil {
			continue
		}
		objects = append(objects, parsed...)
	}
	return objects, nil
}

func parseLive(body []byte) ([]LiveObject, error) {
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}

	out := make([]LiveObject, 0, len(list.Items))
	for _, raw := range list.Items {
		object, err := one(raw)
		if err != nil {
			// An object idem cannot read is not a reason to abandon the rest
			// of the namespace.
			continue
		}
		out = append(out, object)
	}
	return out, nil
}

func one(raw json.RawMessage) (LiveObject, error) {
	var meta struct {
		Metadata struct {
			Labels        map[string]string `json:"labels"`
			Annotations   map[string]string `json:"annotations"`
			ManagedFields []struct {
				Manager string `json:"manager"`
			} `json:"managedFields"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return LiveObject{}, err
	}

	// JSON is YAML, so the manifest parser reads both without a second path.
	live, err := manifest.Parse(strings.NewReader(string(raw)))
	if err != nil || len(live) == 0 {
		return LiveObject{}, err
	}

	out := LiveObject{
		Live:        strip(live[0]),
		Labels:      meta.Metadata.Labels,
		Annotations: meta.Metadata.Annotations,
	}
	for _, f := range meta.Metadata.ManagedFields {
		if f.Manager != "" {
			out.Managers = append(out.Managers, f.Manager)
		}
	}

	if applied := meta.Metadata.Annotations[lastApplied]; applied != "" {
		parsed, err := manifest.Parse(strings.NewReader(applied))
		if err == nil && len(parsed) > 0 {
			out.Applied = strip(parsed[0])
			out.HasApplied = true
		}
	}
	return out, nil
}

// Normalise makes an object comparable with the other side of a diff, by
// removing what the API server owns and applying the rewrites it performs on
// write. Exported because both sides of an apply-side comparison need it: what
// was sent has to be measured on the same terms as what came back.
func Normalise(o manifest.Object) manifest.Object { return strip(o) }

// strip makes an object comparable with the other side, by removing what the
// API server owns and normalising what it rewrites on write.
//
// Both normalisations were found by running this against a real namespace,
// where every Secret written with stringData and every object applied with an
// empty map read as drift.
func strip(o manifest.Object) manifest.Object {
	for _, path := range serverFields {
		remove(o.Body, path)
	}
	foldStringData(o)
	dropEmpty(o.Body)
	return o
}

// foldStringData applies the API server's own rule for Secrets.
//
// stringData is write-only: Kubernetes merges it into data on write and never
// returns it. So what was APPLIED carries stringData and what is LIVE carries
// data, and comparing them raw reports every such Secret as rewritten when
// nothing touched it.
func foldStringData(o manifest.Object) {
	if o.Kind != "Secret" || strings.Contains(o.APIVersion, "/") {
		return
	}

	stringData, ok := o.Body["stringData"].(map[string]any)
	if !ok {
		return
	}

	data, _ := o.Body["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	for k, v := range stringData {
		if text, ok := v.(string); ok {
			data[k] = base64.StdEncoding.EncodeToString([]byte(text))
			continue
		}
		data[k] = v
	}

	o.Body["data"] = data
	delete(o.Body, "stringData")
}

// dropEmpty removes empty maps, empty lists and nulls.
//
// An object applied with `data: {}` or `data: null` comes back with no data
// key at all, and empty is the same state as absent - reporting the difference
// would be reporting the cluster being tidy. Both forms were found in one
// namespace: a chart emitting `data:` with nothing under it produces the null.
func dropEmpty(body map[string]any) {
	for key, value := range body {
		switch v := value.(type) {
		case nil:
			delete(body, key)
		case map[string]any:
			dropEmpty(v)
			if len(v) == 0 {
				delete(body, key)
			}
		case []any:
			if len(v) == 0 {
				delete(body, key)
			}
		}
	}
}

func remove(body map[string]any, path []string) {
	for i := range len(path) - 1 {
		next, ok := body[path[i]].(map[string]any)
		if !ok {
			return
		}
		body = next
	}
	delete(body, path[len(path)-1])
}
