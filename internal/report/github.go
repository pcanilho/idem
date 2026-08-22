package report

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pcanilho/idem/internal/check"
)

// annotationCap is how many annotations of one level GitHub will render per
// step. Findings beyond it are counted rather than allowed to disappear: a
// silently truncated list reads as "that was everything".
const annotationCap = 10

// GitHub writes workflow commands, so findings appear inline on the diff.
//
// No token, no API call, no pull-requests: write permission. What can be
// pinned to a line and what cannot is deliberate: a potential finding knows
// its exact call site, while an observed one only knows the template helm
// named in its "# Source:" comment - which carries no line number. Guessing
// one would annotate the wrong line, which is worse than annotating nothing.
func (r Report) GitHub(w io.Writer) error {
	var b strings.Builder

	var errors, warnings, unplaceable int

	for _, c := range r.inScope() {
		// Both conditions annotate at error level: each is a difference idem
		// observed, differing only in which engines reconcile it - and the
		// message says which, so the reviewer is never left to assume.
		for _, f := range c.Findings {
			annotate(&b, r, c, f, "", &errors, &unplaceable)
		}
		for _, f := range c.ServerOnly {
			annotate(&b, r, c, f, " when `lookup` resolves", &errors, &unplaceable)
		}

		// idem cannot attribute an observed difference to a particular
		// function, so on a chart that churned it must not say this one
		// stayed quiet.
		settled := len(c.Findings) == 0 && len(c.Suppressed) == 0 && len(c.ServerOnly) == 0 && c.Err == nil

		for _, u := range c.Potential {
			file, ok := r.locate(c, u.File)
			if !ok {
				unplaceable++
				continue
			}
			if warnings >= annotationCap {
				warnings++
				continue
			}
			warnings++

			note := ""
			if settled {
				note = "; it did not fire this render"
			}
			fmt.Fprintf(&b, "::warning file=%s,line=%d::%s\n", property(file), u.Line,
				data(fmt.Sprintf("idem: %s is non-deterministic (%s)%s", u.Function, whyOf(u.Function), note)))
		}
	}

	// Every chart, not just the ones in ratchet scope: a chart idem could not
	// render is a gap in what was checked, and the ratchet only ever filters
	// findings about charts it did check.
	for _, c := range r.Charts {
		if c.Err != nil {
			fmt.Fprintf(&b, "::error::idem: %s could not be rendered: %s\n", c.Name, data(c.Err.Error()))
		}
	}

	writeCaps(&b, errors, warnings, unplaceable)

	_, err := io.WriteString(w, b.String())
	return err
}

// writeCaps says what GitHub will not show, rather than letting it vanish.
func writeCaps(b *strings.Builder, errors, warnings, unplaceable int) {
	if over := errors - annotationCap; over > 0 {
		fmt.Fprintf(b, "::notice::idem: %d more findings not annotated — GitHub renders at most %d errors per step; run with -o markdown for the full list\n", over, annotationCap)
	}
	if over := warnings - annotationCap; over > 0 {
		fmt.Fprintf(b, "::notice::idem: %d more potential findings not annotated — GitHub renders at most %d warnings per step\n", over, annotationCap)
	}
	if unplaceable > 0 {
		fmt.Fprintf(b, "::notice::idem: %d findings have no file in this repository — see the summary comment\n", unplaceable)
	}
}

// trimChartPrefix drops the chart directory name helm puts on every
// "# Source:" comment, which the chart's own repository path already supplies.
//
// Paths from the static analyzer are already chart-relative and must NOT go
// through this: trimming one would turn templates/_helpers.tpl into
// _helpers.tpl and place the annotation on a file that does not exist.
func trimChartPrefix(source string) string {
	_, rest, found := strings.Cut(source, "/")
	if !found {
		return source
	}
	return rest
}

// locate turns a chart-relative path into a repository path, and reports
// whether that file actually exists.
//
// The existence check is the whole point. A subchart vendored as a .tgz
// produces paths like "charts/ollama/templates/x.yaml" that resolve to
// somewhere no one can open, and an annotation on a file that is not there is
// worse than no annotation.
func (r Report) locate(c Chart, rel string) (string, bool) {
	if rel == "" || c.RepoDir == "" || r.Root == "" {
		return "", false
	}

	path := filepath.ToSlash(filepath.Join(c.RepoDir, rel))
	if _, err := os.Stat(filepath.Join(r.Root, path)); err != nil {
		return "", false
	}
	return path, true
}

// annotate writes one finding's workflow command, counting what it could not
// place and what the cap held back rather than dropping either silently.
func annotate(b *strings.Builder, r Report, c Chart, f check.Finding, when string, errors, unplaceable *int) {
	file, ok := r.locate(c, trimChartPrefix(f.Source))
	if !ok {
		*unplaceable++
		return
	}
	*errors++
	if *errors > annotationCap {
		return
	}

	cost := consequenceOf(f, siblingsOf(c)).Text
	fmt.Fprintf(b, "::error file=%s::%s\n", property(file), message(f, cost, when))
}

func message(f check.Finding, cost, when string) string {
	var fields []string
	for _, p := range f.Change.Paths {
		fields = append(fields, p.Path.String())
	}

	msg := fmt.Sprintf("idem: %s renders inconsistently%s", f.Change.Object.Display(), when)
	if len(fields) > 0 {
		msg += " at " + strings.Join(fields, ", ")
	}
	// Parenthesised: the consequence text has its own dash, and "at X — silent
	// — no checksum" makes the reader hunt for the clause boundary.
	if cost != "" {
		msg += " (" + cost + ")"
	}
	return data(msg)
}

// data escapes a workflow-command message. Unescaped, a newline ends the
// command and the rest is echoed as plain log output.
func data(s string) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	return r.Replace(s)
}

// property escapes a command property, where "," and ":" are also structural.
func property(s string) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
	return r.Replace(s)
}
