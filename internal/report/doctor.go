package report

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/pcanilho/idem/internal/doctor"
)

// Doctor writes what the cluster says about its own churn.
//
// Deliberately framed as triage. A high rollout count also comes from
// deploying often, so the median is printed alongside and the closing line
// points at the check that would establish the cause rather than claiming to
// have established it here.
func Doctor(w io.Writer, d doctor.Diagnosis, context string, sources map[string]string) error {
	var b strings.Builder

	fmt.Fprintf(&b, "\n  Scanned %d %s for sync churn%s\n",
		d.Scanned, plural(d.Scanned, "workload", "workloads"), contextSuffix(context))

	if len(d.Suspects) == 0 {
		fmt.Fprintf(&b, "\n  Nothing rolling far above the cluster median of %.2f rollouts/day.\n", d.Median)
		return emit(w, b.String())
	}

	// Numbers are padded in the verb rather than by the tabwriter, because
	// AlignRight applies to every column and would drag the names right too.
	b.WriteString("\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintf(tw, "    %5s\t%7s\t%6s\t%s\t%s\t%s\n", "rev", "per day", "age", "workload", "owner", "checksums")
	for _, s := range d.Suspects {
		fmt.Fprintf(tw, "    %5d\t%7.2f\t%5dd\t%s\t%s\t%s\n",
			s.Workload.Revision, s.PerDay, s.Days(), where(s), s.Workload.Owner, checksumNote(s))
	}
	tw.Flush()

	fmt.Fprintf(&b, "\n  Cluster median is %.2f rollouts/day. %s carry a checksum/ annotation and\n",
		d.Median, subjects(len(d.Suspects)))
	b.WriteString("  roll far more often than their images change, consistent with a Secret that\n")
	b.WriteString("  is regenerated on every sync.\n")
	b.WriteString("\n  This is triage, not proof: deploying often looks the same from here.\n")
	writeConfirm(&b, d, context, sources)

	return emit(w, b.String())
}

// writeConfirm names the exact command that would establish the cause.
//
// The workloads say which Application owns them, and the Application says
// which chart it deploys, so idem can hand over a command rather than a shape
// of one. Where it cannot, it says so instead of guessing a path.
func writeConfirm(b *strings.Builder, d doctor.Diagnosis, context string, sources map[string]string) {
	var paths []string
	unresolved := false

	for _, s := range d.Suspects {
		path, ok := sources[s.Workload.Owner.Name]
		if !ok {
			unresolved = true
			continue
		}
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)

	b.WriteString("\n  Confirm the cause:\n")
	for _, path := range paths {
		fmt.Fprintf(b, "    idem %s --context=%s\n", path, context)
	}
	if len(paths) == 0 || unresolved {
		fmt.Fprintf(b, "    idem <their chart> --context=%s\n", context)
	}
}

func contextSuffix(context string) string {
	if context == "" {
		return " (current kube context)"
	}
	return " (context " + context + ")"
}

func subjects(n int) string {
	if n == 1 {
		return "It does"
	}
	return fmt.Sprintf("These %d", n)
}

func where(s doctor.Suspect) string {
	if s.Workload.Namespace == "" {
		return s.Workload.Name
	}
	return s.Workload.Namespace + "/" + s.Workload.Name
}

// checksumNote names one annotation and counts the rest, because a chart
// hashing a dozen values produces a dozen names nobody reads.
func checksumNote(s doctor.Suspect) string {
	names := s.Workload.Checksums
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return fmt.Sprintf("%s + %d more", names[0], len(names)-1)
}
