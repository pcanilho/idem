// Package manifest parses rendered Kubernetes manifests into a comparable form.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// sourcePrefix is how `helm template` marks which template produced a document.
const sourcePrefix = "# Source:"

// Object is a single rendered Kubernetes object.
type Object struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string

	// GenerateName is set instead of Name for objects the API server will name
	// at apply time - hook Jobs, typically. In RENDERED output such an object is
	// stable, because the suffix is assigned on apply rather than on render, so
	// it can be compared like any other.
	GenerateName string

	Body map[string]any

	// Source is the chart-relative template that produced this object, taken
	// from helm's "# Source:" comment. Empty when the stream carries no such
	// comment - `argocd app manifests` output, for instance, has been through
	// the repo-server's decoding and has lost it. Empty means unknown, never
	// a guess.
	Source string

	// Line is where the document begins in the parsed stream, for diagnostics.
	Line int

	// DocIndex is the document's position in the stream, counting from zero.
	DocIndex int
}

// Key is the object's identity for matching one render against another.
//
// The separator is "|" rather than "/" because apiVersion itself contains a
// slash ("apps/v1"), and a key that can be ambiguously parsed is a key that
// will eventually match the wrong object.
func (o Object) Key() string {
	return strings.Join([]string{o.APIVersion, o.Kind, o.Namespace, o.identity()}, "|")
}

// identity is metadata.name, or a distinguishable stand-in derived from
// generateName. The "generateName:" prefix keeps a generated object from
// colliding with a really-named one that happens to share the string.
func (o Object) identity() string {
	if o.Name != "" {
		return o.Name
	}
	return "generateName:" + o.GenerateName
}

// Display is the short human-facing form used in findings, e.g. "Deployment/api".
// Namespaced objects carry the namespace so that prod/api and staging/api are
// not both rendered as "Deployment/api".
func (o Object) Display() string {
	name := o.Name
	if name == "" {
		name = o.GenerateName + "*"
	}
	if o.Namespace != "" {
		return o.Kind + "/" + o.Namespace + "/" + name
	}
	return o.Kind + "/" + name
}

// Parse reads a multi-document YAML stream of rendered manifests.
//
// Empty documents are skipped: `helm template` emits them wherever a
// conditional suppressed an entire template, and they are not objects.
func Parse(r io.Reader) ([]Object, error) {
	dec := yaml.NewDecoder(r)
	var out []Object

	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}

		var body map[string]any
		if err := node.Decode(&body); err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if len(body) == 0 {
			continue
		}

		kind, ok := body["kind"].(string)
		switch {
		case !ok && body["kind"] != nil:
			return nil, fmt.Errorf("document %d: 'kind' is %T, want string", i, body["kind"])
		case kind == "":
			return nil, fmt.Errorf("document %d: missing 'kind'; this does not look like a rendered Kubernetes manifest", i)
		}

		apiVersion, _ := body["apiVersion"].(string)
		meta, _ := body["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		namespace, _ := meta["namespace"].(string)
		generateName, _ := meta["generateName"].(string)

		if name == "" && generateName == "" {
			return nil, fmt.Errorf("document %d: %s has neither metadata.name nor metadata.generateName; it cannot be matched between renders", i, kind)
		}

		out = append(out, Object{
			APIVersion:   apiVersion,
			Kind:         kind,
			Namespace:    namespace,
			Name:         name,
			GenerateName: generateName,
			Body:         body,
			Source:       sourceOf(&node),
			Line:         node.Line,
			DocIndex:     i,
		})
	}

	return out, nil
}

// Encode writes objects back out as a YAML stream.
//
// Needed to hand a render to something that reads manifests - `kubectl apply
// --dry-run=server`, which answers what the API server would actually store.
func Encode(objects []Object) ([]byte, error) {
	var out bytes.Buffer
	for _, o := range objects {
		body, err := yaml.Marshal(o.Body)
		if err != nil {
			return nil, fmt.Errorf("encoding %s: %w", o.Display(), err)
		}
		out.WriteString("---\n")
		out.Write(body)
	}
	return out.Bytes(), nil
}

// sourceOf extracts helm's "# Source:" comment.
//
// yaml.v3 attaches a document's leading comment to the HeadComment of the
// mapping's FIRST KEY, not to the document or mapping node. That is an
// implementation detail of the library rather than a documented contract,
// which is why TestParseCapturesSourceTemplateFromHelmComment pins it against
// real helm output shape.
func sourceOf(node *yaml.Node) string {
	n := node
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode || len(n.Content) == 0 {
		return ""
	}
	for line := range strings.SplitSeq(n.Content[0].HeadComment, "\n") {
		line = strings.TrimSpace(line)
		if after, found := strings.CutPrefix(line, sourcePrefix); found {
			return strings.TrimSpace(after)
		}
	}
	return ""
}
