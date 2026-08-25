package engine

import (
	"encoding/json"
	"testing"
)

func TestZeroVerdictIsUnknown(t *testing.T) {
	// The zero value must be Unknown, never Stable. A verdict nobody set must
	// not read as "this chart is fine" - that is the one wrong answer that
	// would be silently believed.
	var v Verdict
	if v.Result != Unknown {
		t.Errorf("zero Verdict.Result = %v, want Unknown", v.Result)
	}
	if v.Observed {
		t.Error("zero Verdict claims to be Observed")
	}
}

func TestVerdictResultsMarshalAsStrings(t *testing.T) {
	// -o json is a public contract. Marshalling an enum as 0/1/2 makes the
	// output unreadable and breaks the moment a constant is reordered.
	for r, want := range map[Result]string{
		Unknown: `"unknown"`,
		Stable:  `"stable"`,
		Churns:  `"churns"`,
	} {
		got, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal %v: %v", r, err)
		}
		if string(got) != want {
			t.Errorf("json.Marshal(%v) = %s, want %s", r, got, want)
		}
	}
}

func TestVerdictRoundTripsThroughJSON(t *testing.T) {
	in := Verdict{Engine: "argocd", Result: Churns, Because: "lookup returns {}", Observed: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Verdict
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}
func TestSpecSeparatesValuesFilesFromSetOverrides(t *testing.T) {
	// -f and --set are not interchangeable: they have different precedence and
	// different ordering semantics, so they cannot share one []string.
	s := Spec{
		ChartRef:    "oci://example.com/chart",
		Version:     "16.2",
		ValuesFiles: []string{"a.yaml", "b.yaml"},
		SetValues:   []string{"auth.password=x"},
	}
	if len(s.ValuesFiles) != 2 || len(s.SetValues) != 1 {
		t.Fatalf("Spec did not keep values files and --set separate: %+v", s)
	}
	if s.Version != "16.2" {
		t.Errorf("Spec.Version = %q, want 16.2", s.Version)
	}
}
