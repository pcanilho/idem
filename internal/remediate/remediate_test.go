package remediate

import (
	"slices"
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

// --- Phase A: pointers must describe what ArgoCD evaluates, not what helm rendered ---

func refOf(apiVersion, kind, name string) diff.ObjectRef {
	return diff.ObjectRef{APIVersion: apiVersion, Kind: kind, Name: name}
}

func pointersOf(t *testing.T, f check.Finding) []string {
	t.Helper()
	entries := Entries([]check.Finding{f})
	if len(entries) == 0 {
		return nil
	}
	return entries[0].Pointers
}

func TestASecretRenderedWithStringDataIsAddressedUnderData(t *testing.T) {
	// gitops-engine's NormalizeSecret base64-encodes stringData into data and
	// then DELETES the stringData key, and ignoreDifferences is applied after
	// that. So /stringData/KEY targets a path that no longer exists - and
	// ArgoCD's shouldLogError explicitly suppresses "doc is missing path", so
	// the user gets no error, no warning, and no working suppression.
	got := pointersOf(t, finding(secretRef("creds"), path(".stringData.WEBUI_SECRET_KEY")))

	if !slices.Contains(got, "/data/WEBUI_SECRET_KEY") {
		t.Errorf("pointers = %v, want /data/WEBUI_SECRET_KEY - the one the diff engine sees", got)
	}
}

func TestASecretRenderedWithStringDataAlsoKeepsTheStringDataPointer(t *testing.T) {
	// Under RespectIgnoreDifferences=true the sync path applies pointers to the
	// RAW target, which still carries stringData - so /data alone suppresses
	// the diff but does not stop selfHeal overwriting the value. The redundant
	// pointer is a silent no-op in the other path, so emitting both is free.
	got := pointersOf(t, finding(secretRef("creds"), path(".stringData.WEBUI_SECRET_KEY")))

	if !slices.Contains(got, "/stringData/WEBUI_SECRET_KEY") {
		t.Errorf("pointers = %v, want the sync-path pointer too", got)
	}
	if len(got) != 2 {
		t.Errorf("pointers = %v, want exactly the two forms", got)
	}
}

func TestASecretRenderedWithDataGetsOnlyTheDataPointer(t *testing.T) {
	// Nothing to translate, and inventing a stringData pointer for a key the
	// chart never rendered that way would be noise.
	got := pointersOf(t, finding(secretRef("creds"), path(".data.password")))

	if len(got) != 1 || got[0] != "/data/password" {
		t.Errorf("pointers = %v, want just /data/password", got)
	}
}

func TestAnIndexIntoClusterRoleRulesIsNeverEmitted(t *testing.T) {
	// normalizeRole nulls an empty rules array, and nulls rules entirely for
	// any aggregated ClusterRole when ignoreAggregatedRoles is on. An index
	// into it cannot be relied on to address anything.
	got := pointersOf(t, finding(
		refOf("rbac.authorization.k8s.io/v1", "ClusterRole", "viewer"),
		path(".rules.0.verbs"),
	))

	if len(got) != 0 {
		t.Errorf("pointers = %v, want none - an index into /rules is not addressable", got)
	}
}

func TestAnIndexIntoEndpointsSubsetsIsNeverEmitted(t *testing.T) {
	// normalizeEndpoint sorts subsets before diffing, so index N in the render
	// is not index N in what ArgoCD compares.
	got := pointersOf(t, finding(
		refOf("v1", "Endpoints", "api"),
		path(".subsets.0.addresses.1.ip"),
	))

	if len(got) != 0 {
		t.Errorf("pointers = %v, want none - subsets are reordered before diffing", got)
	}
}

func TestCreationTimestampIsNeverEmitted(t *testing.T) {
	// Normalize removes metadata.creationTimestamp unconditionally, on both
	// sides, before ignoreDifferences runs.
	got := pointersOf(t, finding(
		refOf("v1", "ConfigMap", "cm"),
		path(".metadata.creationTimestamp"),
	))

	if len(got) != 0 {
		t.Errorf("pointers = %v, want none - the field is already stripped", got)
	}
}

func TestStatusPointersAreNeverEmitted(t *testing.T) {
	// ArgoCD injects a */* -> /status ignore by default, so any /status
	// pointer idem emits is redundant.
	got := pointersOf(t, finding(
		refOf("apps/v1", "Deployment", "api"),
		path(".status.replicas"),
	))

	if len(got) != 0 {
		t.Errorf("pointers = %v, want none - /status is ignored by default", got)
	}
}

func TestAnOrdinaryPointerIsLeftAlone(t *testing.T) {
	got := pointersOf(t, finding(
		refOf("apps/v1", "Deployment", "api"),
		path(".spec.replicas"),
	))

	if len(got) != 1 || got[0] != "/spec/replicas" {
		t.Errorf("pointers = %v, want /spec/replicas unchanged", got)
	}
}

func TestAnObjectWhoseEveryPointerIsUnusableGetsNoEntry(t *testing.T) {
	// An entry with no jsonPointers would be a block that silently does
	// nothing - the exact failure this whole pass exists to remove.
	entries := Entries([]check.Finding{finding(
		refOf("v1", "Endpoints", "api"),
		path(".subsets.0.addresses.0.ip"),
	)})

	if len(entries) != 0 {
		t.Errorf("Entries() = %+v, want no entry at all", entries)
	}
}

func TestStringDataOnANonSecretIsNotRewritten(t *testing.T) {
	// Only core/v1 Secret is normalised this way. A CRD with a stringData
	// field of its own must be left exactly as rendered.
	got := pointersOf(t, finding(
		refOf("example.com/v1", "Thing", "t"),
		path(".stringData.key"),
	))

	if len(got) != 1 || got[0] != "/stringData/key" {
		t.Errorf("pointers = %v, want /stringData/key untouched", got)
	}
}

func TestACoreObjectNeverCarriesAGroupField(t *testing.T) {
	// A Secret's group is "", not "core". ArgoCD matches rules with
	// glob.Match(rule.Group, object.Group), so `group: core` matches nothing
	// and the entry silently does nothing — the same class of failure as a
	// wrong pointer, and a documented trap people fall into.
	out := YAML(Entries([]check.Finding{finding(secretRef("creds"), path(".data.password"))}))

	if strings.Contains(out, "group: core") {
		t.Errorf("YAML() = %q, want no `group: core` — it matches nothing", out)
	}
	if strings.Contains(out, "group:") {
		t.Errorf("YAML() = %q, want the group omitted entirely for a core object", out)
	}
}

func TestAGroupedObjectCarriesItsRealGroup(t *testing.T) {
	out := YAML(Entries([]check.Finding{finding(
		refOf("networking.k8s.io/v1", "Ingress", "api"), path(".metadata.annotations"),
	)}))

	if !strings.Contains(out, "group: networking.k8s.io") {
		t.Errorf("YAML() = %q, want the real group", out)
	}
}
