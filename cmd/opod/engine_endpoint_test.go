package main

import (
	"testing"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
)

// Every driver this binary links must be startable: `opod up` and `opod join`
// build their engine through engineEndpoint, and a driver the registry knows
// but this function does not is refused at start with an error that lists it as
// valid. That is what happened to sglang — found only when a real SGLang
// endpoint was created and its leader crash-looped.
func TestEveryLinkedEngineHasAnEndpoint(t *testing.T) {
	cfg := config.Default()
	for _, name := range engines.Names() {
		canonical, endpoint, _, ok := engineEndpoint(cfg, name)
		if !ok {
			t.Errorf("engine %q is a linked driver and engineEndpoint does not know it", name)
			continue
		}
		if canonical != name {
			t.Errorf("engines.Names() returned %q, which canonicalises to %q", name, canonical)
		}
		if endpoint == "" {
			t.Errorf("engine %q has no default endpoint in config.Default()", name)
		}
		if _, err := engines.NewWithAuth(name, endpoint, ""); err != nil {
			t.Errorf("engine %q does not construct from its default endpoint %q: %v", name, endpoint, err)
		}
	}
}

// An alias is spelled by the driver that declares it, never again here.
func TestEngineAliasesResolveToTheirDriversEndpoint(t *testing.T) {
	cfg := config.Default()
	for _, name := range engines.Names() {
		d, ok := engines.Lookup(name)
		if !ok {
			t.Fatalf("no descriptor for %q", name)
		}
		_, want, _, _ := engineEndpoint(cfg, name)
		for _, alias := range d.Aliases {
			canonical, got, _, ok := engineEndpoint(cfg, alias)
			if !ok || canonical != name || got != want {
				t.Errorf("alias %q of %q: canonical=%q endpoint=%q ok=%v, want %q at %q", alias, name, canonical, got, ok, name, want)
			}
		}
	}
	if _, _, _, ok := engineEndpoint(cfg, "no-such-engine"); ok {
		t.Error("an unknown engine must not resolve")
	}
}
