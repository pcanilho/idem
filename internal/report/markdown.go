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
	if r.Churning() == 0 && r.Unevaluable() == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "### idem — %s\n\n", r.headline())

	var rows strings.Builder
	var suppressed int
	for _, c := range r.inScope() {
		for _, f := range c.Findings {
			writeRows(&rows, c, f)
		}
		suppressed += len(c.Suppressed)
	}

	// A chart still counts as churning while its findings are all suppressed,
	// so the count can be non-zero with nothing to tabulate. A header over no
	// rows reads as a rendering bug.
	if rows.Len() > 0 {
		b.WriteString("| chart | object | field | consequence |\n|---|---|---|---|\n")
		b.WriteString(rows.String())
		b.WriteString("\n")
	}

	if suppressed > 0 {
		fmt.Fprintf(&b, "%d %s already suppressed by your delivery config.\n\n",
			suppressed, plural(suppressed, "finding", "findings"))
	}

	writeUnevaluableRows(&b, r.inScope())
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
		return fmt.Sprintf("%d %s could not be rendered", r.Unevaluable(), plural(r.Unevaluable(), "chart", "charts"))
	}
	return fmt.Sprintf("%d of %d %s will churn under ArgoCD", churning, total, plural(total, "chart", "charts"))
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
	cost := consequenceOf(f, c.Findings).Text

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

func writeUnevaluableRows(b *strings.Builder, charts []Chart) {
	var failed []Chart
	for _, c := range charts {
		if c.Err != nil {
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
