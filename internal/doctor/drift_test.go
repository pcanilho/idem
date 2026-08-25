package doctor

import (
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/cluster"
	"github.com/pcanilho/idem/internal/manifest"
)

func object(t *testing.T, body string) manifest.Object {
	t.Helper()
	got, err := manifest.Parse(strings.NewReader(body))
	if err != nil || len(got) == 0 {
		t.Fatalf("parse fixture: %v", err)
	}
	return got[0]
}

// gitea is the real shape that motivated this: External Secrets populating a
// Secret that ArgoCD applied empty, with NO managedFields at all.
func gitea(t *testing.T) cluster.LiveObject {
	return cluster.LiveObject{
		Applied: object(t, `
apiVersion: v1
kind: Secret
metadata: {name: lab-gitea-mirror, namespace: lab}
data: {}
`),
		Live: object(t, `
apiVersion: v1
kind: Secret
metadata: {name: lab-gitea-mirror, namespace: lab}
data:
  GITEA_TOKEN: eA==
  GITHUB_TOKEN: eA==
`),
		HasApplied: true,
		Labels:     map[string]string{"reconcile.external-secrets.io/managed": "true"},
	}
}

func TestAValuePopulatedAfterApplyIsReported(t *testing.T) {
	// A dry run reproduces admission mutation and is blind to this: the write
	// happens later, so only the live object's own record of what was applied
	// can show it.
	got := PostApply([]cluster.LiveObject{gitea(t)})

	if len(got) != 1 {
		t.Fatalf("PostApply() = %+v, want one drift", got)
	}
	if got[0].Object.Name != "lab-gitea-mirror" {
		t.Errorf("Object = %q", got[0].Object.Name)
	}
	if len(got[0].Changes) != 2 {
		t.Errorf("Changes = %+v, want both keys", got[0].Changes)
	}
}

func TestTheWriterIsNamedFromItsLabelWhenManagedFieldsIsEmpty(t *testing.T) {
	// The case that decides the design. managedFields would be the tidy
	// answer and is simply absent here, because a client-side apply records
	// nothing there. Supporting only the SSA route would have found the drift
	// and been unable to say who caused it.
	got := PostApply([]cluster.LiveObject{gitea(t)})

	if got[0].Writer != "external-secrets" {
		t.Errorf("Writer = %q, want external-secrets", got[0].Writer)
	}
	if !strings.Contains(got[0].Evidence, "reconcile.external-secrets.io/managed") {
		t.Errorf("Evidence = %q, want the label named", got[0].Evidence)
	}
}

func TestAFieldManagerNamesTheWriterWhenThereIsNoLabel(t *testing.T) {
	o := gitea(t)
	o.Labels = nil
	o.Managers = []string{"argocd-application-controller", "vault-secrets-operator"}

	got := PostApply([]cluster.LiveObject{o})

	if got[0].Writer != "vault-secrets-operator" {
		t.Errorf("Writer = %q, want the manager that is not the applier", got[0].Writer)
	}
}

func TestTheApplierIsNotBlamedForItsOwnApply(t *testing.T) {
	o := gitea(t)
	o.Labels = nil
	o.Managers = []string{"argocd-application-controller"}

	if got := PostApply([]cluster.LiveObject{o})[0].Writer; got != "" {
		t.Errorf("Writer = %q, want none - that manager is what applied it", got)
	}
}

func TestAnUnidentifiedWriterIsAdmittedRatherThanGuessed(t *testing.T) {
	o := gitea(t)
	o.Labels = nil

	got := PostApply([]cluster.LiveObject{o})

	if len(got) != 1 {
		t.Fatalf("PostApply() = %+v, want the drift still reported", got)
	}
	if got[0].Writer != "" {
		t.Errorf("Writer = %q, want empty rather than a guess", got[0].Writer)
	}
}

func TestAnObjectMatchingWhatWasAppliedIsNotDrift(t *testing.T) {
	same := object(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: quiet}\ndata: {k: eA==}\n")
	o := cluster.LiveObject{Applied: same, Live: same, HasApplied: true}

	if got := PostApply([]cluster.LiveObject{o}); len(got) != 0 {
		t.Errorf("PostApply() = %+v, want none", got)
	}
}

func TestAnObjectWithNoAppliedRecordIsSkipped(t *testing.T) {
	// Without a baseline there is nothing to compare, and reporting the whole
	// object as drift would be noise on every object nobody applied with
	// kubectl.
	o := cluster.LiveObject{Live: object(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: x}\ndata: {k: eA==}\n")}

	if got := PostApply([]cluster.LiveObject{o}); len(got) != 0 {
		t.Errorf("PostApply() = %+v, want it skipped", got)
	}
}

func TestDriftIsOrderedDeterministically(t *testing.T) {
	a := gitea(t)
	b := gitea(t)
	b.Applied.Name = "aaa-first"
	b.Live.Name = "aaa-first"
	b.Applied.Body["metadata"].(map[string]any)["name"] = "aaa-first"
	b.Live.Body["metadata"].(map[string]any)["name"] = "aaa-first"

	got := PostApply([]cluster.LiveObject{a, b})

	if len(got) != 2 || got[0].Object.Name != "aaa-first" {
		t.Errorf("PostApply() = %+v, want sorted", got)
	}
}
