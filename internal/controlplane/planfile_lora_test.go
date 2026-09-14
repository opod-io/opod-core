package controlplane

import "testing"

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
