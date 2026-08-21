package remediate

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/objpath"
)

// path turns ".data.password" into the segments the real pipeline builds.
func path(dotted string) objpath.Path {
	var p objpath.Path
	for seg := range strings.SplitSeq(strings.TrimPrefix(dotted, "."), ".") {
		p = p.Append(objpath.Key(seg))
	}
	return p
}

func finding(ref diff.ObjectRef, paths ...objpath.Path) check.Finding {
	var pd []diff.PathDiff
	for _, p := range paths {
		pd = append(pd, diff.PathDiff{Path: p, Left: "a", Right: "b", HasLeft: true, HasRight: true})
	}
	return check.Finding{Change: diff.Change{Object: ref, Type: diff.Differs, Paths: pd}}
}

func secretRef(name string) diff.ObjectRef {
	return diff.ObjectRef{APIVersion: "v1", Kind: "Secret", Name: name}
}

// parsed is the shape an Application's ignoreDifferences block has, so tests
// can assert on structure rather than on formatting.
type parsed struct {
	Spec struct {
		IgnoreDifferences []struct {
			Group        string   `yaml:"group"`
			Kind         string   `yaml:"kind"`
			Namespace    string   `yaml:"namespace"`
			Name         string   `yaml:"name"`
			JSONPointers []string `yaml:"jsonPointers"`
		} `yaml:"ignoreDifferences"`
		SyncPolicy struct {
			SyncOptions []string `yaml:"syncOptions"`
		} `yaml:"syncPolicy"`
	} `yaml:"spec"`
}

func render(t *testing.T, findings ...check.Finding) parsed {
	t.Helper()
	out := YAML(Entries(findings))

	var got parsed
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("emitted YAML does not parse: %v\n%s", err, out)
	}
	return got
}

func TestEntriesGroupEveryPointerOfAnObjectTogether(t *testing.T) {
	// One entry per object, not one per field: the user pastes once.
	got := render(t, finding(secretRef("creds"), path(".data.password"), path(".data.token")))

	if n := len(got.Spec.IgnoreDifferences); n != 1 {
		t.Fatalf("emitted %d entries, want 1", n)
	}
	if want := []string{"/data/password", "/data/token"}; strings.Join(got.Spec.IgnoreDifferences[0].JSONPointers, ",") != strings.Join(want, ",") {
		t.Errorf("pointers = %v, want %v", got.Spec.IgnoreDifferences[0].JSONPointers, want)
	}
}

func TestEveryPointerIsEmittedEvenThoughTheDisplayCapsThem(t *testing.T) {
	// The text output shows at most a handful of fields per object and says
	// "and N more". The remediation block is what the user actually pastes:
	// a capped block would silently fail to stop the churn it promises to.
	var paths []objpath.Path
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		paths = append(paths, path(".data."+k))
	}
	got := render(t, finding(secretRef("big"), paths...))

	if n := len(got.Spec.IgnoreDifferences[0].JSONPointers); n != len(paths) {
		t.Errorf("emitted %d pointers, want all %d", n, len(paths))
	}
}

func TestPointersSurviveTheEscapingTheyNeed(t *testing.T) {
	// checksum/secrets is THE annotation that makes pods roll, and RFC 6901
	// requires it as checksum~1secrets. If that does not survive being written
	// and read back, the pasted block silently targets nothing.
	p := objpath.Path{}.Append(objpath.Key("metadata")).Append(objpath.Key("annotations")).Append(objpath.Key("checksum/secrets"))
	got := render(t, finding(diff.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"}, p))

	if want := "/metadata/annotations/checksum~1secrets"; got.Spec.IgnoreDifferences[0].JSONPointers[0] != want {
		t.Errorf("pointer = %q, want %q", got.Spec.IgnoreDifferences[0].JSONPointers[0], want)
	}
}

func TestPointersHoldingYAMLPunctuationStillParseBack(t *testing.T) {
	// A ConfigMap key may contain a colon, a comma or a brace. Written into a
	// flow sequence unquoted, any of those changes what the YAML means.
	for _, key := range []string{"a:b", "a,b", "a[b]", "a{b}", "#comment", ""} {
		p := objpath.Path{}.Append(objpath.Key("data")).Append(objpath.Key(key))
		got := render(t, finding(secretRef("odd"), p))

		want := p.JSONPointer()
		if len(got.Spec.IgnoreDifferences) == 0 || len(got.Spec.IgnoreDifferences[0].JSONPointers) == 0 {
			t.Fatalf("key %q: nothing emitted", key)
		}
		if gotPtr := got.Spec.IgnoreDifferences[0].JSONPointers[0]; gotPtr != want {
			t.Errorf("key %q: pointer = %q, want %q", key, gotPtr, want)
		}
	}
}

func TestEntriesCarryGroupAndNamespaceOnlyWhenTheyExist(t *testing.T) {
	// ArgoCD matches all groups when group is omitted, and all namespaces when
	// namespace is omitted. Emitting empty strings would be noise; omitting a
	// namespace that exists would over-match prod and staging alike.
	got := render(t,
		finding(secretRef("core-thing"), path(".data.k")),
		finding(diff.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "prod", Name: "api"}, path(".spec.replicas")),
	)

	if len(got.Spec.IgnoreDifferences) != 2 {
		t.Fatalf("emitted %d entries, want 2", len(got.Spec.IgnoreDifferences))
	}
	for _, e := range got.Spec.IgnoreDifferences {
		switch e.Kind {
		case "Secret":
			if e.Group != "" || e.Namespace != "" {
				t.Errorf("core object carries group %q namespace %q, want both omitted", e.Group, e.Namespace)
			}
		case "Deployment":
			if e.Group != "apps" {
				t.Errorf("group = %q, want apps", e.Group)
			}
			if e.Namespace != "prod" {
				t.Errorf("namespace = %q, want prod", e.Namespace)
			}
		}
	}
}

func TestAnObjectThatOnlySometimesRendersIsNotGivenAPointer(t *testing.T) {
	// It has no differing field to point at, and ignoreDifferences cannot
	// express "this object appears and disappears". Emitting an entry with no
	// pointers would be a block that silently does nothing.
	vanishing := check.Finding{Change: diff.Change{Object: secretRef("ghost"), Type: diff.OnlyInLeft}}

	got := render(t, vanishing, finding(secretRef("real"), path(".data.k")))

	if len(got.Spec.IgnoreDifferences) != 1 {
		t.Fatalf("emitted %d entries, want 1", len(got.Spec.IgnoreDifferences))
	}
	if got.Spec.IgnoreDifferences[0].Name != "real" {
		t.Errorf("entry names %q, want real", got.Spec.IgnoreDifferences[0].Name)
	}
}

func TestTheSameObjectFoundTwiceBecomesOneEntry(t *testing.T) {
	got := render(t,
		finding(secretRef("creds"), path(".data.password")),
		finding(secretRef("creds"), path(".data.password"), path(".data.token")),
	)

	if len(got.Spec.IgnoreDifferences) != 1 {
		t.Fatalf("emitted %d entries, want 1", len(got.Spec.IgnoreDifferences))
	}
	if n := len(got.Spec.IgnoreDifferences[0].JSONPointers); n != 2 {
		t.Errorf("emitted %d pointers, want 2 - the repeat must not double up", n)
	}
}

func TestSyncOptionsRespectIgnoreDifferencesIsIncluded(t *testing.T) {
	// Without it, ignoreDifferences suppresses the diff but selfHeal still
	// re-applies the object, so the churn continues and the user concludes
	// idem's advice did not work.
	got := render(t, finding(secretRef("creds"), path(".data.password")))

	if want := "RespectIgnoreDifferences=true"; len(got.Spec.SyncPolicy.SyncOptions) != 1 || got.Spec.SyncPolicy.SyncOptions[0] != want {
		t.Errorf("syncOptions = %v, want [%s]", got.Spec.SyncPolicy.SyncOptions, want)
	}
}

func TestEntriesAreOrderedDeterministically(t *testing.T) {
	got := render(t,
		finding(secretRef("zeta"), path(".data.k")),
		finding(diff.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "alpha"}, path(".spec.replicas")),
	)

	if got.Spec.IgnoreDifferences[0].Kind != "Deployment" {
		t.Errorf("first entry is %q, want Deployment sorted ahead of Secret", got.Spec.IgnoreDifferences[0].Kind)
	}
}

func TestNoEntriesMeansNoBlock(t *testing.T) {
	if out := YAML(Entries(nil)); out != "" {
		t.Errorf("YAML() = %q, want empty when there is nothing to fix", out)
	}
}

func TestASinglePointerIsWrittenInline(t *testing.T) {
	// Cosmetic, and deliberate: one pointer on its own line reads as the start
	// of a list the reader then scans for more.
	out := YAML(Entries([]check.Finding{finding(secretRef("creds"), path(".data.password"))}))

	if !strings.Contains(out, "jsonPointers: [/data/password]") {
		t.Errorf("YAML() = %q, want the single pointer inline", out)
	}
}

func TestAGeneratedNameObjectIsNotGivenAnEntry(t *testing.T) {
	// An entry with no name matches EVERY object of that kind, so emitting one
	// for a hook Job whose name the API server assigns at apply time would
	// quietly tell ArgoCD to ignore differences on all Jobs in the app.
	job := diff.ObjectRef{APIVersion: "batch/v1", Kind: "Job", GenerateName: "pre-install-"}

	got := render(t, finding(job, path(".spec.template.spec.containers")), finding(secretRef("real"), path(".data.k")))

	if len(got.Spec.IgnoreDifferences) != 1 {
		t.Fatalf("emitted %d entries, want 1", len(got.Spec.IgnoreDifferences))
	}
	if got.Spec.IgnoreDifferences[0].Kind != "Secret" {
		t.Errorf("emitted an entry for %q, want only the addressable object", got.Spec.IgnoreDifferences[0].Kind)
	}
}
