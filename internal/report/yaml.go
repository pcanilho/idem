package report

import (
	"io"

	"gopkg.in/yaml.v3"
)

// YAML writes the machine-readable form in YAML.
//
// The same document `-o json` emits, from the same builder - see contract().
// Kubernetes people read and paste YAML all day, and the remediation entries
// are YAML in their final resting place, so `idem -o yaml | yq` is a shorter
// path than piping JSON through a converter first.
//
// Two-space indent because that is what every manifest in the ecosystem uses,
// and this output is meant to be pasted next to one.
func (r Report) YAML(w io.Writer) error {
	return writeYAML(w, r.contract())
}

// writeYAML encodes any contract document, shared for the same reason
// writeJSON is: `idem doctor -o yaml` must be the same document as its JSON,
// down to the indent.
func writeYAML(w io.Writer, v any) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return enc.Close()
}
