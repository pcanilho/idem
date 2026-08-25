package report

import (
	"testing"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/diff"
)

func workload(kind, name, field string) check.Finding {
	f := finding("t.yaml", name, field)
	f.Change.Object = diff.ObjectRef{APIVersion: "apps/v1", Kind: kind, Name: name}
	return f
}

func secretFinding(name, field string) check.Finding {
	f := finding("t.yaml", name, field)
	f.Change.Object = diff.ObjectRef{APIVersion: "v1", Kind: "Secret", Name: name}
	return f
}

const checksumPath = `.spec.template.metadata.annotations.checksum/secrets`

func TestASecretThatRollsWorkloadsSaysHowMany(t *testing.T) {
	// The right-hand column is the whole product in three words.
	secret := secretFinding("creds", ".data.password")
	all := []check.Finding{
		secret,
		workload("Deployment", "api", checksumPath),
		workload("Deployment", "ui", checksumPath),
	}

	got := consequenceOf(secret, all)

	if got.Kind != "rolls" || got.Workloads != 2 {
		t.Fatalf("consequenceOf() = %+v, want 2 rolling", got)
	}
	if got.Text != "rolls 2 Deployments" {
		t.Errorf("Text = %q, want %q", got.Text, "rolls 2 Deployments")
	}
}

func TestASecretNothingHashesIsTheDangerousCase(t *testing.T) {
	// The failure that cost two years: the value drifts, nothing restarts, so
	// nothing alerts either.
	secret := secretFinding("pg", ".data.password")

	got := consequenceOf(secret, []check.Finding{secret})

	if got.Kind != "silent" {
		t.Fatalf("consequenceOf() = %+v, want silent", got)
	}
	if got.Text != "silent, no checksum" {
		t.Errorf("Text = %q", got.Text)
	}
}

func TestAnObjectThatCannotRollIsNotCounted(t *testing.T) {
	// An Ingress can carry the very same checksum annotation and restart
	// nothing at all.
	secret := secretFinding("creds", ".data.password")
	ingress := finding("t.yaml", "web", checksumPath)
	ingress.Change.Object = diff.ObjectRef{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Name: "web"}

	got := consequenceOf(secret, []check.Finding{secret, ingress})

	if got.Kind != "silent" {
		t.Errorf("consequenceOf() = %+v, want silent - an Ingress rolls nothing", got)
	}
}

func TestAWorkloadChangingSomethingElseIsNotCounted(t *testing.T) {
	// Only a changed checksum annotation ties the two together. A Deployment
	// whose replica count differs says nothing about this Secret.
	secret := secretFinding("creds", ".data.password")
	other := workload("Deployment", "api", ".spec.replicas")

	if got := consequenceOf(secret, []check.Finding{secret, other}); got.Kind != "silent" {
		t.Errorf("consequenceOf() = %+v, want silent", got)
	}
}

func TestMixedWorkloadKindsAreNamedGenerically(t *testing.T) {
	// Guessing one of the kinds would be wrong for the others.
	secret := secretFinding("creds", ".data.password")
	all := []check.Finding{
		secret,
		workload("Deployment", "api", checksumPath),
		workload("StatefulSet", "db", checksumPath),
	}

	if got := consequenceOf(secret, all); got.Text != "rolls 2 workloads" {
		t.Errorf("Text = %q, want %q", got.Text, "rolls 2 workloads")
	}
}

func TestOneRollingWorkloadIsSingular(t *testing.T) {
	secret := secretFinding("creds", ".data.password")
	all := []check.Finding{secret, workload("Deployment", "api", checksumPath)}

	if got := consequenceOf(secret, all); got.Text != "rolls 1 Deployment" {
		t.Errorf("Text = %q, want %q", got.Text, "rolls 1 Deployment")
	}
}

func TestIdemMakesNoClaimAboutAnArbitraryObject(t *testing.T) {
	// A Deployment whose own annotation churns is reported as a finding; what
	// it costs is not something idem can say without inventing it.
	w := workload("Deployment", "api", checksumPath)

	if got := consequenceOf(w, []check.Finding{w}); got.Kind != "" || got.Text != "" {
		t.Errorf("consequenceOf() = %+v, want no claim", got)
	}
}
