package helm

import (
	"context"
	"os/exec"
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
	// idem has no Application to derive a namespace from, so it passes none
	// and lets helm apply its own default. Inventing one would change what
	// renders - .Release.Namespace appears in real templates.
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
