package report

import (
	"encoding/json"
	"io"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/remediate"
)

// The JSON shape is the machine-readable contract - the seam the README points
// policy engines at (`idem ./charts -o json | conftest test -`). It is written
// as explicit view types rather than by marshalling the internal structs, so
// the contract changes only when someone means to change it.

type jsonReport struct {
	Helm     string   `json:"helm" yaml:"helm"`
	Rounds   int      `json:"rounds" yaml:"rounds"`
	Engines  []string `json:"engines,omitempty" yaml:"engines,omitempty"`
	Delivery []string `json:"delivery,omitempty" yaml:"delivery,omitempty"`

	// Releases is how each chart was rendered - release name and namespace,
	// and what decided them. Both change the identity of every object below,
	// and a consumer cannot tell "the repository says so" from "idem picked
	// one" without being told.
	Releases []jsonRelease `json:"releases,omitempty" yaml:"releases,omitempty"`

	Summary       jsonSummary         `json:"summary" yaml:"summary"`
	Findings      []jsonFinding       `json:"findings" yaml:"findings"`
	Suppressed    []jsonSuppressed    `json:"suppressed,omitempty" yaml:"suppressed,omitempty"`
	Potential     []jsonPotential     `json:"potential,omitempty" yaml:"potential,omitempty"`
	Unevaluable   []jsonUnevaluable   `json:"unevaluable,omitempty" yaml:"unevaluable,omitempty"`
	Unconstructed []jsonUnconstructed `json:"unconstructed,omitempty" yaml:"unconstructed,omitempty"`
	Verdicts      []jsonVerdict       `json:"verdicts,omitempty" yaml:"verdicts,omitempty"`
	Remediation   []jsonEntry         `json:"remediation,omitempty" yaml:"remediation,omitempty"`
}

type jsonRelease struct {
	Chart string `json:"chart" yaml:"chart"`

	// Release is the release name when the delivery config named one, absent
	// when idem used the chart name.
	Release   string `json:"release,omitempty" yaml:"release,omitempty"`
	Namespace string `json:"namespace" yaml:"namespace"`

	// From is the manifest that decided it, "--namespace" when the user did,
	// and absent when idem defaulted. Absent means idem chose, which is the
	// one case a consumer must not read as a fact about the repository.
	From string `json:"from,omitempty" yaml:"from,omitempty"`
}

// jsonUnconstructed is a release idem could not build, and the values it
// lacked. Separate from unevaluable because the two need opposite responses.
type jsonUnconstructed struct {
	Chart  string   `json:"chart" yaml:"chart"`
	Needs  []string `json:"needs" yaml:"needs"`
	Reason string   `json:"reason,omitempty" yaml:"reason,omitempty"`
}

type jsonSummary struct {
	Charts   int `json:"charts" yaml:"charts"`
	Churning int `json:"churning" yaml:"churning"`

	// Suppressed counts findings the delivery config covers. They are not in
	// Churning and cannot affect the exit code, so a consumer that wants them
	// back has to ask - the same shape as ESLint's suppressedMessages and
	// Trivy's --show-suppressed.
	Suppressed int `json:"suppressed" yaml:"suppressed"`

	// ChurningWithLookup counts charts that were identical under `helm
	// template` and differed with lookup resolved. Its own number because it
	// answers for different engines: churning is ArgoCD's condition, this is
	// Flux's and Helm's.
	ChurningWithLookup int `json:"churningWithLookup" yaml:"churningWithLookup"`

	Unevaluable int `json:"unevaluable" yaml:"unevaluable"`

	// Unconstructed counts releases idem could not build because their values
	// come from a generator it cannot expand. Never fatal, always counted: a
	// gap in what idem checked is not a defect in the chart, and not silence
	// either.
	Unconstructed int `json:"unconstructed" yaml:"unconstructed"`
}

type jsonFinding struct {
	Chart  string `json:"chart" yaml:"chart"`
	Source string `json:"source,omitempty" yaml:"source,omitempty"`
	Type   string `json:"type" yaml:"type"`

	// Condition is the render condition that saw this difference: "client" for
	// `helm template`, which is what ArgoCD's repo-server does, or "cluster"
	// for a server dry run, where lookup resolves. Always emitted - a policy
	// selecting on churn has to be able to say which engine it means.
	Condition string `json:"condition" yaml:"condition"`

	Object jsonObject `json:"object" yaml:"object"`
	Paths  []jsonPath `json:"paths,omitempty" yaml:"paths,omitempty"`

	// Consequence is the category, not the prose, so a policy can select on
	// it: `.findings[] | select(.consequence == "rolls")`.
	Consequence string `json:"consequence,omitempty" yaml:"consequence,omitempty"`
	Workloads   int    `json:"workloads,omitempty" yaml:"workloads,omitempty"`
}

type jsonObject struct {
	APIVersion   string `json:"apiVersion" yaml:"apiVersion"`
	Kind         string `json:"kind" yaml:"kind"`
	Namespace    string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name         string `json:"name,omitempty" yaml:"name,omitempty"`
	GenerateName string `json:"generateName,omitempty" yaml:"generateName,omitempty"`
}

type jsonPath struct {
	Path    string `json:"path" yaml:"path"`
	Pointer string `json:"pointer" yaml:"pointer"`
}

type jsonSuppressed struct {
	Finding jsonFinding `json:"finding" yaml:"finding"`
	By      jsonRule    `json:"by" yaml:"by"`
}

type jsonRule struct {
	File      string   `json:"file" yaml:"file"`
	Pointers  []string `json:"pointers,omitempty" yaml:"pointers,omitempty"`
	SelfHeal  bool     `json:"selfHeal" yaml:"selfHeal"`
	Respected bool     `json:"respected" yaml:"respected"`
	Engine    string   `json:"engine,omitempty" yaml:"engine,omitempty"`
}

type jsonPotential struct {
	Chart    string `json:"chart" yaml:"chart"`
	Function string `json:"function" yaml:"function"`
	Why      string `json:"why" yaml:"why"`
	File     string `json:"file" yaml:"file"`
	Line     int    `json:"line" yaml:"line"`
}

type jsonUnevaluable struct {
	Chart string `json:"chart" yaml:"chart"`
	Error string `json:"error" yaml:"error"`
}

type jsonVerdict struct {
	Chart    string `json:"chart" yaml:"chart"`
	Engine   string `json:"engine" yaml:"engine"`
	Result   string `json:"result" yaml:"result"`
	Because  string `json:"because" yaml:"because"`
	Observed bool   `json:"observed" yaml:"observed"`
}

// jsonEntry is one remediation entry, for whichever engine Engine names.
//
// One array rather than one per engine, because a consumer asking "what do I
// have to change" wants the answer in one place - but every entry says which
// engine it is for, and carries that engine's own field name. `jsonPointers`
// and `paths` are not two spellings of one thing: ArgoCD evaluates its pointers
// against the rendered config and Flux evaluates its paths against the stored
// object, so a pointer that works in one is silently inert in the other. A
// shared field name would invite exactly the swap that fails without an error.
type jsonEntry struct {
	Engine       string   `json:"engine" yaml:"engine"`
	Group        string   `json:"group,omitempty" yaml:"group,omitempty"`
	Version      string   `json:"version,omitempty" yaml:"version,omitempty"`
	Kind         string   `json:"kind" yaml:"kind"`
	Name         string   `json:"name,omitempty" yaml:"name,omitempty"`
	Namespace    string   `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	JSONPointers []string `json:"jsonPointers,omitempty" yaml:"jsonPointers,omitempty"`
	Paths        []string `json:"paths,omitempty" yaml:"paths,omitempty"`
}

// JSON writes the machine-readable form.
func (r Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r.contract())
}

// contract builds the machine-readable document.
//
// Shared by JSON and YAML rather than rendered twice, so the two cannot drift.
// That is not hypothetical here: this repository has already shipped two output
// formats disagreeing about the same run, twice. The yaml tags on every struct
// above exist for the same reason - yaml.v3 does not read json tags, and would
// silently lowercase every key into a different contract.
func (r Report) contract() jsonReport {
	out := jsonReport{
		Helm:     r.Helm,
		Rounds:   r.Rounds,
		Engines:  r.Engines,
		Delivery: r.Delivery,
		Summary: jsonSummary{
			Charts:             len(r.Charts),
			Churning:           r.Churning(),
			Suppressed:         suppressedCount(r),
			ChurningWithLookup: r.ChurningWithLookup(),
			Unevaluable:        r.Unevaluable(),
			Unconstructed:      r.Unconstructed(),
		},
		// Never null: a consumer iterating .findings should not have to guard
		// against the clean case.
		Findings: []jsonFinding{},
	}

	// A chart that would not render is reported whatever the ratchet says, so
	// these come from every chart rather than from those in scope.
	for _, c := range r.Charts {
		switch {
		case unbuilt(c):
			out.Unconstructed = append(out.Unconstructed, jsonUnconstructed{
				Chart: c.Name, Needs: c.Unresolved, Reason: c.Err.Error(),
			})
		case c.Err != nil:
			out.Unevaluable = append(out.Unevaluable, jsonUnevaluable{Chart: c.Name, Error: c.Err.Error()})
		}
	}

	var all []check.Finding
	for _, c := range r.inScope() {
		if c.Namespace != "" {
			out.Releases = append(out.Releases, jsonRelease{
				Chart: c.Name, Release: c.Release, Namespace: c.Namespace, From: c.NamespaceFrom,
			})
		}
		for _, f := range c.Findings {
			out.Findings = append(out.Findings, jsonFindingOf(r, c, f, c.Findings, conditionClient))
		}
		for _, f := range c.ServerOnly {
			out.Findings = append(out.Findings, jsonFindingOf(r, c, f, c.ServerOnly, conditionCluster))
		}
		for _, s := range c.Suppressed {
			out.Suppressed = append(out.Suppressed, jsonSuppressed{
				Finding: jsonFindingOf(r, c, s.Finding, c.Findings, conditionClient),
				By: jsonRule{
					File: s.By.File, Pointers: s.By.Pointers,
					SelfHeal: s.By.SelfHeal, Respected: s.By.Respected, Engine: s.By.Engine,
				},
			})
		}
		for _, u := range c.Potential {
			out.Potential = append(out.Potential, jsonPotential{
				Chart: c.Name, Function: u.Function, Why: whyOf(u.Function),
				// Resolved, so `file` means the same thing here as it does in
				// findings[].source and in a -o github annotation. One document
				// carrying two path conventions makes both unusable.
				File: r.usePath(c, u.File), Line: u.Line,
			})
		}
		for _, v := range c.Verdicts {
			if !shows(r.Engines, v.Engine) {
				continue
			}
			out.Verdicts = append(out.Verdicts, jsonVerdict{
				Chart: c.Name, Engine: v.Engine, Result: v.Result.String(),
				Because: v.Because, Observed: v.Observed,
			})
		}
		all = append(all, c.Findings...)
	}

	// Both engines, each scoped exactly as the text form scopes it - the same
	// selection, from the same helper. `-o json` is the seam idem tells people
	// to gate on, so a fix it cannot see is a fix their policy engine cannot
	// enforce.
	if shows(r.Engines, "argocd") {
		for _, e := range remediate.Entries(all) {
			out.Remediation = append(out.Remediation, jsonEntry{
				Engine: "argocd",
				Group:  e.Group, Kind: e.Kind, Name: e.Name, Namespace: e.Namespace,
				JSONPointers: e.Pointers,
			})
		}
	}
	if shows(r.Engines, "flux") {
		for _, e := range remediate.FluxEntries(fluxFindings(r.inScope())) {
			out.Remediation = append(out.Remediation, jsonEntry{
				Engine: "flux",
				Group:  e.Group, Version: e.Version, Kind: e.Kind, Name: e.Name, Namespace: e.Namespace,
				Paths: e.Paths,
			})
		}
	}

	return out
}

// suppressedCount is every covered finding in scope, including ones selfHeal
// will undo - the JSON carries the rule's SelfHeal and Respected flags, so a
// consumer can tell them apart without idem deciding for it.
func suppressedCount(r Report) int {
	n := 0
	for _, c := range r.inScope() {
		n += len(c.Suppressed)
	}
	return n
}

// The two render conditions, named once so the contract cannot drift between
// the formats that report it.
const (
	conditionClient  = "client"
	conditionCluster = "cluster"
)

func jsonFindingOf(r Report, c Chart, f check.Finding, siblings []check.Finding, condition string) jsonFinding {
	cost := consequenceOf(f, siblings)

	out := jsonFinding{
		Chart:     c.Name,
		Source:    r.sourcePath(c, f.Source),
		Type:      f.Change.Type.String(),
		Condition: condition,
		Object: jsonObject{
			APIVersion:   f.Change.Object.APIVersion,
			Kind:         f.Change.Object.Kind,
			Namespace:    f.Change.Object.Namespace,
			Name:         f.Change.Object.Name,
			GenerateName: f.Change.Object.GenerateName,
		},
		Consequence: cost.Kind,
		Workloads:   cost.Workloads,
	}
	for _, p := range f.Change.Paths {
		out.Paths = append(out.Paths, jsonPath{Path: p.Path.String(), Pointer: p.Path.JSONPointer()})
	}
	return out
}
