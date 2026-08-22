// Package engine models the GitOps engines whose rendering behaviour idem reports on.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/pcanilho/idem/internal/manifest"
)

// Result is what idem concluded about one chart under one engine.
//
// The zero value is Unknown, deliberately. A verdict nobody set must never read
// as "this is fine" - that is the single wrong answer a user would believe
// without checking.
type Result int

const (
	// Unknown means idem could not establish an answer. The zero value.
	Unknown Result = iota
	// Stable means the value does not change between renders under this engine.
	Stable
	// Churns means it does.
	Churns
)

func (r Result) String() string {
	switch r {
	case Stable:
		return "stable"
	case Churns:
		return "churns"
	}
	return "unknown"
}

// MarshalJSON emits the name rather than the ordinal, so `-o json` stays
// readable and survives any future reordering of the constants.
func (r Result) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// UnmarshalJSON accepts the names emitted by MarshalJSON.
func (r *Result) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "stable":
		*r = Stable
	case "churns":
		*r = Churns
	case "unknown":
		*r = Unknown
	default:
		return fmt.Errorf("unknown result %q", s)
	}
	return nil
}

// Verdict is one engine's answer about one finding.
type Verdict struct {
	Engine  string `json:"engine"`
	Result  Result `json:"result"`
	Because string `json:"because"`

	// Observed distinguishes a measurement from an inference. True when idem
	// rendered under this engine's semantics and compared the output; false
	// when the answer was derived some other way. Every claim carries its own
	// provenance so that none has to be taken on trust.
	Observed bool `json:"observed"`
}

// Capabilities are facts about an engine, not about any particular chart.
// These are known with certainty; the uncertainty lives in Verdict.
type Capabilities struct {
	// LookupResolves is whether Helm's lookup function reaches a live cluster.
	LookupResolves bool `json:"lookupResolves"`
	// RerendersOnReconcile is whether the engine re-renders templates on every
	// reconcile, or only when the chart or its values change.
	RerendersOnReconcile bool `json:"rerendersOnReconcile"`
}

// Spec is a request to render one chart.
type Spec struct {
	ChartRef string `json:"chartRef"`
	Version  string `json:"version,omitempty"`
	Repo     string `json:"repo,omitempty"`

	// ValuesFiles are -f arguments, in order. Later files win.
	ValuesFiles []string `json:"valuesFiles,omitempty"`
	// SetValues are --set arguments. Kept separate from ValuesFiles because
	// their precedence and ordering semantics differ; one []string could not
	// represent both faithfully.
	SetValues []string `json:"setValues,omitempty"`

	Namespace string `json:"namespace,omitempty"`
	Release   string `json:"release,omitempty"`

	// KubeVersion and APIVersions mirror helm's flags of the same name. ArgoCD
	// passes the live cluster's values, so reproducing it faithfully means
	// being able to pass them too.
	KubeVersion string   `json:"kubeVersion,omitempty"`
	APIVersions []string `json:"apiVersions,omitempty"`

	// Cluster asks the API server to render, so `lookup` resolves and the
	// chart sees the cluster's real capabilities rather than helm's defaults.
	// Read-only: a server dry run is a render-time query, never an apply.
	Cluster bool `json:"cluster,omitempty"`

	// KubeContext selects which cluster, when Cluster is set.
	KubeContext string `json:"kubeContext,omitempty"`
}

// Engine is any target idem can say something true about.
type Engine interface {
	// ID uniquely identifies this engine instance. Two engines may share a
	// Name - an argocd driving helm 3 and one driving helm 4 - but never an ID.
	ID() string
	// Name is the display name, e.g. "argocd".
	Name() string
	Capabilities() Capabilities
}

// Renderer is an Engine that idem can execute locally.
type Renderer interface {
	Engine
	Render(ctx context.Context, spec Spec) ([]manifest.Object, error)
}

// Registry holds the known engines, keyed by ID.
type Registry struct {
	engines map[string]Engine
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{engines: make(map[string]Engine)}
}

// Register adds an engine. Registering a duplicate ID is an error rather than a
// silent replacement: quietly dropping an engine would make `--engine all`
// report less than it claims to.
func (r *Registry) Register(e Engine) error {
	id := e.ID()
	if _, dup := r.engines[id]; dup {
		return fmt.Errorf("engine %q is already registered", id)
	}
	r.engines[id] = e
	return nil
}

// Lookup returns the engine registered under id.
func (r *Registry) Lookup(id string) (Engine, bool) {
	e, ok := r.engines[id]
	return e, ok
}

// IDs returns every registered engine ID, sorted so that output built from it
// is deterministic.
func (r *Registry) IDs() []string {
	return slices.Sorted(maps.Keys(r.engines))
}

// Renderers returns only the engines idem can execute locally, in ID order.
//
// An engine that cannot be executed - Flux, whose helm-controller performs a
// real install against a live cluster - is deliberately absent here rather than
// present-and-failing, so an inferred verdict can never be mistaken for an
// observed one.
func (r *Registry) Renderers() []Renderer {
	var out []Renderer
	for _, id := range r.IDs() {
		if rd, ok := r.engines[id].(Renderer); ok {
			out = append(out, rd)
		}
	}
	return out
}
