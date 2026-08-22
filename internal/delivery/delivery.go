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
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/remediate"
)

// respectOption is the sync option without which ignoreDifferences hides the
// diff but selfHeal re-applies the object anyway.
const respectOption = "RespectIgnoreDifferences=true"

// createNamespaceOption is a syncOption; serverSideDiffOption is NOT - it lives
// in the argocd.argoproj.io/compare-options annotation. Verified against
// ArgoCD's own diff-strategies page, which documents the annotation and the
// controller-level `controller.diff.server.side` and no syncOption at all.
const (
	createNamespaceOption = "CreateNamespace=true"
	serverSideDiffOption  = "ServerSideDiff=true"
	compareOptions        = "argocd.argoproj.io/compare-options"
)

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

	// CreateNamespace records syncOptions: [CreateNamespace=true]. A dry run
	// into a namespace that does not exist fails, and without this the bare
	// failure reads as "idem could not check" rather than "ArgoCD would have
	// created it first".
	CreateNamespace bool

	// ServerSideDiff records the compare-options annotation asking for
	// server-side diff.
	//
	// Only ever an upgrade from hedging to certainty. It can also be turned on
	// cluster-wide by `controller.diff.server.side` in argocd-cmd-params-cm,
	// which is in no manifest idem reads - so false means "no manifest says
	// so", never "the mode is off".
	ServerSideDiff bool
}

// Values is what a delivery manifest renders a chart WITH.
//
// A chart is not the unit of analysis - a release is - and half of a release
// is its values. Rendering with none makes idem report "could not be rendered"
// about charts whose `required` guards are working exactly as written.
type Values struct {
	Path string
	File string

	// Instance names the generator element this release came from - the file
	// or directory a git generator matched - and is empty for a plain
	// Application. One ApplicationSet is many releases and they are not
	// interchangeable, so a finding has to be able to say which.
	Instance string

	// Namespace is where this release deploys, resolved per element.
	Namespace string

	// Name is spec.source.helm.releaseName. .Release.Name is in the name of
	// nearly every object a chart produces, so getting it from the chart name
	// instead reports identities the cluster will never have.
	Name string

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
	found := c.ReleasesFor(chartPath)
	if len(found) != 1 {
		return Values{}
	}
	return found[0]
}

// ReleasesFor is every release of this chart the delivery config describes.
//
// More than one is the normal shape for an ApplicationSet: one release per
// generator element, each with its own values, namespace and release name.
// They are not a conflict to resolve - they are separate deployments, and
// idem's unit of analysis is a release rather than a chart.
func (c Config) ReleasesFor(chartPath string) []Values {
	var found []Values
	for _, v := range c.Values {
		if v.Path == "" || v.Path != chartPath {
			continue
		}
		found = append(found, v)
	}
	return found
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

// CreatesNamespace reports whether an Application claiming this chart sets
// CreateNamespace=true.
func (c Config) CreatesNamespace(chartPath string) bool {
	return c.destines(chartPath, func(d Destination) bool { return d.CreateNamespace })
}

// ServerSideDiff reports whether an Application claiming this chart is
// annotated for server-side diff. False means no manifest says so, which is not
// the same as the mode being off - see Destination.ServerSideDiff.
func (c Config) ServerSideDiff(chartPath string) bool {
	return c.destines(chartPath, func(d Destination) bool { return d.ServerSideDiff })
}

// destines reports whether any destination for this chart satisfies want.
//
// ANY rather than all, and deliberately unlike NamespaceFor, which refuses to
// answer when two manifests disagree. The difference is what a wrong answer
// costs: a namespace decides every object's identity, so guessing is worse than
// silence. These two only ever add a sentence explaining something the reader
// is already looking at, so the manifest that does set the option is the
// interesting one.
func (c Config) destines(chartPath string, want func(Destination) bool) bool {
	for _, d := range c.Destinations {
		// No Templated check, unlike NamespaceFor. Templated says the
		// path->namespace JOIN is unknowable, and neither fact below depends
		// on that join - the compare-options annotation is not templated at
		// all. Gating on it hid both from every per-cluster ApplicationSet.
		//
		// The empty-path skip IS kept, for the reason its two siblings keep
		// it: chartPaths yields "" for a manifest naming no path, and an empty
		// query must not match it.
		if d.Path != "" && d.Path == chartPath && want(d) {
			return true
		}
	}
	return false
}

// hasCompareOption reads one flag out of the comma-separated compare-options
// annotation.
func hasCompareOption(meta metadata, option string) bool {
	for opt := range strings.SplitSeq(meta.Annotations[compareOptions], ",") {
		if strings.TrimSpace(opt) == option {
			return true
		}
	}
	return false
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

		rules, dests, values, found := parse(root, body, rel)
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

// metadata is the subset idem reads. Annotations only: the compare-options
// annotation is where ArgoCD puts a per-application diff strategy, and it is
// NOT a syncOption, which is the easy thing to assume.
type metadata struct {
	Annotations map[string]string `yaml:"annotations"`
}

type appSpec struct {
	Destination       *destination       `yaml:"destination"`
	Source            *source            `yaml:"source"`
	Sources           []source           `yaml:"sources"`
	IgnoreDifferences []ignoreDifference `yaml:"ignoreDifferences"`
	SyncPolicy        *syncPolicy        `yaml:"syncPolicy"`
}

// gitGenerator is the only generator shape idem expands: its input is the
// repository, which idem already has. Every other generator reads state idem
// cannot see.
type gitGenerator struct {
	Files []struct {
		Path string `yaml:"path"`
	} `yaml:"files"`
	Directories []struct {
		Path    string `yaml:"path"`
		Exclude bool   `yaml:"exclude"`
	} `yaml:"directories"`
}

type document struct {
	Kind     string   `yaml:"kind"`
	Metadata metadata `yaml:"metadata"`
	Spec     struct {
		appSpec `yaml:",inline"`

		// GoTemplate selects Go text/template substitution. Without it ArgoCD
		// uses its own fasttemplate pass with different semantics, and
		// assuming they agree would make idem analyse a release ArgoCD never
		// generates.
		GoTemplate bool `yaml:"goTemplate"`

		Generators []struct {
			Git *gitGenerator `yaml:"git"`
		} `yaml:"generators"`

		// An ApplicationSet carries the same fields one level deeper. Reading
		// only spec.ignoreDifferences would silently miss every app an
		// ApplicationSet manages.
		Template *struct {
			Metadata metadata `yaml:"metadata"`
			Spec     appSpec  `yaml:"spec"`
		} `yaml:"template"`

		// ReleaseName and TargetNamespace are the Flux spellings of what
		// ArgoCD calls releaseName and destination.namespace.
		ReleaseName     string `yaml:"releaseName"`
		TargetNamespace string `yaml:"targetNamespace"`

		// Chart.Spec.Chart is a PATH inside the repository when the source is a
		// GitRepository, and a packaged chart name otherwise.
		Chart *struct {
			Spec struct {
				Chart     string `yaml:"chart"`
				SourceRef struct {
					Kind string `yaml:"kind"`
				} `yaml:"sourceRef"`
			} `yaml:"spec"`
		} `yaml:"chart"`

		// Values is spec.values, inline. ValuesFrom points at ConfigMaps and
		// Secrets that resolve in the cluster, which idem cannot read - it is
		// recorded as unresolved rather than rendered around.
		Values     map[string]any `yaml:"values"`
		ValuesFrom []struct {
			Kind string `yaml:"kind"`
			Name string `yaml:"name"`
		} `yaml:"valuesFrom"`

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
func parse(root string, body []byte, file string) ([]Rule, []Destination, []Values, []string) {
	var rules []Rule
	var dests []Destination
	var values []Values
	var engines []string

	decoder := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var doc document
		if err := decoder.Decode(&doc); err != nil {
			// A type error is confined to ONE document: the decoder has
			// consumed it and can be asked for the next. Breaking here
			// abandoned every later document in the file, and silently - the
			// suppression vanished, the namespace reverted to the default so
			// every round rendered into the wrong one, and --strict turned red
			// because of a typo in an unrelated Application.
			//
			// A syntax error does end the stream: after one there is no
			// dependable document boundary to resume from.
			var typeErr *yaml.TypeError
			if errors.As(err, &typeErr) {
				continue
			}
			break
		}

		switch doc.Kind {
		case "Application":
			engines = append(engines, "argocd")
			rules = append(rules, argoRules(doc.Spec.appSpec, file)...)
			dests = append(dests, argoDestinations(doc.Spec.appSpec, doc.Metadata, file)...)
			values = append(values, argoValues(doc.Spec.appSpec, file)...)
		case "ApplicationSet":
			engines = append(engines, "argocd")
			if doc.Spec.Template == nil {
				break
			}

			// Rules come from the template as written: a suppression is about
			// a shape, and every generated Application shares that shape.
			rules = append(rules, argoRules(doc.Spec.Template.Spec, file)...)

			// Values and namespaces are not shared - they are what differs per
			// element - so they come from expansion where idem can expand.
			elements, ok := repoElements(root, doc)
			if !ok {
				dests = append(dests, argoDestinations(doc.Spec.Template.Spec, doc.Spec.Template.Metadata, file)...)
				values = append(values, argoValues(doc.Spec.Template.Spec, file)...)
				break
			}
			for _, el := range elements {
				spec, resolved := resolveSpec(doc.Spec.Template.Spec, el, doc.Spec.GoTemplate)
				dests = append(dests, argoDestinations(spec, doc.Spec.Template.Metadata, file)...)
				for _, v := range argoValues(spec, file) {
					v.Instance = el.name
					v.Namespace = namespaceOf(spec)
					v.Templated = append(v.Templated, resolved...)
					values = append(values, v)
				}
			}
		case "HelmRelease":
			engines = append(engines, "flux")
			rules = append(rules, fluxRules(doc, file)...)
			values = append(values, fluxValues(doc, file)...)
			dests = append(dests, fluxDestinations(doc, file)...)
		}
	}
	return rules, dests, values, engines
}

// fluxChartPath is the repository path a HelmRelease renders, when there is one.
//
// Only for a GitRepository source. Flux resolves spec.chart.spec.chart against
// the source it names: for a GitRepository that is a path inside the repository
// idem is already reading, and for a HelmRepository or OCIRepository it is a
// packaged chart name that matches no local directory. Joining the second kind
// on name would attach one chart's values to a different chart that happens to
// share it - worse than saying nothing, which is what §9 requires.
//
// spec.chartRef (Flux 2.3+) points at a HelmChart or OCIRepository object
// elsewhere in the cluster and is deliberately not followed.
func fluxChartPath(doc document) string {
	if doc.Spec.Chart == nil || doc.Spec.Chart.Spec.SourceRef.Kind != "GitRepository" {
		return ""
	}
	return path.Clean(strings.TrimPrefix(doc.Spec.Chart.Spec.Chart, "./"))
}

// fluxValues reads what a HelmRelease renders its chart with.
func fluxValues(doc document, file string) []Values {
	chartPath := fluxChartPath(doc)
	if chartPath == "" {
		return nil
	}

	v := Values{
		Path:      chartPath,
		File:      file,
		Name:      doc.Spec.ReleaseName,
		Namespace: doc.Spec.TargetNamespace,
		Inline:    doc.Spec.Values,
	}
	// valuesFrom resolves in the cluster, at reconcile time. Recorded so the
	// report can say the release idem rendered is not the one Flux deploys,
	// rather than quietly rendering without them - docs/design.md §1.
	for _, from := range doc.Spec.ValuesFrom {
		v.Templated = append(v.Templated, "valuesFrom "+from.Kind+"/"+from.Name)
	}
	return []Values{v}
}

// fluxDestinations records the namespace a HelmRelease targets.
func fluxDestinations(doc document, file string) []Destination {
	chartPath := fluxChartPath(doc)
	if chartPath == "" || doc.Spec.TargetNamespace == "" {
		return nil
	}
	return []Destination{{
		Path:      chartPath,
		Namespace: doc.Spec.TargetNamespace,
		File:      file,
		Templated: templated(chartPath) || templated(doc.Spec.TargetNamespace),
	}}
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
			v.Name = src.Helm.ReleaseName
		}

		for _, f := range src.Helm.ValueFiles {
			if templated(f) {
				v.Templated = append(v.Templated, f)
				continue
			}
			// $ref/… names a file in ANOTHER source of a multi-source
			// Application - another repository, which idem is not looking at.
			// Joining it onto the chart path produced a path helm cannot open,
			// so the chart came back unrenderable and exit 2 - always fatal,
			// and it escapes the ratchet. It is an unresolvable value source,
			// not a broken chart.
			if ref, ok := crossSourceRef(f); ok {
				v.Templated = append(v.Templated, ref)
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

// element is one thing a generator produced: the data its templates see, and
// a name for the thing itself.
type element struct {
	name string

	// data is what a goTemplate ApplicationSet's templates see: nested, with
	// `path` as a map. flat is what a legacy one sees: dotted keys, string
	// values only. They are different shapes because ArgoCD renders them with
	// different engines, and collapsing them would make idem substitute where
	// ArgoCD would not.
	data map[string]any
	flat map[string]string
}

// repoElements expands the generators whose input is the repository.
//
// ok is false when idem will not expand this ApplicationSet at all - a
// generator that reads the cluster, an unsupported glob, or legacy
// substitution - which is different from expanding it to nothing. The caller
// reports the template unresolved rather than treating it as zero releases.
func repoElements(root string, doc document) ([]element, bool) {
	if len(doc.Spec.Generators) == 0 {
		return nil, false
	}

	var out []element
	for _, gen := range doc.Spec.Generators {
		if gen.Git == nil {
			// A generator idem cannot expand poisons the whole set: the
			// elements it would have produced are missing, so expanding the
			// rest would report a subset of the releases as if it were all.
			return nil, false
		}

		for _, f := range gen.Git.Files {
			matched, ok := matches(root, f.Path, false)
			if !ok {
				return nil, false
			}
			for _, rel := range matched {
				out = append(out, fileElement(root, rel))
			}
		}
		for _, d := range gen.Git.Directories {
			matched, ok := matches(root, d.Path, true)
			if !ok {
				return nil, false
			}
			for _, rel := range matched {
				if d.Exclude {
					continue
				}
				out = append(out, element{
					name: rel,
					data: pathData(rel, ""),
					flat: flatPathData(rel, ""),
				})
			}
		}
	}
	return out, true
}

// matches globs a generator path against the repository, returning
// repository-relative paths in lexical order.
//
// wantDir selects what a match may be: the files generator matches files and
// the directories generator matches directories, and a file that happens to
// match a directory pattern is not a release nobody declared.
//
// ArgoCD globs these with doublestar.FilepathGlob - verified in
// reposerver/repository/repository.go, whose comment says it is "consistent
// with AppSet generators" - so `**` matches zero or more path segments.
// filepath.Glob does not implement that at all, so a `**` pattern is walked
// instead: matching less than ArgoCD does would drop releases silently.
func matches(root, pattern string, wantDir bool) ([]string, bool) {
	segments := strings.Split(pattern, "/")
	for _, seg := range segments {
		if _, err := path.Match(seg, ""); err != nil && seg != "**" {
			// A pattern idem cannot even parse. Refused rather than expanded
			// to whatever it happens to match.
			return nil, false
		}
	}

	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if d.IsDir() != wantDir {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return nil
		}
		if matchPath(segments, strings.Split(filepath.ToSlash(rel), "/")) {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, false
	}

	slices.Sort(out)
	return out, true
}

// matchPath reports whether a path matches a pattern, segment by segment.
//
// `**` consumes zero or more segments - `a/**/b` matches `a/b` as well as
// `a/x/y/b`. Requiring at least one is the classic way to get this subtly
// wrong and drop a release without saying so.
func matchPath(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}

	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchPath(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}

	if len(name) == 0 {
		return false
	}
	// path.Match never matches across a separator, which is what makes this
	// segment-by-segment walk equivalent to globbing the whole path.
	ok, err := path.Match(pattern[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchPath(pattern[1:], name[1:])
}

// fileElement is one matched file: its parsed contents, plus the path metadata
// the generator injects over them. Injected last, which is ArgoCD's own
// precedence - a `path` key in the file cannot shadow it.
func fileElement(root, rel string) element {
	data := map[string]any{}
	if body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
		var parsed map[string]any
		if yaml.Unmarshal(body, &parsed) == nil {
			data = parsed
		}
	}

	maps.Copy(data, pathData(path.Dir(rel), path.Base(rel)))

	// The legacy view is the same file flattened, with the path keys written
	// over it last - ArgoCD's own order, so a `path` key in the file cannot
	// shadow the generator's.
	flat := flatten(data)
	delete(flat, "path")
	maps.Copy(flat, flatPathData(path.Dir(rel), path.Base(rel)))

	return element{name: rel, data: data, flat: flat}
}

// flatten renders a parsed file the way flatten.DotStyle does, keeping only
// the string leaves.
//
// Not a simplification: ArgoCD's legacy pass asserts replaceMap[tag].(string)
// and writes the tag back verbatim when that fails, so a number or a bool is
// never substituted there either.
func flatten(in map[string]any) map[string]string {
	out := map[string]string{}

	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, val := range v {
				walk(join(prefix, key), val)
			}
		case []any:
			for i, val := range v {
				walk(join(prefix, strconv.Itoa(i)), val)
			}
		case string:
			out[prefix] = v
		}
	}
	walk("", in)

	return out
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// flatPathData is the generator's path metadata as legacy dotted keys.
//
// The directories generator sets no filename - there is no file - and idem
// does not invent one, so a template using it stays unresolved exactly as it
// would under ArgoCD.
func flatPathData(dir, filename string) map[string]string {
	out := map[string]string{
		"path":                    dir,
		"path.basename":           path.Base(dir),
		"path.basenameNormalized": sanitiseName(path.Base(dir)),
	}
	if filename != "" {
		out["path.filename"] = filename
		out["path.filenameNormalized"] = sanitiseName(filename)
	}
	for i, segment := range strings.Split(dir, "/") {
		out["path["+strconv.Itoa(i)+"]"] = segment
	}
	return out
}

// invalidDNSNameChars and maxDNSNameLength mirror argo-cd's utils.SanitizeName.
var invalidDNSNameChars = regexp.MustCompile("[^-a-z0-9.]")

const maxDNSNameLength = 253

// sanitiseName is argo-cd's SanitizeName, which is what feeds the
// `basenameNormalized` and `filenameNormalized` parameters.
func sanitiseName(name string) string {
	name = strings.ToLower(name)
	name = invalidDNSNameChars.ReplaceAllString(name, "-")
	if len(name) > maxDNSNameLength {
		name = name[:maxDNSNameLength]
	}
	return strings.Trim(name, "-.")
}

// pathData is the generator's own metadata for a matched path.
func pathData(dir, filename string) map[string]any {
	meta := map[string]any{
		"path":               dir,
		"basename":           path.Base(dir),
		"basenameNormalized": sanitiseName(path.Base(dir)),
		"segments":           strings.Split(dir, "/"),
	}
	if filename != "" {
		meta["filename"] = filename
		meta["filenameNormalized"] = sanitiseName(filename)
	}
	return map[string]any{"path": meta}
}

// resolveSpec substitutes an element into the fields that decide what gets
// rendered, returning the resolved spec and the keys that would not resolve.
//
// Only these fields: idem is not reimplementing ArgoCD's controller, it is
// working out what one release renders with.
func resolveSpec(spec appSpec, el element, goTemplate bool) (appSpec, []string) {
	var unresolved []string
	sub := func(in string) string {
		out, ok := substitute(in, el, goTemplate)
		if !ok {
			unresolved = append(unresolved, in)
			return in
		}
		return out
	}

	if spec.Destination != nil {
		dest := *spec.Destination
		dest.Namespace = sub(dest.Namespace)
		spec.Destination = &dest
	}
	spec.Source = resolveSource(spec.Source, sub)
	sources := make([]source, 0, len(spec.Sources))
	for _, src := range spec.Sources {
		sources = append(sources, *resolveSource(&src, sub))
	}
	spec.Sources = sources

	return spec, unresolved
}

func resolveSource(src *source, sub func(string) string) *source {
	if src == nil {
		return nil
	}

	out := *src
	out.Path = sub(out.Path)
	if out.Helm == nil {
		return &out
	}

	helm := *out.Helm
	helm.ReleaseName = sub(helm.ReleaseName)
	helm.Values = sub(helm.Values)

	files := make([]string, 0, len(helm.ValueFiles))
	for _, f := range helm.ValueFiles {
		files = append(files, sub(f))
	}
	helm.ValueFiles = files

	object := make(map[string]any, len(helm.ValuesObject))
	for key, val := range helm.ValuesObject {
		if text, ok := val.(string); ok {
			object[key] = sub(text)
			continue
		}
		object[key] = val
	}
	helm.ValuesObject = object

	for i, p := range helm.Parameters {
		helm.Parameters[i].Value = sub(p.Value)
	}

	out.Helm = &helm
	return &out
}

// substitute renders one templated string against an element.
//
// missingkey=error, so a key the element does not carry fails loudly here
// rather than rendering as "<no value>" and being handed to helm as if the
// repository had said it.
func substitute(in string, el element, goTemplate bool) (string, bool) {
	if !templated(in) {
		return in, true
	}
	if !goTemplate {
		return substituteLegacy(in, el.flat)
	}

	tmpl, err := template.New("").Option("missingkey=error").Parse(in)
	if err != nil {
		return in, false
	}

	var out strings.Builder
	if err := tmpl.Execute(&out, el.data); err != nil {
		return in, false
	}
	if templated(out.String()) {
		return in, false
	}
	return out.String(), true
}

// substituteLegacy is ArgoCD's fasttemplate pass: `{{` and `}}`, the tag
// trimmed, and the tag written back verbatim when it names nothing.
//
// Written back rather than emptied, which is what argo-cd does - and it is
// what lets idem notice: a result still carrying `{{` is one idem refuses,
// rather than a value it invented from an absent key.
func substituteLegacy(in string, flat map[string]string) (string, bool) {
	var out strings.Builder
	rest := in

	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			break
		}
		close := strings.Index(rest[open:], "}}")
		if close < 0 {
			break
		}
		close += open

		tag := strings.TrimSpace(rest[open+2 : close])
		out.WriteString(rest[:open])
		if value, ok := flat[tag]; ok && tag != "" {
			out.WriteString(value)
		} else {
			out.WriteString(rest[open : close+2])
		}
		rest = rest[close+2:]
	}
	out.WriteString(rest)

	if templated(out.String()) {
		return in, false
	}
	return out.String(), true
}

// namespaceOf is the destination namespace a resolved template names.
func namespaceOf(spec appSpec) string {
	if spec.Destination == nil || templated(spec.Destination.Namespace) {
		return ""
	}
	return spec.Destination.Namespace
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

// crossSourceRef reports whether a valueFiles entry points into another source.
//
// ArgoCD's multi-source syntax is `$<ref>/path/within/that/repo`, where <ref>
// matches a source declaring `ref: <name>`. That source is a different
// repository by construction - a single-repo Application has no reason to use
// it - so idem cannot read the file and does not pretend to.
//
// Returned as a name rather than resolved: the report says the release it
// rendered is not the one ArgoCD deploys, which is true and useful, where a
// guess would be neither.
func crossSourceRef(file string) (string, bool) {
	if !strings.HasPrefix(file, "$") {
		return "", false
	}
	name, rest, _ := strings.Cut(strings.TrimPrefix(file, "$"), "/")
	if name == "" {
		return "", false
	}
	return "$" + name + " (" + rest + ", from another source)", true
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
func argoDestinations(spec appSpec, meta metadata, file string) []Destination {
	var namespace string
	if spec.Destination != nil {
		namespace = spec.Destination.Namespace
	}
	createNamespace := spec.SyncPolicy != nil && slices.Contains(spec.SyncPolicy.SyncOptions, createNamespaceOption)
	serverSideDiff := hasCompareOption(meta, serverSideDiffOption)

	// A Destination records what the Application claiming this chart says about
	// it. The namespace is ONE of those things, not the reason the record
	// exists - and spec.destination.namespace is optional in ArgoCD, so gating
	// on it made CreateNamespace=true with no explicit namespace, which is a
	// perfectly ordinary Application, invisible to both accessors below.
	//
	// A namespace-less Destination is safe for NamespaceFor: it already skips
	// any destination whose Namespace is empty.
	if namespace == "" && !createNamespace && !serverSideDiff {
		return nil
	}

	var out []Destination
	for _, path := range chartPaths(spec) {
		out = append(out, Destination{
			Path:      path,
			Namespace: namespace,
			File:      file,
			// Either half can be templated, and either makes the join useless.
			Templated: strings.Contains(path, "{{") || strings.Contains(namespace, "{{"),

			CreateNamespace: createNamespace,
			ServerSideDiff:  serverSideDiff,
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
