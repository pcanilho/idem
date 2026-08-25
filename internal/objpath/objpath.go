// Package objpath addresses a location inside a rendered Kubernetes object.
//
// Paths are held as segments rather than as a formatted string because the two
// renderings the tool needs are not derivable from one another. A Kubernetes
// key may itself contain "." or "/" - ".data.application.yaml" and
// ".metadata.annotations.checksum/secrets" are both real and both common - so a
// flat string cannot say where one segment ends and the next begins. The
// checksum annotation in particular is the object the whole tool exists to talk
// about, and it needs RFC 6901 escaping to appear in an ArgoCD
// ignoreDifferences block.
package objpath

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Segment is one step in a path: either a map key or an array index.
type Segment struct {
	key   string
	index int
	isIdx bool
}

// Key returns a map-key segment.
func Key(k string) Segment { return Segment{key: k} }

// Index returns an array-index segment.
func Index(i int) Segment { return Segment{index: i, isIdx: true} }

// Path is a sequence of segments.
type Path []Segment

// Append returns a new Path with s added.
//
// It always copies. walk() descends by appending to a shared parent path, and
// a shared backing array would let sibling branches overwrite each other's
// segments - producing paths that are silently wrong rather than obviously
// broken.
func (p Path) Append(s Segment) Path {
	out := make(Path, len(p), len(p)+1)
	copy(out, p)
	return append(out, s)
}

// String renders the human-facing dotted form. Keys containing "." or "/" are
// bracketed and quoted so the rendering is unambiguous.
func (p Path) String() string {
	var b strings.Builder
	for _, s := range p {
		switch {
		case s.isIdx:
			b.WriteString("[")
			b.WriteString(strconv.Itoa(s.index))
			b.WriteString("]")
		case strings.ContainsAny(s.key, "./"):
			b.WriteString(`["`)
			b.WriteString(s.key)
			b.WriteString(`"]`)
		default:
			b.WriteString(".")
			b.WriteString(s.key)
		}
	}
	return b.String()
}

// JSONPointer renders the RFC 6901 form, suitable for an ArgoCD
// ignoreDifferences jsonPointers entry.
func (p Path) JSONPointer() string {
	var b strings.Builder
	for _, s := range p {
		b.WriteString("/")
		if s.isIdx {
			b.WriteString(strconv.Itoa(s.index))
			continue
		}
		b.WriteString(escape(s.key))
	}
	return b.String()
}

// escape applies RFC 6901 encoding. Order is load-bearing: "~" must be encoded
// before "/", or the "~1" produced for a slash would itself be re-encoded.
func escape(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

// MarshalJSON emits both renderings of the path.
//
// A consumer generating an ArgoCD ignoreDifferences block needs the RFC 6901
// pointer; a human reading the JSON needs the dotted form. Emitting only one
// would force every consumer to reimplement the escaping this package already
// gets right - including the "~" before "/" ordering, which is easy to get
// subtly wrong.
func (p Path) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Path    string `json:"path"`
		Pointer string `json:"pointer"`
	}{p.String(), p.JSONPointer()})
}
