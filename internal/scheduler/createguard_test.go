package scheduler

import (
	"errors"
	"strings"
	"testing"
)

// Two creates of one model's gangs must not run at once.
//
// Every create starts by tearing down what it replaces, so a second one does
// not queue behind the first — it stops the first's rpc-servers and coordinator
// as orphans, and the first then fails polling a process that is gone. On the
// design-partner cell (2026-09-21) a control plane retrying a create and this
// leader's own heal loop made exactly that cycle, and a two-part gang never
// formed in twelve minutes of trying.
func TestOneCreatePerModelAtATime(t *testing.T) {
	var g createGuard

	release, err := g.claim("llama-3.2-3b-sharded", "g0")
	if err != nil {
		t.Fatalf("first create refused: %v", err)
	}

	// A second create of the SAME gang is refused, and says what holds it and why.
	_, err = g.claim("llama-3.2-3b-sharded", "g0")
	if !errors.Is(err, ErrCreateInFlight) {
		t.Fatalf("second create of the same gang: %v — must wrap ErrCreateInFlight", err)
	}
	if !strings.Contains(err.Error(), "gang g0") || !strings.Contains(err.Error(), "torn down") {
		t.Errorf("the refusal must name what holds it and what a second create would do: %q", err)
	}
	// A create of a SIBLING gang runs alongside: it replaces only its own
	// gang's parts, so nothing of g0's is torn down. Keyed by the model alone,
	// g1's stuck create stranded g0 on the design-partner cell (2026-09-29).
	siblingRelease, err := g.claim("llama-3.2-3b-sharded", "g1")
	if err != nil {
		t.Fatalf("a sibling gang's create was refused while g0 forms: %v", err)
	}
	// A whole-model create waits for both, and while it runs nothing else of
	// the model may start.
	if _, err := g.claim("llama-3.2-3b-sharded", ""); !errors.Is(err, ErrCreateInFlight) {
		t.Fatalf("a whole-model create must wait for the named ones: %v", err)
	}
	siblingRelease()

	// Another model is untouched: the guard is per model, not a global lock.
	otherRelease, err := g.claim("mimo-7b", "g0")
	if err != nil {
		t.Fatalf("a create for a different model was refused: %v", err)
	}
	otherRelease()

	// And the model is free again once the create ends.
	release()
	again, err := g.claim("llama-3.2-3b-sharded", "")
	if err != nil {
		t.Fatalf("the model stayed claimed after its create returned: %v", err)
	}
	if _, err := g.claim("llama-3.2-3b-sharded", "g1"); !errors.Is(err, ErrCreateInFlight) {
		t.Fatalf("a named create must wait for a whole-model create: %v", err)
	}
	again()
}

// A bare create replaces every gang, so its hold says so — an operator reading
// the refusal should not think only one gang is busy.
func TestABareCreateHoldsTheWholeModel(t *testing.T) {
	var g createGuard
	release, err := g.claim("llama-3.2-3b-sharded", "")
	if err != nil {
		t.Fatalf("bare create refused: %v", err)
	}
	defer release()
	if _, err := g.claim("llama-3.2-3b-sharded", "g0"); err == nil || !strings.Contains(err.Error(), "the whole model") {
		t.Fatalf("refusal = %v; a bare create holds the whole model and must say so", err)
	}
}
