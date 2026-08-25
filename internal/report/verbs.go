package report

import (
	"io"
	"math"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/doctor"
)

// The machine-readable forms of `idem doctor` and `idem diff`.
//
// Both verbs rendered text and nothing else, and -o was refused outright. That
// sat badly with the reason idem has no rules file: `-o json | conftest` is
// the documented extension point, so two of the three verbs could not reach
// the seam the design points at. `doctor` in particular produces a ranked
// table of what keeps rolling, which is exactly the shape someone graphs or
// alerts on.
//
// Explicit view types, like the chart report's - never the internal structs -
// so the contract changes only when someone means to change it.
//
// `-o markdown` and `-o github` stay refused for both. Those two are shapes,
// not encodings: markdown is a pull-request comment about a chart in a diff,
// and github annotates a file at a line. A cluster's rollout history belongs
// to neither, and rendering one anyway would be a table nobody asked for on a
// pull request that has nothing to do with it.

type doctorDoc struct {
	// Context is the kube context asked, empty when it was whichever is
	// current. Which cluster answered is as much a part of the result here as
	// which helm rendered is in the chart report.
	Context string `json:"context,omitempty" yaml:"context,omitempty"`

	Scanned int     `json:"scanned" yaml:"scanned"`
	Median  float64 `json:"median" yaml:"median"`

	// Suspects is never null: the clean run is the case a consumer's pipeline
	// hits most often, and it should not have to guard against a null there.
	Suspects []doctorSuspect `json:"suspects" yaml:"suspects"`
}

type doctorSuspect struct {
	Kind      string `json:"kind" yaml:"kind"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name      string `json:"name" yaml:"name"`

	Revision int     `json:"revision" yaml:"revision"`
	PerDay   float64 `json:"perDay" yaml:"perDay"`
	AgeDays  int     `json:"ageDays" yaml:"ageDays"`

	// Checksums carries EVERY annotation, where the text form names one and
	// counts the rest. That elision is a column-width decision; a contract
	// that elided would simply be telling a consumer something untrue.
	Checksums []string `json:"checksums,omitempty" yaml:"checksums,omitempty"`

	Owner doctorOwner `json:"owner" yaml:"owner"`

	// Chart is the path the text form's "confirm the cause" command names,
	// resolved through the owner. Empty when idem could not resolve one -
	// absent rather than guessed at.
	Chart string `json:"chart,omitempty" yaml:"chart,omitempty"`
}

type doctorOwner struct {
	Engine string `json:"engine,omitempty" yaml:"engine,omitempty"`
	Name   string `json:"name,omitempty" yaml:"name,omitempty"`
	Chart  string `json:"chart,omitempty" yaml:"chart,omitempty"`
}

func doctorContract(d doctor.Diagnosis, context string, sources map[string]string) doctorDoc {
	out := doctorDoc{
		Context:  context,
		Scanned:  d.Scanned,
		Median:   rate(d.Median),
		Suspects: make([]doctorSuspect, 0, len(d.Suspects)),
	}
	for _, s := range d.Suspects {
		w := s.Workload
		out.Suspects = append(out.Suspects, doctorSuspect{
			Kind: w.Kind, Namespace: w.Namespace, Name: w.Name,
			Revision:  w.Revision,
			PerDay:    rate(s.PerDay),
			AgeDays:   s.Days(),
			Checksums: w.Checksums,
			Owner:     doctorOwner{Engine: w.Owner.Engine, Name: w.Owner.Name, Chart: w.Owner.Chart},
			Chart:     sources[w.Owner.Name],
		})
	}
	return out
}

// DoctorJSON writes the diagnosis as the machine-readable contract.
func DoctorJSON(w io.Writer, d doctor.Diagnosis, context string, sources map[string]string) error {
	return writeJSON(w, doctorContract(d, context, sources))
}

// DoctorYAML writes the same document in YAML.
func DoctorYAML(w io.Writer, d doctor.Diagnosis, context string, sources map[string]string) error {
	return writeYAML(w, doctorContract(d, context, sources))
}

type driftDoc struct {
	Namespace string      `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Drifts    []driftItem `json:"drifts" yaml:"drifts"`
}

type driftItem struct {
	Object jsonObject `json:"object" yaml:"object"`

	// Writer and Evidence are what idem believes wrote the field and what
	// identified it. Both empty when it cannot say - never inferred.
	Writer   string `json:"writer,omitempty" yaml:"writer,omitempty"`
	Evidence string `json:"evidence,omitempty" yaml:"evidence,omitempty"`

	Changes []jsonPath `json:"changes" yaml:"changes"`
}

func driftContract(drifts []doctor.Drift, namespace string) driftDoc {
	out := driftDoc{Namespace: namespace, Drifts: make([]driftItem, 0, len(drifts))}
	for _, d := range drifts {
		item := driftItem{
			Object:   objectOf(d.Object),
			Writer:   d.Writer,
			Evidence: d.Evidence,
			Changes:  make([]jsonPath, 0, len(d.Changes)),
		}
		for _, p := range d.Changes {
			item.Changes = append(item.Changes, pathOf(p))
		}
		out.Drifts = append(out.Drifts, item)
	}
	return out
}

// DriftJSON writes post-apply drift as the machine-readable contract.
func DriftJSON(w io.Writer, drifts []doctor.Drift, namespace string) error {
	return writeJSON(w, driftContract(drifts, namespace))
}

// DriftYAML writes the same document in YAML.
func DriftYAML(w io.Writer, drifts []doctor.Drift, namespace string) error {
	return writeYAML(w, driftContract(drifts, namespace))
}

type diffDoc struct {
	Left  string `json:"left" yaml:"left"`
	Right string `json:"right" yaml:"right"`

	Findings []diffFinding `json:"findings" yaml:"findings"`
}

type diffFinding struct {
	Object jsonObject `json:"object" yaml:"object"`
	Type   string     `json:"type" yaml:"type"`
	Paths  []jsonPath `json:"paths,omitempty" yaml:"paths,omitempty"`
}

func diffContract(findings []check.Finding, left, right string) diffDoc {
	out := diffDoc{Left: left, Right: right, Findings: make([]diffFinding, 0, len(findings))}
	for _, f := range findings {
		item := diffFinding{
			Object: objectOf(f.Change.Object),
			// The name, not the ordinal - the same rule the chart report's
			// enums follow, so the contract survives a reordering.
			Type:  f.Change.Type.String(),
			Paths: make([]jsonPath, 0, len(f.Change.Paths)),
		}
		for _, p := range f.Change.Paths {
			item.Paths = append(item.Paths, pathOf(p))
		}
		out.Findings = append(out.Findings, item)
	}
	return out
}

// DiffJSON writes a two-file comparison as the machine-readable contract.
//
// No maxFields cap, unlike the text form: the cap exists so one object cannot
// fill a terminal, and a consumer that asked for the document wants all of it.
// `-v` means the same thing on the chart report for the same reason.
func DiffJSON(w io.Writer, findings []check.Finding, left, right string) error {
	return writeJSON(w, diffContract(findings, left, right))
}

// DiffYAML writes the same document in YAML.
func DiffYAML(w io.Writer, findings []check.Finding, left, right string) error {
	return writeYAML(w, diffContract(findings, left, right))
}

// rate rounds a per-day rollout rate to the two decimals the text form has
// always displayed.
//
// Not cosmetic: these are derived from time.Now(), so at full float64 precision
// two runs of `idem doctor -o json` a second apart differ in every one of them.
// Piped to a file, hashed, or diffed, that is churn idem produced itself - and a
// tool that reports non-determinism cannot exhibit it. The lost precision is
// spurious anyway, since the inputs are a rollout count and an age in days.
func rate(f float64) float64 {
	return math.Round(f*100) / 100
}

// pathOf is the shared rendering of a differing path, so the verbs and the
// chart report cannot describe the same thing differently.
func pathOf(p diff.PathDiff) jsonPath {
	return jsonPath{
		Path:      p.Path.String(),
		Pointer:   p.Path.JSONPointer(),
		Reordered: p.Reordered,
	}
}
