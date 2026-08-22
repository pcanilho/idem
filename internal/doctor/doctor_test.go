package doctor

import (
	"testing"
	"time"

	"github.com/pcanilho/idem/internal/cluster"
)

var now = time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

func workload(name string, revision, ageDays int, checksums ...string) cluster.Workload {
	return cluster.Workload{
		Kind:      "Deployment",
		Namespace: "lab",
		Name:      name,
		Revision:  revision,
		Created:   now.AddDate(0, 0, -ageDays),
		Checksums: checksums,
	}
}

// quiet is a cluster of ordinary workloads, to set a median.
func quiet(n int) []cluster.Workload {
	var out []cluster.Workload
	for i := range n {
		out = append(out, workload("quiet", 100, 700))
		_ = i
	}
	return out
}

func TestAWorkloadRollingFarAboveItsPeersIsASuspect(t *testing.T) {
	// The shape that cost two years: revision 660 over 743 days, still rolling.
	workloads := append(quiet(9), workload("harbor-registry", 660, 743, "checksum/secret"))

	got := Diagnose(workloads, now)

	if len(got.Suspects) != 1 {
		t.Fatalf("Diagnose() = %+v, want one suspect", got.Suspects)
	}
	if got.Suspects[0].Workload.Name != "harbor-registry" {
		t.Errorf("suspect is %q, want harbor-registry", got.Suspects[0].Workload.Name)
	}
}

func TestAWorkloadWithNoChecksumAnnotationIsNotASuspect(t *testing.T) {
	// Without one, a regenerated Secret has no route to the pods. Rolling
	// often is then just deploying often, which is not a defect.
	workloads := append(quiet(9), workload("busy", 660, 743))

	if got := Diagnose(workloads, now); len(got.Suspects) != 0 {
		t.Errorf("Diagnose() = %+v, want none - nothing ties it to a Secret", got.Suspects)
	}
}

func TestAWorkloadRollingAtTheClusterNormIsNotASuspect(t *testing.T) {
	// A checksum annotation is good practice, not an accusation.
	workloads := append(quiet(9), workload("normal", 100, 700, "checksum/secret"))

	if got := Diagnose(workloads, now); len(got.Suspects) != 0 {
		t.Errorf("Diagnose() = %+v, want none - it rolls like everything else", got.Suspects)
	}
}

func TestAFreshWorkloadIsNotJudgedOnItsFirstDay(t *testing.T) {
	// Five rollouts on the day it was created is a busy afternoon, and
	// dividing by a fraction of a day would rank it above everything.
	workloads := append(quiet(9), workload("new", 5, 0, "checksum/secret"))

	if got := Diagnose(workloads, now); len(got.Suspects) != 0 {
		t.Errorf("Diagnose() = %+v, want none - too new to say", got.Suspects)
	}
}

func TestSuspectsAreRankedByHowOftenTheyRoll(t *testing.T) {
	workloads := append(quiet(9),
		workload("slower", 400, 700, "checksum/secret"),
		workload("faster", 660, 700, "checksum/secret"),
	)

	got := Diagnose(workloads, now)

	if len(got.Suspects) != 2 {
		t.Fatalf("Diagnose() = %+v, want two", got.Suspects)
	}
	if got.Suspects[0].Workload.Name != "faster" {
		t.Errorf("first suspect is %q, want faster", got.Suspects[0].Workload.Name)
	}
}

func TestTheMedianIsReportedSoTheReaderCanJudge(t *testing.T) {
	// "Far more often than its peers" is a judgement call, so idem shows the
	// number it made the call against.
	workloads := append(quiet(9), workload("harbor", 660, 743, "checksum/secret"))

	got := Diagnose(workloads, now)

	if got.Median <= 0 {
		t.Errorf("Median = %v, want the cluster norm reported", got.Median)
	}
	if got.Scanned != len(workloads) {
		t.Errorf("Scanned = %d, want %d", got.Scanned, len(workloads))
	}
}

func TestAnEmptyClusterDiagnosesNothing(t *testing.T) {
	got := Diagnose(nil, now)

	if got.Scanned != 0 || len(got.Suspects) != 0 {
		t.Errorf("Diagnose(nil) = %+v, want empty", got)
	}
}

func TestDaysRoundsTheAgeForDisplay(t *testing.T) {
	s := Suspect{Age: 743*24*time.Hour + 11*time.Hour}

	if got := s.Days(); got != 743 {
		t.Errorf("Days() = %d, want 743", got)
	}
}
