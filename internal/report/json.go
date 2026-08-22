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
	Helm     string   `json:"helm"`
	Rounds   int      `json:"rounds"`
	Engines  []string `json:"engines,omitempty"`
	Delivery []string `json:"delivery,omitempty"`

	// Releases is how each chart was rendered - release name and namespace,
	// and what decided them. Both change the identity of every object below,
	// and a consumer cannot tell "the repository says so" from "idem picked
	// one" without being told.
	Releases []jsonRelease `json:"releases,omitempty"`

	Summary       jsonSummary         `json:"summary"`
	Findings      []jsonFinding       `json:"findings"`
	Suppressed    []jsonSuppressed    `json:"suppressed,omitempty"`
	Potential     []jsonPotential     `json:"potential,omitempty"`
	Unevaluable   []jsonUnevaluable   `json:"unevaluable,omitempty"`
	Unconstructed []jsonUnconstructed `json:"unconstructed,omitempty"`
	Verdicts      []jsonVerdict       `json:"verdicts,omitempty"`
	Remediation   []jsonEntry         `json:"remediation,omitempty"`
}

type jsonRelease struct {
	Chart string `json:"chart"`

	// Release is the release name when the delivery config named one, absent
	// when idem used the chart name.
	Release   string `json:"release,omitempty"`
	Namespace string `json:"namespace"`

	// From is the manifest that decided it, "--namespace" when the user did,
	// and absent when idem defaulted. Absent means idem chose, which is the
	// one case a consumer must not read as a fact about the repository.
	From string `json:"from,omitempty"`
}

// jsonUnconstructed is a release idem could not build, and the values it
// lacked. Separate from unevaluable because the two need opposite responses.
type jsonUnconstructed struct {
	Chart  string   `json:"chart"`
	Needs  []string `json:"needs"`
	Reason string   `json:"reason,omitempty"`
}

type jsonSummary struct {
	Charts   int `json:"charts"`
	Churning int `json:"churning"`

	// Suppressed counts findings the delivery config covers. They are not in
	// Churning and cannot affect the exit code, so a consumer that wants them
	// back has to ask - the same shape as ESLint's suppressedMessages and
	// Trivy's --show-suppressed.
	Suppressed int `json:"suppressed"`

	// ChurningWithLookup counts charts that were identical under `helm
	// template` and differed with lookup resolved. Its own number because it
	// answers for different engines: churning is ArgoCD's condition, this is
	// Flux's and Helm's.
	ChurningWithLookup int `json:"churningWithLookup"`

	Unevaluable int `json:"unevaluable"`

	// Unconstructed counts releases idem could not build because their values
	// come from a generator it cannot expand. Never fatal, always counted: a
	// gap in what idem checked is not a defect in the chart, and not silence
	// either.
	Unconstructed int `json:"unconstructed"`
}

type jsonFinding struct {
	Chart  string `json:"chart"`
	Source string `json:"source,omitempty"`
	Type   string `json:"type"`

	// Condition is the render condition that saw this difference: "client" for
	// `helm template`, which is what ArgoCD's repo-server does, or "cluster"
	// for a server dry run, where lookup resolves. Always emitted - a policy
	// selecting on churn has to be able to say which engine it means.
	Condition string `json:"condition"`

	Object jsonObject `json:"object"`
	Paths  []jsonPath `json:"paths,omitempty"`

	// Consequence is the category, not the prose, so a policy can select on
	// it: `.findings[] | select(.consequence == "rolls")`.
	Consequence string `json:"consequence,omitempty"`
	Workloads   int    `json:"workloads,omitempty"`
}

type jsonObject struct {
	APIVersion   string `json:"apiVersion"`
	Kind         string `json:"kind"`
	Namespace    string `json:"namespace,omitempty"`
	Name         string `json:"name,omitempty"`
	GenerateName string `json:"generateName,omitempty"`
}

type jsonPath struct {
	Path    string `json:"path"`
	Pointer string `json:"pointer"`
}

type jsonSuppressed struct {
	Finding jsonFinding `json:"finding"`
	By      jsonRule    `json:"by"`
}

type jsonRule struct {
	File      string   `json:"file"`
	Pointers  []string `json:"pointers,omitempty"`
	SelfHeal  bool     `json:"selfHeal"`
	Respected bool     `json:"respected"`
	Engine    string   `json:"engine,omitempty"`
}

type jsonPotential struct {
	Chart    string `json:"chart"`
	Function string `json:"function"`
	Why      string `json:"why"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

type jsonUnevaluable struct {
	Chart string `json:"chart"`
	Error string `json:"error"`
}

type jsonVerdict struct {
	Chart    string `json:"chart"`
	Engine   string `json:"engine"`
	Result   string `json:"result"`
	Because  string `json:"because"`
	Observed bool   `json:"observed"`
}

type jsonEntry struct {
	Group        string   `json:"group,omitempty"`
	Kind         string   `json:"kind"`
	Name         string   `json:"name,omitempty"`
	Namespace    string   `json:"namespace,omitempty"`
	JSONPointers []string `json:"jsonPointers"`
}

// JSON writes the machine-readable form.
func (r Report) JSON(w io.Writer) error {
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
				Chart: c.Name, Function: u.Function, Why: whyOf(u.Function), File: u.File, Line: u.Line,
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

	for _, e := range remediate.Entries(all) {
		out.Remediation = append(out.Remediation, jsonEntry{
			Group: e.Group, Kind: e.Kind, Name: e.Name, Namespace: e.Namespace, JSONPointers: e.Pointers,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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
