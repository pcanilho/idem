package lookup

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
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

func TestFindReportsNothingForAChartThatNeverCallsLookup(t *testing.T) {
	dir := chartDir(t, "acme")
	write(t, filepath.Join(dir, "templates", "secret.yaml"),
		"data:\n  key: {{ randAlphaNum 32 | b64enc }}\n")

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
