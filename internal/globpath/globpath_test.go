package globpath

import "testing"

func TestMatchWalksSegmentBySegment(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"charts/app", "charts/app", true},
		{"charts/*", "charts/app", true},
		{"charts/*", "charts/app/sub", false},
		{"charts/**", "charts/app/sub", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/c", false},
		{"**", "anything/at/all", true},
		{"*.yaml", "values.yaml", true},
		{"*", "a/b", false},
	} {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMatchTreatsDoubleStarAsZeroOrMoreSegments(t *testing.T) {
	// Requiring at least one segment is the classic way to get this wrong and
	// drop a path without saying so.
	if !Match("a/**/b", "a/b") {
		t.Error(`Match("a/**/b", "a/b") = false, want true - ** matches zero segments`)
	}
}

func TestMatchRejectsAPatternItCannotParse(t *testing.T) {
	if Match("[", "x") {
		t.Error(`Match("[", "x") = true, want false - an unparseable pattern matches nothing`)
	}
}
