package analyze

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// chartDir writes a minimal chart and returns its directory.
func chartDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	write(t, filepath.Join(dir, "Chart.yaml"), "apiVersion: v2\nname: "+name+"\nversion: 0.1.0\n")
	return dir
}

// tgz writes a gzipped tar of the given entries, as `helm dependency build`
// vendors a subchart.
func tgz(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func files(uses []Use) []string {
	out := make([]string, len(uses))
	for i, u := range uses {
		out[i] = u.File
	}
	return out
}

func TestFindReportsNothingForAChartThatNamesNoFlaggedFunction(t *testing.T) {
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "secret.yaml"),
		"data:\n  key: {{ .Values.key | b64enc }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Find() = %v, want none", files(got))
	}
}

func TestFindLocatesLookupWithFileAndLine(t *testing.T) {
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "secret.yaml"),
		"apiVersion: v1\nkind: Secret\ndata:\n  p: {{ (lookup \"v1\" \"Secret\" .Release.Namespace \"creds\").data.p }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Find() = %v, want 1 use", files(got))
	}
	if want := "templates/secret.yaml"; got[0].File != want {
		t.Errorf("File = %q, want %q (chart-relative)", got[0].File, want)
	}
	if got[0].Line != 4 {
		t.Errorf("Line = %d, want 4", got[0].Line)
	}
}

func TestFindScansHelperTemplates(t *testing.T) {
	// The bitnami idiom lives in a _helpers-style .tpl reached through
	// include, not in a rendered template file.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "_helpers.tpl"),
		"{{- define \"acme.pw\" -}}\n{{ lookup \"v1\" \"Secret\" .Release.Namespace \"creds\" }}\n{{- end -}}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Find() = %v, want the .tpl helper scanned", files(got))
	}
}

func TestFindScansVendoredSubchartDirectories(t *testing.T) {
	dir := chartDir(t, "parent")
	write(t, filepath.Join(dir, "templates", "main.yaml"), "kind: ConfigMap\n")
	write(t, filepath.Join(dir, "charts", "common", "templates", "_secrets.tpl"),
		"{{ lookup \"v1\" \"Secret\" .Release.Namespace .name }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Find() = %v, want the subchart scanned", files(got))
	}
	if want := filepath.Join("charts", "common", "templates", "_secrets.tpl"); got[0].File != want {
		t.Errorf("File = %q, want %q", got[0].File, want)
	}
}

func TestFindScansInsideVendoredArchives(t *testing.T) {
	// This is the case that makes the sound verdict safe. A GitOps monorepo
	// vendors dependencies as charts/*.tgz, and bitnami's lookup lives in
	// common/templates/_secrets.tpl inside one. Missing it would let idem
	// claim "no lookup anywhere" - which is a CHURNS verdict for Flux and
	// Helm, stated as sound, and wrong.
	dir := chartDir(t, "parent")
	tgz(t, filepath.Join(dir, "charts", "common-2.0.0.tgz"), map[string]string{
		"common/Chart.yaml":             "apiVersion: v2\nname: common\n",
		"common/values.yaml":            "lookup: not-a-call\n",
		"common/templates/_secrets.tpl": "{{- define \"c.pw\" -}}\n{{ lookup \"v1\" \"Secret\" $ns $name }}\n{{- end -}}\n",
	})

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Find() = %v, want the archive scanned", files(got))
	}
	if !strings.Contains(got[0].File, "_secrets.tpl") {
		t.Errorf("File = %q, want the template inside the archive", got[0].File)
	}
	if got[0].Line != 2 {
		t.Errorf("Line = %d, want 2", got[0].Line)
	}
}

func TestFindIgnoresFilesThatAreNotTemplates(t *testing.T) {
	// values.yaml may legitimately hold a key called "lookup". Matching it
	// would be harmless for the verdict, but the evidence line would point at
	// a file that never calls anything.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "values.yaml"), "lookup: enabled\n")
	write(t, filepath.Join(dir, "README.md"), "we do not use lookup here\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Find() = %v, want none", files(got))
	}
}

func TestFindOverDetectsRatherThanMissing(t *testing.T) {
	// The bias is deliberate and load-bearing. A false positive downgrades the
	// Flux/Helm verdict to `unknown`, which is honest. A false negative
	// produces a confident CHURNS that is wrong. So a bare mention in a
	// template counts, and idem never tries to parse the call.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "note.yaml"), "# we deliberately avoid lookup here\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Find() = %v, want the mention counted - over-detection is the safe direction", files(got))
	}
}

func TestFindDoesNotMatchLookupAsPartOfALongerWord(t *testing.T) {
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "cm.yaml"), "data:\n  dnsLookupTimeout: \"5s\"\n  lookups: \"3\"\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Find() = %v, want no match on a longer identifier", files(got))
	}
}

func TestFindReturnsUsesInAStableOrder(t *testing.T) {
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "zeta.yaml"), "{{ lookup \"v1\" \"Secret\" a b }}\n")
	write(t, filepath.Join(dir, "templates", "alpha.yaml"), "{{ lookup \"v1\" \"Secret\" a b }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Find() = %v, want 2", files(got))
	}
	if !strings.Contains(got[0].File, "alpha") {
		t.Errorf("Find() = %v, want sorted order", files(got))
	}
}

func TestFindReportsAnUnreadableArchiveRatherThanCallingItClean(t *testing.T) {
	// "I could not read this" must never be reported as "there is no lookup
	// here", because that is the difference between unknown and a sound
	// CHURNS verdict.
	dir := chartDir(t, "parent")
	write(t, filepath.Join(dir, "charts", "broken-1.0.0.tgz"), "this is not a gzip stream")

	if _, err := Find(dir); err == nil {
		t.Fatal("Find() error = nil, want the unreadable archive reported")
	}
}

func TestFindDistinguishesACallFromAMention(t *testing.T) {
	// Both count toward the verdict - over-detection is the safe direction -
	// but only one is worth citing as evidence. A finding that says
	// "chart calls `lookup` (secret.yaml:2)" and points at a comment reads as
	// a bug in idem.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "secret.yaml"),
		"{{/* the lookup finds the existing Secret */}}\n"+
			"data:\n"+
			"  p: {{ (lookup \"v1\" \"Secret\" .Release.Namespace \"creds\").data.p }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Find() = %+v, want both lines counted", got)
	}
	if got[0].Call {
		t.Errorf("line 1 marked as a call, want it marked a mention: %+v", got[0])
	}
	if !got[1].Call {
		t.Errorf("line 3 not marked as a call: %+v", got[1])
	}
}

func TestBestPrefersARealCallOverAMention(t *testing.T) {
	uses := []Use{
		{File: "templates/secret.yaml", Line: 2},
		{File: "templates/secret.yaml", Line: 12, Call: true},
	}

	got, ok := Best(uses)
	if !ok {
		t.Fatal("Best() ok = false, want a use")
	}
	if got.Line != 12 {
		t.Errorf("Best() = line %d, want the call on line 12", got.Line)
	}
}

func TestBestFallsBackToAMentionWhenThereIsNoCall(t *testing.T) {
	// Still evidence, and still enough to make the verdict unknown.
	uses := []Use{{File: "templates/note.yaml", Line: 4}}

	got, ok := Best(uses)
	if !ok || got.Line != 4 {
		t.Errorf("Best() = %+v, %v; want the mention", got, ok)
	}
}

func TestBestReportsWhenThereIsNothing(t *testing.T) {
	if _, ok := Best(nil); ok {
		t.Error("Best(nil) ok = true, want false")
	}
}

// --- the generalised sweep ---

func namesOf(uses []Use) []string {
	out := make([]string, len(uses))
	for i, u := range uses {
		out[i] = u.Function
	}
	return out
}

func TestFindNamesWhichFunctionItFound(t *testing.T) {
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "secret.yaml"),
		"data:\n  key: {{ randAlphaNum 32 | b64enc }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 1 || got[0].Function != "randAlphaNum" {
		t.Fatalf("Find() = %+v, want one randAlphaNum", got)
	}
	if got[0].Line != 2 {
		t.Errorf("Line = %d, want 2", got[0].Line)
	}
}

func TestFindReportsSeveralFunctionsOnOneLine(t *testing.T) {
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "cert.yaml"),
		"  ca: {{ $ca := genCA \"x\" 365 }}{{ $c := genSignedCert \"y\" nil nil 365 $ca }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	names := namesOf(got)
	for _, want := range []string{"genCA", "genSignedCert"} {
		if !slices.Contains(names, want) {
			t.Errorf("Find() = %v, want %q - one line can call more than one", names, want)
		}
	}
}

func TestALongerNameIsNotMistakenForItsPrefix(t *testing.T) {
	// randAlpha is a prefix of randAlphaNum, and genCA of genCAWithKey. A
	// naive alternation reports the short one and the reader chases the wrong
	// function.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "s.yaml"),
		"a: {{ randAlphaNum 8 }}\nb: {{ genCAWithKey \"x\" 365 $k }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	names := namesOf(got)
	for _, want := range []string{"randAlphaNum", "genCAWithKey"} {
		if !slices.Contains(names, want) {
			t.Errorf("Find() = %v, want %q", names, want)
		}
	}
	for _, unwanted := range []string{"randAlpha", "genCA"} {
		if slices.Contains(names, unwanted) {
			t.Errorf("Find() = %v, want no bare %q", names, unwanted)
		}
	}
}

func TestFindStillSeparatesLookupFromTheRest(t *testing.T) {
	// The Flux and Helm verdicts turn on lookup specifically, so it has to be
	// retrievable on its own.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "s.yaml"),
		"a: {{ (lookup \"v1\" \"Secret\" $ns $n).data.p | default (randAlphaNum 32) }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if n := len(Of(got, Lookup)); n != 1 {
		t.Errorf("Of(lookup) found %d, want 1", n)
	}
	if n := len(Of(got, "randAlphaNum")); n != 1 {
		t.Errorf("Of(randAlphaNum) found %d, want 1", n)
	}
}

func TestDeterministicLookalikesAreNotFlagged(t *testing.T) {
	// derivePassword is deterministic given its inputs, buildCustomCert only
	// assembles what it is handed, and decryptAES is the inverse of a flagged
	// function rather than a source of new randomness. Flagging them would be
	// crying wolf, which teaches people to distrust the observed findings too.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "s.yaml"),
		"a: {{ derivePassword 1 \"long\" $p $u $s }}\nb: {{ buildCustomCert $c $k }}\nc: {{ decryptAES $k $v }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Find() = %v, want none", namesOf(got))
	}
}

func TestEveryFlaggedFunctionExplainsWhy(t *testing.T) {
	// The reason is printed next to the finding; a blank one reads as a bug.
	for _, f := range Functions {
		if strings.TrimSpace(f.Why) == "" {
			t.Errorf("%s has no reason", f.Name)
		}
		if Why(f.Name) != f.Why {
			t.Errorf("Why(%q) = %q, want %q", f.Name, Why(f.Name), f.Why)
		}
	}
}

func TestTheFlaggedSetMatchesTheAudit(t *testing.T) {
	// docs/design.md §5 pins this list, audited against sprig v3.3.0 - which
	// both Helm 3.19 and Helm 4.2 pin, so one list covers both lines. If the
	// set changes, that document changes with it.
	//
	// The names, not the count. This asserted len(Functions) == 24 and was
	// VACUOUS for everything the rest of the suite does not happen to name:
	// renaming "htpasswd" to "htpasswdd" - so it is never detected in any
	// chart - left the count at 24 and the whole suite green. About fifteen of
	// the twenty-four were pinned by cardinality alone.
	want := []string{
		"ago",
		"bcrypt",
		"encryptAES",
		"genCA",
		"genCAWithKey",
		"genPrivateKey",
		"genSelfSignedCert",
		"genSelfSignedCertWithKey",
		"genSignedCert",
		"genSignedCertWithKey",
		"getHostByName",
		"htpasswd",
		"keys",
		"lookup",
		"now",
		"randAlpha",
		"randAlphaNum",
		"randAscii",
		"randBytes",
		"randInt",
		"randNumeric",
		"shuffle",
		"uuidv4",
		"values",
	}

	got := make([]string, 0, len(Functions))
	for _, f := range Functions {
		got = append(got, f.Name)
	}
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("Functions = %v,\nwant %v", got, want)
	}
}

func TestFindLocatesAFunctionInsideAVendoredArchive(t *testing.T) {
	dir := chartDir(t, "parent")
	tgz(t, filepath.Join(dir, "charts", "common-2.0.0.tgz"), map[string]string{
		"common/templates/_certs.tpl": "{{- define \"c.crt\" -}}\n{{ genSelfSignedCert $cn nil nil 365 }}\n{{- end -}}\n",
	})

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(got) != 1 || got[0].Function != "genSelfSignedCert" {
		t.Errorf("Find() = %+v, want the archived call", got)
	}
}

func TestPotentialReportsEachFunctionOnce(t *testing.T) {
	// A chart reaching a shared helper ninety times is one thing to look at,
	// not ninety.
	uses := []Use{
		{Function: "randAlphaNum", File: "a.yaml", Line: 1, Call: true},
		{Function: "randAlphaNum", File: "b.yaml", Line: 9, Call: true},
		{Function: "genCA", File: "c.yaml", Line: 3, Call: true},
	}

	got := Potential(uses)

	if len(got) != 2 {
		t.Fatalf("Potential() = %+v, want one per function", got)
	}
}

func TestPotentialPrefersACallSiteOverAMention(t *testing.T) {
	uses := []Use{
		{Function: "now", File: "a.yaml", Line: 1},
		{Function: "now", File: "a.yaml", Line: 8, Call: true},
	}

	got := Potential(uses)

	if len(got) != 1 || got[0].Line != 8 {
		t.Errorf("Potential() = %+v, want the call on line 8", got)
	}
}

func TestPotentialUsesTheFirstCallSiteWhenThereAreSeveral(t *testing.T) {
	uses := []Use{
		{Function: "now", File: "a.yaml", Line: 3, Call: true},
		{Function: "now", File: "b.yaml", Line: 9, Call: true},
	}

	if got := Potential(uses); len(got) != 1 || got[0].Line != 3 {
		t.Errorf("Potential() = %+v, want the first call site", got)
	}
}

func TestPotentialExcludesLookup(t *testing.T) {
	// lookup is what STABILISES a value under an engine that resolves it, and
	// its presence is already reported as the Flux and Helm verdict. Listing
	// it as a potential cause would contradict that verdict on the same screen.
	uses := []Use{
		{Function: Lookup, File: "a.yaml", Line: 1, Call: true},
		{Function: "randAlphaNum", File: "a.yaml", Line: 1, Call: true},
	}

	got := Potential(uses)

	if len(got) != 1 || got[0].Function != "randAlphaNum" {
		t.Errorf("Potential() = %+v, want lookup left out", got)
	}
}

func TestPotentialIsOrderedBySourcePosition(t *testing.T) {
	uses := []Use{
		{Function: "now", File: "z.yaml", Line: 1, Call: true},
		{Function: "genCA", File: "a.yaml", Line: 4, Call: true},
	}

	got := Potential(uses)

	if got[0].File != "a.yaml" {
		t.Errorf("Potential() = %+v, want source order", got)
	}
}

func TestMapOrderFunctionsAreFlagged(t *testing.T) {
	// `keys` and `values` build a SLICE from Go map iteration order and never
	// sort it, so they reorder on every render. Unlike a fresh UUID this looks
	// plausible in a diff, so a human reviewing the output waves it through -
	// which makes it the worst of the set, not the most obscure.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "cm.yaml"),
		"data:\n  a: {{ keys .Values.config | join \",\" }}\n  b: {{ values .Values.config | join \",\" }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	names := namesOf(got)
	for _, want := range []string{"keys", "values"} {
		if !slices.Contains(names, want) {
			t.Errorf("Find() = %v, want %q flagged", names, want)
		}
	}
}

func TestAFieldNamedKeysIsNotMistakenForTheFunction(t *testing.T) {
	// "keys" and "values" are ordinary words. Flagging `.Values.keys` or a
	// YAML field called keys: would bury the real warnings in noise, and a
	// tool that cries wolf about the potential case teaches you to distrust it
	// about the observed one.
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "cm.yaml"),
		"keys:\n  - a\ndata:\n  x: {{ .Values.keys }}\n  y: {{ .Values.values }}\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if len(Potential(got)) != 0 {
		t.Errorf("Potential() = %+v, want none - none of these is a call", Potential(got))
	}
}

func TestPotentialKeepsOnlyCallSites(t *testing.T) {
	// The asymmetry is deliberate and the reverse of lookup's. A false
	// positive in a verdict costs nothing (it downgrades to unknown); a false
	// positive here costs trust. So potential findings take only what reads as
	// a real call, accepting that an unrecognised one is missed - the observed
	// check still catches it if it ever actually fires.
	uses := []Use{
		{Function: "now", File: "a.yaml", Line: 1, Call: false},
		{Function: "genCA", File: "a.yaml", Line: 2, Call: true},
	}

	got := Potential(uses)

	if len(got) != 1 || got[0].Function != "genCA" {
		t.Errorf("Potential() = %+v, want only the call site", got)
	}
}

func TestACallIsRecognisedInEveryOrdinaryTemplatePosition(t *testing.T) {
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "s.yaml"), strings.Join([]string{
		`a: {{ now }}`,
		`b: {{- randAlphaNum 8 }}`,
		`c: {{ $x := uuidv4 }}`,
		`d: {{ .Values.p | default (randBytes 16) }}`,
		`e: {{ if getHostByName "x" }}y{{ end }}`,
	}, "\n")+"\n")

	got, err := Find(dir)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	names := namesOf(Potential(got))
	for _, want := range []string{"now", "randAlphaNum", "uuidv4", "randBytes", "getHostByName"} {
		if !slices.Contains(names, want) {
			t.Errorf("Potential() = %v, want %q recognised as a call", names, want)
		}
	}
}
