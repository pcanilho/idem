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
        cluster: prod-a
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

	if got := load(t, dir).ValuesFor("charts/home").Name; got != "home-release" {
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

	if got["cluster"] != "prod-a" {
		t.Errorf("Inline[cluster] = %v, want prod-a", got["cluster"])
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
        path: charts/platform/agent
        helm:
          valuesObject:
            cluster: '{{ .name }}'
          parameters:
            - name: webRoute.enabled
              value: '{{ .enabled }}'
`)

	got := load(t, dir).ValuesFor("charts/platform/agent")

	if len(got.Inline) != 0 || len(got.Sets) != 0 {
		t.Errorf("Inline = %v, Sets = %v, want neither guessed at", got.Inline, got.Sets)
	}
	if !slices.Contains(got.Templated, "cluster") || !slices.Contains(got.Templated, "webRoute.enabled") {
		t.Errorf("Templated = %v, want both keys recorded as unresolvable", got.Templated)
	}
}

// An ApplicationSet template is not a release — it is a template for many, one
// per generator element. idem expands the generators whose input is the
// repository, because it has the repository; every other generator reads state
// idem cannot see, and those releases are reported as unconstructible rather
// than invented.

const filesGenerator = `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: tenants}
spec:
  goTemplate: true
  generators:
    - git:
        repoURL: https://example.com/repo.git
        files:
          - path: "config/tenants/*.yaml"
  template:
    spec:
      destination:
        namespace: '{{ .tenant }}-system'
      source:
        path: charts/app
        helm:
          releaseName: '{{ .tenant }}'
          valueFiles:
            - '/{{ .path.path }}/{{ .path.filename }}'
`

func TestAGitFilesGeneratorIsOneReleasePerMatchedFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", filesGenerator)
	write(t, dir, "config/tenants/alpha.yaml", "tenant: alpha\n")
	write(t, dir, "config/tenants/beta.yaml", "tenant: beta\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 2 {
		t.Fatalf("ReleasesFor() returned %d releases, want one per matched file: %+v", len(got), got)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"alpha", "beta"}) {
		t.Errorf("release names = %v, want each element's own value", names)
	}
}

func TestTheMatchedFileIsItselfAValuesFile(t *testing.T) {
	// `.path.path` and `.path.filename` are the generator's metadata for the
	// file it matched. Resolving them turns a templated valueFiles entry into
	// a path that exists.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", filesGenerator)
	write(t, dir, "config/tenants/alpha.yaml", "tenant: alpha\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want one release", got)
	}
	if !slices.Contains(got[0].ValueFiles, "config/tenants/alpha.yaml") {
		t.Errorf("ValueFiles = %v, want the matched file", got[0].ValueFiles)
	}
	if got[0].Namespace != "alpha-system" {
		t.Errorf("Namespace = %q, want alpha-system - resolved from the element", got[0].Namespace)
	}
}

func TestEachReleaseNamesTheElementItCameFrom(t *testing.T) {
	// Two releases of one chart are not a conflict to resolve; they are
	// separate deployments, and a finding has to say which one it is about.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", filesGenerator)
	write(t, dir, "config/tenants/alpha.yaml", "tenant: alpha\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 || got[0].Instance != "config/tenants/alpha.yaml" {
		t.Errorf("Instance = %+v, want the element identified", got)
	}
}

func TestAGitDirectoriesGeneratorExpandsToo(t *testing.T) {
	// The other generator whose input is the repository.
	dir := t.TempDir()
	write(t, dir, "apps/addons.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: addons}
spec:
  goTemplate: true
  generators:
    - git:
        repoURL: https://example.com/repo.git
        directories:
          - path: "addons/*"
  template:
    spec:
      source:
        path: charts/app
        helm:
          releaseName: '{{ .path.basename }}'
`)
	write(t, dir, "addons/ingress/values.yaml", "x: 1\n")
	write(t, dir, "addons/metrics/values.yaml", "x: 1\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 2 {
		t.Fatalf("ReleasesFor() returned %d, want one per directory: %+v", len(got), got)
	}
}

func TestAGeneratorReadingTheClusterIsNotExpanded(t *testing.T) {
	// A clusters generator enumerates registered clusters, which live in the
	// cluster. idem records the values it could not supply and invents none.
	dir := t.TempDir()
	write(t, dir, "apps/platform.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: platform}
spec:
  goTemplate: true
  generators:
    - clusters:
        selector:
          matchLabels: {platform: "true"}
  template:
    spec:
      source:
        path: charts/agent
        helm:
          valuesObject:
            cluster: '{{ .name }}'
`)

	got := load(t, dir).ReleasesFor("charts/agent")

	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want the release still reported", got)
	}
	if len(got[0].Inline) != 0 {
		t.Errorf("Inline = %v, want no invented value", got[0].Inline)
	}
	if !slices.Contains(got[0].Templated, "cluster") {
		t.Errorf("Templated = %v, want the key idem could not supply", got[0].Templated)
	}
}

func TestAGeneratorThatMatchesNothingProducesNoRelease(t *testing.T) {
	// Not an error, and emphatically not one release with the template strings
	// left unexpanded.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", filesGenerator)

	if got := load(t, dir).ReleasesFor("charts/app"); len(got) != 0 {
		t.Errorf("ReleasesFor() = %+v, want nothing to expand", got)
	}
}

// Without goTemplate, ArgoCD substitutes with fasttemplate over a FLAT map of
// dotted keys, not Go templates. Verified against argo-cd master:
// applicationset/utils/utils.go builds the template with "{{" and "}}", trims
// the tag, and writes `{{tag}}` back VERBATIM when the key is absent or its
// value is not a string; applicationset/generators/git.go flattens the matched
// file with flatten.DotStyle and adds the path keys below.
//
// goTemplate: false is still the schema default, so this is the common shape.

const legacyGenerator = `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: tenants}
spec:
  generators:
    - git:
        repoURL: https://example.com/repo.git
        files:
          - path: "config/tenants/*.yaml"
  template:
    spec:
      destination:
        namespace: '{{tenant}}-system'
      source:
        path: charts/app
        helm:
          releaseName: '{{tenant}}'
          valueFiles:
            - '/{{path}}/{{path.filename}}'
`

func TestALegacyGeneratorExpandsWithFlatDottedKeys(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", legacyGenerator)
	write(t, dir, "config/tenants/alpha.yaml", "tenant: alpha\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want one release", got)
	}
	if got[0].Name != "alpha" {
		t.Errorf("Name = %q, want alpha", got[0].Name)
	}
	if got[0].Namespace != "alpha-system" {
		t.Errorf("Namespace = %q, want alpha-system", got[0].Namespace)
	}
	if !slices.Contains(got[0].ValueFiles, "config/tenants/alpha.yaml") {
		t.Errorf("ValueFiles = %v, want the matched file", got[0].ValueFiles)
	}
}

func TestALegacyNestedKeyIsFlattenedWithDots(t *testing.T) {
	// flatten.DotStyle: {cluster: {name: x}} is reachable as {{cluster.name}}
	// and NOT as {{ .cluster.name }} - the legacy pass has no Go template.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", strings.Replace(legacyGenerator, "{{tenant}}'", "{{cluster.name}}'", 2))
	write(t, dir, "config/tenants/alpha.yaml", "cluster:\n  name: alpha\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 || got[0].Name != "alpha" {
		t.Errorf("ReleasesFor() = %+v, want the nested key flattened", got)
	}
}

func TestALegacyNonStringValueIsNotSubstituted(t *testing.T) {
	// argo-cd asserts replaceMap[tag].(string) and writes the tag back when it
	// fails, so a number never substitutes. idem then sees a value still
	// carrying {{ }} and refuses it rather than inventing one.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", strings.Replace(legacyGenerator, "releaseName: '{{tenant}}'", "releaseName: '{{replicas}}'", 1))
	write(t, dir, "config/tenants/alpha.yaml", "tenant: alpha\nreplicas: 3\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want one release", got)
	}
	if got[0].Name != "" {
		t.Errorf("Name = %q, want it unresolved - argo-cd would not substitute a number either", got[0].Name)
	}
}

func TestALegacyMissingKeyIsLeftUnresolved(t *testing.T) {
	// The tag is written back verbatim rather than emptied, and the difference
	// only shows inside a larger string: emptied, `{{tenant}}-system` becomes
	// the namespace `-system`, which idem would then render into and report as
	// though the repository had said it.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", legacyGenerator)
	write(t, dir, "config/tenants/alpha.yaml", "somethingElse: alpha\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want one release", got)
	}
	if got[0].Name != "" {
		t.Errorf("Name = %q, want the absent key left unresolved", got[0].Name)
	}
	if got[0].Namespace != "" {
		t.Errorf("Namespace = %q, want nothing rather than a namespace built around a hole", got[0].Namespace)
	}
}

func TestTheNormalizedPathKeysAreSanitisedTheWayArgoCDSanitisesThem(t *testing.T) {
	// utils.SanitizeName: lowercase, every character outside [-a-z0-9.] becomes
	// a hyphen, truncate at 253, then trim leading and trailing "-." .
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", strings.Replace(legacyGenerator,
		"releaseName: '{{tenant}}'", "releaseName: '{{path.basenameNormalized}}'", 1))
	write(t, dir, "config/Team_Alpha/one.yaml", "tenant: alpha\n")
	write(t, dir, "apps/other.yaml", strings.Replace(
		strings.Replace(legacyGenerator, "config/tenants/*.yaml", "config/Team_Alpha/*.yaml", 1),
		"releaseName: '{{tenant}}'", "releaseName: '{{path.basenameNormalized}}'", 1))

	for _, r := range load(t, dir).ReleasesFor("charts/app") {
		if r.Instance == "config/Team_Alpha/one.yaml" && r.Name != "team-alpha" {
			t.Errorf("Name = %q, want team-alpha", r.Name)
		}
	}
}

func TestALegacyPathSegmentIsIndexed(t *testing.T) {
	// argo-cd writes params["path[0]"], params["path[1]"] ... for the segments.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", strings.Replace(legacyGenerator,
		"releaseName: '{{tenant}}'", "releaseName: '{{path[1]}}'", 1))
	write(t, dir, "config/tenants/alpha.yaml", "tenant: alpha\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 || got[0].Name != "tenants" {
		t.Errorf("ReleasesFor() = %+v, want the second path segment", got)
	}
}

func TestASubstitutionThatStaysTemplatedIsNotAccepted(t *testing.T) {
	// The element resolved, but what it resolved TO is itself a template. idem
	// cannot tell whose template it is or who expands it next, so it does not
	// hand it to helm as though the repository had stated a value.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", filesGenerator)
	write(t, dir, "config/tenants/alpha.yaml", "tenant: '{{ .Release.Name }}'\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want one release", got)
	}
	if got[0].Name != "" {
		t.Errorf("Name = %q, want it left unresolved rather than passed through", got[0].Name)
	}
	if len(got[0].Templated) == 0 {
		t.Error("Templated is empty, want idem to record what it could not resolve rather than drop it")
	}
}

// ArgoCD globs generator paths with doublestar.FilepathGlob — verified in
// reposerver/repository/repository.go, whose own comment says it is
// "consistent with AppSet generators". `**` matches zero or more path
// segments, which filepath.Glob does not implement at all; refusing the whole
// ApplicationSet over it means an estate on the pattern ArgoCD's own docs use
// gets no expansion whatsoever.

func doublestarSet(pattern string) string {
	return strings.Replace(filesGenerator, "config/tenants/*.yaml", pattern, 1)
}

func TestADoublestarMatchesFilesAtAnyDepth(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", doublestarSet("config/**/tenant.yaml"))
	write(t, dir, "config/eu/west/tenant.yaml", "tenant: eu-west\n")
	write(t, dir, "config/us/tenant.yaml", "tenant: us\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 2 {
		t.Fatalf("ReleasesFor() returned %d, want both depths: %+v", len(got), got)
	}
}

func TestADoublestarAlsoMatchesNoSegmentsAtAll(t *testing.T) {
	// `a/**/b` matches `a/b`. Requiring at least one segment is the classic
	// way to get this subtly wrong and silently drop a release.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", doublestarSet("config/**/tenant.yaml"))
	write(t, dir, "config/tenant.yaml", "tenant: flat\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want the un-nested file matched", got)
	}
}

func TestADoublestarDoesNotDescendIntoGit(t *testing.T) {
	// `.git` holds YAML that is not configuration, and a repository is full of
	// it. Walking in would manufacture releases from the object store.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", doublestarSet("**/tenant.yaml"))
	write(t, dir, ".git/tenant.yaml", "tenant: notreal\n")
	write(t, dir, "config/tenant.yaml", "tenant: real\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 || got[0].Name != "real" {
		t.Errorf("ReleasesFor() = %+v, want only the file outside .git", got)
	}
}

func TestADirectoriesGeneratorMatchesDirectoriesOnly(t *testing.T) {
	// A file that happens to match the pattern is not a directory, and turning
	// one into a release invents a deployment nobody declared.
	dir := t.TempDir()
	write(t, dir, "apps/addons.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: {name: addons}
spec:
  goTemplate: true
  generators:
    - git:
        repoURL: https://example.com/repo.git
        directories:
          - path: "addons/*"
  template:
    spec:
      source:
        path: charts/app
        helm:
          releaseName: '{{ .path.basename }}'
`)
	write(t, dir, "addons/ingress/values.yaml", "x: 1\n")
	write(t, dir, "addons/README.md", "not a directory\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 || got[0].Name != "ingress" {
		t.Errorf("ReleasesFor() = %+v, want only the directory", got)
	}
}

func TestAMalformedPatternLeavesTheSetUnexpanded(t *testing.T) {
	// Refusing is the conservative answer: expanding to a subset would report
	// some of the releases as though they were all of them.
	dir := t.TempDir()
	write(t, dir, "apps/tenants.yaml", doublestarSet("config/[.yaml"))
	write(t, dir, "config/tenants/alpha.yaml", "tenant: alpha\n")

	got := load(t, dir).ReleasesFor("charts/app")

	if len(got) != 1 || got[0].Name != "" {
		t.Errorf("ReleasesFor() = %+v, want the template reported unresolved", got)
	}
}

const withCreateNamespace = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: home-app
spec:
  destination:
    namespace: home
  source:
    path: charts/home
  syncPolicy:
    syncOptions:
      - CreateNamespace=true
`

// A dry run into a namespace that does not exist fails, and the bare failure
// reads as "idem could not check this" when the truth is "ArgoCD would have
// created it first". The option was parsed into SyncOptions all along and
// nothing ever asked for it.
func TestAnApplicationThatWouldCreateItsNamespaceSaysSo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withCreateNamespace)

	if !load(t, dir).CreatesNamespace("charts/home") {
		t.Error("CreatesNamespace() = false, want true - the Application sets CreateNamespace=true")
	}
}

func TestAnApplicationWithoutCreateNamespaceDoesNotClaimIt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withDestination)

	if load(t, dir).CreatesNamespace("charts/home") {
		t.Error("CreatesNamespace() = true, want false - nothing sets the option")
	}
}

// Absence of the annotation is not evidence the mode is off: it can also be set
// cluster-wide by `controller.diff.server.side` in argocd-cmd-params-cm, which
// is in no manifest idem reads. So this only ever upgrades idem from hedging to
// certainty, never the other way.
func TestAnApplicationAnnotatedForServerSideDiffIsRead(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: home-app
  annotations:
    argocd.argoproj.io/compare-options: ServerSideDiff=true
spec:
  destination:
    namespace: home
  source:
    path: charts/home
`)

	if !load(t, dir).ServerSideDiff("charts/home") {
		t.Error("ServerSideDiff() = false, want true - the annotation says so")
	}
}

func TestAnApplicationWithoutTheAnnotationIsNotAssumedToBeOnEitherMode(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", withDestination)

	if load(t, dir).ServerSideDiff("charts/home") {
		t.Error("ServerSideDiff() = true, want false - no annotation says so")
	}
}

// spec.destination.namespace is optional in ArgoCD, and CreateNamespace=true
// with no explicit namespace is exactly the shape you would expect to meet.
//
// Both facts were stored on Destination, whose EXISTENCE is gated on the
// namespace being set - so an Application saying either of these things while
// omitting the namespace produced no Destination at all and both answers came
// back false. Every fixture written for them set a namespace, so nothing caught
// it.
func TestAnApplicationWithNoNamespaceStillReportsCreateNamespace(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: home-app
spec:
  source:
    path: charts/home
  syncPolicy:
    syncOptions:
      - CreateNamespace=true
`)

	if !load(t, dir).CreatesNamespace("charts/home") {
		t.Error("CreatesNamespace() = false, want true - the option does not depend on a namespace being named")
	}
}

func TestAnApplicationWithNoNamespaceStillReportsServerSideDiff(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: home-app
  annotations:
    argocd.argoproj.io/compare-options: ServerSideDiff=true
spec:
  source:
    path: charts/home
`)

	if !load(t, dir).ServerSideDiff("charts/home") {
		t.Error("ServerSideDiff() = false, want true - the annotation does not depend on a namespace being named")
	}
}

// The namespace-less destinations these two now create must stay invisible to
// NamespaceFor, which answers a different question and must not start claiming
// a chart deploys into "".
func TestANamespacelessApplicationStillNamesNoNamespace(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: home-app
spec:
  source:
    path: charts/home
  syncPolicy:
    syncOptions:
      - CreateNamespace=true
`)

	if ns, file := load(t, dir).NamespaceFor("charts/home"); ns != "" || file != "" {
		t.Errorf("NamespaceFor() = %q from %q, want empty - no manifest names a namespace", ns, file)
	}
}

// Templated says the path->namespace JOIN is unknowable. Neither of these two
// facts depends on that join - the compare-options annotation is not templated
// at all - so gating them on it made an ordinary per-cluster ApplicationSet
// report "no manifest says so" about a manifest that says so.
//
// This is the same confusion that was fixed at the top of argoDestinations and
// left in place here.
func TestATemplatedNamespaceDoesNotHideWhatTheApplicationStates(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/set.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: per-cluster
spec:
  generators:
    - clusters: {}
  template:
    metadata:
      annotations:
        argocd.argoproj.io/compare-options: ServerSideDiff=true
    spec:
      source:
        path: charts/home
      destination:
        namespace: '{{.metadata.labels.env}}'
      syncPolicy:
        syncOptions:
          - CreateNamespace=true
`)

	cfg := load(t, dir)
	if !cfg.CreatesNamespace("charts/home") {
		t.Error("CreatesNamespace() = false, want true - a templated namespace does not unset the option")
	}
	if !cfg.ServerSideDiff("charts/home") {
		t.Error("ServerSideDiff() = false, want true - the annotation is not templated")
	}
	// The join really is unknowable, and that answer must not change.
	if ns, _ := cfg.NamespaceFor("charts/home"); ns != "" {
		t.Errorf("NamespaceFor() = %q, want empty - the namespace is templated", ns)
	}
}

// chartPaths returns []string{""} for a manifest that names no path -
// unjoinable, but visible. NamespaceFor and For both skip those; destines did
// not, so a query that arrived with an empty path would match an unrelated
// manifest.
func TestAManifestThatNamesNoPathIsNotMatchedByAnEmptyQuery(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/remote.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: remote-app
spec:
  source:
    repoURL: https://charts.example.com
    chart: nginx
  syncPolicy:
    syncOptions:
      - CreateNamespace=true
`)

	if load(t, dir).CreatesNamespace("") {
		t.Error("CreatesNamespace(\"\") = true, want false - an empty path joins to nothing")
	}
}

const helmRelease = `
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: home
  namespace: flux-system
spec:
  releaseName: home-release
  targetNamespace: home
  chart:
    spec:
      chart: ./charts/home
      sourceRef:
        kind: GitRepository
        name: estate
  values:
    replicas: 3
    image:
      tag: "1.2.3"
`

// A HelmRelease says what its release is rendered with, and idem ignored all of
// it: only rules and the engine name were read, so every Flux user's chart was
// rendered with defaults and the findings described a release nobody deploys.
//
// The join is possible in exactly one case and this is it: with
// sourceRef.kind: GitRepository, spec.chart.spec.chart is a path inside the
// repository idem is already looking at. An OCIRepository or HelmRepository
// source names a packaged chart idem cannot match to a local directory, and
// guessing would be worse than saying nothing.
func TestAHelmReleaseSuppliesTheValuesItsChartIsRenderedWith(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/home.yaml", helmRelease)

	got := load(t, dir).ReleasesFor("charts/home")
	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want one release", got)
	}

	if got[0].Name != "home-release" {
		t.Errorf("Name = %q, want home-release", got[0].Name)
	}
	if got[0].Namespace != "home" {
		t.Errorf("Namespace = %q, want home (targetNamespace)", got[0].Namespace)
	}
	if got[0].Inline["replicas"] != 3 {
		t.Errorf("Inline = %+v, want replicas: 3 from spec.values", got[0].Inline)
	}
}

// A source idem cannot join to a local directory must not be guessed at.
func TestAHelmReleaseFromAPackagedChartClaimsNoLocalPath(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/remote.yaml", `
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: remote
spec:
  chart:
    spec:
      chart: podinfo
      sourceRef:
        kind: HelmRepository
        name: podinfo
  values:
    replicas: 9
`)

	if got := load(t, dir).ReleasesFor("podinfo"); len(got) != 0 {
		t.Errorf("ReleasesFor() = %+v, want none - a HelmRepository chart is not a path in this repo", got)
	}
}

// $values/… names a file in ANOTHER source, which is another repository.
//
// valueFilePath did path.Join(chartPath, file) with no $ref handling, so idem
// handed helm `charts/app/$values/env/prod.yaml`, helm could not open it, and
// the chart came back unrenderable - exit 2, which is always fatal and escapes
// the ratchet. That is the documented ArgoCD pattern for keeping values in a
// second repository, so any estate using it was permanently red.
//
// It is an unresolvable value source, which idem already has a shape for: name
// it, count it as unconstructed, and never fail the run over a limit of idem's.
func TestAMultiSourceValueFileFromAnotherRepositoryIsNamedNotJoined(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "apps/app.yaml", `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: app
spec:
  destination:
    namespace: apps
  sources:
    - path: charts/app
      repoURL: https://example.com/estate.git
      helm:
        valueFiles:
          - $values/env/prod.yaml
          - overrides.yaml
    - repoURL: https://example.com/values.git
      ref: values
`)

	got := load(t, dir).ReleasesFor("charts/app")
	if len(got) != 1 {
		t.Fatalf("ReleasesFor() = %+v, want one release", got)
	}

	// The one idem CAN resolve is still resolved, relative to the chart path.
	if !slices.Contains(got[0].ValueFiles, "charts/app/overrides.yaml") {
		t.Errorf("ValueFiles = %v, want the ordinary file still joined", got[0].ValueFiles)
	}
	// The one it cannot must not be handed to helm as a path.
	for _, f := range got[0].ValueFiles {
		if strings.Contains(f, "$values") {
			t.Errorf("ValueFiles = %v, want no $ref path passed through to helm", got[0].ValueFiles)
		}
	}
	// And it must be named, so the report can say why this is not the release
	// ArgoCD deploys.
	if !slices.ContainsFunc(got[0].Templated, func(s string) bool { return strings.Contains(s, "$values") }) {
		t.Errorf("Templated = %v, want the unresolvable source named", got[0].Templated)
	}
}
