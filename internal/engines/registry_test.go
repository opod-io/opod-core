package engines

import (
	"strings"
	"testing"
)

type stubEngine struct {
	Engine
	name, endpoint string
}

func (s stubEngine) Name() string     { return s.name }
func (s stubEngine) Endpoint() string { return s.endpoint }

func registerStub(t *testing.T, name string, aliases ...string) Descriptor {
	t.Helper()
	d := Descriptor{
		Name:    name,
		Aliases: aliases,
		New: func(endpoint, apiKey string) Engine {
			return stubEngine{name: name, endpoint: endpoint + "|" + apiKey}
		},
		NativeName: func(src Source) string { return src.Repo },
		StartHint:  "start " + name,
	}
	Register(d)
	return d
}

func TestRegisterLookupAliases(t *testing.T) {
	registerStub(t, "stub-a", "stub-a-alias", "STUB-A-UPPER")
	for _, n := range []string{"stub-a", "stub-a-alias", " Stub-A ", "stub-a-upper"} {
		d, ok := Lookup(n)
		if !ok || d.Name != "stub-a" {
			t.Errorf("Lookup(%q) = (%q, %v), want stub-a", n, d.Name, ok)
		}
	}
	if got := Canonical("stub-a-alias"); got != "stub-a" {
		t.Errorf("Canonical = %q", got)
	}
	if got := Canonical("nobody"); got != "nobody" {
		t.Errorf("Canonical(unknown) = %q, want passthrough", got)
	}
	found := false
	for _, n := range Names() {
		if n == "stub-a" {
			found = true
		}
		if n == "stub-a-alias" {
			t.Error("Names() must list canonical names only")
		}
	}
	if !found {
		t.Error("Names() lacks stub-a")
	}
}

func TestNewWithAuthForwardsAndErrors(t *testing.T) {
	registerStub(t, "stub-b")
	eng, err := NewWithAuth("stub-b", "http://x", "k")
	if err != nil {
		t.Fatal(err)
	}
	if eng.Name() != "stub-b" || eng.Endpoint() != "http://x|k" {
		t.Errorf("factory not invoked with args: %q %q", eng.Name(), eng.Endpoint())
	}
	if _, err := New("stub-nope", "http://x"); err == nil || !strings.Contains(err.Error(), "stub-b") {
		t.Errorf("unknown engine error must list linked drivers, got %v", err)
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	registerStub(t, "stub-c", "stub-c2")
	for _, dup := range []string{"stub-c", "stub-c2"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) again must panic", dup)
				}
			}()
			Register(Descriptor{Name: dup, New: func(string, string) Engine { return nil }})
		}()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Register without New must panic")
			}
		}()
		Register(Descriptor{Name: "stub-no-new"})
	}()
}

func TestNativeNameAndCatalogID(t *testing.T) {
	registerStub(t, "stub-d")
	src := Source{ID: "cat", Repo: "org/repo", OllamaName: "cat:latest", Path: "/w/cat.gguf"}
	if got := NativeName("stub-d", src); got != "org/repo" {
		t.Errorf("NativeName = %q", got)
	}
	if got := NativeName("stub-d", Source{ID: "only-id"}); got != "only-id" {
		t.Errorf("NativeName fallback = %q, want catalog id", got)
	}
	if got := NativeName("unknown-engine", src); got != "cat" {
		t.Errorf("NativeName(unknown) = %q, want catalog id", got)
	}
	sources := []Source{src, {ID: "other", Repo: "o/o"}}
	cases := map[string]string{
		"cat":         "cat", // catalog id itself (vLLM lists it too)
		"org/repo":    "cat", // the driver's native name
		"cat:latest":  "cat", // any source field as last resort
		"/w/cat.gguf": "cat",
		"o/o":         "other",
		"custom:x":    "custom:x", // unknown passes through
	}
	for native, want := range cases {
		if got := CatalogID("stub-d", native, sources); got != want {
			t.Errorf("CatalogID(stub-d, %q) = %q, want %q", native, got, want)
		}
		if got := CatalogID("", native, sources); got != want {
			t.Errorf("CatalogID(any, %q) = %q, want %q", native, got, want)
		}
	}
}

func TestStartHint(t *testing.T) {
	registerStub(t, "stub-e")
	if got := StartHint("stub-e"); got != "start stub-e" {
		t.Errorf("StartHint = %q", got)
	}
	if got := StartHint("nope"); !strings.Contains(got, "opod status") {
		t.Errorf("StartHint(unknown) = %q, want generic", got)
	}
}
