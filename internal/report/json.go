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

	Summary     jsonSummary       `json:"summary"`
	Findings    []jsonFinding     `json:"findings"`
	Suppressed  []jsonSuppressed  `json:"suppressed,omitempty"`
	Potential   []jsonPotential   `json:"potential,omitempty"`
	Unevaluable []jsonUnevaluable `json:"unevaluable,omitempty"`
	Verdicts    []jsonVerdict     `json:"verdicts,omitempty"`
	Remediation []jsonEntry       `json:"remediation,omitempty"`
}

type jsonSummary struct {
	Charts      int `json:"charts"`
	Churning    int `json:"churning"`
	Unevaluable int `json:"unevaluable"`
}

type jsonFinding struct {
	Chart  string `json:"chart"`
	Source string `json:"source,omitempty"`
	Type   string `json:"type"`

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
			Charts:      len(r.Charts),
			Churning:    r.Churning(),
			Unevaluable: r.Unevaluable(),
		},
		// Never null: a consumer iterating .findings should not have to guard
		// against the clean case.
		Findings: []jsonFinding{},
	}

	var all []check.Finding
	for _, c := range r.inScope() {
		if c.Err != nil {
			out.Unevaluable = append(out.Unevaluable, jsonUnevaluable{Chart: c.Name, Error: c.Err.Error()})
		}
		for _, f := range c.Findings {
			out.Findings = append(out.Findings, jsonFindingOf(c, f))
		}
		for _, s := range c.Suppressed {
			out.Suppressed = append(out.Suppressed, jsonSuppressed{
				Finding: jsonFindingOf(c, s.Finding),
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

func jsonFindingOf(c Chart, f check.Finding) jsonFinding {
	cost := consequenceOf(f, c.Findings)

	out := jsonFinding{
		Chart:  c.Name,
		Source: f.Source,
		Type:   f.Change.Type.String(),
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
