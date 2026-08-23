package diff

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/manifest"
)

func parse(t *testing.T, in string) []manifest.Object {
	t.Helper()
	objs, err := manifest.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return objs
}

func compare(t *testing.T, left, right string) []Change {
	t.Helper()
	got, err := Compare(parse(t, left), parse(t, right))
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return got
}

const twoObjects = `
apiVersion: v1
kind: ConfigMap
metadata: {name: alpha}
data: {a: "1"}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: beta}
spec: {replicas: 2}
`

const twoObjectsReordered = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: beta}
spec: {replicas: 2}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: alpha}
data: {a: "1"}
`

const onlyBeta = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: beta}
spec: {replicas: 2}
`

func TestCompareIgnoresDocumentOrder(t *testing.T) {
	// The single most important property. If document order counted as a
	// difference, every render would "differ" and the tool would be useless.
	if got := compare(t, twoObjects, twoObjectsReordered); len(got) != 0 {
		t.Fatalf("reordered identical manifests reported %d changes, want 0: %+v", len(got), got)
	}
}

func TestCompareReportsObjectOnlyInLeftSide(t *testing.T) {
	got := compare(t, twoObjects, onlyBeta)
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(got), got)
	}
	if got[0].Type != OnlyInLeft {
		t.Errorf("Type = %v, want OnlyInLeft", got[0].Type)
	}
	if d := got[0].Object.Display(); d != "ConfigMap/alpha" {
		t.Errorf("Display = %q, want %q", d, "ConfigMap/alpha")
	}
}

func TestCompareReportsObjectOnlyInRightSide(t *testing.T) {
	got := compare(t, onlyBeta, twoObjects)
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(got), got)
	}
	if got[0].Type != OnlyInRight {
		t.Errorf("Type = %v, want OnlyInRight", got[0].Type)
	}
	// Asserted because the right-hand branch must read its identity from the
	// RIGHT object; taking it from the (absent) left one yields an empty ref
	// and no other test would notice.
	if d := got[0].Object.Display(); d != "ConfigMap/alpha" {
		t.Errorf("Display = %q, want %q", d, "ConfigMap/alpha")
	}
	if got[0].Object.Kind != "ConfigMap" || got[0].Object.Name != "alpha" {
		t.Errorf("ObjectRef = %+v, want Kind=ConfigMap Name=alpha", got[0].Object)
	}
}

func TestCompareReportsChangedLeafWithPathAndBothValues(t *testing.T) {
	got := compare(t, `
apiVersion: v1
kind: Secret
metadata: {name: creds}
data: {password: "aaaa"}
`, `
apiVersion: v1
kind: Secret
metadata: {name: creds}
data: {password: "bbbb"}
`)
	if len(got) != 1 || got[0].Type != Differs {
		t.Fatalf("unexpected changes: %+v", got)
	}
	if len(got[0].Paths) != 1 {
		t.Fatalf("got %d path diffs, want 1: %+v", len(got[0].Paths), got[0].Paths)
	}
	p := got[0].Paths[0]
	if s := p.Path.String(); s != ".data.password" {
		t.Errorf("Path = %q, want %q", s, ".data.password")
	}
	if p.Left != "aaaa" || p.Right != "bbbb" {
		t.Errorf("values = (%v, %v), want (aaaa, bbbb)", p.Left, p.Right)
	}
	if !p.HasLeft || !p.HasRight {
		t.Errorf("HasLeft/HasRight = (%v, %v), want both true", p.HasLeft, p.HasRight)
	}
}

func TestCompareIndexesIntoArrays(t *testing.T) {
	got := compare(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  containers:
    - image: api:1.4.0
`, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  containers:
    - image: api:1.4.2
`)
	if len(got) != 1 || len(got[0].Paths) != 1 {
		t.Fatalf("unexpected changes: %+v", got)
	}
	if s := got[0].Paths[0].Path.String(); s != ".spec.containers[0].image" {
		t.Errorf("Path = %q, want %q", s, ".spec.containers[0].image")
	}
}

func TestCompareDistinguishesAbsentKeyFromNullValue(t *testing.T) {
	// "key is missing on the right" and "key is present on the right with a
	// null value" must not produce the same record, or a generated
	// ignoreDifferences entry could target a field that does not exist.
	absent := compare(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
data: {a: "1"}
`, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
data: {}
`)
	if len(absent) != 1 || len(absent[0].Paths) != 1 {
		t.Fatalf("absent case: unexpected changes: %+v", absent)
	}
	ap := absent[0].Paths[0]
	if !ap.HasLeft || ap.HasRight {
		t.Errorf("absent case: HasLeft/HasRight = (%v, %v), want (true, false)", ap.HasLeft, ap.HasRight)
	}

	null := compare(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
data: {a: "1"}
`, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
data: {a: null}
`)
	if len(null) != 1 || len(null[0].Paths) != 1 {
		t.Fatalf("null case: unexpected changes: %+v", null)
	}
	np := null[0].Paths[0]
	if !np.HasLeft || !np.HasRight {
		t.Errorf("null case: HasLeft/HasRight = (%v, %v), want both true", np.HasLeft, np.HasRight)
	}
}

func TestCompareReportsArrayElementsAddedAndRemoved(t *testing.T) {
	shorter := `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
spec: {items: ["a"]}
`
	longer := `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
spec: {items: ["a", "b"]}
`
	removed := compare(t, longer, shorter)
	if len(removed) != 1 || len(removed[0].Paths) != 1 {
		t.Fatalf("removed: unexpected changes: %+v", removed)
	}
	rp := removed[0].Paths[0]
	if s := rp.Path.String(); s != ".spec.items[1]" {
		t.Errorf("removed: Path = %q, want %q", s, ".spec.items[1]")
	}
	if !rp.HasLeft || rp.HasRight {
		t.Errorf("removed: HasLeft/HasRight = (%v, %v), want (true, false)", rp.HasLeft, rp.HasRight)
	}
	if rp.Left != "b" {
		t.Errorf("removed: Left = %v, want b", rp.Left)
	}

	added := compare(t, shorter, longer)
	ap := added[0].Paths[0]
	if ap.HasLeft || !ap.HasRight {
		t.Errorf("added: HasLeft/HasRight = (%v, %v), want (false, true)", ap.HasLeft, ap.HasRight)
	}
	if ap.Right != "b" {
		t.Errorf("added: Right = %v, want b", ap.Right)
	}
}

func TestPathsWithinAnObjectAreDeterministicallyOrdered(t *testing.T) {
	// Ordering of the paths INSIDE one object matters as much as ordering of
	// the objects themselves: unstable path order means idem's own output is
	// non-deterministic, which is the exact defect it reports on others.
	left := `
apiVersion: v1
kind: Secret
metadata: {name: creds}
data: {alpha: "1", bravo: "1", charlie: "1", delta: "1", echo: "1", foxtrot: "1"}
`
	right := `
apiVersion: v1
kind: Secret
metadata: {name: creds}
data: {alpha: "2", bravo: "2", charlie: "2", delta: "2", echo: "2", foxtrot: "2"}
`
	want := []string{".data.alpha", ".data.bravo", ".data.charlie", ".data.delta", ".data.echo", ".data.foxtrot"}

	for run := range 20 {
		got := compare(t, left, right)
		if len(got) != 1 || len(got[0].Paths) != len(want) {
			t.Fatalf("run %d: unexpected changes: %+v", run, got)
		}
		for i, w := range want {
			if s := got[0].Paths[i].Path.String(); s != w {
				t.Fatalf("run %d: Paths[%d] = %q, want %q", run, i, s, w)
			}
		}
	}
}

func TestObjectsAreDeterministicallyOrdered(t *testing.T) {
	left := parse(t, twoObjects)
	right := parse(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: alpha}
data: {a: "changed"}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: beta}
spec: {replicas: 99}
`)

	first, err := Compare(left, right)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	for i := range 20 {
		again, err := Compare(left, right)
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		if len(again) != len(first) {
			t.Fatalf("run %d produced %d changes, first produced %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j].Object.Key() != first[j].Object.Key() {
				t.Fatalf("run %d ordered differently: %q vs %q", i, again[j].Object.Key(), first[j].Object.Key())
			}
		}
	}
}

func TestCompareRejectsDuplicateObjectIdentities(t *testing.T) {
	// Two objects with the same identity cannot both survive indexing. Silently
	// keeping the last one would drop an object before comparison, in a tool
	// whose entire claim is that it compares everything.
	dupes := `
apiVersion: v1
kind: ConfigMap
metadata: {name: same}
data: {a: "1"}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: same}
data: {a: "2"}
`
	_, err := Compare(parse(t, dupes), parse(t, onlyBeta))
	if err == nil {
		t.Fatal("Compare accepted duplicate object identities, want error")
	}
	if !strings.Contains(err.Error(), "same") {
		t.Errorf("error %q should name the duplicated object", err)
	}
}

func TestZeroChangeTypeIsNotAValidVerdict(t *testing.T) {
	// A zero-valued Change must not read as a real difference.
	var c Change
	if c.Type == Differs || c.Type == OnlyInLeft || c.Type == OnlyInRight {
		t.Errorf("zero ChangeType = %v, want an invalid/unknown value", c.Type)
	}
}

func TestChangeTypeMarshalsAsAString(t *testing.T) {
	for ct, want := range map[ChangeType]string{
		Differs:     `"differs"`,
		OnlyInLeft:  `"only-in-left"`,
		OnlyInRight: `"only-in-right"`,
	} {
		got, err := json.Marshal(ct)
		if err != nil {
			t.Fatalf("marshal %v: %v", ct, err)
		}
		if string(got) != want {
			t.Errorf("json.Marshal(%v) = %s, want %s", ct, got, want)
		}
	}
}

func TestJSONOutputCarriesBothPathForms(t *testing.T) {
	// -o json is the documented substitute for the rules/exceptions we cut, so
	// its shape is a public contract. A consumer generating an ignoreDifferences
	// block needs the RFC 6901 pointer; a human reading the JSON needs the
	// dotted form. Emitting only one would force every consumer to reimplement
	// the escaping we already got right.
	got := compare(t, `
apiVersion: v1
kind: Secret
metadata: {name: creds}
metadata2: {}
data: {"checksum/secrets": "aaa"}
`, `
apiVersion: v1
kind: Secret
metadata: {name: creds}
metadata2: {}
data: {"checksum/secrets": "bbb"}
`)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d changes, want 1: %s", len(out), b)
	}
	if out[0]["type"] != "differs" {
		t.Errorf(`type = %v, want "differs"`, out[0]["type"])
	}

	paths, _ := out[0]["paths"].([]any)
	if len(paths) != 1 {
		t.Fatalf("got %d paths, want 1: %s", len(paths), b)
	}
	p, _ := paths[0].(map[string]any)
	if p["path"] != `.data["checksum/secrets"]` {
		t.Errorf("path = %v, want the bracketed dotted form", p["path"])
	}
	if p["pointer"] != "/data/checksum~1secrets" {
		t.Errorf("pointer = %v, want the RFC 6901 escaped form", p["pointer"])
	}
}

func TestJSONOutputNamesTheObjectStructurally(t *testing.T) {
	// A consumer building ignoreDifferences needs kind and name as fields, not
	// parsed back out of a display string.
	got := compare(t, twoObjects, onlyBeta)
	b, _ := json.Marshal(got)
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	obj, _ := out[0]["object"].(map[string]any)
	if obj["kind"] != "ConfigMap" || obj["name"] != "alpha" {
		t.Errorf("object = %v, want kind=ConfigMap name=alpha", obj)
	}
}

const twoGenerateNameJobs = `
apiVersion: batch/v1
kind: Job
metadata: {generateName: pre-install-}
spec: {backoffLimit: 1}
---
apiVersion: batch/v1
kind: Job
metadata: {generateName: post-upgrade-}
spec: {backoffLimit: 1}
`

func TestObjectRefDistinguishesGenerateNameObjects(t *testing.T) {
	// Hook Jobs are named by the API server at apply time, so a rendered
	// stream can hold several with no metadata.name at all. Compare matches
	// them correctly because manifest.Object.Key falls back to generateName -
	// but ObjectRef is what every consumer downstream reads, and if its Key
	// collides then two findings cannot be told apart, and the source
	// attribution for one silently lands on the other.
	objs := parse(t, twoGenerateNameJobs)
	if len(objs) != 2 {
		t.Fatalf("fixture parsed to %d objects, want 2", len(objs))
	}

	first, second := refOf(objs[0]), refOf(objs[1])
	if first.Key() == second.Key() {
		t.Errorf("both refs have Key() = %q; distinct objects must have distinct keys", first.Key())
	}
	if first.Display() == second.Display() {
		t.Errorf("both refs display as %q; a finding must name which object it is about", first.Display())
	}
}

func TestObjectRefKeyMatchesTheManifestIdentityItCameFrom(t *testing.T) {
	// The checker joins findings back to the render they came from - to
	// recover the "# Source:" template - by key. Two different key functions
	// for the same object is a join that silently misses.
	for _, o := range parse(t, twoGenerateNameJobs) {
		if got, want := refOf(o).Key(), o.Key(); got != want {
			t.Errorf("ObjectRef.Key() = %q, manifest.Object.Key() = %q; they must agree", got, want)
		}
	}
}

func TestObjectRefDisplayMarksAGeneratedName(t *testing.T) {
	ref := refOf(parse(t, twoGenerateNameJobs)[0])
	if got, want := ref.Display(), "Job/pre-install-*"; got != want {
		t.Errorf("Display() = %q, want %q", got, want)
	}
}

// A mapping whose keys are not all strings must still be compared key by key.
//
// yaml.v3 decodes `8080: x` into map[any]any, not map[string]any, so walk fell
// through to DeepEqual at the PARENT and reported `.data` as one leaf. That is
// not merely imprecise: remediate turns the reported path into a jsonPointer,
// so idem emitted `jsonPointers: [/data]` and told the user to suppress every
// key in the ConfigMap - including the stable ones. Its own fix block
// manufactured a permanent false negative for all future drift on that object.
//
// Numeric keys are ordinary in Kubernetes: port maps, error-code maps, and
// anything a chart builds with `toYaml` over a dict keyed by number.
func TestCompareDescendsIntoAMappingWithNonStringKeys(t *testing.T) {
	const left = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: ports
data:
  8080: alpha
  9090: stable
`
	const right = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: ports
data:
  8080: beta
  9090: stable
`

	got := compare(t, left, right)
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(got), got)
	}
	if len(got[0].Paths) != 1 {
		t.Fatalf("got %d paths, want 1: %+v", len(got[0].Paths), got[0].Paths)
	}

	// The changed key, not its parent - so the emitted pointer suppresses one
	// key and leaves 9090 answerable.
	if p := got[0].Paths[0].Path.JSONPointer(); p != "/data/8080" {
		t.Errorf("pointer = %q, want /data/8080 - suppressing /data would hide 9090 forever", p)
	}
}

// A list whose elements are unchanged and whose ORDER is not is one finding at
// the list, not one per leaf of every element that moved.
//
// The leaf-per-element form is not merely verbose, it is wrong twice over. It
// describes field churn that is not happening - every element is byte-identical
// on both sides - and remediate then turns each leaf into a jsonPointer, so the
// emitted block permanently un-reconciles the whole list's CONTENTS to suppress
// its ORDER. See the reorder tests in internal/remediate.
func TestAPermutedListIsOneFindingAtTheListRatherThanOnePerMovedLeaf(t *testing.T) {
	got := compare(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  env:
    - {name: ALPHA, value: "1"}
    - {name: BRAVO, value: "2"}
    - {name: CHARLIE, value: "3"}
`, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  env:
    - {name: CHARLIE, value: "3"}
    - {name: ALPHA, value: "1"}
    - {name: BRAVO, value: "2"}
`)
	if len(got) != 1 {
		t.Fatalf("unexpected changes: %+v", got)
	}
	if n := len(got[0].Paths); n != 1 {
		t.Fatalf("Paths = %d, want 1: a permutation is one fact about the list, not %d facts about its leaves", n, n)
	}

	p := got[0].Paths[0]
	if s := p.Path.String(); s != ".spec.env" {
		t.Errorf("Path = %q, want %q - the list itself, since no leaf changed", s, ".spec.env")
	}
	if !p.Reordered {
		t.Errorf("Reordered = false, want true")
	}
	if !p.HasLeft || !p.HasRight {
		t.Errorf("HasLeft/HasRight = (%v, %v), want both true: the list is present on both sides", p.HasLeft, p.HasRight)
	}

	// Both orderings survive into -o json, so a consumer can see what moved.
	left, ok := p.Left.([]any)
	if !ok || len(left) != 3 {
		t.Fatalf("Left = %#v, want the three-element list", p.Left)
	}
	right, ok := p.Right.([]any)
	if !ok || len(right) != 3 {
		t.Fatalf("Right = %#v, want the three-element list", p.Right)
	}
}

// An element whose VALUE changed is not a permutation, however similar it looks.
//
// The multiset has to be equal, not merely the same size: comparing only sorted
// order would call a changed value a reorder, drop it from the fix block, and
// leave real churn with no remediation at all.
func TestAListWithAChangedElementIsNotAReorder(t *testing.T) {
	got := compare(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  env:
    - {name: ALPHA, value: "1"}
    - {name: BRAVO, value: "2"}
`, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  env:
    - {name: BRAVO, value: "2"}
    - {name: ALPHA, value: "9"}
`)
	if len(got) != 1 {
		t.Fatalf("unexpected changes: %+v", got)
	}
	for _, p := range got[0].Paths {
		if p.Reordered {
			t.Fatalf("Reordered = true at %s, want false: ALPHA's value changed, so this is churn a fix block must still cover", p.Path)
		}
	}
	if len(got[0].Paths) == 0 {
		t.Fatal("no paths: a changed value must still be reported")
	}
}

// An element ADDED is not a permutation either.
//
// This pins the `idem diff` avalanche as unchanged. Inserting a container at
// index 0 of a two-element list still reports every leaf of every shifted
// element, which is a real shortcoming of positional matching - and a separate
// problem from this one. Detecting a permutation must not quietly half-fix it.
func TestAListWithAnAddedElementIsNotAReorder(t *testing.T) {
	got := compare(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  containers:
    - {name: app, image: "app:1.0"}
    - {name: sidecar, image: "envoy:1.2"}
`, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  containers:
    - {name: logger, image: "fluentd:2.0"}
    - {name: app, image: "app:1.0"}
    - {name: sidecar, image: "envoy:1.2"}
`)
	if len(got) != 1 {
		t.Fatalf("unexpected changes: %+v", got)
	}
	for _, p := range got[0].Paths {
		if p.Reordered {
			t.Fatalf("Reordered = true at %s, want false: an element was added, so the sets differ", p.Path)
		}
	}
	if n := len(got[0].Paths); n < 2 {
		t.Errorf("Paths = %d, want the positional walk's several: this case is deliberately not collapsed", n)
	}
}

// Equality is on the element's whole VALUE, not on a name key.
//
// Nothing here matches list elements by name - see the note in CLAUDE.md. A
// permutation is recognised because the two multisets are identical, which is
// exact; matching by name would be a heuristic, and would call a reordered list
// whose elements ALSO changed a clean permutation.
func TestAPermutationIsRecognisedByDeepValueNotByAnyNameKey(t *testing.T) {
	got := compare(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
spec:
  rules:
    - {host: a, paths: [{p: "/x"}, {p: "/y"}]}
    - {host: b, paths: [{p: "/z"}]}
`, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
spec:
  rules:
    - {host: b, paths: [{p: "/z"}]}
    - {host: a, paths: [{p: "/x"}, {p: "/y"}]}
`)
	if len(got) != 1 || len(got[0].Paths) != 1 {
		t.Fatalf("unexpected changes: %+v", got)
	}
	if s := got[0].Paths[0].Path.String(); s != ".spec.rules" {
		t.Errorf("Path = %q, want %q", s, ".spec.rules")
	}
	if !got[0].Paths[0].Reordered {
		t.Errorf("Reordered = false, want true: the elements are nested maps and lists, and both sides hold the same two")
	}
}

// A single-element list that "reorders" is not a thing, and a list identical on
// both sides reports nothing at all.
func TestAnIdenticalListIsNotReportedAsReordered(t *testing.T) {
	got := compare(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
spec: {items: ["a", "b"], other: "1"}
`, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
spec: {items: ["a", "b"], other: "2"}
`)
	if len(got) != 1 || len(got[0].Paths) != 1 {
		t.Fatalf("unexpected changes: %+v", got)
	}
	p := got[0].Paths[0]
	if p.Reordered {
		t.Errorf("Reordered = true at %s, want false: the list is identical, only .spec.other changed", p.Path)
	}
	if s := p.Path.String(); s != ".spec.other" {
		t.Errorf("Path = %q, want %q", s, ".spec.other")
	}
}

// -o json carries the marker, so a consumer gating on .findings[].paths[] can
// tell a reorder from a leaf it could suppress.
func TestJSONOutputMarksAReorderedList(t *testing.T) {
	got := compare(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
spec: {items: ["a", "b"]}
`, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
spec: {items: ["b", "a"]}
`)
	raw, err := json.Marshal(got[0].Paths[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Path      string `json:"path"`
		Pointer   string `json:"pointer"`
		Reordered bool   `json:"reordered"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Reordered {
		t.Errorf("reordered = false, want true in %s", raw)
	}
	if decoded.Pointer != "/spec/items" {
		t.Errorf("pointer = %q, want %q", decoded.Pointer, "/spec/items")
	}

	// An ordinary leaf must not carry the key at all, so the field's presence
	// means something.
	plain := compare(t, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
data: {a: "1"}
`, `
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
data: {a: "2"}
`)
	raw, err = json.Marshal(plain[0].Paths[0])
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if strings.Contains(string(raw), "reordered") {
		t.Errorf("plain leaf carries a reordered key: %s", raw)
	}
}

// An empty list is not reordered, and the length guard is what says so.
//
// Called directly because YAML cannot currently hand walk this pair: `items:`
// decodes to an untyped nil, which never reaches the sequence branch. It is
// pinned anyway because the failure is silent - a nil []any and an empty one
// are both length zero, DeepEqual reports them as different, and both
// canonicalise to nothing, so the multiset comparison alone would call them a
// permutation and invent a finding about an empty list.
func TestAnEmptyListIsNeverReordered(t *testing.T) {
	if permuted(nil, []any{}) {
		t.Errorf("permuted(nil, []any{}) = true, want false")
	}
	if permuted([]any{"a"}, []any{"b"}) {
		t.Errorf("permuted([a], [b]) = true, want false: one element cannot be out of order")
	}
}
