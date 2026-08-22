package doctor

import (
	"slices"
	"strings"

	"github.com/pcanilho/idem/internal/cluster"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/manifest"
)

// appliers are the field managers that put objects there in the first place.
// A manager outside this set wrote afterwards, which is the whole finding.
var appliers = []string{
	"kubectl", "kubectl-client-side-apply", "kubectl-edit", "kubectl-create",
	"argocd-application-controller", "argocd-controller",
	"helm", "helm-controller",
}

// writers maps a marker some controller leaves to the controller's name.
//
// managedFields would be the tidy way to answer this, and it is often simply
// empty: a client-side apply records nothing there. On the estate that
// motivated this, the Secret External Secrets was rewriting had NO
// managedFields at all and was identifiable only by its label. So both routes
// are needed, and where neither answers, idem says it does not know.
var writers = []struct{ marker, name string }{
	{"reconcile.external-secrets.io/managed", "external-secrets"},
	{"external-secrets.io/", "external-secrets"},
	{"cert-manager.io/certificate-name", "cert-manager"},
	{"kubed.appscode.com/sync", "kubed"},
	{"replicator.v1.mittwald.de/", "kubernetes-replicator"},
	{"secretgen.carvel.dev/", "secretgen-controller"},
	{"reflector.v1.k8s.emberstack.com/", "reflector"},
}

// Drift is an object something changed after it was applied.
//
// A dry run reproduces admission mutation and is blind to this: the write
// happens later, so the only way to see it is to compare the live object with
// its own record of what was applied.
type Drift struct {
	Object  diff.ObjectRef
	Changes []diff.PathDiff

	// Writer is the controller idem believes did it, empty when it cannot say.
	Writer string

	// Evidence is what identified the writer - a label, or a field manager.
	Evidence string
}

// PostApply reports objects that differ from what was last applied to them.
//
// Objects with no last-applied record are skipped rather than guessed at:
// without a baseline there is nothing to compare, and reporting the whole
// object as drift would be noise.
func PostApply(objects []cluster.LiveObject) []Drift {
	var out []Drift

	for _, o := range objects {
		if !o.HasApplied {
			continue
		}

		changes, err := diff.Compare(
			[]manifest.Object{o.Applied},
			[]manifest.Object{o.Live},
		)
		if err != nil || len(changes) == 0 {
			continue
		}

		writer, evidence := attribute(o)
		out = append(out, Drift{
			Object:   changes[0].Object,
			Changes:  changes[0].Paths,
			Writer:   writer,
			Evidence: evidence,
		})
	}

	slices.SortFunc(out, func(a, b Drift) int {
		return strings.Compare(a.Object.Key(), b.Object.Key())
	})
	return out
}

// attribute works out who wrote after the apply.
func attribute(o cluster.LiveObject) (string, string) {
	for _, w := range writers {
		if marker, ok := markerIn(o.Labels, w.marker); ok {
			return w.name, "label " + marker
		}
		if marker, ok := markerIn(o.Annotations, w.marker); ok {
			return w.name, "annotation " + marker
		}
	}

	for _, manager := range o.Managers {
		if !slices.Contains(appliers, manager) {
			return manager, "field manager"
		}
	}
	return "", ""
}

// markerIn matches a marker either exactly or as a prefix, since some
// controllers namespace a family of keys rather than using one.
func markerIn(set map[string]string, marker string) (string, bool) {
	for key := range set {
		if key == marker || (strings.HasSuffix(marker, "/") && strings.HasPrefix(key, marker)) {
			return key, true
		}
	}
	return "", false
}
