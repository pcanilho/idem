package report

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pcanilho/idem/internal/check"
)

// checksumAnnotation is the path fragment that makes a workload roll.
//
// A chart that hashes a Secret into a pod annotation is doing the right thing:
// it is how you get a rollout when a credential legitimately changes. It is
// also what turns a non-deterministic Secret into pods restarting on every
// sync, forever.
const checksumAnnotation = "/annotations/checksum"

// rollable are the kinds where a changed pod annotation actually restarts
// something. An Ingress can carry the same annotation and roll nothing.
var rollable = []string{"Deployment", "StatefulSet", "DaemonSet"}

// Consequence is what a finding costs, in the reader's terms.
//
// It is an OBSERVATION, never an inference: idem does not claim a workload's
// checksum annotation was derived from a given Secret - that value is a hash
// and cannot be inverted. It reports that the two change together across
// renders, which is the strongest honest claim available.
type Consequence struct {
	// Kind is "rolls", "silent", or empty when idem makes no claim.
	Kind string

	// Workloads is how many rollable objects changed a checksum annotation
	// in the same render.
	Workloads int

	// Text is the human form, e.g. "rolls 2 Deployments".
	Text string
}

// carriesSecrets are the kinds whose churn a workload can be hashing.
var carriesSecrets = []string{"Secret", "ConfigMap"}

// consequenceOf works out what one finding costs, given everything else that
// changed in the same chart.
func consequenceOf(f check.Finding, all []check.Finding) Consequence {
	if !slices.Contains(carriesSecrets, f.Change.Object.Kind) {
		// idem has nothing to say about the consequence of an arbitrary object
		// changing, and inventing something would be worse than silence.
		return Consequence{}
	}

	var kinds []string
	for _, other := range all {
		if !slices.Contains(rollable, other.Change.Object.Kind) || !changesChecksum(other) {
			continue
		}
		kinds = append(kinds, other.Change.Object.Kind)
	}

	if len(kinds) == 0 {
		// The dangerous case, and the one that cost two years: the value drifts
		// and nothing restarts, so nothing alerts either.
		return Consequence{Kind: "silent", Text: "silent, no checksum"}
	}

	return Consequence{
		Kind:      "rolls",
		Workloads: len(kinds),
		Text:      fmt.Sprintf("rolls %d %s", len(kinds), rolledNoun(kinds)),
	}
}

// rolledNoun names what is rolling, falling back to a generic word when the
// kinds are mixed rather than guessing at one of them.
func rolledNoun(kinds []string) string {
	first := kinds[0]
	for _, k := range kinds[1:] {
		if k != first {
			return "workloads"
		}
	}
	if len(kinds) == 1 {
		return first
	}
	return first + "s"
}

func changesChecksum(f check.Finding) bool {
	for _, p := range f.Change.Paths {
		if strings.Contains(p.Path.JSONPointer(), checksumAnnotation) {
			return true
		}
	}
	return false
}
