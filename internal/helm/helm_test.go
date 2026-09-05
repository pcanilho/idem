package helm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/engine"
)

// requireHelm skips when no helm binary is available. The exec tests render
// real charts rather than mocking the command: what idem asserts about helm's
// behaviour is only true if helm actually behaves that way.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
}

func indexOf(args []string, want string) int { return slices.Index(args, want) }

func TestTemplateArgsPutsReleaseNameBeforeChartReference(t *testing.T) {
	// `helm template RELEASE CHART` - reversing them renders a chart named
	// after the release, which either fails or renders the wrong thing.
	args := templateArgs(engine.Spec{Release: "home", ChartRef: "./charts/home"})

	want := []string{"template", "home", "./charts/home"}
	if len(args) < 3 || !slices.Equal(args[:3], want) {
		t.Errorf("templateArgs() = %v, want it to start with %v", args, want)
	}
}

func TestTemplateArgsPassesValuesFilesInOrder(t *testing.T) {
	// Later -f wins in helm, so order is semantic, not cosmetic.
	args := templateArgs(engine.Spec{
		Release:     "home",
		ChartRef:    "./c",
		ValuesFiles: []string{"base.yaml", "prod.yaml"},
	})

	first, second := indexOf(args, "base.yaml"), indexOf(args, "prod.yaml")
	if first < 0 || second < 0 {
		t.Fatalf("templateArgs() = %v, want both values files", args)
	}
	if first > second {
		t.Errorf("templateArgs() = %v, want base.yaml before prod.yaml", args)
	}
	if args[first-1] != "-f" || args[second-1] != "-f" {
		t.Errorf("templateArgs() = %v, want each values file preceded by -f", args)
	}
}

func TestTemplateArgsPassesEachSetValueSeparately(t *testing.T) {
	args := templateArgs(engine.Spec{
		Release:   "home",
		ChartRef:  "./c",
		SetValues: []string{"a=1", "b=2"},
	})

	for _, v := range []string{"a=1", "b=2"} {
		i := indexOf(args, v)
		if i < 0 {
			t.Fatalf("templateArgs() = %v, want %q", args, v)
		}
		if args[i-1] != "--set" {
			t.Errorf("templateArgs() = %v, want %q preceded by --set", args, v)
		}
	}
}

func TestTemplateArgsOmitsNamespaceWhenUnset(t *testing.T) {
	// helm's own default is the current kube context's namespace, which is
	// invisible local state - so the caller always supplies one now. This pins
	// the library's half of that contract: an unset Namespace passes no flag,
	// and it is main's job never to leave it unset.
	args := templateArgs(engine.Spec{Release: "home", ChartRef: "./c"})

	if i := indexOf(args, "--namespace"); i >= 0 {
		t.Errorf("templateArgs() = %v, want no --namespace when Spec.Namespace is empty", args)
	}
}

func TestTemplateArgsPassesNamespaceWhenSet(t *testing.T) {
	args := templateArgs(engine.Spec{Release: "home", ChartRef: "./c", Namespace: "lab"})

	i := indexOf(args, "--namespace")
	if i < 0 || i+1 >= len(args) || args[i+1] != "lab" {
		t.Errorf("templateArgs() = %v, want --namespace lab", args)
	}
}

func TestTemplateArgsPassesRepoAndVersion(t *testing.T) {
	args := templateArgs(engine.Spec{
		Release:  "pg",
		ChartRef: "postgresql",
		Repo:     "https://charts.example.com",
		Version:  "12.1.0",
	})

	for flag, value := range map[string]string{
		"--repo":    "https://charts.example.com",
		"--version": "12.1.0",
	} {
		i := indexOf(args, flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != value {
			t.Errorf("templateArgs() = %v, want %s %s", args, flag, value)
		}
	}
}

func TestTemplateArgsPassesEachAPIVersionSeparately(t *testing.T) {
	// helm's --api-versions is repeatable; joining them with a comma into one
	// flag is accepted by helm but reads back as a single bogus GVK.
	args := templateArgs(engine.Spec{
		Release:     "home",
		ChartRef:    "./c",
		APIVersions: []string{"v1", "apps/v1"},
	})

	for _, v := range []string{"v1", "apps/v1"} {
		i := indexOf(args, v)
		if i < 0 || args[i-1] != "--api-versions" {
			t.Errorf("templateArgs() = %v, want --api-versions %s", args, v)
		}
	}
}

func TestTemplateArgsPassesKubeVersionWhenSet(t *testing.T) {
	args := templateArgs(engine.Spec{Release: "h", ChartRef: "./c", KubeVersion: "v1.31.0"})

	i := indexOf(args, "--kube-version")
	if i < 0 || i+1 >= len(args) || args[i+1] != "v1.31.0" {
		t.Errorf("templateArgs() = %v, want --kube-version v1.31.0", args)
	}
}

func TestParseVersionStripsPrefixAndBuildMetadata(t *testing.T) {
	// `helm version --short` emits "v4.2.4+g3900f43". The README prints
	// "helm 4.2.4", because the git hash is noise to the reader.
	for raw, want := range map[string]string{
		"v4.2.4+g3900f43\n": "4.2.4",
		"v3.19.4+gd0d1c0f":  "3.19.4",
		"4.2.4":             "4.2.4",
	} {
		if got := parseVersion(raw); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseVersionLeavesAnUnrecognisedStringAlone(t *testing.T) {
	// Guessing at a version idem cannot parse would print a confident lie in
	// the provenance line that exists precisely to be trustworthy.
	// The input that makes this matter: a helm build that ignores --short
	// prints the whole BuildInfo line. Stripping a leading "v" unconditionally
	// turns it into "ersion.BuildInfo{...", which then gets printed in the
	// provenance line as if it were a version.
	for _, raw := range []string{
		`version.BuildInfo{Version:"v4.2.4", GitCommit:"3900f43"}`,
		"something unexpected",
	} {
		if got := parseVersion(raw); got != raw {
			t.Errorf("parseVersion(%q) = %q, want it returned verbatim", raw, got)
		}
	}
}

func TestRenderParsesRealHelmOutput(t *testing.T) {
	requireHelm(t)

	objs, err := New("").Render(context.Background(), engine.Spec{
		Release:  "stable",
		ChartRef: "testdata/charts/stable",
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("Render() returned %d objects, want 1", len(objs))
	}
	if got, want := objs[0].Name, "stable-config"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := objs[0].Source, "stable/templates/configmap.yaml"; got != want {
		t.Errorf("Source = %q, want %q - provenance must survive rendering", got, want)
	}
}

func TestRenderIsStableForADeterministicChart(t *testing.T) {
	requireHelm(t)

	h := New("")
	spec := engine.Spec{Release: "stable", ChartRef: "testdata/charts/stable"}
	first, err := h.Render(context.Background(), spec)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	second, err := h.Render(context.Background(), spec)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if first[0].Body["data"].(map[string]any)["greeting"] != second[0].Body["data"].(map[string]any)["greeting"] {
		t.Error("a chart with no non-deterministic function rendered differently twice")
	}
}

func TestRenderReportsHelmStderrWhenRenderingFails(t *testing.T) {
	requireHelm(t)

	_, err := New("").Render(context.Background(), engine.Spec{
		Release:  "broken",
		ChartRef: "testdata/charts/broken",
	})
	if err == nil {
		t.Fatal("Render() error = nil, want a failure for a chart that cannot render")
	}
	// helm's own message is the actionable part; swallowing it leaves the user
	// with "render failed" and nothing to do about it.
	if !strings.Contains(err.Error(), "auth.password is required") {
		t.Errorf("Render() error = %q, want it to carry helm's own message", err)
	}
}

func TestRenderOfAPathThatDoesNotExistFailsAsARepoLookup(t *testing.T) {
	requireHelm(t)

	// Not a curiosity - this is the whole reason internal/chartref exists.
	// helm cannot tell "charts/home" from "bitnami/postgresql", so a mistyped
	// or missing local path comes back as "repo <first-segment> not found",
	// which says nothing about the real problem. idem classifies the reference
	// before rendering so it can say something better.
	_, err := New("").Render(context.Background(), engine.Spec{
		Release:  "nope",
		ChartRef: "testdata/charts/does-not-exist",
	})
	if err == nil {
		t.Fatal("Render() error = nil, want a failure")
	}
	if !strings.Contains(err.Error(), "repo testdata not found") {
		t.Errorf("Render() error = %q, want helm's repo-alias error", err)
	}
}

func TestRenderReportsAMissingHelmBinary(t *testing.T) {
	_, err := New("helm-that-does-not-exist").Render(context.Background(), engine.Spec{
		Release:  "x",
		ChartRef: "testdata/charts/stable",
	})
	if err == nil {
		t.Fatal("Render() error = nil, want a failure naming the missing binary")
	}
	if !strings.Contains(err.Error(), "helm-that-does-not-exist") {
		t.Errorf("Render() error = %q, want it to name the binary idem tried", err)
	}
}

func TestVersionReportsTheBinaryVersion(t *testing.T) {
	requireHelm(t)

	v, err := New("").Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error = %v", err)
	}
	if v == "" || strings.HasPrefix(v, "v") {
		t.Errorf("Version() = %q, want a bare version like 4.2.4", v)
	}
}

func TestStripPullPreambleRemovesHelmsOCIChatter(t *testing.T) {
	// `helm template oci://...` writes two lines to STDOUT before the YAML:
	//
	//   Pulled: registry-1.docker.io/bitnamicharts/postgresql:18.8.12
	//   Digest: sha256:a501...
	//   ---
	//   # Source: ...
	//
	// They parse as a perfectly valid YAML mapping, so the stream decodes as a
	// document with no 'kind' and every OCI chart came back unevaluable.
	in := "Pulled: registry-1.docker.io/bitnamicharts/postgresql:18.8.12\n" +
		"Digest: sha256:a501179fbc20fd33d426444213ab8e1446cf981fb554788e93e2e250b245319e\n" +
		"---\n# Source: postgresql/templates/secret.yaml\nkind: Secret\n"

	got := string(stripPullPreamble([]byte(in)))
	want := "---\n# Source: postgresql/templates/secret.yaml\nkind: Secret\n"
	if got != want {
		t.Errorf("stripPullPreamble() = %q, want %q", got, want)
	}
}

func TestStripPullPreambleLeavesOrdinaryOutputAlone(t *testing.T) {
	in := "---\n# Source: a/templates/b.yaml\nkind: ConfigMap\n"

	if got := string(stripPullPreamble([]byte(in))); got != in {
		t.Errorf("stripPullPreamble() = %q, want it unchanged", got)
	}
}

func TestStripPullPreambleOnlyStripsAtTheStart(t *testing.T) {
	// A ConfigMap may legitimately carry a key called Digest. Only helm's own
	// preamble, before any document has begun, is chatter.
	in := "kind: ConfigMap\ndata:\n  Pulled: yes\n  Digest: sha256:abc\n"

	if got := string(stripPullPreamble([]byte(in))); got != in {
		t.Errorf("stripPullPreamble() = %q, want it unchanged", got)
	}
}

func TestStripPullPreambleLeavesAnUnrecognisedPreambleForTheParserToReject(t *testing.T) {
	// If helm starts emitting something else, idem must fail loudly rather
	// than guess at which leading lines are safe to discard.
	in := "Fetched: something new\n---\nkind: Secret\n"

	if got := string(stripPullPreamble([]byte(in))); got != in {
		t.Errorf("stripPullPreamble() = %q, want unknown chatter left in place", got)
	}
}

func TestTemplateArgsAsksTheServerWhenClusterIsSet(t *testing.T) {
	// `helm template` defaults to a client-side render, which resolves lookup
	// to {} and uses helm's own default capabilities. --dry-run=server is what
	// makes lookup resolve and hands the chart the cluster's real
	// KubeVersion and APIVersions.
	args := templateArgs(engine.Spec{Release: "h", ChartRef: "./c", Cluster: true})

	if !slices.Contains(args, "--dry-run=server") {
		t.Errorf("templateArgs() = %v, want --dry-run=server", args)
	}
}

func TestTemplateArgsStaysClientSideByDefault(t *testing.T) {
	// The default has to reproduce ArgoCD's repo-server, which renders with no
	// cluster access at all.
	args := templateArgs(engine.Spec{Release: "h", ChartRef: "./c"})

	for _, unwanted := range []string{"--dry-run=server", "--kube-context"} {
		if slices.Contains(args, unwanted) {
			t.Errorf("templateArgs() = %v, want no %s", args, unwanted)
		}
	}
}

func TestTemplateArgsPassesTheKubeContext(t *testing.T) {
	args := templateArgs(engine.Spec{Release: "h", ChartRef: "./c", Cluster: true, KubeContext: "prod"})

	i := indexOf(args, "--kube-context")
	if i < 0 || i+1 >= len(args) || args[i+1] != "prod" {
		t.Errorf("templateArgs() = %v, want --kube-context prod", args)
	}
}

func TestCapabilitiesAreNotOverriddenWhenAskingTheServer(t *testing.T) {
	// --dry-run=server already hands the chart the cluster's own KubeVersion
	// and APIVersions. Passing helm's defaults alongside would override the
	// real ones with the guesses idem was trying to escape.
	args := templateArgs(engine.Spec{Release: "h", ChartRef: "./c", Cluster: true})

	for _, unwanted := range []string{"--kube-version", "--api-versions"} {
		if slices.Contains(args, unwanted) {
			t.Errorf("templateArgs() = %v, want no %s alongside --dry-run=server", args, unwanted)
		}
	}
}

// A chart rendered straight from a registry never lands on disk, so idem
// cannot read it for `lookup` and both Flux and Helm degrade to `unknown` -
// on exactly the charts idem exists for, since a consumer of a third-party
// chart is the user who cannot simply patch it. Fetching a copy is the same
// temp-dir work dependency resolution already does.

func TestPullArgsFetchAndUnpackIntoTheGivenDirectory(t *testing.T) {
	args := pullArgs(engine.Spec{ChartRef: "podinfo", Repo: "https://example.com/charts"}, "/tmp/scan")

	if got := strings.Join(args, " "); !strings.Contains(got, "pull podinfo") {
		t.Errorf("pullArgs() = %v, want the chart pulled", args)
	}
	if !slices.Contains(args, "--untar") {
		t.Errorf("pullArgs() = %v, want it unpacked - a .tgz is not a source tree", args)
	}
	i := indexOf(args, "--untardir")
	if i < 0 || i+1 >= len(args) || args[i+1] != "/tmp/scan" {
		t.Errorf("pullArgs() = %v, want it unpacked into the directory idem chose", args)
	}
}

func TestPullArgsCarryTheRepoAndVersion(t *testing.T) {
	// Pulling a different version than was rendered would scan a chart that is
	// not the one the verdict is about.
	args := pullArgs(engine.Spec{ChartRef: "podinfo", Repo: "https://example.com/charts", Version: "6.5.0"}, "/tmp/scan")

	if i := indexOf(args, "--repo"); i < 0 || args[i+1] != "https://example.com/charts" {
		t.Errorf("pullArgs() = %v, want the repo", args)
	}
	if i := indexOf(args, "--version"); i < 0 || args[i+1] != "6.5.0" {
		t.Errorf("pullArgs() = %v, want the version", args)
	}
}

func TestPullArgsCarryNoRenderOnlyFlags(t *testing.T) {
	// A pull is a fetch, not a render: values, namespace and the cluster
	// condition mean nothing to it, and helm rejects most of them.
	args := pullArgs(engine.Spec{
		ChartRef: "oci://example.com/charts/podinfo", Release: "podinfo",
		ValuesFiles: []string{"values.yaml"}, SetValues: []string{"a=1"},
		Namespace: "lab", Cluster: true, KubeContext: "prod",
	}, "/tmp/scan")

	for _, unwanted := range []string{"-f", "--set", "--namespace", "--dry-run=server", "--kube-context"} {
		if slices.Contains(args, unwanted) {
			t.Errorf("pullArgs() = %v, want no %s", args, unwanted)
		}
	}
	if !slices.Contains(args, "oci://example.com/charts/podinfo") {
		t.Errorf("pullArgs() = %v, want the OCI reference pulled as it stands", args)
	}
}

// chartAt writes a one-template chart and returns its directory.
func chartAt(t *testing.T, name, template string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Chart.yaml", "apiVersion: v2\nname: "+name+"\nversion: 0.1.0\n")
	write("values.yaml", "{}\n")
	write(filepath.Join("templates", "cm.yaml"), template)
	return dir
}

func renderErr(t *testing.T, name, template string) error {
	t.Helper()
	_, err := New("").Render(context.Background(), engine.Spec{
		Release: name, ChartRef: chartAt(t, name, template),
	})
	if err == nil {
		t.Fatalf("Render(%s) error = nil, want a failure", name)
	}
	return err
}

func TestMayLackValuesSeparatesATemplateDefectFromAWithheldValue(t *testing.T) {
	requireHelm(t)

	// Run against real helm, because what idem asserts about helm's errors is
	// only true if helm actually emits them.
	for _, c := range []struct {
		name     string
		template string
		want     bool
	}{
		// A withheld value can produce all of these.
		{"guard", "x: {{ required \"x is required\" .Values.x }}\n", true},
		{"failed", "{{ if not .Values.x }}{{ fail \"x must be set\" }}{{ end }}\n", true},
		{"nilptr", "x: {{ .Values.a.b }}\n", true},
		{"wrongtype", "x: {{ .Values.a.b }}\n", true},

		// None of these can be. helm parses every template from raw bytes
		// before any value is bound, so a parse error precedes values.
		{"undefined", "x: {{ nosuchfunction . }}\n", false},
		{"unclosed", "x: {{ if .Values.x }}\n", false},
	} {
		tpl := c.template
		if c.name == "wrongtype" {
			tpl = "{{ $_ := .Values.a }}x: {{ .Values.a.b }}\n"
		}
		if got := MayLackValues(renderErr(t, c.name, tpl)); got != c.want {
			t.Errorf("MayLackValues(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMayLackValuesRefusesAnErrorThatNeverReachedATemplate(t *testing.T) {
	requireHelm(t)

	// The loader fails before rendering, so no value could have changed the
	// outcome. helm's 5 MiB chart-file limit is the case that motivated this:
	// it names no template, and was being excused as "could not be built".
	dir := chartAt(t, "big", "x: y\n")
	if err := os.MkdirAll(filepath.Join(dir, "charts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "charts", "vendored.tgz"), make([]byte, 6<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := New("").Render(context.Background(), engine.Spec{Release: "big", ChartRef: dir})
	if err == nil {
		t.Fatal("Render() error = nil, want helm to refuse the oversized file")
	}
	if MayLackValues(err) {
		t.Errorf("MayLackValues(%q) = true, want false - the loader never reached a template", err)
	}
}

func TestMayLackValuesAcceptsASchemaViolation(t *testing.T) {
	requireHelm(t)

	// Value-caused by definition, and the one such failure with no template
	// location, so it needs its own clause.
	dir := chartAt(t, "schema", "x: {{ .Values.x }}\n")
	schema := `{"$schema":"https://json-schema.org/draft-07/schema#","type":"object","required":["x"]}`
	if err := os.WriteFile(filepath.Join(dir, "values.schema.json"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := New("").Render(context.Background(), engine.Spec{Release: "schema", ChartRef: dir})
	if err == nil {
		t.Fatal("Render() error = nil, want the schema to reject absent x")
	}
	if !MayLackValues(err) {
		t.Errorf("MayLackValues(%q) = false, want true", err)
	}
}
