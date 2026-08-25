// Package analyze finds template functions whose output can differ between
// two renders of identical input.
//
// It serves two purposes. `lookup` decides the Flux and Helm verdicts: those
// engines resolve it, so whether a chart calls it at all is what separates
// "nothing could stabilise this value" from "something might". The rest power
// the potential findings of docs/design.md §5 - a chart that calls
// randAlphaNum but rendered identically twice has a pin holding it, and the
// failure worth knowing about is a pin that silently stops applying.
//
// **Detection deliberately over-matches.** A false positive downgrades a Flux
// or Helm verdict to `unknown`, which is honest and costs the reader nothing.
// A false negative produces `CHURNS`, which §5 states as *sound* - "no lookup
// anywhere, so nothing could stabilise this value" - and would be confidently
// wrong. So a bare mention of the word in a template counts, and idem never
// parses the call or traces whether it guards a particular value. That tracer
// is cut permanently: Bitnami's idiom reaches
// charts/common/templates/_secrets.tpl through _helpers.tpl, so the flagship
// chart would return unknown anyway.
package analyze

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Lookup is the function whose presence decides the Flux and Helm verdicts.
const Lookup = "lookup"

// Function is a template function whose output idem cannot predict.
type Function struct {
	Name string

	// Why is the source of the nondeterminism, in the reader's terms.
	Why string
}

// Functions is the set idem flags, audited against sprig v3.3.0 - which both
// Helm 3.19 and Helm 4.2 pin, so one list covers both lines.
//
// Deterministic despite appearances, and deliberately absent: derivePassword,
// buildCustomCert, decryptAES.
var Functions = []Function{
	{"randAlphaNum", "random"},
	{"randAlpha", "random"},
	{"randNumeric", "random"},
	{"randAscii", "random"},
	{"randBytes", "random"},
	{"randInt", "random"},
	{"shuffle", "random order"},
	{"uuidv4", "random"},
	{"bcrypt", "random salt"},
	{"htpasswd", "random salt"},
	{"genPrivateKey", "new key material"},
	{"genCA", "new key material"},
	{"genCAWithKey", "new certificate"},
	{"genSelfSignedCert", "new key material"},
	{"genSelfSignedCertWithKey", "new certificate"},
	{"genSignedCert", "new key material"},
	{"genSignedCertWithKey", "new certificate"},
	{"encryptAES", "random IV"},
	{"now", "time"},
	{"ago", "time"},
	{"getHostByName", "DNS"},
	// keys and values build a slice from Go map iteration order and never
	// sort it. Unlike a fresh UUID the result looks plausible in a diff, so a
	// reviewer waves it through - which makes these the most dangerous of the
	// set rather than the most obscure. sortAlpha is the fix.
	{"keys", "map order"},
	{"values", "map order"},
	{Lookup, "cluster state"},
}

// mention matches any flagged function name on its own. The word boundaries
// keep `dnsLookupTimeout` and `lookups` out, and stop `randAlpha` matching
// inside `randAlphaNum`; nothing else is excluded on purpose.
var mention = regexp.MustCompile(`\b(` + strings.Join(names(), "|") + `)\b`)

// names lists the function names longest-first, so an alternation prefers
// randAlphaNum over randAlpha where both could start at the same offset.
func names() []string {
	out := make([]string, 0, len(Functions))
	for _, f := range Functions {
		out = append(out, f.Name)
	}
	slices.SortFunc(out, func(a, b string) int {
		if d := len(b) - len(a); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return out
}

// Why returns the reason a function is flagged.
func Why(name string) string {
	for _, f := range Functions {
		if f.Name == name {
			return f.Why
		}
	}
	return ""
}

// maxScan caps how much of any one file is read. A chart is text, and an
// archive entry claiming to be gigabytes is not something to load into memory.
const maxScan = 8 << 20

// Use is one place a chart names a non-deterministic function.
type Use struct {
	// Function is which one.
	Function string

	// File is chart-relative for files on disk, and archive-relative for
	// files inside a vendored .tgz.
	File string
	Line int

	// Call is true when the line looks like an actual invocation rather than
	// a mention in a comment. Evidence quality only; both count.
	Call bool
}

// Of filters uses down to one function.
func Of(uses []Use, name string) []Use {
	var out []Use
	for _, u := range uses {
		if u.Function == name {
			out = append(out, u)
		}
	}
	return out
}

// Best returns the use worth citing as evidence, preferring a real call.
func Best(uses []Use) (Use, bool) {
	for _, u := range uses {
		if u.Call {
			return u, true
		}
	}
	if len(uses) > 0 {
		return uses[0], true
	}
	return Use{}, false
}

// Find returns every use of a flagged function in the chart at dir.
//
// An unreadable archive is an error, never an empty result: "I could not look"
// and "there is nothing there" are the difference between `unknown` and a
// sound `CHURNS`.
func Find(dir string) ([]Use, error) {
	var uses []Use

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			rel = p
		}

		if strings.HasSuffix(p, ".tgz") || strings.HasSuffix(p, ".tar.gz") {
			found, archiveErr := scanArchive(p)
			if archiveErr != nil {
				return fmt.Errorf("scanning %s: %w", rel, archiveErr)
			}
			uses = append(uses, found...)
			return nil
		}

		if !isTemplate(filepath.ToSlash(rel)) {
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("scanning %s: %w", rel, readErr)
		}
		uses = append(uses, scan(rel, string(body))...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(uses, func(a, b Use) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		if d := a.Line - b.Line; d != 0 {
			return d
		}
		return strings.Compare(a.Function, b.Function)
	})
	return uses, nil
}

// isTemplate reports whether a chart-relative path holds Go template source.
//
// Only templates/ and .tpl files: values.yaml may legitimately have a key
// called `lookup`, and matching it would point the evidence line at a file
// that never calls anything.
func isTemplate(rel string) bool {
	if strings.HasSuffix(rel, ".tpl") {
		return true
	}
	return slices.Contains(strings.Split(rel, "/"), "templates")
}

// scan records every flagged function named on each line of body.
func scan(file, body string) []Use {
	var uses []Use
	for i, line := range strings.Split(body, "\n") {
		for _, m := range mention.FindAllStringIndex(line, -1) {
			uses = append(uses, Use{
				Function: line[m[0]:m[1]],
				File:     file,
				Line:     i + 1,
				Call:     looksLikeCall(line, m[0], m[1]),
			})
		}
	}
	return uses
}

// looksLikeCall reports whether the name at this position reads as an
// invocation rather than a word in prose or a field path.
//
// It has to be strict, because `keys` and `values` are ordinary words: without
// this, every `.Values.keys` and every YAML field called `keys:` becomes a
// warning, and a tool that cries wolf about the potential case teaches you to
// distrust it about the observed one.
//
// The asymmetry with lookup is deliberate and runs the other way. There, a
// false positive downgrades a verdict to `unknown` and costs nothing, so
// detection over-matches; every use is still recorded here whatever this
// returns. Only the potential findings are filtered by it, where a false
// positive costs trust and a false negative costs at most a warning about
// something the observed check would catch anyway if it ever fired.
func looksLikeCall(line string, start, end int) bool {
	rest := strings.TrimLeft(line[end:], " \t")
	if rest != "" && unicode.IsLetter(rune(rest[0])) {
		return false
	}
	return inFunctionPosition(line[:start])
}

// callPrefixes are the tokens a function name can directly follow.
var callPrefixes = []string{"{{", "{{-", "(", "|", ":=", "="}

// callKeywords are template keywords that take a function as their argument.
var callKeywords = []string{"if", "with", "range", "and", "or", "not", "default", "else"}

// inFunctionPosition reports whether what precedes the name puts it in
// function position rather than in a field path or a sentence.
func inFunctionPosition(before string) bool {
	before = strings.TrimRight(before, " \t")
	if before == "" {
		return false
	}
	if slices.ContainsFunc(callPrefixes, func(p string) bool { return strings.HasSuffix(before, p) }) {
		return true
	}

	fields := strings.Fields(before)
	return slices.Contains(callKeywords, fields[len(fields)-1])
}

// scanArchive reads a vendored subchart without unpacking it.
//
// `helm dependency build` leaves dependencies as charts/*.tgz, and a GitOps
// monorepo commits them. Bitnami's lookup lives inside one.
func scanArchive(p string) ([]Use, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	var uses []Use
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || !isTemplate(path.Clean(header.Name)) {
			continue
		}

		body, err := io.ReadAll(io.LimitReader(tr, maxScan))
		if err != nil {
			return nil, err
		}
		uses = append(uses, scan(header.Name, string(body))...)
	}
	return uses, nil
}

// Potential returns one use per function that could make a chart churn,
// citing the best call site for each.
//
// lookup is excluded deliberately. It is the function that STABILISES a value
// under an engine that resolves it, and its presence is already reported as
// the Flux and Helm verdict. Listing it here as a potential cause would
// contradict that verdict on the same screen.
//
// One line per function rather than per call site: a chart reaching a shared
// helper ninety times is one thing to look at, not ninety.
func Potential(uses []Use) []Use {
	best := make(map[string]Use)
	for _, u := range uses {
		if u.Function == Lookup || !u.Call {
			continue
		}
		if _, ok := best[u.Function]; ok {
			continue
		}
		best[u.Function] = u
	}

	out := make([]Use, 0, len(best))
	for _, u := range best {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b Use) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		if d := a.Line - b.Line; d != 0 {
			return d
		}
		return strings.Compare(a.Function, b.Function)
	})
	return out
}
