package delivery

import (
	"strings"
	"testing"

	"github.com/pcanilho/idem/internal/check"
	"github.com/pcanilho/idem/internal/diff"
	"github.com/pcanilho/idem/internal/objpath"
)

func dotted(path string) objpath.Path {
	var p objpath.Path
	for seg := range strings.SplitSeq(strings.TrimPrefix(path, "."), ".") {
		p = p.Append(objpath.Key(seg))
	}
	return p
}

func secretFinding(name string, fields ...string) check.Finding {
	var paths []diff.PathDiff
	for _, f := range fields {
		paths = append(paths, diff.PathDiff{Path: dotted(f), Left: "a", Right: "b", HasLeft: true, HasRight: true})
	}
	return check.Finding{Change: diff.Change{
		Object: diff.ObjectRef{APIVersion: "v1", Kind: "Secret", Name: name},
		Type:   diff.Differs,
		Paths:  paths,
	}}
}

func TestApplyKeepsAFindingNothingCovers(t *testing.T) {
	got := Apply(
		[]Rule{rule("", "Secret", "other", "/data/password")},
		[]check.Finding{secretFinding("creds", ".data.password")},
	)

	if len(got.Churning) != 1 || len(got.Suppressed) != 0 {
		t.Errorf("Apply() = %+v, want the finding kept", got)
	}
}

func TestApplyMovesAFullyCoveredFindingToSuppressed(t *testing.T) {
	got := Apply(
		[]Rule{rule("", "Secret", "creds", "/data/password")},
		[]check.Finding{secretFinding("creds", ".data.password")},
	)

	if len(got.Churning) != 0 {
		t.Errorf("Churning = %+v, want none", got.Churning)
	}
	if len(got.Suppressed) != 1 {
		t.Fatalf("Suppressed = %+v, want 1", got.Suppressed)
	}
}

func TestApplyNamesTheRuleThatSuppressedIt(t *testing.T) {
	// A suppression that changes the verdict has to be traceable to the
	// manifest that caused it, or the user cannot check idem's reasoning.
	r := rule("", "Secret", "creds", "/data/password")
	r.File = "deployment/apps/home.yaml"

	got := Apply([]Rule{r}, []check.Finding{secretFinding("creds", ".data.password")})

	if got.Suppressed[0].By.File != "deployment/apps/home.yaml" {
		t.Errorf("By.File = %q, want the manifest named", got.Suppressed[0].By.File)
	}
}

func TestApplyKeepsOnlyTheUncoveredFieldsOfAPartlyCoveredFinding(t *testing.T) {
	// Suppressing the whole finding because one of its fields is handled
	// would hide real churn - the exact failure this must not have.
	got := Apply(
		[]Rule{rule("", "Secret", "creds", "/data/password")},
		[]check.Finding{secretFinding("creds", ".data.password", ".data.token")},
	)

	if len(got.Suppressed) != 0 {
		t.Errorf("Suppressed = %+v, want none - one field is still unhandled", got.Suppressed)
	}
	if len(got.Churning) != 1 {
		t.Fatalf("Churning = %+v, want 1", got.Churning)
	}
	paths := got.Churning[0].Change.Paths
	if len(paths) != 1 || paths[0].Path.JSONPointer() != "/data/token" {
		t.Errorf("remaining paths = %+v, want only /data/token", paths)
	}
}

func TestApplySuppressesAStringDataFindingWithADataRule(t *testing.T) {
	// The whole reason pointer normalisation had to land first. The chart
	// renders stringData; the working rule says /data. Without the
	// translation idem re-reports a finding that was fixed long ago.
	got := Apply(
		[]Rule{rule("", "Secret", "creds", "/data/WEBUI_SECRET_KEY")},
		[]check.Finding{secretFinding("creds", ".stringData.WEBUI_SECRET_KEY")},
	)

	if len(got.Suppressed) != 1 {
		t.Errorf("Apply() = %+v, want the finding recognised as already suppressed", got)
	}
}

func TestApplyReportsAJQRuleAsMaybeRatherThanSuppressed(t *testing.T) {
	r := Rule{Kind: "Secret", Name: "creds", JQ: []string{".data.password"}, Path: "charts/home"}

	got := Apply([]Rule{r}, []check.Finding{secretFinding("creds", ".data.password")})

	if len(got.Suppressed) != 0 {
		t.Errorf("Suppressed = %+v, want none - idem cannot evaluate jq", got.Suppressed)
	}
	if len(got.Churning) != 1 {
		t.Errorf("Churning = %+v, want the finding kept", got.Churning)
	}
	if len(got.Maybe) != 1 {
		t.Errorf("Maybe = %+v, want the jq rule surfaced", got.Maybe)
	}
}

func TestApplyKeepsAFindingThatHasNoFieldToCover(t *testing.T) {
	// An object rendered in one round and not another has no differing field,
	// so no pointer can address it and no rule can cover it.
	vanishing := check.Finding{Change: diff.Change{
		Object: diff.ObjectRef{APIVersion: "v1", Kind: "Secret", Name: "ghost"},
		Type:   diff.OnlyInLeft,
	}}

	got := Apply([]Rule{rule("", "Secret", "ghost", "/data")}, []check.Finding{vanishing})

	if len(got.Churning) != 1 || len(got.Suppressed) != 0 {
		t.Errorf("Apply() = %+v, want the finding kept", got)
	}
}

func TestApplyWithNoRulesChangesNothing(t *testing.T) {
	findings := []check.Finding{secretFinding("creds", ".data.password")}

	got := Apply(nil, findings)

	if len(got.Churning) != 1 || len(got.Suppressed) != 0 || len(got.Maybe) != 0 {
		t.Errorf("Apply() = %+v, want the findings untouched", got)
	}
}
