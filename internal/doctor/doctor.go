// Package doctor finds churn that has already happened.
//
// Everything else in idem predicts "this will churn". doctor finds "this has
// been churning for two years". No chart, no git, no rendering - it asks the
// cluster what has been going on.
//
// This is triage, not proof. A high rollout count also comes from deploying
// often. What makes it a signal is the combination: rolling far above the
// cluster's own median AND carrying a checksum/ annotation, which is how a
// regenerated Secret reaches a workload. doctor ranks suspects; rendering the
// chart establishes the cause.
package doctor

import (
	"math"
	"slices"
	"time"

	"github.com/pcanilho/idem/internal/cluster"
)

// suspicion is how far above the cluster's own median a workload has to roll
// before it is worth looking at.
//
// Relative to the cluster rather than an absolute number, because "a lot" for
// a homelab is nothing for a shop deploying twenty times a day. Three times
// the median is a judgement call, and the median is printed so the reader can
// make their own.
const suspicion = 3.0

// settled is how long a workload must have existed before its rate means
// anything. A workload created this morning with five rollouts is a busy first
// day, not churn.
const settled = 7 * 24 * time.Hour

// Suspect is a workload rolling far more often than its peers.
type Suspect struct {
	Workload cluster.Workload
	PerDay   float64
	Age      time.Duration
}

// Diagnosis is what the cluster says about its own churn.
type Diagnosis struct {
	Scanned  int
	Median   float64
	Suspects []Suspect
}

// Diagnose ranks workloads by how much they roll.
func Diagnose(workloads []cluster.Workload, now time.Time) Diagnosis {
	rates := make([]float64, 0, len(workloads))
	for _, w := range workloads {
		rates = append(rates, perDay(w, now))
	}

	out := Diagnosis{Scanned: len(workloads), Median: median(rates)}

	for i, w := range workloads {
		age := now.Sub(w.Created)
		switch {
		case len(w.Checksums) == 0:
			// Without one, a regenerated Secret has no route to the pods, so
			// rolling often is just deploying often.
			continue
		case age < settled:
			continue
		case rates[i] < out.Median*suspicion:
			continue
		}
		out.Suspects = append(out.Suspects, Suspect{Workload: w, PerDay: rates[i], Age: age})
	}

	slices.SortFunc(out.Suspects, func(a, b Suspect) int {
		if a.PerDay != b.PerDay {
			return sign(b.PerDay - a.PerDay)
		}
		return sign(float64(b.Workload.Revision - a.Workload.Revision))
	})
	return out
}

// perDay is rollouts per day of existence.
func perDay(w cluster.Workload, now time.Time) float64 {
	days := now.Sub(w.Created).Hours() / 24
	if days < 1 {
		// Anything younger than a day would divide into a huge rate; its own
		// age gate excludes it from the suspects either way.
		days = 1
	}
	return float64(w.Revision) / days
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func sign(f float64) int {
	switch {
	case f > 0:
		return 1
	case f < 0:
		return -1
	}
	return 0
}

// Days is the workload's age, rounded for display.
func (s Suspect) Days() int { return int(math.Round(s.Age.Hours() / 24)) }
