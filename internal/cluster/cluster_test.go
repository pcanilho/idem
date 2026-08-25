package cluster

import (
	"strings"
	"testing"
	"time"
)

// The fixtures below are the real shapes from a live estate, not invented ones.
const workloadList = `{
  "items": [
    {
      "kind": "Deployment",
      "metadata": {
        "name": "lab-harbor-registry",
        "namespace": "lab",
        "generation": 12,
        "creationTimestamp": "2024-08-09T00:00:00Z",
        "labels": {
          "app.kubernetes.io/instance": "lab",
          "app.kubernetes.io/managed-by": "Helm",
          "chart": "harbor"
        },
        "annotations": {
          "deployment.kubernetes.io/revision": "660",
          "argocd.argoproj.io/tracking-id": "lab-app:apps/Deployment:lab/lab-harbor-registry"
        }
      },
      "spec": {"template": {"metadata": {"annotations": {
        "checksum/configmap": "abc", "checksum/secret": "def", "prometheus.io/scrape": "true"
      }}}}
    },
    {
      "kind": "Deployment",
      "metadata": {
        "name": "podinfo",
        "namespace": "demo",
        "generation": 5,
        "creationTimestamp": "2026-08-12T00:00:00Z",
        "labels": {
          "app.kubernetes.io/managed-by": "Helm",
          "helm.sh/chart": "podinfo-6.14.1",
          "helm.toolkit.fluxcd.io/name": "podinfo",
          "helm.toolkit.fluxcd.io/namespace": "demo"
        },
        "annotations": {"meta.helm.sh/release-name": "podinfo"}
      },
      "spec": {"template": {"metadata": {"annotations": {}}}}
    },
    {
      "kind": "StatefulSet",
      "metadata": {
        "name": "client",
        "namespace": "demo",
        "generation": 7,
        "creationTimestamp": "2026-08-12T00:00:00Z",
        "labels": {"kustomize.toolkit.fluxcd.io/name": "demo"},
        "annotations": {}
      },
      "spec": {"template": {"metadata": {"annotations": {}}}}
    }
  ]
}`

func parse(t *testing.T) []Workload {
	t.Helper()
	got, err := parseWorkloads([]byte(workloadList))
	if err != nil {
		t.Fatalf("parseWorkloads() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("parseWorkloads() = %d workloads, want 3", len(got))
	}
	return got
}

func TestADeploymentsRevisionComesFromItsOwnAnnotation(t *testing.T) {
	// generation counts spec changes; the revision annotation counts rollouts,
	// and rollouts are what churn actually costs.
	if got := parse(t)[0].Revision; got != 660 {
		t.Errorf("Revision = %d, want 660 rather than the generation", got)
	}
}

func TestAStatefulSetFallsBackToItsGeneration(t *testing.T) {
	// StatefulSets and DaemonSets record no revision, so the nearest honest
	// stand-in is how many times their spec changed.
	if got := parse(t)[2].Revision; got != 7 {
		t.Errorf("Revision = %d, want the generation", got)
	}
}

func TestOnlyChecksumAnnotationsAreCollected(t *testing.T) {
	got := parse(t)[0].Checksums

	if len(got) != 2 {
		t.Fatalf("Checksums = %v, want the two checksum annotations only", got)
	}
	// Sorted, because Go map iteration order is deliberately random and idem's
	// own output must not vary between runs.
	if got[0] != "checksum/configmap" || got[1] != "checksum/secret" {
		t.Errorf("Checksums = %v, want them sorted", got)
	}
}

func TestArgoCDTrackingIdNamesTheApplication(t *testing.T) {
	// "lab-app:apps/Deployment:lab/lab-harbor-registry" - the Application is
	// everything before the first colon, and it is what the reader needs.
	owner := parse(t)[0].Owner

	if owner.Engine != "argocd" || owner.Name != "lab-app" {
		t.Errorf("Owner = %+v, want argocd lab-app", owner)
	}
	if owner.String() != "argocd lab-app" {
		t.Errorf("String() = %q", owner.String())
	}
}

func TestFluxIsCreditedOverTheHelmInstallItPerformed(t *testing.T) {
	// A Flux-managed release carries BOTH helm.toolkit.fluxcd.io/name and
	// meta.helm.sh/release-name, because Flux installs through Helm. Flux is
	// what reconciles it, so Flux is the answer; checking helm first would
	// name the mechanism instead of the owner.
	owner := parse(t)[1].Owner

	if owner.Engine != "flux" || owner.Name != "podinfo" {
		t.Errorf("Owner = %+v, want flux podinfo", owner)
	}
	if owner.Chart != "podinfo-6.14.1" {
		t.Errorf("Chart = %q, want podinfo-6.14.1", owner.Chart)
	}
}

func TestAKustomizationOwnedWorkloadIsCreditedToFlux(t *testing.T) {
	owner := parse(t)[2].Owner

	if owner.Engine != "flux" || owner.Name != "demo" {
		t.Errorf("Owner = %+v, want flux demo", owner)
	}
}

func TestAWorkloadNothingClaimedHasNoOwner(t *testing.T) {
	got, err := parseWorkloads([]byte(`{"items":[{"kind":"Deployment","metadata":{"name":"x","namespace":"y"},"spec":{"template":{"metadata":{}}}}]}`))
	if err != nil {
		t.Fatalf("parseWorkloads() error = %v", err)
	}
	if got[0].Owner.String() != "" {
		t.Errorf("Owner = %q, want empty rather than a guess", got[0].Owner)
	}
}

func TestCreationTimeIsRead(t *testing.T) {
	want := time.Date(2024, 8, 9, 0, 0, 0, 0, time.UTC)

	if got := parse(t)[0].Created; !got.Equal(want) {
		t.Errorf("Created = %v, want %v", got, want)
	}
}

func TestUnreadableOutputIsReported(t *testing.T) {
	// kubectl printing something unexpected must not read as an empty cluster.
	if _, err := parseWorkloads([]byte("not json")); err == nil {
		t.Error("parseWorkloads() error = nil, want the garbage reported")
	}
}

func TestAMissingKubectlIsNamed(t *testing.T) {
	_, err := New("kubectl-does-not-exist", "").Workloads(t.Context())

	if err == nil {
		t.Fatal("Workloads() error = nil, want the missing binary reported")
	}
	if !strings.Contains(err.Error(), "kubectl-does-not-exist") {
		t.Errorf("error = %q, want it to name the binary idem tried", err)
	}
}

// --- normalisation, all three cases found by running against a real namespace ---

func stripped(t *testing.T, body string) map[string]any {
	t.Helper()
	got, err := parseLive([]byte(`{"items":[` + body + `]}`))
	if err != nil || len(got) != 1 {
		t.Fatalf("parseLive() = %v, %v", got, err)
	}
	return got[0].Live.Body
}

func TestStringDataIsFoldedIntoDataTheWayTheAPIServerDoes(t *testing.T) {
	// stringData is write-only: Kubernetes merges it into data on write and
	// never returns it. Comparing raw reports every Secret written that way
	// as rewritten when nothing touched it.
	body := stripped(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"stringData":{"TOKEN":"x"}}`)

	if _, present := body["stringData"]; present {
		t.Error("stringData survived, want it folded away")
	}
	data, ok := body["data"].(map[string]any)
	if !ok || data["TOKEN"] != "eA==" {
		t.Errorf("data = %v, want TOKEN base64-encoded", body["data"])
	}
}

func TestAnExistingDataKeyIsKeptWhenStringDataIsFolded(t *testing.T) {
	body := stripped(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"data":{"A":"eA=="},"stringData":{"B":"y"}}`)

	data := body["data"].(map[string]any)
	if data["A"] != "eA==" || data["B"] != "eQ==" {
		t.Errorf("data = %v, want both keys", data)
	}
}

func TestACustomResourceCalledSecretIsLeftAlone(t *testing.T) {
	// Only core/v1 Secret has this rule.
	body := stripped(t, `{"apiVersion":"example.com/v1","kind":"Secret","metadata":{"name":"s"},"stringData":{"A":"x"}}`)

	if _, present := body["stringData"]; !present {
		t.Error("stringData was folded on a non-core kind")
	}
}

func TestAnEmptyMapIsTreatedAsAbsent(t *testing.T) {
	// An object applied with `data: {}` comes back with no data key at all.
	body := stripped(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{}}`)

	if _, present := body["data"]; present {
		t.Error("empty data survived, want it treated as absent")
	}
}

func TestANullIsTreatedAsAbsent(t *testing.T) {
	// A chart emitting `data:` with nothing under it produces null rather than
	// an empty map, and the API server drops it just the same. Found in a real
	// namespace, where it read as the whole field having been deleted.
	body := stripped(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":null}`)

	if _, present := body["data"]; present {
		t.Error("null data survived, want it treated as absent")
	}
}

func TestServerOwnedFieldsAreRemoved(t *testing.T) {
	// Comparing these would report the cluster being a cluster.
	body := stripped(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c","resourceVersion":"9","uid":"u"},"data":{"k":"v"},"status":{"x":1}}`)

	if _, present := body["status"]; present {
		t.Error("status survived")
	}
	meta := body["metadata"].(map[string]any)
	for _, field := range []string{"resourceVersion", "uid"} {
		if _, present := meta[field]; present {
			t.Errorf("metadata.%s survived", field)
		}
	}
}
