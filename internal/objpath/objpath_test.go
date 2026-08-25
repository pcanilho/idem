package objpath

import "testing"

func TestStringRendersDottedForm(t *testing.T) {
	p := Path{Key("spec"), Key("replicas")}
	if got, want := p.String(), ".spec.replicas"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestStringRendersArrayIndices(t *testing.T) {
	p := Path{Key("spec"), Key("containers"), Index(0), Key("image")}
	if got, want := p.String(), ".spec.containers[0].image"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestStringBracketsKeysContainingDots(t *testing.T) {
	// A ConfigMap key like "application.yaml" must not render as two segments,
	// or the displayed path lies about the structure.
	p := Path{Key("data"), Key("application.yaml")}
	if got, want := p.String(), `.data["application.yaml"]`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestStringBracketsKeysContainingSlashes(t *testing.T) {
	p := Path{Key("metadata"), Key("annotations"), Key("checksum/secrets")}
	if got, want := p.String(), `.metadata.annotations["checksum/secrets"]`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestJSONPointerBasic(t *testing.T) {
	p := Path{Key("data"), Key("password")}
	if got, want := p.JSONPointer(), "/data/password"; got != want {
		t.Errorf("JSONPointer() = %q, want %q", got, want)
	}
}

func TestJSONPointerEscapesSlashInKey(t *testing.T) {
	// RFC 6901: "/" inside a key becomes "~1". This is THE case the tool
	// exists for — checksum/secrets is the annotation that makes pods roll.
	p := Path{Key("metadata"), Key("annotations"), Key("checksum/secrets")}
	if got, want := p.JSONPointer(), "/metadata/annotations/checksum~1secrets"; got != want {
		t.Errorf("JSONPointer() = %q, want %q", got, want)
	}
}

func TestJSONPointerEscapesTildeBeforeSlash(t *testing.T) {
	// RFC 6901 requires "~" -> "~0" FIRST, then "/" -> "~1". Doing it the
	// other way round turns "a/b" into "a~01b" via the intermediate "~1".
	p := Path{Key("data"), Key("a~b/c")}
	if got, want := p.JSONPointer(), "/data/a~0b~1c"; got != want {
		t.Errorf("JSONPointer() = %q, want %q", got, want)
	}
}

func TestJSONPointerLeavesDotsAlone(t *testing.T) {
	// A "." is legal in a pointer segment and must NOT be treated as a separator.
	p := Path{Key("data"), Key("application.yaml")}
	if got, want := p.JSONPointer(), "/data/application.yaml"; got != want {
		t.Errorf("JSONPointer() = %q, want %q", got, want)
	}
}

func TestJSONPointerRendersArrayIndices(t *testing.T) {
	p := Path{Key("spec"), Key("containers"), Index(2), Key("image")}
	if got, want := p.JSONPointer(), "/spec/containers/2/image"; got != want {
		t.Errorf("JSONPointer() = %q, want %q", got, want)
	}
}

func TestEmptyPath(t *testing.T) {
	var p Path
	if got := p.String(); got != "" {
		t.Errorf("String() on empty path = %q, want empty", got)
	}
	if got := p.JSONPointer(); got != "" {
		t.Errorf("JSONPointer() on empty path = %q, want empty", got)
	}
}

func TestAppendDoesNotAliasTheParent(t *testing.T) {
	// walk() builds paths by appending as it descends. If Append shares the
	// backing array, sibling branches overwrite each other's segments and
	// every reported path is silently wrong.
	// The parent must have spare capacity, or append() reallocates and the
	// aliasing bug cannot occur. walk() descends through paths built this way.
	base := make(Path, 1, 8)
	base[0] = Key("spec")

	a := base.Append(Key("alpha"))
	b := base.Append(Key("beta"))

	if got, want := a.String(), ".spec.alpha"; got != want {
		t.Errorf("first branch = %q, want %q", got, want)
	}
	if got, want := b.String(), ".spec.beta"; got != want {
		t.Errorf("second branch = %q, want %q (aliasing bug)", got, want)
	}
}
