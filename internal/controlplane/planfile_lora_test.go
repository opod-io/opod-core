package controlplane

import (
	"strings"
	"testing"
)

// A LoRA adapter is a variant of the plan's one identity (R9.2): the
// "<model>:<adapter>" id passes the single-model gate; anything else does not.
func TestPlanAllowsAdapterVariants(t *testing.T) {
	s := &Server{}
	s.plan.modelID = "mimo-7b"
	for id, want := range map[string]bool{"mimo-7b": true, "mimo-7b:sql": true, "mimo-7b:": false, "other": false, "mimo-7b-x": false, "other:mimo-7b": false} {
		if _, ok := s.planAllowsModel(id); ok != want {
			t.Errorf("%q → %v, want %v", id, ok, want)
		}
	}
}

// R15.15 · the plan names its adapters, so an unknown suffix is refused by the
// leader rather than discovered by the router as a missing placement. The
// refusal has to name what DOES exist — "model:typo not found" with no list is
// the same dead end from one layer up.
func TestUnknownAdapterSuffixIsRefusedByName(t *testing.T) {
	s := &Server{}
	s.plan.modelID = "qwen2.5-7b"
	s.plan.adapters = []string{"support", "legal"}
	s.plan.present = true

	if _, ok := s.planAllowsModel("qwen2.5-7b:support"); !ok {
		t.Fatal("a declared adapter was refused")
	}
	msg, ok := s.planAllowsModel("qwen2.5-7b:supprt")
	if ok {
		t.Fatal("a typo in the adapter name passed the gate")
	}
	if !strings.Contains(msg, "support") || !strings.Contains(msg, "legal") {
		t.Fatalf("the refusal does not name the adapters that exist: %q", msg)
	}
}

// A plan that carries no adapters field at all is an OLDER plan document, not
// a statement that there are none: refusing there would break every endpoint
// whose control plane has not been upgraded.
func TestPlanWithNoAdapterListStillAllowsASuffix(t *testing.T) {
	s := &Server{}
	s.plan.modelID = "qwen2.5-7b"
	s.plan.present = true
	if _, ok := s.planAllowsModel("qwen2.5-7b:support"); !ok {
		t.Fatal("a suffix was refused by a plan that declares no adapter set")
	}
}
