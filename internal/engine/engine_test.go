package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/manifest"
)

// stubEngine can be reasoned about but not executed locally — the Flux case.
type stubEngine struct {
	id   string
	name string
	caps Capabilities
}

func (s stubEngine) ID() string {
	if s.id != "" {
		return s.id
	}
	return s.name
}
func (s stubEngine) Name() string               { return s.name }
func (s stubEngine) Capabilities() Capabilities { return s.caps }

// stubRenderer can be executed locally — the ArgoCD and Helm case.
type stubRenderer struct {
	stubEngine
	rendered []manifest.Object
}

func (s stubRenderer) Render(context.Context, Spec) ([]manifest.Object, error) {
	return s.rendered, nil
}

func TestRegistryReturnsRegisteredEngineByName(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(stubEngine{name: "flux"})

	got, ok := r.Lookup("flux")
	if !ok {
		t.Fatal("Lookup(flux) not found after Register")
	}
	if got.Name() != "flux" {
		t.Errorf("Name() = %q, want %q", got.Name(), "flux")
	}
}

func TestRegistryReportsUnknownEngine(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Lookup("nope"); ok {
		t.Error("Lookup returned ok for an unregistered engine")
	}
}

func TestNamesAreSortedForDeterministicOutput(t *testing.T) {
	// Enough entries that an unsorted map iteration cannot land in sorted
	// order by chance. With three, a mutant that drops the sort passes about
	// one run in six - a flake, not a guard.
	r := NewRegistry()
	for _, n := range []string{"sveltos", "flux", "argocd", "timoni", "helm", "kapp", "fleet", "helmfile"} {
		_ = r.Register(stubEngine{name: n})
	}

	want := []string{"argocd", "fleet", "flux", "helm", "helmfile", "kapp", "sveltos", "timoni"}
	got := r.IDs()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

// These are compile-time assertions, which is what they always were. Asserting
// them at runtime only re-checked the type checker: a stub the test itself
// defines without a Render method cannot satisfy an interface requiring one.
//
// The real risk is a FUTURE engine - fluxEngine, say - accidentally gaining a
// Render method and silently becoming executable. That is guarded by the line
// below, which will stop compiling if it ever happens.
var (
	_ Engine   = stubEngine{}
	_ Renderer = stubRenderer{}
)

func TestRenderersReturnsOnlyExecutableEngines(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(stubEngine{name: "flux"})
	r.Register(stubRenderer{stubEngine: stubEngine{name: "argocd"}})

	got := r.Renderers()
	if len(got) != 1 {
		t.Fatalf("Renderers() returned %d engines, want 1", len(got))
	}
	if got[0].Name() != "argocd" {
		t.Errorf("Renderers()[0] = %q, want %q", got[0].Name(), "argocd")
	}
}

func TestRenderersAreReturnedInNameOrder(t *testing.T) {
	// With a single renderer registered, any iteration order looks correct.
	r := NewRegistry()
	for _, n := range []string{"zulu", "alpha", "mike", "bravo", "yankee", "charlie"} {
		_ = r.Register(stubRenderer{stubEngine: stubEngine{name: n}})
	}
	r.Register(stubEngine{name: "delta"}) // not executable; must be excluded

	want := []string{"alpha", "bravo", "charlie", "mike", "yankee", "zulu"}
	got := r.Renderers()
	if len(got) != len(want) {
		t.Fatalf("Renderers() returned %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name() != w {
			t.Fatalf("Renderers() = %v..., want %v", got[i].Name(), w)
		}
	}
}

func TestZeroVerdictIsUnknown(t *testing.T) {
	// The zero value must be Unknown, never Stable. A verdict nobody set must
	// not read as "this chart is fine" - that is the one wrong answer that
	// would be silently believed.
	var v Verdict
	if v.Result != Unknown {
		t.Errorf("zero Verdict.Result = %v, want Unknown", v.Result)
	}
	if v.Observed {
		t.Error("zero Verdict claims to be Observed")
	}
}

func TestVerdictResultsMarshalAsStrings(t *testing.T) {
	// -o json is a public contract. Marshalling an enum as 0/1/2 makes the
	// output unreadable and breaks the moment a constant is reordered.
	for r, want := range map[Result]string{
		Unknown: `"unknown"`,
		Stable:  `"stable"`,
		Churns:  `"churns"`,
	} {
		got, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal %v: %v", r, err)
		}
		if string(got) != want {
			t.Errorf("json.Marshal(%v) = %s, want %s", r, got, want)
		}
	}
}

func TestVerdictRoundTripsThroughJSON(t *testing.T) {
	in := Verdict{Engine: "argocd", Result: Churns, Because: "lookup returns {}", Observed: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Verdict
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestRegistryHoldsTwoEnginesOfTheSameName(t *testing.T) {
	// Comparing Helm 3 against Helm 4 means two argocd engines differing only in
	// which helm binary they drive. Keying the registry on Name() made that
	// impossible; it is keyed on ID().
	r := NewRegistry()
	if err := r.Register(stubRenderer{stubEngine: stubEngine{id: "argocd@helm3", name: "argocd"}}); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := r.Register(stubRenderer{stubEngine: stubEngine{id: "argocd@helm4", name: "argocd"}}); err != nil {
		t.Fatalf("register second: %v", err)
	}
	if n := len(r.IDs()); n != 2 {
		t.Fatalf("registry holds %d engines, want 2", n)
	}
	e, ok := r.Lookup("argocd@helm3")
	if !ok || e.Name() != "argocd" {
		t.Errorf("Lookup(argocd@helm3) = %v, %v", e, ok)
	}
}

func TestRegisteringTheSameIDTwiceIsAnError(t *testing.T) {
	// Silently replacing an engine would make `--engine all` quietly drop one.
	r := NewRegistry()
	if err := r.Register(stubEngine{id: "argocd", name: "argocd"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := r.Register(stubEngine{id: "argocd", name: "argocd"})
	if err == nil {
		t.Fatal("registering a duplicate ID succeeded, want error")
	}
	if !strings.Contains(err.Error(), "argocd") {
		t.Errorf("error %q should name the duplicate", err)
	}
}

func TestSpecSeparatesValuesFilesFromSetOverrides(t *testing.T) {
	// -f and --set are not interchangeable: they have different precedence and
	// different ordering semantics, so they cannot share one []string.
	s := Spec{
		ChartRef:    "oci://example.com/chart",
		Version:     "16.2",
		ValuesFiles: []string{"a.yaml", "b.yaml"},
		SetValues:   []string{"auth.password=x"},
	}
	if len(s.ValuesFiles) != 2 || len(s.SetValues) != 1 {
		t.Fatalf("Spec did not keep values files and --set separate: %+v", s)
	}
	if s.Version != "16.2" {
		t.Errorf("Spec.Version = %q, want 16.2", s.Version)
	}
}
