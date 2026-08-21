// Package lookup finds calls to helm's lookup function in a chart.
//
// This is the only new logic three-engine verdicts need. `helm template`
// resolves lookup to {} by construction, so a chart that renders differently
// twice churns under ArgoCD as an observed fact. What that means for Flux and
// Helm - which do a real install, where lookup resolves - depends on one
// question: could a lookup anywhere in this chart be stabilising the value?
//
// **The detection deliberately over-matches.** A false positive downgrades the
// Flux/Helm verdict to `unknown`, which is honest and costs the reader
// nothing. A false negative produces `CHURNS`, which §5 states as *sound* -
// "no lookup anywhere, so nothing could stabilise this value" - and would be
// confidently wrong. So a bare mention of the word in a template counts, and
// idem never parses the call or traces whether it guards a particular value.
// That tracer is cut permanently: Bitnami's idiom reaches
// charts/common/templates/_secrets.tpl through _helpers.tpl, so the flagship
// chart would return unknown anyway.
package lookup

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
)

// mention matches the word `lookup` on its own. The word boundaries keep
// `dnsLookupTimeout` and `lookups` out; nothing else is excluded on purpose.
var mention = regexp.MustCompile(`\blookup\b`)

// invocation matches what a real call looks like: the function name followed
// by its first argument, which is an apiVersion string or a variable holding
// one. Used only to pick the best line to CITE - never to decide the verdict,
// because a call idem failed to recognise must still count.
var invocation = regexp.MustCompile(`\blookup\b\s+["$]`)

// maxScan caps how much of any one file is read. A chart is text, and an
// archive entry claiming to be gigabytes is not something to load into memory.
const maxScan = 8 << 20

// Use is one place a chart calls lookup.
type Use struct {
	// File is chart-relative for files on disk, and archive-relative for
	// files inside a vendored .tgz.
	File string
	Line int

	// Call is true when the line looks like an actual invocation rather than
	// a mention in a comment. Evidence quality only; both count.
	Call bool
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

// Find returns every call to lookup in the chart at dir.
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
				return fmt.Errorf("scanning %s for lookup: %w", rel, archiveErr)
			}
			uses = append(uses, found...)
			return nil
		}

		if !isTemplate(filepath.ToSlash(rel)) {
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("scanning %s for lookup: %w", rel, readErr)
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
		return a.Line - b.Line
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

// scan records every line of body that mentions lookup.
func scan(file, body string) []Use {
	var uses []Use
	for i, line := range strings.Split(body, "\n") {
		if mention.MatchString(line) {
			uses = append(uses, Use{File: file, Line: i + 1, Call: invocation.MatchString(line)})
		}
	}
	return uses
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
