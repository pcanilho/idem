package doctor

import (
	"slices"
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/manifest"
)

func objects(t *testing.T, body string) []manifest.Object {
	t.Helper()
	got, err := manifest.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return got
}

// The shapes below are what a real cluster returned for the simplest possible
// Service: eight fields the chart never wrote.
const sentService = `
apiVersion: v1
kind: Service
metadata: {name: probe-svc, namespace: default}
spec:
  selector: {app: probe}
  ports:
    - port: 80
`

const returnedService = `
apiVersion: v1
kind: Service
metadata: {name: probe-svc, namespace: default}
spec:
  selector: {app: probe}
  type: ClusterIP
  clusterIP: 172.17.0.0
  sessionAffinity: None
  ports:
    - port: 80
      protocol: TCP
      targetPort: 80
`

func rewrites(t *testing.T, sent, returned string) []Rewrite {
	t.Helper()
	got, err := Rewrites(objects(t, sent), objects(t, returned))
	if err != nil {
		t.Fatalf("Rewrites() error = %v", err)
	}
	return got
}

func paths(r Rewrite) []string {
	out := make([]string, len(r.Changes))
	for i, c := range r.Changes {
		out[i] = c.Path.String()
	}
	return out
}

func TestWhatTheClusterAddedIsReported(t *testing.T) {
	// Admission is synchronous, so this happens between sending an object and
	// it being stored - where no amount of rendering can see it.
	got := rewrites(t, sentService, returnedService)

	if len(got) != 1 {
		t.Fatalf("Rewrites() = %+v, want one object", got)
	}
	for _, want := range []string{".spec.type", ".spec.clusterIP", ".spec.sessionAffinity"} {
		if !contains(paths(got[0]), want) {
			t.Errorf("paths = %v, want %q", paths(got[0]), want)
		}
	}
}

func TestAnAssignedValueIsDistinguishedFromAPlainDefault(t *testing.T) {
	// sessionAffinity: None is the same everywhere and reads as noise. A
	// clusterIP is minted for this object and is worth noticing.
	got := rewrites(t, sentService, returnedService)

	for _, c := range got[0].Changes {
		switch c.Path.String() {
		case ".spec.clusterIP":
			if !c.Assigned {
				t.Error("clusterIP not marked as assigned")
			}
		case ".spec.sessionAffinity":
			if c.Assigned {
				t.Error("sessionAffinity marked as assigned, want a plain default")
			}
		}
	}
}

func TestPortsAndResourcesAreSuppressedRatherThanReportedWrongly(t *testing.T) {
	// The API server canonicalises IntOrString ports and resource quantities,
	// so `targetPort: "80"` comes back `80` and `cpu: 0.1` comes back `100m`.
	// Structural comparison calls those differences when nothing changed, and
	// reporting them wrongly is worse than not reporting them.
	got := rewrites(t, sentService, returnedService)

	if contains(paths(got[0]), ".spec.ports[0].targetPort") {
		t.Errorf("paths = %v, want targetPort suppressed", paths(got[0]))
	}
	if got[0].Suppressed == 0 {
		t.Error("Suppressed = 0, want the dropped fields counted rather than vanishing")
	}
}

func TestAnObjectTheClusterLeftAloneIsNotReported(t *testing.T) {
	same := `
apiVersion: v1
kind: ConfigMap
metadata: {name: quiet, namespace: default}
data: {greeting: hello}
`
	if got := rewrites(t, same, same); len(got) != 0 {
		t.Errorf("Rewrites() = %+v, want none", got)
	}
}

func TestServerOwnedFieldsAreNotMistakenForRewrites(t *testing.T) {
	// Every returned object carries a creationTimestamp, a uid and the
	// last-applied annotation. Reporting those would drown the real answer.
	sent := `
apiVersion: v1
kind: ConfigMap
metadata: {name: c, namespace: default}
data: {k: v}
`
	returned := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: c
  namespace: default
  uid: 1b5cdfb1
  creationTimestamp: "2026-08-22T09:09:59Z"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "{}"
data: {k: v}
`
	if got := rewrites(t, sent, returned); len(got) != 0 {
		t.Errorf("Rewrites() = %+v, want none - all of that is the API server's own", got)
	}
}

func TestAFieldTheRenderHasAndTheClusterDroppedIsNotARewrite(t *testing.T) {
	// A different story, and one the API server tells far better than idem
	// could - it rejects what it will not accept.
	sent := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {k: v}\nbogus: yes\n"
	returned := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {k: v}\n"

	got := rewrites(t, sent, returned)

	for _, r := range got {
		if contains(paths(r), ".bogus") {
			t.Errorf("paths = %v, want a dropped field left out", paths(r))
		}
	}
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func TestObjectsPairUpWhenOnlyTheClusterSuppliedTheNamespace(t *testing.T) {
	// A render usually carries no metadata.namespace -- helm sets
	// .Release.Namespace without writing the field -- and kubectl fills it in
	// from the context. Left in, the two sides have different identities and
	// never pair up, so every object reads as added-and-removed and no rewrite
	// is ever found. That failure is silent, which is what makes it dangerous.
	sent := "apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {selector: {app: s}}\n"
	returned := "apiVersion: v1\nkind: Service\nmetadata: {name: s, namespace: argocd}\nspec: {selector: {app: s}, type: ClusterIP}\n"

	got := rewrites(t, sent, returned)

	if len(got) != 1 {
		t.Fatalf("Rewrites() = %+v, want the objects paired", got)
	}
	if !contains(paths(got[0]), ".spec.type") {
		t.Errorf("paths = %v, want the actual rewrite found", paths(got[0]))
	}
}

func TestTheSuppliedNamespaceIsNotItselfARewrite(t *testing.T) {
	sent := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {k: v}\n"
	returned := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: argocd}\ndata: {k: v}\n"

	if got := rewrites(t, sent, returned); len(got) != 0 {
		t.Errorf("Rewrites() = %+v, want none - the context supplied that", got)
	}
}
