package chartref

import "testing"

// onDisk builds an existence probe for the given paths, so classification can
// be tested without touching the filesystem.
func onDisk(paths ...string) func(string) bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

func nothingOnDisk(string) bool { return false }

func TestClassifyOCIReference(t *testing.T) {
	got := Classify("oci://registry-1.docker.io/bitnamicharts/postgresql", nothingOnDisk)
	if got.Kind != OCI {
		t.Fatalf("Kind = %v, want OCI", got.Kind)
	}
	if !got.NeedsNetwork() {
		t.Error("an OCI reference needs network")
	}
	if got.SetupHint() != "" {
		t.Errorf("OCI needs no setup, got hint %q", got.SetupHint())
	}
}

func TestClassifyExplicitLocalPaths(t *testing.T) {
	for _, raw := range []string{"./charts/home", "../other/chart", "/abs/chart"} {
		// A leading ./ ../ or / is unambiguous even if the path does not exist:
		// the user clearly meant a path, and "not found" is a better error than
		// silently trying to reach a chart repository.
		got := Classify(raw, nothingOnDisk)
		if got.Kind != Local {
			t.Errorf("Classify(%q).Kind = %v, want Local", raw, got.Kind)
		}
		if got.NeedsNetwork() {
			t.Errorf("Classify(%q) should not need network", raw)
		}
	}
}

func TestClassifyBareDirectoryThatExistsIsLocal(t *testing.T) {
	got := Classify("charts/home", onDisk("charts/home"))
	if got.Kind != Local {
		t.Fatalf("Kind = %v, want Local", got.Kind)
	}
}

func TestClassifyBareTwoPartNameThatIsNotOnDiskIsRepoAlias(t *testing.T) {
	// The ambiguity that matters: "bitnami/postgresql" and "charts/home" are
	// the same shape. Only disk existence separates them.
	got := Classify("bitnami/postgresql", nothingOnDisk)
	if got.Kind != RepoAlias {
		t.Fatalf("Kind = %v, want RepoAlias", got.Kind)
	}
	if got.Repo != "bitnami" || got.Chart != "postgresql" {
		t.Errorf("split = (%q, %q), want (bitnami, postgresql)", got.Repo, got.Chart)
	}
}

func TestRepoAliasCarriesASetupHint(t *testing.T) {
	// A repo alias is the one form that can fail on a clean machine with
	// "Error: repo bitnami not found". The user must be told what to run.
	got := Classify("bitnami/postgresql", nothingOnDisk)
	hint := got.SetupHint()
	if hint == "" {
		t.Fatal("a repo alias must carry a setup hint")
	}
	if !contains(hint, "helm repo add bitnami") {
		t.Errorf("hint %q should name the exact `helm repo add bitnami` command", hint)
	}
}

func TestClassifyLocalArchive(t *testing.T) {
	got := Classify("./postgresql-16.2.tgz", onDisk("./postgresql-16.2.tgz"))
	if got.Kind != Local {
		t.Fatalf("Kind = %v, want Local", got.Kind)
	}
	if got.NeedsNetwork() {
		t.Error("a local archive should not need network")
	}
}

func TestClassifySingleNameWithRepoURLIsRemote(t *testing.T) {
	// `helm template pg postgresql --repo https://...` works with no prior
	// `helm repo add`, which makes it the other zero-setup form besides OCI.
	got := ClassifyWithRepo("postgresql", "https://charts.example.com", nothingOnDisk)
	if got.Kind != RepoURL {
		t.Fatalf("Kind = %v, want RepoURL", got.Kind)
	}
	if got.Repo != "https://charts.example.com" {
		t.Errorf("Repo = %q, want the URL", got.Repo)
	}
	if !got.NeedsNetwork() {
		t.Error("an explicit repo URL needs network")
	}
	if got.SetupHint() != "" {
		t.Errorf("--repo needs no setup, got hint %q", got.SetupHint())
	}
}

func TestRepoURLBeatsAliasInterpretation(t *testing.T) {
	// If --repo is given, "bitnami/postgresql" is not an alias lookup.
	got := ClassifyWithRepo("bitnami/postgresql", "https://charts.example.com", nothingOnDisk)
	if got.Kind != RepoURL {
		t.Fatalf("Kind = %v, want RepoURL", got.Kind)
	}
}

func TestClassifySingleBareNameIsLocal(t *testing.T) {
	// No slash, no --repo: it can only be a directory, present or not.
	got := Classify("mychart", nothingOnDisk)
	if got.Kind != Local {
		t.Fatalf("Kind = %v, want Local", got.Kind)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestClassifyAnyOCIRegistry(t *testing.T) {
	// idem is registry-agnostic by construction: it matches the oci:// scheme
	// and hands the reference to helm. ECR, GHCR, Harbor, GAR, ACR and a
	// self-hosted registry are all the same code path, and authentication is
	// helm's business, not idem's.
	for _, raw := range []string{
		"oci://123456789012.dkr.ecr.eu-west-1.amazonaws.com/charts/api",
		"oci://ghcr.io/pcanilho/charts/idem",
		"oci://harbor.internal.example.com/library/postgresql",
		"oci://europe-docker.pkg.dev/proj/charts/api",
		"oci://myreg.azurecr.io/helm/api",
		"oci://localhost:5000/charts/api",
	} {
		got := Classify(raw, nothingOnDisk)
		if got.Kind != OCI {
			t.Errorf("Classify(%q).Kind = %v, want OCI", raw, got.Kind)
		}
		if got.SetupHint() != "" {
			t.Errorf("Classify(%q) should need no setup hint, got %q", raw, got.SetupHint())
		}
	}
}

func TestOCIWithPortIsNotMistakenForSomethingElse(t *testing.T) {
	// A registry on a non-default port contains a colon and two slashes; none
	// of that may push it down the path or alias branches.
	got := Classify("oci://localhost:5000/charts/api", nothingOnDisk)
	if got.Kind != OCI {
		t.Fatalf("Kind = %v, want OCI", got.Kind)
	}
}
