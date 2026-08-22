package delivery

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, dir string) Config {
	t.Helper()
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return got
}

const application = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: home-app
spec:
  source:
    repoURL: https://example.com/repo.git
    path: charts/home
  syncPolicy:
    automated:
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - RespectIgnoreDifferences=true
  ignoreDifferences:
    - group: ""
      kind: Secret
      name: home-creds
      jsonPointers:
        - /data/WEBUI_SECRET_KEY
`

const applicationSet = `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: platform
spec:
  generators:
    - clusters: {}
  template:
    spec:
      source:
        path: charts/platform/tekton-bootstrap
      syncPolicy:
        automated:
          selfHeal: true
        syncOptions:
          - ServerSideApply=true
      ignoreDifferences:
        - group: apiextensions.k8s.io
          kind: CustomResourceDefinition
          jsonPointers:
            - /spec/preserveUnknownFields
`

func TestLoadReadsAnApplication(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", application)

	got := load(t, dir)

	if len(got.Rules) != 1 {
		t.Fatalf("Load() found %d rules, want 1: %+v", len(got.Rules), got.Rules)
	}
	r := got.Rules[0]
	if r.Path != "charts/home" {
		t.Errorf("Path = %q, want charts/home", r.Path)
	}
	if r.Kind != "Secret" || r.Name != "home-creds" {
		t.Errorf("rule targets %s/%s, want Secret/home-creds", r.Kind, r.Name)
	}
	if !slices.Contains(r.Pointers, "/data/WEBUI_SECRET_KEY") {
		t.Errorf("Pointers = %v, want the jsonPointer", r.Pointers)
	}
	if r.Engine != "argocd" {
		t.Errorf("Engine = %q, want argocd", r.Engine)
	}
}

func TestLoadReadsSelfHealAndRespectIgnoreDifferences(t *testing.T) {
	// Both are needed to tell a working suppression from one selfHeal will
	// silently undo.
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", application)

	r := load(t, dir).Rules[0]

	if !r.SelfHeal {
		t.Error("SelfHeal = false, want true")
	}
	if !r.Respected {
		t.Error("Respected = false, want true - RespectIgnoreDifferences=true is set")
	}
}

func TestLoadNoticesASuppressionSelfHealWillUndo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "appsets/platform.yaml", applicationSet)

	r := load(t, dir).Rules[0]

	if !r.SelfHeal {
		t.Error("SelfHeal = false, want true")
	}
	if r.Respected {
		t.Error("Respected = true, want false - only ServerSideApply=true is set, which is a different option entirely")
	}
}

func TestLoadReadsAnApplicationSetNestedUnderTemplate(t *testing.T) {
	// An ApplicationSet carries the same fields one level deeper. Reading only
	// spec.ignoreDifferences would silently miss every appset-managed app.
	dir := t.TempDir()
	write(t, dir, "appsets/platform.yaml", applicationSet)

	got := load(t, dir)

	if len(got.Rules) != 1 {
		t.Fatalf("Load() found %d rules, want 1: %+v", len(got.Rules), got.Rules)
	}
	if got.Rules[0].Path != "charts/platform/tekton-bootstrap" {
		t.Errorf("Path = %q, want the templated spec's path", got.Rules[0].Path)
	}
}

func TestLoadMarksATemplatedPathAsUnjoinable(t *testing.T) {
	// A generator-substituted path cannot be tied to a chart directory. It is
	// recorded as such rather than guessed at, and must never suppress.
	dir := t.TempDir()
	write(t, dir, "appsets/x.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: x}
spec:
  template:
    spec:
      source:
        path: "charts/{{ .name }}"
      ignoreDifferences:
        - group: ""
          kind: Secret
          jsonPointers: [/data/k]
`)

	r := load(t, dir).Rules[0]

	if !r.Templated {
		t.Error("Templated = false, want true for a path holding {{ }}")
	}
}

func TestLoadReadsTheSourcesArray(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/multi.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: multi}
spec:
  sources:
    - repoURL: https://example.com/a.git
      path: charts/first
    - repoURL: https://example.com/b.git
      path: charts/second
  ignoreDifferences:
    - group: ""
      kind: Secret
      jsonPointers: [/data/k]
`)

	got := load(t, dir)

	var paths []string
	for _, r := range got.Rules {
		paths = append(paths, r.Path)
	}
	for _, want := range []string{"charts/first", "charts/second"} {
		if !slices.Contains(paths, want) {
			t.Errorf("paths = %v, want %q", paths, want)
		}
	}
}

func TestLoadReadsAHelmRelease(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "flux/release.yaml", `
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: podinfo}
spec:
  driftDetection:
    mode: enabled
    ignore:
      - paths: ["/spec/replicas"]
`)

	got := load(t, dir)

	if !slices.Contains(got.Engines, "flux") {
		t.Errorf("Engines = %v, want flux detected", got.Engines)
	}
	if len(got.Rules) != 1 || !slices.Contains(got.Rules[0].Pointers, "/spec/replicas") {
		t.Errorf("Rules = %+v, want the driftDetection ignore path", got.Rules)
	}
}

func TestLoadDetectsTheEnginesInUse(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", application)

	got := load(t, dir)

	if !slices.Contains(got.Engines, "argocd") {
		t.Errorf("Engines = %v, want argocd", got.Engines)
	}
	if slices.Contains(got.Engines, "flux") {
		t.Errorf("Engines = %v, want no flux - there is no HelmRelease here", got.Engines)
	}
}

func TestLoadSkipsChartTemplatesAndUnrelatedYAML(t *testing.T) {
	// A chart template is Go template source, not YAML, and will not parse.
	// Neither a parse failure nor an unrelated document may derail the scan.
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", application)
	write(t, dir, "charts/home/templates/deploy.yaml", "kind: Deployment\nmetadata:\n  name: {{ .Release.Name }}\n")
	write(t, dir, "charts/home/values.yaml", "replicas: 2\n")
	write(t, dir, "notes.txt", "kind: Application\n")

	got := load(t, dir)

	if len(got.Rules) != 1 {
		t.Errorf("Load() found %d rules, want 1: %+v", len(got.Rules), got.Rules)
	}
}

func TestLoadReadsEveryDocumentOfAMultiDocFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/all.yaml", "kind: ConfigMap\nmetadata: {name: unrelated}\n---"+application)

	if n := len(load(t, dir).Rules); n != 1 {
		t.Errorf("Load() found %d rules, want 1", n)
	}
}

func TestLoadOnATreeWithNoDeliveryConfigIsNotAnError(t *testing.T) {
	// Plenty of estates split an app repo from a config repo. Finding nothing
	// is the normal case, not a failure.
	dir := t.TempDir()
	write(t, dir, "charts/home/Chart.yaml", "apiVersion: v2\nname: home\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(got.Rules) != 0 || len(got.Engines) != 0 {
		t.Errorf("Load() = %+v, want empty", got)
	}
}

func TestLoadNamesTheFilesItRead(t *testing.T) {
	// idem says what it checked. A suppression that changes the verdict must
	// be traceable to the manifest that caused it.
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", application)

	got := load(t, dir)

	if len(got.Files) != 1 || !strings.Contains(got.Files[0], "home.yaml") {
		t.Errorf("Files = %v, want the manifest named", got.Files)
	}
	if !strings.Contains(got.Rules[0].File, "home.yaml") {
		t.Errorf("Rule.File = %q, want the manifest named", got.Rules[0].File)
	}
}

func TestRootWalksUpToTheEnclosingRepository(t *testing.T) {
	// Charts and delivery manifests routinely live in sibling directories, so
	// scanning only the directory idem was pointed at finds nothing.
	repo := t.TempDir()
	write(t, repo, ".git/HEAD", "ref: refs/heads/main\n")
	write(t, repo, "charts/home/Chart.yaml", "apiVersion: v2\nname: home\n")

	if got := Root(filepath.Join(repo, "charts", "home")); got != repo {
		t.Errorf("Root() = %q, want %q", got, repo)
	}
}

func TestRootFallsBackToTheGivenPathOutsideARepository(t *testing.T) {
	dir := t.TempDir()

	if got := Root(dir); got != dir {
		t.Errorf("Root() = %q, want %q", got, dir)
	}
}

// --- matching ---

func rule(group, kind, name string, pointers ...string) Rule {
	return Rule{Group: group, Kind: kind, Name: name, Pointers: pointers, Path: "charts/home", Engine: "argocd"}
}

func secret(name string) ObjectRef { return ObjectRef{Kind: "Secret", Name: name} }

func TestForReturnsOnlyRulesForThatChart(t *testing.T) {
	cfg := Config{Rules: []Rule{
		{Path: "charts/home", Kind: "Secret"},
		{Path: "charts/lab", Kind: "Secret"},
	}}

	got := cfg.For("charts/home")

	if len(got) != 1 || got[0].Path != "charts/home" {
		t.Errorf("For() = %+v, want only the home rule", got)
	}
}

func TestForRefusesATemplatedPathEvenAskedForVerbatim(t *testing.T) {
	// A generator substitutes it at runtime, so idem cannot know which chart it
	// governs. Asking for the unsubstituted string still must not match, or the
	// refusal is only an accident of the two strings differing.
	cfg := Config{Rules: []Rule{{Path: "charts/{{ .name }}", Templated: true, Kind: "Secret"}}}

	if got := cfg.For("charts/{{ .name }}"); len(got) != 0 {
		t.Errorf("For() = %+v, want nothing - the path is unresolved", got)
	}
}

func TestForMatchesNothingWhenThereIsNoChartPathToJoinOn(t *testing.T) {
	// An Application pointing at a remote chart names no source.path. If the
	// caller also cannot place the chart, both sides are "" - and a bare
	// equality check would then let that rule suppress findings for a chart it
	// has nothing to do with.
	cfg := Config{Rules: []Rule{{Path: "", Kind: "Secret", Pointers: []string{"/data/k"}}}}

	if got := cfg.For(""); len(got) != 0 {
		t.Errorf("For() = %+v, want nothing - an empty path joins to nothing", got)
	}
}

func TestCoversAnExactPointer(t *testing.T) {
	r := rule("", "Secret", "creds", "/data/password")

	if !r.Covers(secret("creds"), []string{"/data/password"}) {
		t.Error("Covers() = false, want true")
	}
}

func TestCoversAChildOfTheRulesPointer(t *testing.T) {
	// Removing /data removes /data/password with it.
	r := rule("", "Secret", "creds", "/data")

	if !r.Covers(secret("creds"), []string{"/data/password"}) {
		t.Error("Covers() = false, want true - a parent pointer covers its children")
	}
}

func TestDoesNotCoverAPointerThatMerelySharesAPrefix(t *testing.T) {
	// /data must not be read as covering /database.
	r := rule("", "Secret", "creds", "/data")

	if r.Covers(secret("creds"), []string{"/database/password"}) {
		t.Error("Covers() = true, want false")
	}
}

func TestADataRuleCoversAStringDataFinding(t *testing.T) {
	// The case that makes the pointer normalisation a prerequisite. A chart
	// renders stringData; the user's working rule says /data. Without both
	// forms these never match and idem re-reports a finding already handled.
	r := rule("", "Secret", "creds", "/data/WEBUI_SECRET_KEY")

	pointers := []string{"/data/WEBUI_SECRET_KEY", "/stringData/WEBUI_SECRET_KEY"}
	if !r.Covers(secret("creds"), pointers) {
		t.Error("Covers() = false, want true")
	}
}

func TestARuleWithNoNameCoversAnyObjectOfThatKind(t *testing.T) {
	// Real rules routinely omit name - one entry covering every CRD in an app.
	r := Rule{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition", Pointers: []string{"/spec/preserveUnknownFields"}}

	ref := ObjectRef{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition", Name: "anything.example.com"}
	if !r.Covers(ref, []string{"/spec/preserveUnknownFields"}) {
		t.Error("Covers() = false, want true - an absent name selects any")
	}
}

func TestARuleWithNoGroupSelectsOnlyCoreObjects(t *testing.T) {
	// ArgoCD matches group as a plain value, so an omitted group is "" and
	// does NOT mean "any group". This is why `group: core` is a documented way
	// to write a rule that silently matches nothing.
	r := Rule{Kind: "Deployment", Pointers: []string{"/spec/replicas"}}

	if r.Covers(ObjectRef{Group: "apps", Kind: "Deployment", Name: "api"}, []string{"/spec/replicas"}) {
		t.Error("Covers() = true, want false - an apps/v1 Deployment is not in the core group")
	}
	if !r.Covers(ObjectRef{Kind: "Deployment", Name: "api"}, []string{"/spec/replicas"}) {
		t.Error("Covers() = false, want true for a core-group object")
	}
}

func TestADifferentKindIsNotCovered(t *testing.T) {
	r := rule("", "Secret", "", "/data/password")

	if r.Covers(ObjectRef{Kind: "ConfigMap", Name: "creds"}, []string{"/data/password"}) {
		t.Error("Covers() = true, want false")
	}
}

func TestANamespacedRuleOnlyCoversThatNamespace(t *testing.T) {
	r := Rule{Kind: "Secret", Namespace: "prod", Name: "creds", Pointers: []string{"/data/k"}}

	if r.Covers(ObjectRef{Kind: "Secret", Namespace: "staging", Name: "creds"}, []string{"/data/k"}) {
		t.Error("Covers() = true, want false - prod and staging are different objects")
	}
}

func TestAJQRuleNeverCoversButIsReportedAsMaybe(t *testing.T) {
	// idem will not vendor a jq engine, so it says "may already be covered"
	// rather than assuming either way.
	r := Rule{Kind: "Secret", Name: "creds", JQ: []string{".data.password"}, Path: "charts/home"}

	if r.Covers(secret("creds"), []string{"/data/password"}) {
		t.Error("Covers() = true, want false - idem cannot evaluate jq")
	}
	if !r.MayCover(secret("creds")) {
		t.Error("MayCover() = false, want true")
	}
}

func TestAJQRuleDoesNotEvenMaybeCoverAnUnrelatedObject(t *testing.T) {
	r := Rule{Kind: "Secret", Name: "creds", JQ: []string{".data.password"}}

	if r.MayCover(ObjectRef{Kind: "ConfigMap", Name: "other"}) {
		t.Error("MayCover() = true, want false")
	}
}

// The namespace a chart's objects land in is a fact the Application states.
// Without reading it, idem renders with whatever the local kube context points
// at - which differs between a laptop and CI, changes the displayed identity
// of every object, and can make a namespaced ignoreDifferences rule match in
// one place and silently not in the other.

const withDestination = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: home-app
spec:
  destination:
    name: in-cluster
    namespace: home
  source:
    path: charts/home
`

func TestTheNamespaceComesFromTheApplicationThatDeploysTheChart(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withDestination)

	ns, file := load(t, dir).NamespaceFor("charts/home")

	if ns != "home" {
		t.Errorf("NamespaceFor() = %q, want home", ns)
	}
	if file != "apps/home.yaml" {
		t.Errorf("file = %q, want the manifest it came from", file)
	}
}

func TestAChartNoManifestClaimsHasNoNamespace(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withDestination)

	if ns, _ := load(t, dir).NamespaceFor("charts/other"); ns != "" {
		t.Errorf("NamespaceFor() = %q, want empty - nothing claims that chart", ns)
	}
}

func TestATemplatedDestinationNamespaceIsNotGuessedAt(t *testing.T) {
	// An ApplicationSet substitutes this per generated Application. There is
	// no single answer, and inventing one would render every object into a
	// namespace no Application ever names.
	dir := t.TempDir()
	write(t, dir, "apps/set.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: platform
spec:
  template:
    spec:
      destination:
        namespace: '{{ .name }}-system'
      source:
        path: charts/platform
`)

	if ns, _ := load(t, dir).NamespaceFor("charts/platform"); ns != "" {
		t.Errorf("NamespaceFor() = %q, want empty for a templated namespace", ns)
	}
}

func TestAnApplicationSetsDestinationIsReadThroughItsTemplate(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/set.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: platform
spec:
  template:
    spec:
      destination:
        namespace: tekton-pipelines
      source:
        path: charts/platform/tekton-bootstrap
`)

	if ns, _ := load(t, dir).NamespaceFor("charts/platform/tekton-bootstrap"); ns != "tekton-pipelines" {
		t.Errorf("NamespaceFor() = %q, want tekton-pipelines", ns)
	}
}

func TestTwoApplicationsDisagreeingAboutTheNamespaceIsUnknown(t *testing.T) {
	// Two Applications claiming one chart path is a real shape - the same
	// chart deployed to staging and production. idem has no Application of its
	// own to pick between them, so it picks neither.
	dir := t.TempDir()
	write(t, dir, "apps/staging.yaml", withDestination)
	write(t, dir, "apps/prod.yaml", strings.Replace(withDestination, "namespace: home", "namespace: home-prod", 1))

	if ns, _ := load(t, dir).NamespaceFor("charts/home"); ns != "" {
		t.Errorf("NamespaceFor() = %q, want empty - two manifests disagree", ns)
	}
}

func TestTwoApplicationsAgreeingAboutTheNamespaceIsNotAConflict(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/one.yaml", withDestination)
	write(t, dir, "apps/two.yaml", withDestination)

	if ns, _ := load(t, dir).NamespaceFor("charts/home"); ns != "home" {
		t.Errorf("NamespaceFor() = %q, want home", ns)
	}
}

func TestAHelmReleaseNamesNoChartPathToAttachANamespaceTo(t *testing.T) {
	// The chart reaches Flux through a separate source object, so there is
	// nothing to join a namespace to. Stated, not silently half-supported.
	dir := t.TempDir()
	write(t, dir, "releases/home.yaml", `
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: home, namespace: home}
spec:
  targetNamespace: home
  driftDetection: {mode: enabled}
`)

	if ns, _ := load(t, dir).NamespaceFor("charts/home"); ns != "" {
		t.Errorf("NamespaceFor() = %q, want empty", ns)
	}
}

// What a chart is rendered WITH is as much a fact of the delivery config as
// where it is deployed. Rendering a chart with no values at all makes idem
// report "could not be rendered" about charts that render perfectly well for
// the Application that owns them - and their `required` guards say exactly
// that, which is the chart working, not failing.

const withHelmValues = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: home-app}
spec:
  destination: {namespace: home}
  source:
    path: charts/home
    helm:
      releaseName: home-release
      valueFiles:
        - values-prod.yaml
        - /shared/base.yaml
      valuesObject:
        cluster: truenas
        replicas: 3
      parameters:
        - name: image.tag
          value: v1.2.3
`

func TestTheReleaseNameComesFromTheApplication(t *testing.T) {
	// ArgoCD renders with spec.source.helm.releaseName, and .Release.Name is
	// in the name of nearly every object a chart produces.
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withHelmValues)

	if got := load(t, dir).ValuesFor("charts/home").Release; got != "home-release" {
		t.Errorf("Release = %q, want home-release", got)
	}
}

func TestValueFilesAreReadInOrder(t *testing.T) {
	// Later files win in helm, so order is semantic, not cosmetic.
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withHelmValues)

	got := load(t, dir).ValuesFor("charts/home").ValueFiles
	want := []string{"charts/home/values-prod.yaml", "shared/base.yaml"}

	if !slices.Equal(got, want) {
		t.Errorf("ValueFiles = %v, want %v - a leading slash is repo-root relative, otherwise source-relative", got, want)
	}
}

func TestParametersBecomeSetArguments(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withHelmValues)

	got := load(t, dir).ValuesFor("charts/home").Sets

	if !slices.Contains(got, "image.tag=v1.2.3") {
		t.Errorf("Sets = %v, want the parameter", got)
	}
}

func TestValuesObjectIsCarriedAsAMap(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withHelmValues)

	got := load(t, dir).ValuesFor("charts/home").Inline

	if got["cluster"] != "truenas" {
		t.Errorf("Inline[cluster] = %v, want truenas", got["cluster"])
	}
	if got["replicas"] != 3 {
		t.Errorf("Inline[replicas] = %v, want 3 - the type has to survive", got["replicas"])
	}
}

func TestTheValuesStringIsParsedLikeAValuesFile(t *testing.T) {
	// spec.source.helm.values is raw YAML in a string. ArgoCD parses it; so
	// must idem, or a nested key arrives as one flat string.
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: home-app}
spec:
  source:
    path: charts/home
    helm:
      values: |
        image:
          tag: v9
`)

	got := load(t, dir).ValuesFor("charts/home").Inline
	image, ok := got["image"].(map[string]any)

	if !ok || image["tag"] != "v9" {
		t.Errorf("Inline = %#v, want the YAML parsed into a nested map", got)
	}
}

func TestATemplatedValueIsRecordedRatherThanGuessed(t *testing.T) {
	// A generator substitutes this per generated Application. idem has no
	// generator, so it neither invents a value nor pretends the key is absent.
	dir := t.TempDir()
	write(t, dir, "apps/set.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: flux}
spec:
  template:
    spec:
      source:
        path: charts/platform/flux-bootstrap
        helm:
          valuesObject:
            cluster: '{{ .name }}'
          parameters:
            - name: webRoute.enabled
              value: '{{ .enabled }}'
`)

	got := load(t, dir).ValuesFor("charts/platform/flux-bootstrap")

	if len(got.Inline) != 0 || len(got.Sets) != 0 {
		t.Errorf("Inline = %v, Sets = %v, want neither guessed at", got.Inline, got.Sets)
	}
	if !slices.Contains(got.Templated, "cluster") || !slices.Contains(got.Templated, "webRoute.enabled") {
		t.Errorf("Templated = %v, want both keys recorded as unresolvable", got.Templated)
	}
}
