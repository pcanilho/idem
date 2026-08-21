// Package engines holds the GitOps engines idem reports on, and what a
// render-side difference means under each of them.
//
// The same finding means three different things depending on what reconciles
// it, and telling them apart is the single most useful thing idem does: it
// answers whether to file an upstream issue or add an ignoreDifferences block.
package engines

import (
	"fmt"
	"strings"

	"github.com/pcanilho/idem/internal/analyze"
	"github.com/pcanilho/idem/internal/engine"
)

// Target is one engine idem can say something true about.
type Target struct {
	id   string
	name string

	// churn is what churning costs under this engine, in the reader's terms.
	churn string

	caps engine.Capabilities
}

func (t Target) ID() string                        { return t.id }
func (t Target) Name() string                      { return t.name }
func (t Target) Capabilities() engine.Capabilities { return t.caps }

// All returns every engine, in the order they are read: the one idem measures
// first, then the two it reasons about.
func All() []Target {
	return []Target{
		{
			id:   "argocd",
			name: "argocd",
			// Verified: ArgoCD's repo-server shells out to `helm template`
			// (util/helm/cmd.go) with no cluster access, and re-renders on
			// every reconcile.
			churn: "every sync, forever — repo-server renders without cluster access",
			caps:  engine.Capabilities{LookupResolves: false, RerendersOnReconcile: true},
		},
		{
			id:   "flux",
			name: "flux",
			// Verified: helm-controller embeds helm.sh/helm/v4 and performs a
			// real install - "we never render the templates" - so lookup
			// resolves and templates are not re-rendered on reconcile.
			churn: "on every chart or values change",
			caps:  engine.Capabilities{LookupResolves: true, RerendersOnReconcile: false},
		},
		{
			id:    "helm",
			name:  "helm",
			churn: "on every `helm upgrade`",
			caps:  engine.Capabilities{LookupResolves: true, RerendersOnReconcile: false},
		},
	}
}

// Select resolves an --engine value.
func Select(spec string) ([]Target, error) {
	all := All()
	if spec == "all" {
		return all, nil
	}
	for _, e := range all {
		if e.id == spec {
			return []Target{e}, nil
		}
	}

	valid := []string{"all"}
	for _, e := range all {
		valid = append(valid, e.id)
	}
	return nil, fmt.Errorf("unknown engine %q: valid values are %s", spec, strings.Join(valid, ", "))
}

// Evidence is what idem established about a chart's use of lookup.
//
// Err is a first-class state rather than an empty Uses: "I could not read the
// chart" and "this chart contains no lookup" lead to opposite verdicts, and
// collapsing them would turn a failed scan into a confident CHURNS.
type Evidence struct {
	// Uses are the `lookup` call sites specifically - not every flagged
	// function. Only lookup can stabilise a value, so only lookup decides
	// these verdicts.
	Uses []analyze.Use
	Err  error
}

// Verdict says what this engine does with a chart that rendered inconsistently
// under `helm template`.
//
// Certainty is carried on the verdict itself rather than implied by wording,
// because the answers here are reached three different ways.
func (t Target) Verdict(ev Evidence) engine.Verdict {
	uses := ev.Uses
	// `helm template` resolves lookup to {} by construction, which IS the
	// condition an engine without cluster access renders under. So the
	// observed difference was not extrapolated to this engine - it was
	// measured there, and no lookup in the chart can change it.
	if !t.caps.LookupResolves {
		return engine.Verdict{Engine: t.name, Result: engine.Churns, Because: t.churn, Observed: true}
	}

	// The scan failed, so whether a lookup could stabilise this value is not
	// something idem established. That is unknown, not clean.
	if ev.Err != nil {
		return engine.Verdict{
			Engine:   t.name,
			Result:   engine.Unknown,
			Because:  fmt.Sprintf("could not scan this chart for `lookup`: %v", ev.Err),
			Observed: false,
		}
	}

	// lookup resolves here, but there is no lookup to resolve. Nothing in the
	// chart could stabilise the value under any engine, which makes this a
	// chart defect rather than an engine limitation - sound, not measured.
	if len(uses) == 0 {
		return engine.Verdict{Engine: t.name, Result: engine.Churns, Because: t.churn, Observed: false}
	}

	// The chart does call lookup. Whether it guards this particular value would
	// take tracing the template AST through include into subchart helpers,
	// which idem does not do and never will. Say unknown, and cite the lookup
	// so the reader can judge it themselves.
	best, _ := analyze.Best(uses)
	return engine.Verdict{
		Engine:   t.name,
		Result:   engine.Unknown,
		Because:  fmt.Sprintf("chart calls `lookup` (%s:%d) — may guard this value", best.File, best.Line),
		Observed: false,
	}
}
