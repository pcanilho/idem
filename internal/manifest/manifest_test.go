package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseExtractsObjectIdentity(t *testing.T) {
	in := `
apiVersion: v1
kind: Secret
metadata:
  name: db-creds
  namespace: prod
data:
  password: aGVsbG8=
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
`

	objs, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want 2", len(objs))
	}

	if got, want := objs[0].Key(), "v1|Secret|prod|db-creds"; got != want {
		t.Errorf("objs[0].Key() = %q, want %q", got, want)
	}
	if got, want := objs[1].Key(), "apps/v1|Deployment|prod|api"; got != want {
		t.Errorf("objs[1].Key() = %q, want %q", got, want)
	}
}

func TestObjectDisplayOmitsNamespaceWhenUnset(t *testing.T) {
	// `helm template` does not set metadata.namespace unless the chart does;
	// the namespace is applied at install time. So the common case is the
	// short form, which is what the README shows.
	in := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
`
	objs, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1", len(objs))
	}
	if got, want := objs[0].Display(), "Deployment/api"; got != want {
		t.Errorf("Display() = %q, want %q", got, want)
	}
}

func TestObjectDisplayIncludesNamespaceWhenSet(t *testing.T) {
	// Without it, prod/api and staging/api both render as "Deployment/api",
	// which is ambiguous in any multi-namespace run.
	in := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
`
	objs, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := objs[0].Display(), "Deployment/prod/api"; got != want {
		t.Errorf("Display() = %q, want %q", got, want)
	}
}

func TestParseSkipsEmptyDocuments(t *testing.T) {
	// `helm template` routinely emits empty documents where a conditional
	// suppressed an entire template. They are not objects, and counting them
	// would create phantom entries that differ between renders for no reason.
	in := `
---
# a comment renders to nothing
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: only-real-one
---
`

	objs, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1: %+v", len(objs), objs)
	}
	if got, want := objs[0].Key(), "v1|ConfigMap||only-real-one"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
}

func TestParseRejectsDocumentMissingKind(t *testing.T) {
	// A document that is valid YAML but not a Kubernetes object means we are
	// not looking at rendered manifests. Failing loudly beats silently
	// comparing an empty set and reporting "no differences".
	in := `
foo: bar
baz: 1
`
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("Parse succeeded on a non-Kubernetes document, want error")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error %q should mention the missing 'kind'", err)
	}
}

func TestParseCapturesSourceTemplateFromHelmComment(t *testing.T) {
	// `helm template` prefixes each document with the template that produced
	// it. That comment is the only link between a rendered object and its
	// source file, so losing it means findings cannot say where to look.
	in := `---
# Source: postgresql/templates/secrets.yaml
apiVersion: v1
kind: Secret
metadata:
  name: pg-postgresql
---
# Source: postgresql/templates/primary/svc.yaml
apiVersion: v1
kind: Service
metadata:
  name: pg-postgresql-hl
`
	objs, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want 2", len(objs))
	}
	if got, want := objs[0].Source, "postgresql/templates/secrets.yaml"; got != want {
		t.Errorf("objs[0].Source = %q, want %q", got, want)
	}
	if got, want := objs[1].Source, "postgresql/templates/primary/svc.yaml"; got != want {
		t.Errorf("objs[1].Source = %q, want %q", got, want)
	}
}

func TestParseLeavesSourceEmptyWhenAbsent(t *testing.T) {
	// `argocd app manifests` output has been through the repo-server's
	// decoding and carries no Source comments. Absent must read as absent,
	// never as a guess.
	in := `
apiVersion: v1
kind: Secret
metadata:
  name: pg
`
	objs, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if objs[0].Source != "" {
		t.Errorf("Source = %q, want empty", objs[0].Source)
	}
}

func TestParseRejectsObjectWithoutName(t *testing.T) {
	// Every unnamed object collapses to the same identity key, so all but the
	// last would be silently dropped before comparison. In a tool whose claim
	// is "your output is stable", silently discarding objects is a soundness
	// hole, not a nicety.
	in := `
apiVersion: v1
kind: Secret
metadata:
  namespace: prod
`
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("Parse accepted an object with no metadata.name, want error")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error %q should mention the missing name", err)
	}
}

func TestParseReportsWrongTypedKindAccurately(t *testing.T) {
	// "kind: 3" is present but not a string. Reporting it as *missing* sends
	// the reader looking for the wrong problem.
	in := `
apiVersion: v1
kind: 3
metadata:
  name: x
`
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("Parse accepted a non-string kind, want error")
	}
	if strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q says 'missing' for a kind that is present but mistyped", err)
	}
}

func TestParseRealHelmOutput(t *testing.T) {
	// A regression test against genuine `helm template` output, captured from
	// oci://registry-1.docker.io/bitnamicharts/postgresql. The Source capture
	// depends on where yaml.v3 chooses to attach a document's leading comment,
	// which is an implementation detail rather than a documented contract - so
	// it must be pinned against real output, not a hand-written fixture.
	f, err := os.Open(filepath.Join("testdata", "helm-real-output.yaml"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	objs, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("got %d objects, want 3", len(objs))
	}

	want := []struct{ display, source string }{
		{"ServiceAccount/argocd/pg-postgresql", "postgresql/templates/serviceaccount.yaml"},
		{"Secret/argocd/pg-postgresql", "postgresql/templates/secrets.yaml"},
		{"Service/argocd/pg-postgresql", "postgresql/templates/primary/svc.yaml"},
	}
	for i, w := range want {
		if got := objs[i].Display(); got != w.display {
			t.Errorf("objs[%d].Display() = %q, want %q", i, got, w.display)
		}
		if got := objs[i].Source; got != w.source {
			t.Errorf("objs[%d].Source = %q, want %q", i, got, w.source)
		}
	}
}

func TestParseAcceptsGenerateNameObjects(t *testing.T) {
	// `helm template` emits hook Jobs with generateName and no name. Rejecting
	// them fails the whole run on a very common chart shape. In RENDERED output
	// they are stable - the server assigns the suffix at apply time, not render
	// time - so they can be compared like anything else.
	in := `
apiVersion: batch/v1
kind: Job
metadata:
  generateName: migrate-
  annotations:
    "helm.sh/hook": post-install
`
	objs, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse rejected a generateName object: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1", len(objs))
	}
	if got, want := objs[0].Display(), "Job/migrate-*"; got != want {
		t.Errorf("Display() = %q, want %q", got, want)
	}
	// Identity must be stable across renders, and must not collide with a
	// really-named object that happens to share the prefix.
	if got, want := objs[0].Key(), "batch/v1|Job||generateName:migrate-"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
}

func TestParseStillRejectsObjectWithNeitherNameNorGenerateName(t *testing.T) {
	in := `
apiVersion: v1
kind: Secret
metadata:
  namespace: prod
`
	if _, err := Parse(strings.NewReader(in)); err == nil {
		t.Fatal("Parse accepted an object with neither name nor generateName")
	}
}
