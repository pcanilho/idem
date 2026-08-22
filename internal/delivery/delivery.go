// Package delivery reads the GitOps config that decides a chart's fate.
//
// idem reports what a chart renders. Whether that churn reaches a cluster also
// depends on what the user already told their engine to ignore - and a tool
// that keeps reporting what you have already handled is a tool you switch off.
//
// Designed from the ArgoCD and Flux schemas, never from a directory layout.
// Charts and delivery manifests routinely live in sibling trees, so the join
// key is spec.source.path, not where a file happens to sit on disk.
package delivery

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/remediate"
)

// respectOption is the sync option without which ignoreDifferences hides the
// diff but selfHeal re-applies the object anyway.
const respectOption = "RespectIgnoreDifferences=true"

// Rule is one suppression an engine has been told to apply.
type Rule struct {
	// Group, Kind, Namespace and Name select objects. An absent field is not
	// the same in each case - see Covers.
	Group     string
	Kind      string
	Namespace string
	Name      string

	// Pointers are RFC 6901; JQ expressions idem will not evaluate.
	Pointers []string
	JQ       []string

	// File is the manifest this came from, relative to Root.
	File string

	// Path is spec.source.path - the chart this rule governs. Empty when the
	// manifest names no path idem can join to a chart.
	Path string

	// Templated marks a path a generator substitutes at runtime, which cannot
	// be tied to a chart directory and must never suppress.
	Templated bool

	SelfHeal  bool
	Respected bool
	Engine    string
}

// Destination is where a delivery manifest says one chart's objects go.
//
// Separate from Rule because a chart can have a destination and no
// ignoreDifferences at all, which is the common case - and the namespace
// matters even when nothing is suppressed.
type Destination struct {
	// Path is spec.source.path, the same join key rules use.
	Path      string
	Namespace string
	File      string

	// Templated marks a namespace a generator substitutes per generated
	// Application. There is no single answer, so idem uses none.
	Templated bool
}

// Values is what a delivery manifest renders a chart WITH.
//
// A chart is not the unit of analysis - a release is - and half of a release
// is its values. Rendering with none makes idem report "could not be rendered"
// about charts whose `required` guards are working exactly as written.
type Values struct {
	Path string
	File string

	// Release is spec.source.helm.releaseName. .Release.Name is in the name of
	// nearly every object a chart produces, so getting it from the chart name
	// instead reports identities the cluster will never have.
	Release string

	// ValueFiles are repository-relative, in order: later files win, so the
	// order is semantic. A leading slash in the manifest means "from the repo
	// root"; anything else is relative to spec.source.path.
	ValueFiles []string

	// Inline is spec.source.helm.valuesObject merged under the parsed
	// spec.source.helm.values string, and Sets are the parameters as
	// name=value. Kept apart because helm's precedence differs between them.
	Inline map[string]any
	Sets   []string

	// Templated names the keys a generator substitutes, which idem cannot
	// resolve and refuses to invent. Recorded so the reason a chart would not
	// render can name them rather than reporting a bare failure.
	Templated []string
}

// Config is what the delivery manifests in a tree say.
type Config struct {
	Root         string
	Engines      []string
	Rules        []Rule
	Destinations []Destination
	Values       []Values
	Files        []string
}

// ValuesFor is what the delivery config renders this chart with.
//
// Two manifests claiming one chart are merged in neither direction: as with
// NamespaceFor, idem has no Application of its own to choose between them, and
// rendering with a mixture of two releases' values would produce a release
// nobody deploys. The first is used and the rest ignored only when they agree
// on being absent; otherwise nothing is returned.
func (c Config) ValuesFor(chartPath string) Values {
	var found []Values
	for _, v := range c.Values {
		if v.Path == "" || v.Path != chartPath {
			continue
		}
		found = append(found, v)
	}
	if len(found) != 1 {
		return Values{}
	}
	return found[0]
}

// NamespaceFor is the namespace the delivery config says this chart deploys
// into, and the manifest that said so. Empty when nothing does.
//
// Two manifests disagreeing means the same chart goes to two namespaces - the
// same chart in staging and production is exactly this shape - and idem has no
// Application of its own to pick between them. It picks neither: a namespace
// stated with confidence and wrong is worse than none at all, because every
// object identity and every namespaced suppression rule turns on it.
func (c Config) NamespaceFor(chartPath string) (string, string) {
	var ns, file string
	for _, d := range c.Destinations {
		if d.Templated || d.Path == "" || d.Path != chartPath || d.Namespace == "" {
			continue
		}
		if ns != "" && ns != d.Namespace {
			return "", ""
		}
		ns, file = d.Namespace, d.File
	}
	return ns, file
}

// Root finds the repository containing start.
//
// Falls back to start itself: a directory outside any repository is a fine
// thing to point idem at, and is not an error.
func Root(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

// Load reads every delivery manifest at or below root.
//
// Finding nothing is the normal case, not an error - plenty of estates keep
// charts and delivery config in separate repositories.
func Load(root string) (Config, error) {
	cfg := Config{Root: root}
	engines := make(map[string]struct{})

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}

		body, readErr := os.ReadFile(path)
		// An unreadable file is not a reason to abandon the scan; it simply
		// tells idem nothing.
		if readErr != nil || !mentionsDeliveryKind(body) {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		rules, dests, values, found := parse(body, rel)
		if len(found) == 0 {
			return nil
		}
		for _, engine := range found {
			engines[engine] = struct{}{}
		}
		cfg.Rules = append(cfg.Rules, rules...)
		cfg.Destinations = append(cfg.Destinations, dests...)
		cfg.Values = append(cfg.Values, values...)
		cfg.Files = append(cfg.Files, rel)
		return nil
	})
	if err != nil {
		return Config{}, err
	}

	cfg.Engines = slices.Sorted(mapKeys(engines))
	slices.Sort(cfg.Files)
	return cfg, nil
}

func mapKeys(m map[string]struct{}) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// mentionsDeliveryKind is a cheap pre-filter, so a repository of chart
// templates is not fully YAML-parsed to discover it holds no Applications.
func mentionsDeliveryKind(body []byte) bool {
	text := string(body)
	for _, kind := range []string{"Application", "HelmRelease"} {
		if strings.Contains(text, kind) {
			return true
		}
	}
	return false
}

// --- the shapes idem reads, and nothing more ---

type source struct {
	Path string      `yaml:"path"`
	Helm *helmSource `yaml:"helm"`
}

// helmSource is spec.source.helm - what ArgoCD renders the chart with.
type helmSource struct {
	ReleaseName  string         `yaml:"releaseName"`
	ValueFiles   []string       `yaml:"valueFiles"`
	Values       string         `yaml:"values"`
	ValuesObject map[string]any `yaml:"valuesObject"`
	Parameters   []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"parameters"`
}

type ignoreDifference struct {
	Group             string   `yaml:"group"`
	Kind              string   `yaml:"kind"`
	Namespace         string   `yaml:"namespace"`
	Name              string   `yaml:"name"`
	JSONPointers      []string `yaml:"jsonPointers"`
	JQPathExpressions []string `yaml:"jqPathExpressions"`
}

type syncPolicy struct {
	Automated *struct {
		SelfHeal bool `yaml:"selfHeal"`
	} `yaml:"automated"`
	SyncOptions []string `yaml:"syncOptions"`
}

type destination struct {
	Namespace string `yaml:"namespace"`
}

type appSpec struct {
	Destination       *destination       `yaml:"destination"`
	Source            *source            `yaml:"source"`
	Sources           []source           `yaml:"sources"`
	IgnoreDifferences []ignoreDifference `yaml:"ignoreDifferences"`
	SyncPolicy        *syncPolicy        `yaml:"syncPolicy"`
}

type document struct {
	Kind string `yaml:"kind"`
	Spec struct {
		appSpec `yaml:",inline"`

		// An ApplicationSet carries the same fields one level deeper. Reading
		// only spec.ignoreDifferences would silently miss every app an
		// ApplicationSet manages.
		Template *struct {
			Spec appSpec `yaml:"spec"`
		} `yaml:"template"`

		DriftDetection *struct {
			Mode   string `yaml:"mode"`
			Ignore []struct {
				Paths []string `yaml:"paths"`
			} `yaml:"ignore"`
		} `yaml:"driftDetection"`
	} `yaml:"spec"`
}

// parse extracts rules from one file, and names the engines it saw.
//
// A document that will not decode is skipped rather than reported: most YAML
// in a chart repository is Go template source and was never meant to parse.
func parse(body []byte, file string) ([]Rule, []Destination, []Values, []string) {
	var rules []Rule
	var dests []Destination
	var values []Values
	var engines []string

	decoder := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var doc document
		if err := decoder.Decode(&doc); err != nil {
			break
		}

		switch doc.Kind {
		case "Application":
			engines = append(engines, "argocd")
			rules = append(rules, argoRules(doc.Spec.appSpec, file)...)
			dests = append(dests, argoDestinations(doc.Spec.appSpec, file)...)
			values = append(values, argoValues(doc.Spec.appSpec, file)...)
		case "ApplicationSet":
			engines = append(engines, "argocd")
			if doc.Spec.Template != nil {
				rules = append(rules, argoRules(doc.Spec.Template.Spec, file)...)
				dests = append(dests, argoDestinations(doc.Spec.Template.Spec, file)...)
				values = append(values, argoValues(doc.Spec.Template.Spec, file)...)
			}
		case "HelmRelease":
			engines = append(engines, "flux")
			rules = append(rules, fluxRules(doc, file)...)
		}
	}
	return rules, dests, values, engines
}

// templated reports whether a generator substitutes this at runtime. idem has
// no generator, so anything carrying `{{` is a value it cannot know.
func templated(s string) bool { return strings.Contains(s, "{{") }

// argoValues reads spec.source.helm for every chart path the manifest claims.
//
// Templated entries are dropped and named rather than guessed at: a fabricated
// value renders a release nobody deploys, and idem would be reporting on it as
// if it were real.
func argoValues(spec appSpec, file string) []Values {
	sources := []source{}
	if spec.Source != nil {
		sources = append(sources, *spec.Source)
	}
	sources = append(sources, spec.Sources...)

	var out []Values
	for _, src := range sources {
		if src.Helm == nil || src.Path == "" || templated(src.Path) {
			continue
		}

		v := Values{Path: src.Path, File: file}
		if !templated(src.Helm.ReleaseName) {
			v.Release = src.Helm.ReleaseName
		}

		for _, f := range src.Helm.ValueFiles {
			if templated(f) {
				v.Templated = append(v.Templated, f)
				continue
			}
			v.ValueFiles = append(v.ValueFiles, valueFilePath(src.Path, f))
		}

		// The `values` string is a values FILE in a string, so it parses the
		// same way. valuesObject is layered over it, which is ArgoCD's own
		// precedence.
		v.Inline = parseValues(src.Helm.Values)
		for key, val := range src.Helm.ValuesObject {
			if text, ok := val.(string); ok && templated(text) {
				v.Templated = append(v.Templated, key)
				continue
			}
			if v.Inline == nil {
				v.Inline = map[string]any{}
			}
			v.Inline[key] = val
		}

		for _, p := range src.Helm.Parameters {
			if templated(p.Value) || templated(p.Name) {
				v.Templated = append(v.Templated, p.Name)
				continue
			}
			v.Sets = append(v.Sets, p.Name+"="+p.Value)
		}

		out = append(out, v)
	}
	return out
}

// valueFilePath resolves a valueFiles entry to a repository-relative path.
//
// A leading slash means the repository root rather than the filesystem root -
// ArgoCD's own rule, and the reason a chart can move without its Application
// changing.
func valueFilePath(chartPath, file string) string {
	if after, ok := strings.CutPrefix(file, "/"); ok {
		return after
	}
	return path.Join(chartPath, file)
}

// parseValues reads the spec.source.helm.values string. Unparseable YAML is
// nothing rather than an error: the manifest is the user's, not idem's, and a
// values block idem cannot read is not a reason to refuse the whole run.
func parseValues(text string) map[string]any {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out map[string]any
	if err := yaml.Unmarshal([]byte(text), &out); err != nil {
		return nil
	}
	return out
}

// argoDestinations reads spec.destination.namespace against every chart path
// the manifest claims.
//
// A HelmRelease deliberately has no equivalent: the chart reaches Flux through
// a separate source object, so there is no path to join a namespace to, and
// guessing from the HelmRelease's own metadata.namespace would be inventing a
// join idem cannot make.
func argoDestinations(spec appSpec, file string) []Destination {
	if spec.Destination == nil || spec.Destination.Namespace == "" {
		return nil
	}

	var out []Destination
	for _, path := range chartPaths(spec) {
		out = append(out, Destination{
			Path:      path,
			Namespace: spec.Destination.Namespace,
			File:      file,
			// Either half can be templated, and either makes the join useless.
			Templated: strings.Contains(path, "{{") || strings.Contains(spec.Destination.Namespace, "{{"),
		})
	}
	return out
}

func argoRules(spec appSpec, file string) []Rule {
	var selfHeal, respected bool
	if spec.SyncPolicy != nil {
		selfHeal = spec.SyncPolicy.Automated != nil && spec.SyncPolicy.Automated.SelfHeal
		respected = slices.Contains(spec.SyncPolicy.SyncOptions, respectOption)
	}

	paths := chartPaths(spec)

	var rules []Rule
	for _, ignore := range spec.IgnoreDifferences {
		for _, path := range paths {
			rules = append(rules, Rule{
				Group:     ignore.Group,
				Kind:      ignore.Kind,
				Namespace: ignore.Namespace,
				Name:      ignore.Name,
				Pointers:  ignore.JSONPointers,
				JQ:        ignore.JQPathExpressions,
				File:      file,
				Path:      path,
				Templated: strings.Contains(path, "{{"),
				SelfHeal:  selfHeal,
				Respected: respected,
				Engine:    "argocd",
			})
		}
	}
	return rules
}

// chartPaths lists the source paths, or one empty path so that a manifest
// naming none still produces its rules - unjoinable, but visible.
func chartPaths(spec appSpec) []string {
	var paths []string
	if spec.Source != nil {
		paths = append(paths, spec.Source.Path)
	}
	for _, s := range spec.Sources {
		paths = append(paths, s.Path)
	}
	if len(paths) == 0 {
		return []string{""}
	}
	return paths
}

// fluxRules reads driftDetection.ignore.
//
// A HelmRelease names no chart path idem can join to a directory - the chart
// arrives through a separate source object - so these rules are unjoinable by
// construction and can annotate but never suppress.
func fluxRules(doc document, file string) []Rule {
	if doc.Spec.DriftDetection == nil {
		return nil
	}

	var rules []Rule
	for _, ignore := range doc.Spec.DriftDetection.Ignore {
		rules = append(rules, Rule{
			Pointers: ignore.Paths,
			File:     file,
			Engine:   "flux",
		})
	}
	return rules
}

// For returns the rules governing the chart at path, relative to Root.
//
// A rule whose path idem could not resolve governs nothing: it is visible, but
// it can never suppress. Under-reporting suppression is recoverable; hiding
// real churn is not.
func (c Config) For(chartPath string) []Rule {
	var out []Rule
	for _, r := range c.Rules {
		if r.Templated || r.Path == "" || r.Path != chartPath {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Selects reports whether the rule targets this object at all.
//
// The asymmetry between the fields is ArgoCD's, not idem's. `group` is matched
// as a plain value, so an omitted group is the empty string and selects only
// core objects - `glob.Match("", "apps")` is false, which is why `group: core`
// is a well-known way to write a rule that matches nothing. The other three
// are presence-checked, so omitting them selects any value.
func (r Rule) Selects(ref ObjectRef) bool {
	if r.Group != ref.Group {
		return false
	}
	if r.Kind != "" && r.Kind != ref.Kind {
		return false
	}
	if r.Name != "" && r.Name != ref.Name {
		return false
	}
	return r.Namespace == "" || r.Namespace == ref.Namespace
}

// ObjectRef is the identity a rule is matched against.
type ObjectRef struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// Covers reports whether this rule definitely suppresses the given pointer.
//
// pointers are the forms ArgoCD will actually evaluate for this object, which
// is not what the chart rendered - a Secret written with stringData is diffed
// under data, so a user's /data/KEY rule must match a finding idem derived as
// /stringData/KEY. Phase A's normalisation supplies both forms.
func (r Rule) Covers(ref ObjectRef, pointers []string) bool {
	if !r.Selects(ref) {
		return false
	}
	for _, candidate := range pointers {
		for _, p := range r.Pointers {
			if covers(p, candidate) {
				return true
			}
		}
	}
	return false
}

// covers reports whether rule pointer p suppresses candidate.
//
// Prefix-aware: a rule for /data suppresses /data/password, because removing
// the parent removes the child. The trailing separator matters - /data must
// not be read as covering /database.
func covers(p, candidate string) bool {
	return p == candidate || strings.HasPrefix(candidate, p+"/")
}

// MayCover reports whether the rule targets this object through a jq
// expression idem cannot evaluate.
//
// idem will not vendor a jq engine, so this is reported as "may already be
// covered" and never counted either way.
func (r Rule) MayCover(ref ObjectRef) bool {
	return len(r.JQ) > 0 && r.Selects(ref)
}

// Suppressed is a finding an engine has already been told to ignore.
type Suppressed struct {
	Finding check.Finding
	By      Rule
}

// Applied is what the delivery config says about a chart's findings.
type Applied struct {
	// Churning are the findings still unaccounted for, with any covered
	// fields removed.
	Churning []check.Finding

	// Suppressed are findings a rule definitely covers.
	Suppressed []Suppressed

	// Maybe are findings a rule reaches only through a jq expression idem
	// cannot evaluate. Never counted either way.
	Maybe []Suppressed
}

// Apply splits findings by what the delivery config already handles.
//
// Works field by field rather than finding by finding. Suppressing a whole
// finding because one of its fields is covered would hide the rest, which is
// the one mistake this must not make.
func Apply(rules []Rule, findings []check.Finding) Applied {
	var out Applied

	for _, f := range findings {
		ref := objectRef(f.Change.Object)

		var uncovered []diff.PathDiff
		var by Rule
		for _, p := range f.Change.Paths {
			pointers := remediate.EvaluablePointers(f.Change.Object, p.Path.JSONPointer())
			rule, covered := coveringRule(rules, ref, pointers)
			if !covered {
				uncovered = append(uncovered, p)
				continue
			}
			by = rule
		}

		// A finding with no fields - an object rendered in one round and not
		// another - has nothing a pointer could address, so no rule covers it.
		if len(f.Change.Paths) > 0 && len(uncovered) == 0 {
			out.Suppressed = append(out.Suppressed, Suppressed{Finding: f, By: by})
			continue
		}

		f.Change.Paths = uncovered
		out.Churning = append(out.Churning, f)

		for _, r := range rules {
			if r.MayCover(ref) {
				out.Maybe = append(out.Maybe, Suppressed{Finding: f, By: r})
				break
			}
		}
	}

	return out
}

func coveringRule(rules []Rule, ref ObjectRef, pointers []string) (Rule, bool) {
	for _, r := range rules {
		if r.Covers(ref, pointers) {
			return r, true
		}
	}
	return Rule{}, false
}

// objectRef narrows a rendered object to the identity a rule matches on.
func objectRef(o diff.ObjectRef) ObjectRef {
	group, _, found := strings.Cut(o.APIVersion, "/")
	if !found {
		// A core object's apiVersion is just "v1" - no group at all.
		group = ""
	}
	return ObjectRef{Group: group, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name}
}
