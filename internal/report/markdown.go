package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/remediate"
)

// Markdown writes the form shaped for a pull-request comment.
//
// It writes NOTHING when there is nothing to say. The documented CI snippet
// guards on `hashFiles('/tmp/idem.md') != ”`, so a clean run must produce an
// empty file rather than a comment saying everything is fine on every pull
// request that touches a chart.
func (r Report) Markdown(w io.Writer) error {
	if r.Churning() == 0 && r.Unevaluable() == 0 && r.ChurningWithLookup() == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "### idem — %s\n\n", r.headline())

	var rows strings.Builder
	var covered, undoneBySelfHeal int
	for _, c := range r.inScope() {
		for _, f := range c.Findings {
			writeRows(&rows, c, f)
		}
		for _, sup := range c.Suppressed {
			// Counted apart, and said apart. "Already suppressed" over a rule
			// selfHeal will undo is the most reassuring sentence idem could
			// print about the one case that is not handled at all.
			if sup.By.SelfHeal && !sup.By.Respected {
				undoneBySelfHeal++
				continue
			}
			covered++
		}
	}

	// A chart still counts as churning while its findings are all suppressed,
	// so the count can be non-zero with nothing to tabulate. A header over no
	// rows reads as a rendering bug.
	if rows.Len() > 0 {
		b.WriteString("| chart | object | field | consequence |\n|---|---|---|---|\n")
		b.WriteString(rows.String())
		b.WriteString("\n")
	}

	writeLookupRows(&b, r.inScope())

	if covered > 0 {
		fmt.Fprintf(&b, "%d %s already suppressed by your delivery config.\n\n",
			covered, plural(covered, "finding", "findings"))
	}

	if undoneBySelfHeal > 0 {
		fmt.Fprintf(&b, "%d %s suppressed by a rule `selfHeal` will re-apply anyway — add `RespectIgnoreDifferences=true` to that Application's `syncOptions`.\n\n",
			undoneBySelfHeal, plural(undoneBySelfHeal, "finding", "findings"))
	}

	writeUnevaluableRows(&b, r.Charts)
	writeFixBlock(&b, r.inScope())

	fmt.Fprintf(&b, "<sub>helm %s · %d rounds%s</sub>\n", r.Helm, r.Rounds, r.unevaluableNote())

	_, err := io.WriteString(w, b.String())
	return err
}

// headline is the churn sentence alone. What could not be rendered goes in the
// footer, so the heading answers one question.
func (r Report) headline() string {
	total, churning := len(r.inScope()), r.Churning()
	if churning == 0 {
		// ArgoCD's condition was identical, so the heading must not name it.
		// The engines that do a real install saw something else entirely.
		if lookupOnly := r.ChurningWithLookup(); lookupOnly > 0 {
			return fmt.Sprintf("%d of %d %s will churn under Flux and Helm", lookupOnly, total, plural(total, "chart", "charts"))
		}
		return fmt.Sprintf("%d %s could not be rendered", r.Unevaluable(), plural(r.Unevaluable(), "chart", "charts"))
	}
	return fmt.Sprintf("%d of %d %s will churn under ArgoCD%s", churning, total, plural(total, "chart", "charts"), lookupClause(r.ChurningWithLookup()))
}

func (r Report) unevaluableNote() string {
	n := r.Unevaluable()
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d %s could not be rendered", n, plural(n, "chart", "charts"))
}

// writeRows emits one row per differing field, as the spec shows: a reader
// scanning the table is looking for a field name, not a count.
func writeRows(b *strings.Builder, c Chart, f check.Finding) {
	cost := consequenceOf(f, siblingsOf(c)).Text

	if len(f.Change.Paths) == 0 {
		fmt.Fprintf(b, "| `%s` | `%s` | | %s |\n",
			cell(c.Name), cell(f.Change.Object.Display()), cell(f.Change.Type.String()))
		return
	}
	for _, p := range f.Change.Paths {
		fmt.Fprintf(b, "| `%s` | `%s` | `%s` | %s |\n",
			cell(c.Name), cell(f.Change.Object.Display()), cell(p.Path.String()), cell(cost))
	}
}

// writeLookupRows tables what only the API-server condition saw.
//
// A second table rather than a column on the first: the two conditions answer
// for different engines, and a reader scanning rows for their own engine
// should not have to check a per-row qualifier to know which apply.
func writeLookupRows(b *strings.Builder, charts []Chart) {
	var rows strings.Builder
	for _, c := range charts {
		for _, f := range c.ServerOnly {
			writeRows(&rows, c, f)
		}
	}
	if rows.Len() == 0 {
		return
	}

	b.WriteString("Identical under `helm template`, different with `lookup` resolved — ArgoCD is unaffected, Flux and Helm will churn.\n\n")
	b.WriteString("| chart | object | field | consequence |\n|---|---|---|---|\n")
	b.WriteString(rows.String())
	b.WriteString("\n")
}

func writeUnevaluableRows(b *strings.Builder, charts []Chart) {
	var failed []Chart
	for _, c := range charts {
		if c.Err != nil && !unbuilt(c) {
			failed = append(failed, c)
		}
	}
	if len(failed) == 0 {
		return
	}

	b.WriteString("<details>\n<summary>Could not be rendered</summary>\n\n")
	for _, c := range failed {
		fmt.Fprintf(b, "    %s\n", c.Name)
		for _, line := range errorLines(c.Err) {
			fmt.Fprintf(b, "      %s\n", line)
		}
	}
	b.WriteString("\n</details>\n\n")
}

// writeFixBlock collapses the remediation, because it is long and only some
// readers need it.
func writeFixBlock(b *strings.Builder, charts []Chart) {
	var all []check.Finding
	for _, c := range charts {
		if c.Err == nil {
			all = append(all, c.Findings...)
		}
	}

	entries := remediate.Entries(all)
	if len(entries) == 0 {
		return
	}

	b.WriteString("<details>\n<summary>Fix — add to your ArgoCD Application</summary>\n\n")
	// Indented rather than fenced: a fenced block inside <details> is rendered
	// inconsistently, and this survives GitHub's renderer as-is.
	for line := range strings.SplitSeq(strings.TrimRight(remediate.YAML(entries), "\n"), "\n") {
		fmt.Fprintf(b, "    %s\n", line)
	}
	b.WriteString("\n</details>\n\n")
}

// cell escapes what would otherwise break out of a table cell.
//
// A pipe ends a cell even inside backticks, and ConfigMap keys and annotation
// names are user data that can contain one.
func cell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
